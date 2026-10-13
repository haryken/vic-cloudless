package xiaozhi

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"

	"github.com/gorilla/websocket"

	"github.com/digital-dream-labs/vector-cloud/internal/log"
	"github.com/digital-dream-labs/vector-cloud/internal/xiaozhi/listenvad"
)

// Mode A: OpenAI Realtime API over one WebSocket. Server VAD ends the user's
// turn, the model answers with audio and calls self.* tools as functions.
// Models named "*preview*" speak the beta protocol. The Grok backend (xAI
// Voice Agent) speaks the same events with its own session.update shape.

const (
	rtSampleRate     = 24000
	rtSilenceMs      = 700
	rtFirstEventWait = 25 * time.Second
	rtEventGap       = 15 * time.Second
)

type rtEvent struct {
	Type string
	Raw  []byte
}

type rtConn struct {
	ws     *websocket.Conn
	beta   bool
	tag    string
	sig    string
	wmu    sync.Mutex
	events chan rtEvent
	// stopped carries server end-of-speech to listenvad.Capture.
	stopped chan string
	errCh   chan error
	dead    chan struct{}
	deadErr error
	once    sync.Once
}

func (c *rtConn) fail(err error) {
	c.once.Do(func() {
		c.deadErr = err
		close(c.dead)
		select {
		case c.errCh <- err:
		default:
		}
		_ = c.ws.Close()
	})
}

func (c *rtConn) close() { c.fail(errors.New("closed")) }

func (c *rtConn) alive() bool {
	select {
	case <-c.dead:
		return false
	default:
		return true
	}
}

func (c *rtConn) send(v interface{}) error {
	data, err := json.Marshal(v)
	if err != nil {
		return err
	}
	c.wmu.Lock()
	defer c.wmu.Unlock()
	_ = c.ws.SetWriteDeadline(time.Now().Add(5 * time.Second))
	if err := c.ws.WriteMessage(websocket.TextMessage, data); err != nil {
		c.fail(err)
		return err
	}
	return nil
}

func (c *rtConn) readLoop() {
	for {
		_, data, err := c.ws.ReadMessage()
		if err != nil {
			c.fail(err)
			return
		}
		var head struct {
			Type string `json:"type"`
		}
		if json.Unmarshal(data, &head) != nil {
			continue
		}
		if head.Type == "input_audio_buffer.speech_stopped" {
			select {
			case c.stopped <- "":
			default:
			}
		}
		select {
		case c.events <- rtEvent{Type: head.Type, Raw: data}:
		default:
			log.Println(c.tag, "event queue full — dropped", head.Type)
		}
	}
}

// drain drops events left over from the previous turn.
func (c *rtConn) drain() {
	for {
		select {
		case <-c.events:
		case <-c.stopped:
		default:
			return
		}
	}
}

func rtIsBeta(model string) bool {
	return strings.Contains(strings.ToLower(model), "preview")
}

var grokRealtimeURL = "wss://api.x.ai/v1/realtime"

// rtTarget is the realtime server: OpenAI (mode A) or xAI Grok.
type rtTarget struct {
	grok  bool
	url   string
	key   string
	model string
	voice string
	tag   string
}

func rtTargetFor(backend string, ocfg OpenAIConfig) rtTarget {
	if backend == BackendGrok {
		return rtTarget{grok: true, url: grokRealtimeURL + "?model=" + url.QueryEscape(ocfg.GrokModel),
			key: ocfg.XAIAPIKey, model: ocfg.GrokModel, voice: ocfg.GrokVoice, tag: "[Grok]"}
	}
	return rtTarget{url: rtURL(ocfg), key: ocfg.APIKey, model: ocfg.RealtimeModel, voice: ocfg.Voice, tag: "[OpenAI][RT]"}
}

func rtSignature(cfg Config, ocfg OpenAIConfig, t rtTarget) string {
	var names []string
	for _, s := range oaiToolSpecs(cfg) {
		names = append(names, s.Name)
	}
	return strings.Join([]string{t.key, t.url, t.voice,
		ocfg.Language, ocfg.Instructions, ocfg.STTModel, strings.Join(names, ",")}, "|")
}

// grokNoSearch remembers models whose session refused the web_search tool.
var grokNoSearch sync.Map

// grokSessionUpdate follows the xAI Voice Agent session schema: voice and
// turn_detection at the top, reasoning off for a fast spoken reply.
func grokSessionUpdate(cfg Config, ocfg OpenAIConfig, t rtTarget, instructions string, search bool) map[string]interface{} {
	input := map[string]interface{}{
		"format": map[string]interface{}{"type": "audio/pcm", "rate": rtSampleRate},
	}
	if lang := ocfg.sttLanguage(); lang != "" {
		input["transcription"] = map[string]interface{}{"language_hint": lang}
	}
	tools := oaiRealtimeTools(cfg)
	if search {
		tools = append(tools, map[string]interface{}{"type": "web_search"})
	}
	return map[string]interface{}{
		"type": "session.update",
		"session": map[string]interface{}{
			"instructions": instructions,
			"voice":        t.voice,
			"reasoning":    map[string]interface{}{"effort": "none"},
			"turn_detection": map[string]interface{}{
				"type":                "server_vad",
				"threshold":           0.5,
				"prefix_padding_ms":   300,
				"silence_duration_ms": rtSilenceMs,
			},
			"audio": map[string]interface{}{
				"input":  input,
				"output": map[string]interface{}{"format": map[string]interface{}{"type": "audio/pcm", "rate": rtSampleRate}},
			},
			"tools": tools,
		},
	}
}

func rtSessionUpdate(cfg Config, ocfg OpenAIConfig, beta bool, instructions string) map[string]interface{} {
	turn := map[string]interface{}{
		"type":                "server_vad",
		"threshold":           0.5,
		"prefix_padding_ms":   300,
		"silence_duration_ms": rtSilenceMs,
		"create_response":     true,
		// Vector's mic is closed while it speaks; barge-in is the wake word.
		"interrupt_response": false,
	}
	transcription := map[string]interface{}{"model": ocfg.STTModel}
	if lang := ocfg.sttLanguage(); lang != "" {
		transcription["language"] = lang
	}
	tools := oaiRealtimeTools(cfg)
	if beta {
		return map[string]interface{}{
			"type": "session.update",
			"session": map[string]interface{}{
				"modalities":                []string{"audio", "text"},
				"instructions":              instructions,
				"voice":                     ocfg.Voice,
				"input_audio_format":        "pcm16",
				"output_audio_format":       "pcm16",
				"input_audio_transcription": transcription,
				"turn_detection":            turn,
				"tools":                     tools,
				"tool_choice":               "auto",
			},
		}
	}
	return map[string]interface{}{
		"type": "session.update",
		"session": map[string]interface{}{
			"type":              "realtime",
			"instructions":      instructions,
			"output_modalities": []string{"audio"},
			"audio": map[string]interface{}{
				"input": map[string]interface{}{
					"format":          map[string]interface{}{"type": "audio/pcm", "rate": rtSampleRate},
					"transcription":   transcription,
					"turn_detection":  turn,
					"noise_reduction": map[string]interface{}{"type": "far_field"},
				},
				"output": map[string]interface{}{
					"format": map[string]interface{}{"type": "audio/pcm", "rate": rtSampleRate},
					"voice":  ocfg.Voice,
				},
			},
			"tools":       tools,
			"tool_choice": "auto",
		},
	}
}

func rtURL(ocfg OpenAIConfig) string {
	base := ocfg.BaseURL
	switch {
	case strings.HasPrefix(base, "https://"):
		base = "wss://" + strings.TrimPrefix(base, "https://")
	case strings.HasPrefix(base, "http://"):
		base = "ws://" + strings.TrimPrefix(base, "http://")
	}
	return base + "/realtime?model=" + url.QueryEscape(ocfg.RealtimeModel)
}

func dialRealtime(ctx context.Context, cfg Config, ocfg OpenAIConfig, t rtTarget) (*rtConn, error) {
	if !t.grok {
		return dialRealtimeOnce(ctx, cfg, ocfg, t, false)
	}
	if _, no := grokNoSearch.Load(t.model); no {
		return dialRealtimeOnce(ctx, cfg, ocfg, t, false)
	}
	c, err := dialRealtimeOnce(ctx, cfg, ocfg, t, true)
	if err == nil || !errors.Is(err, errRTSessionRejected) || ctx.Err() != nil {
		return c, err
	}
	c, err2 := dialRealtimeOnce(ctx, cfg, ocfg, t, false)
	if err2 != nil {
		return nil, err
	}
	log.Printf("%s session refused web_search (%v) — continuing without it", t.tag, err)
	grokNoSearch.Store(t.model, true)
	return c, nil
}

var errRTSessionRejected = errors.New("session.update rejected")

func dialRealtimeOnce(ctx context.Context, cfg Config, ocfg OpenAIConfig, t rtTarget, search bool) (*rtConn, error) {
	beta := !t.grok && rtIsBeta(t.model)
	h := http.Header{}
	h.Set("Authorization", "Bearer "+t.key)
	if beta {
		h.Set("OpenAI-Beta", "realtime=v1")
	}
	d := websocket.Dialer{HandshakeTimeout: 10 * time.Second, Proxy: http.ProxyFromEnvironment}
	dctx, cancel := context.WithTimeout(ctx, 12*time.Second)
	defer cancel()
	started := time.Now()
	ws, resp, err := d.DialContext(dctx, t.url, h)
	if err != nil {
		if resp != nil {
			body, _ := io.ReadAll(io.LimitReader(resp.Body, 400))
			resp.Body.Close()
			return nil, fmt.Errorf("realtime dial: %s %s", resp.Status, strings.TrimSpace(string(body)))
		}
		return nil, fmt.Errorf("realtime dial: %w", err)
	}
	ws.SetReadLimit(8 << 20)
	c := &rtConn{
		ws:      ws,
		beta:    beta,
		tag:     t.tag,
		sig:     rtSignature(cfg, ocfg, t),
		events:  make(chan rtEvent, 4096),
		stopped: make(chan string, 1),
		errCh:   make(chan error, 1),
		dead:    make(chan struct{}),
	}
	go c.readLoop()

	mem := oaiMemoryTail(loadOAIMemory(), ocfg.MemoryTurns)
	instructions := oaiInstructions(ocfg, mem)
	update := rtSessionUpdate(cfg, ocfg, beta, instructions)
	if t.grok {
		update = grokSessionUpdate(cfg, ocfg, t, instructions, search)
	}
	if err := c.send(update); err != nil {
		c.close()
		return nil, fmt.Errorf("realtime session.update: %w", err)
	}
	wait := time.NewTimer(8 * time.Second)
	defer wait.Stop()
	for {
		select {
		case ev := <-c.events:
			switch ev.Type {
			case "session.updated":
				log.Printf("%s session ready model=%s beta=%v voice=%s memory=%d search=%v (+%v)",
					t.tag, t.model, beta, t.voice, len(mem), search, time.Since(started).Round(time.Millisecond))
				return c, nil
			case "error":
				msg, _ := rtErrorOf(ev.Raw)
				c.close()
				return nil, fmt.Errorf("realtime session: %w: %s", errRTSessionRejected, msg)
			}
		case <-c.dead:
			return nil, fmt.Errorf("realtime closed during setup: %v", c.deadErr)
		case <-wait.C:
			c.close()
			return nil, fmt.Errorf("realtime: no session.updated")
		case <-ctx.Done():
			c.close()
			return nil, ctx.Err()
		}
	}
}

// rtEnsure reuses the session socket while settings and tools are unchanged.
func rtEnsure(ctx context.Context, cfg Config, ocfg OpenAIConfig, t rtTarget) (*rtConn, error) {
	sig := rtSignature(cfg, ocfg, t)
	oaiSess.mu.Lock()
	c := oaiSess.rt
	oaiSess.mu.Unlock()
	if c != nil && c.alive() && c.sig == sig {
		return c, nil
	}
	if c != nil {
		c.close()
	}
	c, err := dialRealtime(ctx, cfg, ocfg, t)
	if err != nil {
		return nil, err
	}
	oaiSess.mu.Lock()
	oaiSess.rt = c
	oaiSess.mu.Unlock()
	return c, nil
}

func rtErrorOf(raw []byte) (msg, code string) {
	var e struct {
		Error struct {
			Message string `json:"message"`
			Code    string `json:"code"`
			Type    string `json:"type"`
		} `json:"error"`
	}
	_ = json.Unmarshal(raw, &e)
	msg = e.Error.Message
	if msg == "" {
		msg = string(raw)
	}
	return msg, e.Error.Code
}

// rtUplink adapts the socket to listenvad.Capture.
type rtUplink struct{ c *rtConn }

func (u rtUplink) SendAudio(pcm16k []byte) error {
	pcm24 := resampleS16(pcm16k, 16000, rtSampleRate)
	return u.c.send(map[string]interface{}{
		"type":  "input_audio_buffer.append",
		"audio": base64.StdEncoding.EncodeToString(pcm24),
	})
}
func (u rtUplink) ListenStop() error    { return nil }
func (u rtUplink) STTCh() <-chan string { return u.c.stopped }
func (u rtUplink) ErrCh() <-chan error  { return u.c.errCh }

func runRealtimeTurn(ctx context.Context, audio <-chan []byte, cfg Config, ocfg OpenAIConfig,
	afterSTT func(string), detectText string, st *oaiTurnState) error {
	t := rtTargetFor(cfg.Backend, ocfg)
	c, err := rtEnsure(ctx, cfg, ocfg, t)
	if err != nil {
		if t.grok {
			return fmt.Errorf("grok %w", err)
		}
		return fmt.Errorf("openai %w", err)
	}
	c.drain()

	if detectText != "" {
		st.stt = detectText
		if err := c.send(map[string]interface{}{
			"type": "conversation.item.create",
			"item": map[string]interface{}{
				"type": "message", "role": "user",
				"content": []map[string]interface{}{{"type": "input_text", "text": detectText}},
			},
		}); err != nil {
			return fmt.Errorf("%w: %v", errOAISessionBroken, err)
		}
		if err := c.send(map[string]interface{}{"type": "response.create"}); err != nil {
			return fmt.Errorf("%w: %v", errOAISessionBroken, err)
		}
		log.Printf(c.tag+" detect %q sent", detectText)
	} else {
		_ = c.send(map[string]interface{}{"type": "input_audio_buffer.clear"})
		log.Println(c.tag, "listening (server VAD)")
		res, err := oaiCapture(ctx, rtUplink{c}, audio, 0)
		if errors.Is(err, ErrNoSpeech) {
			_ = c.send(map[string]interface{}{"type": "input_audio_buffer.clear"})
			return err
		}
		if err != nil {
			if errors.Is(err, listenvad.ErrServer) || errors.Is(err, listenvad.ErrSend) {
				return fmt.Errorf("%w: %v", errOAISessionBroken, err)
			}
			return err
		}
		if afterSTT != nil {
			afterSTT("")
		}
		if res.Reason != listenvad.ReasonServerSTT {
			// Mic closed or hit the cap before server VAD fired: answer what we have.
			_ = c.send(map[string]interface{}{"type": "input_audio_buffer.commit"})
			_ = c.send(map[string]interface{}{"type": "response.create"})
		}
	}
	return rtAwaitResponse(ctx, c, detectText != "", st)
}

func rtAwaitResponse(ctx context.Context, c *rtConn, detect bool, st *oaiTurnState) error {
	var carry []byte
	followUp := false
	responding := false
	audioMuted := false
	waitStart := time.Now()
	timer := time.NewTimer(rtFirstEventWait)
	defer timer.Stop()

	for {
		select {
		case <-ctx.Done():
			_ = c.send(map[string]interface{}{"type": "response.cancel"})
			return ctx.Err()
		case <-c.dead:
			return fmt.Errorf("%w: %v", errOAISessionBroken, c.deadErr)
		case <-timer.C:
			_ = c.send(map[string]interface{}{"type": "response.cancel"})
			return fmt.Errorf("openai realtime: reply timed out after %v", time.Since(waitStart).Round(time.Second))
		case ev := <-c.events:
			if !timer.Stop() {
				select {
				case <-timer.C:
				default:
				}
			}
			timer.Reset(rtEventGap)

			switch ev.Type {
			case "response.created":
				responding = true
			case "response.output_audio.delta", "response.audio.delta":
				if audioMuted {
					continue
				}
				raw, ok := rtAudioDelta(ev.Raw)
				if !ok {
					continue
				}
				first := !st.speaker.started
				if err := st.speaker.write(down24to16(&carry, raw)); err != nil {
					log.Println(c.tag, "speaker:", err)
					audioMuted = true
					_ = c.send(map[string]interface{}{"type": "response.cancel"})
					continue
				}
				if first && st.speaker.started {
					log.Printf(c.tag+" playing +%v", time.Since(waitStart).Round(time.Millisecond))
				}
			case "response.output_audio_transcript.delta", "response.audio_transcript.delta":
				var d struct {
					Delta string `json:"delta"`
				}
				if json.Unmarshal(ev.Raw, &d) == nil {
					st.reply.WriteString(d.Delta)
				}
			case "conversation.item.input_audio_transcription.completed":
				var d struct {
					Transcript string `json:"transcript"`
				}
				if json.Unmarshal(ev.Raw, &d) == nil && !detect {
					st.stt = strings.TrimSpace(d.Transcript)
					log.Printf(c.tag+" STT: %q", st.stt)
				}
			case "response.function_call_arguments.done":
				var d struct {
					CallID    string `json:"call_id"`
					Name      string `json:"name"`
					Arguments string `json:"arguments"`
				}
				if json.Unmarshal(ev.Raw, &d) != nil {
					continue
				}
				r := runOAITool(d.Name, d.Arguments)
				if r.End {
					st.end = true
				}
				_ = c.send(map[string]interface{}{
					"type": "conversation.item.create",
					"item": map[string]interface{}{
						"type": "function_call_output", "call_id": d.CallID, "output": r.Output,
					},
				})
				// Goodbye is usually spoken before the end call; don't add more.
				if !r.End || !st.speaker.started {
					followUp = true
				}
			case "response.done":
				responding = false
				var d struct {
					Response struct {
						Status        string          `json:"status"`
						StatusDetails json.RawMessage `json:"status_details"`
					} `json:"response"`
				}
				_ = json.Unmarshal(ev.Raw, &d)
				if d.Response.Status == "failed" {
					log.Printf(c.tag+" response failed: %s", truncate(string(d.Response.StatusDetails), 300))
				}
				if followUp && !audioMuted && d.Response.Status != "failed" {
					followUp = false
					if err := c.send(map[string]interface{}{"type": "response.create"}); err != nil {
						return fmt.Errorf("%w: %v", errOAISessionBroken, err)
					}
					continue
				}
				if st.stt == "" && !detect {
					rtWaitTranscript(c, st)
				}
				if d.Response.Status == "failed" && !st.speaker.started {
					return fmt.Errorf("openai realtime response failed: %s", truncate(string(d.Response.StatusDetails), 200))
				}
				return nil
			case "error":
				msg, code := rtErrorOf(ev.Raw)
				switch code {
				case "response_cancel_not_active", "conversation_already_has_active_response":
					continue
				case "input_audio_buffer_commit_empty":
					if !responding && !st.speaker.started {
						return ErrNoSpeech
					}
					continue
				}
				log.Printf(c.tag+" server error %s: %s", code, msg)
				if !responding && !st.speaker.started {
					return fmt.Errorf("openai realtime: %s", msg)
				}
			}
		}
	}
}

// rtAudioDelta decodes an output audio event; xAI may name the field "audio".
func rtAudioDelta(raw []byte) ([]byte, bool) {
	var d struct {
		Delta string `json:"delta"`
		Audio string `json:"audio"`
	}
	if json.Unmarshal(raw, &d) != nil {
		return nil, false
	}
	if d.Delta == "" {
		d.Delta = d.Audio
	}
	pcm, err := base64.StdEncoding.DecodeString(d.Delta)
	if err != nil || len(pcm) == 0 {
		return nil, false
	}
	return pcm, true
}

// rtWaitTranscript gives the input transcription a moment to land after the
// reply (it is generated in parallel and only feeds memory/logs).
func rtWaitTranscript(c *rtConn, st *oaiTurnState) {
	deadline := time.NewTimer(1500 * time.Millisecond)
	defer deadline.Stop()
	for {
		select {
		case ev := <-c.events:
			if ev.Type == "conversation.item.input_audio_transcription.completed" {
				var d struct {
					Transcript string `json:"transcript"`
				}
				if json.Unmarshal(ev.Raw, &d) == nil {
					st.stt = strings.TrimSpace(d.Transcript)
					log.Printf(c.tag+" STT: %q", st.stt)
				}
				return
			}
		case <-deadline.C:
			return
		case <-c.dead:
			return
		}
	}
}
