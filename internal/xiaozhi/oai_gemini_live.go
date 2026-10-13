package xiaozhi

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/gorilla/websocket"

	"github.com/digital-dream-labs/vector-cloud/internal/log"
	"github.com/digital-dream-labs/vector-cloud/internal/xiaozhi/listenvad"
)

// Gemini Live backend: one BidiGenerateContent WebSocket per conversation.
// Server VAD is off; Vector's local gate brackets each utterance with
// activityStart/activityEnd, so the mic closes on end-of-speech and silent
// wakes never reach the model. Replies are 24 kHz audio plus transcripts;
// self.* tools are blocking function calls.

const (
	glOutputRate     = 24000
	glFirstEventWait = 25 * time.Second
	glEventGap       = 15 * time.Second
	// After a tool response the model usually keeps talking in the same
	// turn; give a turnComplete that arrives first a moment to be followed.
	glToolGrace = 4 * time.Second
	// Mic audio kept from before the gate fired (it lags speech onset).
	glPreRollBytes = 16000 * 2 * 3 / 2
)

var geminiLiveURL = "wss://" + geminiAPIHost + "/ws/google.ai.generativelanguage.v1beta.GenerativeService.BidiGenerateContent"

type glConn struct {
	ws      *websocket.Conn
	sig     string
	wmu     sync.Mutex
	msgs    chan []byte
	errCh   chan error
	dead    chan struct{}
	deadErr error
	once    sync.Once

	mu       sync.Mutex
	resume   string
	goingOff bool
}

func (c *glConn) fail(err error) {
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

func (c *glConn) close() { c.fail(errors.New("closed")) }

func (c *glConn) alive() bool {
	select {
	case <-c.dead:
		return false
	default:
		return true
	}
}

// reusable: alive and the server has not announced a disconnect (goAway).
func (c *glConn) reusable() bool {
	c.mu.Lock()
	off := c.goingOff
	c.mu.Unlock()
	return !off && c.alive()
}

func (c *glConn) resumeHandle() string {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.resume
}

func (c *glConn) send(v interface{}) error {
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

func (c *glConn) sendAudio(pcm16k []byte) error {
	return c.send(map[string]interface{}{
		"realtimeInput": map[string]interface{}{
			"audio": map[string]interface{}{
				"data":     base64.StdEncoding.EncodeToString(pcm16k),
				"mimeType": "audio/pcm;rate=16000",
			},
		},
	})
}

func (c *glConn) sendActivity(kind string) error {
	return c.send(map[string]interface{}{
		"realtimeInput": map[string]interface{}{kind: map[string]interface{}{}},
	})
}

// readLoop queues server messages; session bookkeeping (resumption handle,
// goAway) is applied here so it also lands between turns.
func (c *glConn) readLoop() {
	for {
		_, data, err := c.ws.ReadMessage()
		if err != nil {
			c.fail(err)
			return
		}
		var head struct {
			SessionResumptionUpdate *struct {
				NewHandle string `json:"newHandle"`
				Resumable bool   `json:"resumable"`
			} `json:"sessionResumptionUpdate"`
			GoAway *struct {
				TimeLeft string `json:"timeLeft"`
			} `json:"goAway"`
		}
		if json.Unmarshal(data, &head) != nil {
			continue
		}
		if u := head.SessionResumptionUpdate; u != nil {
			if u.Resumable && u.NewHandle != "" {
				c.mu.Lock()
				c.resume = u.NewHandle
				c.mu.Unlock()
			}
			continue
		}
		if head.GoAway != nil {
			log.Println("[Gemini][Live] server goAway, time left", head.GoAway.TimeLeft)
			c.mu.Lock()
			c.goingOff = true
			c.mu.Unlock()
			continue
		}
		select {
		case c.msgs <- data:
		default:
			log.Println("[Gemini][Live] message queue full — dropped")
		}
	}
}

// drain drops messages left over from the previous turn.
func (c *glConn) drain() {
	for {
		select {
		case <-c.msgs:
		default:
			return
		}
	}
}

func glModelName(ocfg OpenAIConfig) string {
	return "models/" + strings.TrimPrefix(ocfg.GeminiLiveModel, "models/")
}

func glSignature(cfg Config, ocfg OpenAIConfig) string {
	var names []string
	for _, s := range oaiToolSpecs(cfg) {
		names = append(names, s.Name)
	}
	return strings.Join([]string{ocfg.geminiKey(), ocfg.GeminiLiveModel, ocfg.GeminiLiveVoice,
		ocfg.Language, ocfg.Instructions, strings.Join(names, ",")}, "|")
}

// glSchemaKeys is the JSON Schema subset Gemini function parameters accept.
var glSchemaKeys = map[string]bool{
	"type": true, "description": true, "enum": true, "properties": true, "required": true,
	"items": true, "minimum": true, "maximum": true, "format": true, "nullable": true,
	"minItems": true, "maxItems": true, "minLength": true, "maxLength": true, "pattern": true,
}

func glSchema(v interface{}) interface{} {
	m, ok := v.(map[string]interface{})
	if !ok {
		return v
	}
	out := make(map[string]interface{}, len(m))
	for k, val := range m {
		if !glSchemaKeys[k] {
			continue
		}
		switch k {
		case "properties":
			props, _ := val.(map[string]interface{})
			clean := make(map[string]interface{}, len(props))
			for name, p := range props {
				clean[name] = glSchema(p)
			}
			out[k] = clean
		case "items":
			out[k] = glSchema(val)
		default:
			out[k] = val
		}
	}
	return out
}

// glFunctionDecls: Gemini rejects OBJECT parameters with no properties, so
// argument-less tools omit "parameters".
func glFunctionDecls(cfg Config) []map[string]interface{} {
	specs := oaiToolSpecs(cfg)
	out := make([]map[string]interface{}, 0, len(specs))
	for _, s := range specs {
		d := map[string]interface{}{
			"name":        s.Name,
			"description": s.Description,
			// Robot actions must finish before the model confirms them.
			"behavior": "BLOCKING",
		}
		if props, _ := s.Parameters["properties"].(map[string]interface{}); len(props) > 0 {
			d["parameters"] = glSchema(s.Parameters)
		}
		out = append(out, d)
	}
	return out
}

// glThinkingOff: 2.5 native-audio models think before every reply (seconds
// of silence); Gemini 3 Live rejects thinking settings.
func glThinkingOff(model string) bool {
	return strings.Contains(model, "2.5")
}

// glNoSearch remembers models whose key has no Google Search quota (free
// tier on Gemini 3 Live): their setup is refused with a quota error.
var glNoSearch sync.Map

func glSearchQuotaErr(err error) bool {
	return err != nil && strings.Contains(err.Error(), "exceeded your current quota")
}

func glSetup(cfg Config, ocfg OpenAIConfig, instructions, resume string, search bool) map[string]interface{} {
	transcription := map[string]interface{}{}
	if lang := ocfg.sttLanguage(); lang != "" {
		transcription["languageCodes"] = []string{lang}
	}
	resumption := map[string]interface{}{}
	if resume != "" {
		resumption["handle"] = resume
	}
	tools := []map[string]interface{}{{"functionDeclarations": glFunctionDecls(cfg)}}
	if search {
		// News, weather, prices: answers the self.* tools cannot give.
		tools = append(tools, map[string]interface{}{"googleSearch": map[string]interface{}{}})
	}
	gen := map[string]interface{}{
		"responseModalities": []string{"AUDIO"},
		"speechConfig": map[string]interface{}{
			"voiceConfig": map[string]interface{}{
				"prebuiltVoiceConfig": map[string]interface{}{"voiceName": ocfg.GeminiLiveVoice},
			},
		},
	}
	if glThinkingOff(ocfg.GeminiLiveModel) {
		gen["thinkingConfig"] = map[string]interface{}{"thinkingBudget": 0}
	}
	return map[string]interface{}{
		"setup": map[string]interface{}{
			"model":            glModelName(ocfg),
			"generationConfig": gen,
			"systemInstruction": map[string]interface{}{
				"parts": []map[string]interface{}{{"text": instructions}},
			},
			"tools": tools,
			"realtimeInputConfig": map[string]interface{}{
				"automaticActivityDetection": map[string]interface{}{"disabled": true},
			},
			"inputAudioTranscription":  transcription,
			"outputAudioTranscription": map[string]interface{}{},
			// Audio sessions end after ~15 min without compression.
			"contextWindowCompression": map[string]interface{}{"slidingWindow": map[string]interface{}{}},
			"sessionResumption":        resumption,
		},
	}
}

func dialGeminiLive(ctx context.Context, cfg Config, ocfg OpenAIConfig, resume string) (*glConn, error) {
	_, noSearch := glNoSearch.Load(ocfg.GeminiLiveModel)
	c, err := dialGeminiLiveOnce(ctx, cfg, ocfg, resume, !noSearch)
	if !noSearch && glSearchQuotaErr(err) && ctx.Err() == nil {
		log.Printf("[Gemini][Live] %s refused Google Search (no quota on this key) — continuing without it", ocfg.GeminiLiveModel)
		glNoSearch.Store(ocfg.GeminiLiveModel, true)
		c, err = dialGeminiLiveOnce(ctx, cfg, ocfg, resume, false)
	}
	return c, err
}

func dialGeminiLiveOnce(ctx context.Context, cfg Config, ocfg OpenAIConfig, resume string, search bool) (*glConn, error) {
	h := http.Header{}
	h.Set("x-goog-api-key", ocfg.geminiKey())
	d := websocket.Dialer{HandshakeTimeout: 10 * time.Second, Proxy: http.ProxyFromEnvironment}
	dctx, cancel := context.WithTimeout(ctx, 12*time.Second)
	defer cancel()
	started := time.Now()
	ws, resp, err := d.DialContext(dctx, geminiLiveURL, h)
	if err != nil {
		if resp != nil {
			body, _ := io.ReadAll(io.LimitReader(resp.Body, 400))
			resp.Body.Close()
			return nil, fmt.Errorf("dial: %s %s", resp.Status, strings.TrimSpace(string(body)))
		}
		return nil, fmt.Errorf("dial: %w", err)
	}
	ws.SetReadLimit(16 << 20)
	c := &glConn{
		ws:    ws,
		sig:   glSignature(cfg, ocfg),
		msgs:  make(chan []byte, 4096),
		errCh: make(chan error, 1),
		dead:  make(chan struct{}),
	}
	go c.readLoop()

	mem := oaiMemoryTail(loadOAIMemory(), ocfg.MemoryTurns)
	if err := c.send(glSetup(cfg, ocfg, oaiInstructions(ocfg, mem), resume, search)); err != nil {
		c.close()
		return nil, fmt.Errorf("setup: %w", err)
	}
	wait := time.NewTimer(10 * time.Second)
	defer wait.Stop()
	for {
		select {
		case data := <-c.msgs:
			var m struct {
				SetupComplete json.RawMessage `json:"setupComplete"`
			}
			if json.Unmarshal(data, &m) == nil && m.SetupComplete != nil {
				log.Printf("[Gemini][Live] session ready model=%s voice=%s memory=%d resumed=%v search=%v (+%v)",
					ocfg.GeminiLiveModel, ocfg.GeminiLiveVoice, len(mem), resume != "", search, time.Since(started).Round(time.Millisecond))
				return c, nil
			}
		case <-c.dead:
			return nil, fmt.Errorf("closed during setup: %v", c.deadErr)
		case <-wait.C:
			c.close()
			return nil, fmt.Errorf("no setupComplete")
		case <-ctx.Done():
			c.close()
			return nil, ctx.Err()
		}
	}
}

// glEnsure reuses the conversation socket while settings and tools are
// unchanged. A socket the server is retiring (goAway, ~10 min lifetime) or
// that dropped is replaced by resuming the same Gemini session.
func glEnsure(ctx context.Context, cfg Config, ocfg OpenAIConfig) (*glConn, error) {
	sig := glSignature(cfg, ocfg)
	oaiSess.mu.Lock()
	c := oaiSess.gl
	oaiSess.mu.Unlock()
	if c != nil && c.sig == sig && c.reusable() {
		return c, nil
	}
	resume := ""
	if c != nil {
		if c.sig == sig {
			resume = c.resumeHandle()
		}
		c.close()
	}
	nc, err := dialGeminiLive(ctx, cfg, ocfg, resume)
	if err != nil && resume != "" && ctx.Err() == nil {
		log.Println("[Gemini][Live] resume failed, starting fresh:", err)
		nc, err = dialGeminiLive(ctx, cfg, ocfg, "")
	}
	if err != nil {
		return nil, err
	}
	oaiSess.mu.Lock()
	oaiSess.gl = nc
	oaiSess.mu.Unlock()
	return nc, nil
}

// glUplink holds mic audio until the local gate hears speech, then opens
// the user activity and streams.
type glUplink struct {
	c       *glConn
	gate    *listenvad.SpeechGate
	pending []byte
	active  bool
}

func (u *glUplink) SendAudio(pcm []byte) error {
	u.gate.Feed(pcm)
	if u.active {
		return u.c.sendAudio(pcm)
	}
	u.pending = append(u.pending, pcm...)
	if !u.gate.Started() {
		if extra := len(u.pending) - glPreRollBytes; extra > 0 {
			u.pending = append(u.pending[:0], u.pending[extra&^1:]...)
		}
		return nil
	}
	u.active = true
	if err := u.c.sendActivity("activityStart"); err != nil {
		return err
	}
	err := u.c.sendAudio(u.pending)
	u.pending = nil
	return err
}
func (u *glUplink) ListenStop() error    { return nil }
func (u *glUplink) STTCh() <-chan string { return nil }
func (u *glUplink) ErrCh() <-chan error  { return u.c.errCh }

func runGeminiLiveTurn(ctx context.Context, audio <-chan []byte, cfg Config, ocfg OpenAIConfig,
	afterSTT func(string), detectText string, st *oaiTurnState) error {
	c, err := glEnsure(ctx, cfg, ocfg)
	if err != nil {
		return fmt.Errorf("gemini live %w", err)
	}
	c.drain()

	if detectText != "" {
		st.stt = detectText
		if err := c.send(map[string]interface{}{
			"clientContent": map[string]interface{}{
				"turns": []map[string]interface{}{{
					"role":  "user",
					"parts": []map[string]interface{}{{"text": detectText}},
				}},
				"turnComplete": true,
			},
		}); err != nil {
			return fmt.Errorf("%w: %v", errOAISessionBroken, err)
		}
		log.Printf("[Gemini][Live] detect %q sent", detectText)
	} else {
		up := &glUplink{c: c, gate: listenvad.NewSpeechGate()}
		log.Println("[Gemini][Live] listening (local end-of-speech)")
		_, err := oaiCapture(ctx, up, audio, plEndOfSpeech)
		if err != nil {
			if up.active {
				// An open activity would answer half an utterance later.
				c.close()
			}
			if errors.Is(err, listenvad.ErrServer) || errors.Is(err, listenvad.ErrSend) {
				return fmt.Errorf("%w: %v", errOAISessionBroken, err)
			}
			return err
		}
		if !up.active {
			return ErrNoSpeech
		}
		if afterSTT != nil {
			afterSTT("")
		}
		if err := c.sendActivity("activityEnd"); err != nil {
			return fmt.Errorf("%w: %v", errOAISessionBroken, err)
		}
	}
	err = glAwaitResponse(ctx, c, detectText != "", st)
	if ctx.Err() != nil {
		// Barge-in: the reply is still generating; resume on a fresh socket.
		c.close()
	}
	return err
}

type glServerMsg struct {
	ServerContent *struct {
		ModelTurn *struct {
			Parts []struct {
				Thought    bool `json:"thought"`
				InlineData *struct {
					MimeType string `json:"mimeType"`
					Data     string `json:"data"`
				} `json:"inlineData"`
			} `json:"parts"`
		} `json:"modelTurn"`
		TurnComplete       bool `json:"turnComplete"`
		InputTranscription *struct {
			Text string `json:"text"`
		} `json:"inputTranscription"`
		OutputTranscription *struct {
			Text string `json:"text"`
		} `json:"outputTranscription"`
		InteractionStatus string `json:"interactionStatus"`
	} `json:"serverContent"`
	ToolCall *struct {
		FunctionCalls []struct {
			ID   string          `json:"id"`
			Name string          `json:"name"`
			Args json.RawMessage `json:"args"`
		} `json:"functionCalls"`
	} `json:"toolCall"`
}

func glAwaitResponse(ctx context.Context, c *glConn, detect bool, st *oaiTurnState) error {
	var carry []byte
	var heard strings.Builder
	audioMuted := false
	// toolPending: a tool response went out and no reply has followed yet.
	toolPending := false
	completed := false
	waitStart := time.Now()
	timer := time.NewTimer(glFirstEventWait)
	defer timer.Stop()
	resetTimer := func(d time.Duration) {
		if !timer.Stop() {
			select {
			case <-timer.C:
			default:
			}
		}
		timer.Reset(d)
	}
	finish := func() error {
		if !detect {
			if heard.Len() == 0 {
				glWaitTranscript(c, &heard)
			}
			st.stt = strings.TrimSpace(heard.String())
			log.Printf("[Gemini][Live] STT: %q", st.stt)
		}
		return nil
	}

	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-c.dead:
			if completed {
				return finish()
			}
			return fmt.Errorf("%w: %v", errOAISessionBroken, c.deadErr)
		case <-timer.C:
			if completed {
				return finish()
			}
			if st.speaker.started {
				return finish()
			}
			c.close()
			return fmt.Errorf("gemini live: reply timed out after %v", time.Since(waitStart).Round(time.Second))
		case data := <-c.msgs:
			var m glServerMsg
			if json.Unmarshal(data, &m) != nil {
				continue
			}
			resetTimer(glEventGap)

			if tc := m.ToolCall; tc != nil {
				var responses []map[string]interface{}
				for _, fc := range tc.FunctionCalls {
					args := strings.TrimSpace(string(fc.Args))
					if args == "null" {
						args = ""
					}
					r := runOAITool(fc.Name, args)
					if r.End {
						st.end = true
					}
					responses = append(responses, map[string]interface{}{
						"id": fc.ID, "name": fc.Name,
						"response": map[string]interface{}{"result": r.Output},
					})
				}
				if err := c.send(map[string]interface{}{
					"toolResponse": map[string]interface{}{"functionResponses": responses},
				}); err != nil {
					return fmt.Errorf("%w: %v", errOAISessionBroken, err)
				}
				toolPending = true
				completed = false
				continue
			}

			sc := m.ServerContent
			if sc == nil {
				continue
			}
			if t := sc.InputTranscription; t != nil && !detect {
				heard.WriteString(t.Text)
			}
			if t := sc.OutputTranscription; t != nil {
				st.reply.WriteString(t.Text)
				toolPending = false
			}
			if sc.ModelTurn != nil {
				for _, p := range sc.ModelTurn.Parts {
					if p.Thought || p.InlineData == nil || !strings.HasPrefix(p.InlineData.MimeType, "audio/pcm") {
						continue
					}
					toolPending = false
					if audioMuted {
						continue
					}
					raw, err := base64.StdEncoding.DecodeString(p.InlineData.Data)
					if err != nil {
						continue
					}
					var pcm []byte
					if rate := pcmRateFromMime(p.InlineData.MimeType, glOutputRate); rate == glOutputRate {
						pcm = down24to16(&carry, raw)
					} else {
						pcm = resampleS16(raw, rate, 16000)
					}
					first := !st.speaker.started
					if err := st.speaker.write(pcm); err != nil {
						log.Println("[Gemini][Live] speaker:", err)
						audioMuted = true
						continue
					}
					if first && st.speaker.started {
						log.Printf("[Gemini][Live] playing +%v", time.Since(waitStart).Round(time.Millisecond))
					}
				}
			}
			if sc.TurnComplete {
				if sc.InteractionStatus == "IN_PROGRESS" {
					continue
				}
				if toolPending && !st.end {
					completed = true
					resetTimer(glToolGrace)
					continue
				}
				return finish()
			}
		}
	}
}

// glWaitTranscript gives the input transcription a moment to land after the
// reply (no ordering guarantee; it only feeds memory/logs). It may arrive in
// several fragments.
func glWaitTranscript(c *glConn, heard *strings.Builder) {
	deadline := time.NewTimer(1500 * time.Millisecond)
	defer deadline.Stop()
	for {
		select {
		case data := <-c.msgs:
			var m glServerMsg
			if json.Unmarshal(data, &m) == nil && m.ServerContent != nil && m.ServerContent.InputTranscription != nil {
				heard.WriteString(m.ServerContent.InputTranscription.Text)
				if !deadline.Stop() {
					<-deadline.C
				}
				deadline.Reset(300 * time.Millisecond)
			}
		case <-deadline.C:
			return
		case <-c.dead:
			return
		}
	}
}
