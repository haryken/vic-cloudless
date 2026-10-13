package xiaozhi

import (
	"bufio"
	"bytes"
	"context"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"mime/multipart"
	"net/http"
	"strings"
	"sync"
	"time"
	"unicode"
	"unicode/utf8"

	"github.com/digital-dream-labs/vector-cloud/internal/log"
)

// Mode B (wire-pod style): local end-of-speech → STT → chat completions with
// tools (OpenAI-compatible, base URL configurable) → TTS sentence by sentence.

const (
	plEndOfSpeech  = 800 * time.Millisecond
	plMaxToolLoops = 4
	plMaxPCMBytes  = 32000 * 30
	// Long unpunctuated replies are spoken in chunks of about this many runes.
	plMaxSentenceRunes = 160
)

var oaiHTTP = &http.Client{Timeout: 90 * time.Second}

type chatToolCall struct {
	ID       string `json:"id"`
	Type     string `json:"type"`
	Function struct {
		Name      string `json:"name"`
		Arguments string `json:"arguments"`
	} `json:"function"`
	// Gemini's thought signature; must be echoed back on the next request.
	ExtraContent json.RawMessage `json:"extra_content,omitempty"`
}

type chatMsg struct {
	Role       string         `json:"role"`
	Content    interface{}    `json:"content"`
	ToolCalls  []chatToolCall `json:"tool_calls,omitempty"`
	ToolCallID string         `json:"tool_call_id,omitempty"`
}

// plUplink buffers the utterance; end-of-speech comes from the local gate.
type plUplink struct {
	mu  sync.Mutex
	pcm []byte
}

func (u *plUplink) SendAudio(pcm []byte) error {
	u.mu.Lock()
	if len(u.pcm)+len(pcm) <= plMaxPCMBytes {
		u.pcm = append(u.pcm, pcm...)
	}
	u.mu.Unlock()
	return nil
}
func (u *plUplink) ListenStop() error    { return nil }
func (u *plUplink) STTCh() <-chan string { return nil }
func (u *plUplink) ErrCh() <-chan error  { return nil }

func (u *plUplink) audio() []byte {
	u.mu.Lock()
	defer u.mu.Unlock()
	return u.pcm
}

func runPipelineTurn(ctx context.Context, audio <-chan []byte, cfg Config, ocfg OpenAIConfig,
	afterSTT func(string), detectText string, st *oaiTurnState) error {
	if detectText != "" {
		st.stt = detectText
	} else {
		up := &plUplink{}
		log.Println("[OpenAI][PL] listening (local end-of-speech)")
		if _, err := oaiCapture(ctx, up, audio, plEndOfSpeech); err != nil {
			return err
		}
		if afterSTT != nil {
			afterSTT("")
		}
		started := time.Now()
		text, err := oaiTranscribe(ctx, ocfg, up.audio())
		if err != nil {
			return fmt.Errorf("openai stt: %w", err)
		}
		text = strings.TrimSpace(text)
		if text == "" || sttHallucination(text) {
			log.Printf("[OpenAI][PL] STT empty/noise %q", text)
			return fmt.Errorf("empty STT result")
		}
		st.stt = text
		log.Printf("[OpenAI][PL] STT (+%v): %q", time.Since(started).Round(time.Millisecond), text)
	}

	msgs := append(plHistory(ocfg), chatMsg{Role: "user", Content: st.stt})
	tools := oaiChatTools(cfg)
	tts := startPLTTS(ctx, ocfg, st)

	var turnErr error
	llmStart := time.Now()
	for round := 0; round < plMaxToolLoops; round++ {
		var split sentenceSplitter
		content, calls, err := oaiChatStream(ctx, ocfg, msgs, tools, func(delta string) {
			st.reply.WriteString(delta)
			for _, s := range split.push(delta) {
				tts.say(s)
			}
		})
		for _, s := range split.flush() {
			tts.say(s)
		}
		if err != nil {
			turnErr = fmt.Errorf("openai llm: %w", err)
			break
		}
		if round == 0 {
			log.Printf("[OpenAI][PL] LLM first round (+%v) tools=%d", time.Since(llmStart).Round(time.Millisecond), len(calls))
		}
		assistant := chatMsg{Role: "assistant", Content: content, ToolCalls: calls}
		if content == "" && len(calls) > 0 {
			assistant.Content = nil
		}
		msgs = append(msgs, assistant)
		if len(calls) == 0 {
			break
		}
		for _, call := range calls {
			r := runOAITool(call.Function.Name, call.Function.Arguments)
			if r.End {
				st.end = true
			}
			msgs = append(msgs, chatMsg{Role: "tool", ToolCallID: call.ID, Content: r.Output})
		}
		if st.end && strings.TrimSpace(content) != "" {
			break
		}
	}
	tts.finish()

	if turnErr == nil {
		plSaveHistory(msgs, ocfg)
	}
	return turnErr
}

// plHistory is the in-session transcript, seeded from memory on a new session.
func plHistory(ocfg OpenAIConfig) []chatMsg {
	oaiSess.mu.Lock()
	h := oaiSess.history
	oaiSess.mu.Unlock()
	sys := chatMsg{Role: "system", Content: oaiInstructions(ocfg, nil)}
	if len(h) == 0 {
		for _, e := range oaiMemoryTail(loadOAIMemory(), ocfg.MemoryTurns) {
			if e.User != "" {
				h = append(h, chatMsg{Role: "user", Content: e.User})
			}
			if e.Assistant != "" {
				h = append(h, chatMsg{Role: "assistant", Content: e.Assistant})
			}
		}
	}
	return append([]chatMsg{sys}, h...)
}

// plSaveHistory keeps the newest exchanges (system prompt dropped), always
// starting at a user message so no orphan tool result leads the list.
func plSaveHistory(msgs []chatMsg, ocfg OpenAIConfig) {
	if len(msgs) > 0 && msgs[0].Role == "system" {
		msgs = msgs[1:]
	}
	keep := ocfg.MemoryTurns
	if keep < 2 {
		keep = 2
	}
	users := 0
	start := 0
	for i := len(msgs) - 1; i >= 0; i-- {
		if msgs[i].Role == "user" {
			users++
			start = i
			if users >= keep {
				break
			}
		}
	}
	out := append([]chatMsg(nil), msgs[start:]...)
	oaiSess.mu.Lock()
	oaiSess.history = out
	oaiSess.mu.Unlock()
}

// sttHallucination drops Whisper's well-known silence captions.
func sttHallucination(text string) bool {
	low := strings.ToLower(text)
	for _, s := range []string{"subscribe", "ghiền mì gõ", "đăng ký kênh", "cảm ơn các bạn đã theo dõi", "thanks for watching"} {
		if strings.Contains(low, s) {
			return true
		}
	}
	return false
}

func pcmToWAV(pcm []byte, rate int) []byte {
	var b bytes.Buffer
	b.WriteString("RIFF")
	_ = binary.Write(&b, binary.LittleEndian, uint32(36+len(pcm)))
	b.WriteString("WAVEfmt ")
	_ = binary.Write(&b, binary.LittleEndian, uint32(16))
	_ = binary.Write(&b, binary.LittleEndian, uint16(1))
	_ = binary.Write(&b, binary.LittleEndian, uint16(1))
	_ = binary.Write(&b, binary.LittleEndian, uint32(rate))
	_ = binary.Write(&b, binary.LittleEndian, uint32(rate*2))
	_ = binary.Write(&b, binary.LittleEndian, uint16(2))
	_ = binary.Write(&b, binary.LittleEndian, uint16(16))
	b.WriteString("data")
	_ = binary.Write(&b, binary.LittleEndian, uint32(len(pcm)))
	b.Write(pcm)
	return b.Bytes()
}

func oaiAPIError(resp *http.Response) error {
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 600))
	var e struct {
		Error struct {
			Message string `json:"message"`
		} `json:"error"`
	}
	if json.Unmarshal(body, &e) == nil && e.Error.Message != "" {
		return fmt.Errorf("%s: %s", resp.Status, e.Error.Message)
	}
	return fmt.Errorf("%s: %s", resp.Status, truncate(strings.TrimSpace(string(body)), 200))
}

func oaiTranscribe(ctx context.Context, ocfg OpenAIConfig, pcm []byte) (string, error) {
	if len(pcm) < 16000*2/4 {
		return "", nil
	}
	var body bytes.Buffer
	mw := multipart.NewWriter(&body)
	fw, err := mw.CreateFormFile("file", "speech.wav")
	if err != nil {
		return "", err
	}
	_, _ = fw.Write(pcmToWAV(pcm, 16000))
	_ = mw.WriteField("model", ocfg.STTModel)
	if lang := ocfg.sttLanguage(); lang != "" {
		_ = mw.WriteField("language", lang)
	}
	_ = mw.WriteField("response_format", "json")
	_ = mw.Close()

	rctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(rctx, http.MethodPost, ocfg.sttBase()+"/audio/transcriptions", &body)
	if err != nil {
		return "", err
	}
	req.Header.Set("Content-Type", mw.FormDataContentType())
	if key := ocfg.sttKey(); key != "" {
		req.Header.Set("Authorization", "Bearer "+key)
	}
	resp, err := oaiHTTP.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return "", oaiAPIError(resp)
	}
	var out struct {
		Text string `json:"text"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return "", err
	}
	return out.Text, nil
}

// oaiChatStream runs one streamed chat completion; onDelta gets text as it
// arrives, tool calls are returned once complete.
func oaiChatStream(ctx context.Context, ocfg OpenAIConfig, msgs []chatMsg, tools []map[string]interface{},
	onDelta func(string)) (string, []chatToolCall, error) {
	payload := map[string]interface{}{
		"model":    ocfg.LLMModel,
		"messages": msgs,
		"stream":   true,
	}
	if len(tools) > 0 {
		payload["tools"] = tools
		payload["tool_choice"] = "auto"
	}
	data, err := json.Marshal(payload)
	if err != nil {
		return "", nil, err
	}
	rctx, cancel := context.WithTimeout(ctx, 60*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(rctx, http.MethodPost, ocfg.llmBase()+"/chat/completions", bytes.NewReader(data))
	if err != nil {
		return "", nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "text/event-stream")
	if key := ocfg.llmKey(); key != "" {
		req.Header.Set("Authorization", "Bearer "+key)
	}
	resp, err := oaiHTTP.Do(req)
	if err != nil {
		return "", nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return "", nil, oaiAPIError(resp)
	}

	var content strings.Builder
	var calls []chatToolCall
	remap := map[int]int{}
	sc := bufio.NewScanner(resp.Body)
	sc.Buffer(make([]byte, 64*1024), 1<<20)
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if !strings.HasPrefix(line, "data:") {
			continue
		}
		line = strings.TrimSpace(strings.TrimPrefix(line, "data:"))
		if line == "[DONE]" {
			break
		}
		var chunk struct {
			Choices []struct {
				Delta struct {
					Content   string `json:"content"`
					ToolCalls []struct {
						Index    int    `json:"index"`
						ID       string `json:"id"`
						Type     string `json:"type"`
						Function struct {
							Name      string `json:"name"`
							Arguments string `json:"arguments"`
						} `json:"function"`
						ExtraContent json.RawMessage `json:"extra_content"`
					} `json:"tool_calls"`
				} `json:"delta"`
			} `json:"choices"`
		}
		if json.Unmarshal([]byte(line), &chunk) != nil || len(chunk.Choices) == 0 {
			continue
		}
		d := chunk.Choices[0].Delta
		if d.Content != "" {
			content.WriteString(d.Content)
			onDelta(d.Content)
		}
		for _, tc := range d.ToolCalls {
			// Some servers (Gemini) send several complete calls all at index 0;
			// a new id at an occupied slot is a new call.
			idx := tc.Index
			if m, ok := remap[tc.Index]; ok {
				idx = m
			}
			if tc.ID != "" && idx < len(calls) && calls[idx].ID != "" && calls[idx].ID != tc.ID {
				idx = len(calls)
				remap[tc.Index] = idx
			}
			for len(calls) <= idx {
				calls = append(calls, chatToolCall{Type: "function"})
			}
			c := &calls[idx]
			if tc.ID != "" {
				c.ID = tc.ID
			}
			if tc.Function.Name != "" {
				c.Function.Name += tc.Function.Name
			}
			c.Function.Arguments += tc.Function.Arguments
			if len(tc.ExtraContent) > 0 && string(tc.ExtraContent) != "null" {
				c.ExtraContent = tc.ExtraContent
			}
		}
	}
	if err := sc.Err(); err != nil && !errors.Is(err, io.EOF) {
		return content.String(), nil, err
	}
	out := calls[:0]
	for i, c := range calls {
		if c.Function.Name == "" {
			continue
		}
		if c.ID == "" {
			c.ID = fmt.Sprintf("call_%d", i)
		}
		out = append(out, c)
	}
	return content.String(), out, nil
}

// sentenceSplitter cuts streamed text into speakable sentences.
type sentenceSplitter struct{ buf string }

func (s *sentenceSplitter) push(delta string) []string {
	s.buf += delta
	var out []string
	for {
		cut := sentenceCut(s.buf)
		if cut <= 0 {
			break
		}
		if sent := cleanSpeech(s.buf[:cut]); sent != "" {
			out = append(out, sent)
		}
		s.buf = s.buf[cut:]
	}
	return out
}

func (s *sentenceSplitter) flush() []string {
	sent := cleanSpeech(s.buf)
	s.buf = ""
	if sent == "" {
		return nil
	}
	return []string{sent}
}

// sentenceCut returns the byte index just past the first complete sentence,
// or 0. A terminator only counts once followed by whitespace ("3.5" stays).
func sentenceCut(text string) int {
	prevTerm := false
	runes := 0
	lastSoft := 0
	for i, r := range text {
		if prevTerm && unicode.IsSpace(r) {
			return i
		}
		if r == '\n' {
			return i + 1
		}
		prevTerm = r == '.' || r == '!' || r == '?' || r == '…' || r == '。' || r == '！' || r == '？'
		if r == ',' || r == ';' || r == ':' {
			lastSoft = i + utf8.RuneLen(r)
		}
		runes++
		if runes >= plMaxSentenceRunes && lastSoft > 0 {
			return lastSoft
		}
	}
	return 0
}

// cleanSpeech strips markdown/emoji that TTS would read out.
func cleanSpeech(s string) string {
	var b strings.Builder
	for _, r := range s {
		switch {
		case r == '*' || r == '#' || r == '`' || r == '_' || r == '~':
			continue
		case r >= 0x1F000 || (r >= 0x2600 && r <= 0x27BF) || r == 0xFE0F:
			continue
		}
		b.WriteRune(r)
	}
	out := strings.TrimSpace(b.String())
	out = strings.TrimLeft(out, "-• ")
	return strings.TrimSpace(out)
}

// plTTS speaks sentences in order on its own goroutine while the LLM streams.
type plTTS struct {
	ctx  context.Context
	ocfg OpenAIConfig
	st   *oaiTurnState
	tts  *ttsTurn
	ch   chan string
	done chan struct{}
}

// plBatchRunes caps how much queued text is merged into one TTS request.
const plBatchRunes = 400

func startPLTTS(ctx context.Context, ocfg OpenAIConfig, st *oaiTurnState) *plTTS {
	t := &plTTS{ctx: ctx, ocfg: ocfg, st: st, tts: newTTSTurn(ctx, ocfg),
		ch: make(chan string, 64), done: make(chan struct{})}
	go t.loop()
	return t
}

func (t *plTTS) say(sentence string) {
	if strings.TrimSpace(sentence) == "" {
		return
	}
	select {
	case t.ch <- sentence:
	case <-t.ctx.Done():
	}
}

func (t *plTTS) finish() {
	close(t.ch)
	<-t.done
}

func (t *plTTS) loop() {
	defer close(t.done)
	defer t.tts.close()
	stopped := false
	batch := oaiTTSBatches(t.ocfg.TTSEngine)
	for s := range t.ch {
		if stopped || t.ctx.Err() != nil {
			continue
		}
		if batch {
			s = t.drainInto(s)
		}
		if err := oaiSpeak(t.ctx, t.ocfg, s, t.tts, t.st.speaker.write); err != nil {
			if t.ctx.Err() == nil {
				log.Printf("[OpenAI][TTS] %q: %v", truncate(s, 60), err)
			}
			if errors.Is(err, errSpeakerCapped) || t.st.speaker.failed {
				stopped = true
			}
		}
	}
}

// drainInto appends sentences already queued (without waiting) to s.
func (t *plTTS) drainInto(s string) string {
	runes := utf8.RuneCountInString(s)
	for runes < plBatchRunes {
		select {
		case next, ok := <-t.ch:
			if !ok {
				return s
			}
			s += " " + next
			runes += utf8.RuneCountInString(next) + 1
		default:
			return s
		}
	}
	return s
}

func oaiSpeakOpenAI(ctx context.Context, ocfg OpenAIConfig, text string, write func([]byte) error) error {
	payload := map[string]interface{}{
		"model":           ocfg.TTSModel,
		"input":           text,
		"voice":           ocfg.Voice,
		"response_format": "pcm",
	}
	if strings.HasPrefix(ocfg.TTSModel, "gpt-") {
		payload["instructions"] = "Giọng thân thiện, vui vẻ, tự nhiên như một chú robot nhỏ."
	}
	data, _ := json.Marshal(payload)
	rctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(rctx, http.MethodPost, ocfg.BaseURL+"/audio/speech", bytes.NewReader(data))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+ocfg.APIKey)
	resp, err := oaiHTTP.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return oaiAPIError(resp)
	}
	var carry []byte
	buf := make([]byte, 4800)
	for {
		n, rerr := resp.Body.Read(buf)
		if n > 0 {
			if err := write(down24to16(&carry, buf[:n])); err != nil {
				return err
			}
		}
		if rerr == io.EOF {
			return nil
		}
		if rerr != nil {
			return rerr
		}
	}
}
