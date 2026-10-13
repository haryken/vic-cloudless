package xiaozhi

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"encoding/xml"
	"errors"
	"fmt"
	"io"
	"net/http"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"
	"unicode"

	"github.com/gorilla/websocket"

	"github.com/digital-dream-labs/vector-cloud/internal/log"
)

// Mode B speech engines besides OpenAI: Google Translate (one language per
// request, so replies are cut into language segments), Microsoft Edge
// Read Aloud neural voices and Gemini TTS. Any failure falls back to Google.

// --- language segments (Google) ---

var langTagRe = regexp.MustCompile(`</?\s*([a-zA-Z]{2,3}(?:-[a-zA-Z]{2,4})?)\s*>`)

// oaiLangTagInstructions asks the LLM to mark foreign-language spans so the
// single-language Google voice can switch per segment.
func oaiLangTagInstructions(main string) string {
	return "Giọng đọc chỉ đọc được một ngôn ngữ mỗi đoạn. Ngôn ngữ chính là \"" + main +
		"\" (không cần đánh dấu). Mọi từ hoặc câu thuộc ngôn ngữ khác phải bọc trong thẻ mã ngôn ngữ ISO, ví dụ " +
		"<en>machine learning</en>, <zh>你好</zh>, <vi>xin chào</vi>, <ja>ありがとう</ja>. " +
		"Không giải thích hay nhắc tới các thẻ này."
}

func stripLangTags(s string) string {
	return langTagRe.ReplaceAllString(s, "")
}

type langSeg struct {
	lang string
	text string
}

// langSegmenter splits text into per-language segments. The open tag carries
// over between calls since the sentence splitter may cut inside a tag pair.
// Untagged CJK runs are detected by script.
type langSegmenter struct {
	def string
	cur string
}

func (s *langSegmenter) split(text string) []langSeg {
	var out []langSeg
	add := func(lang, t string) {
		if t == "" {
			return
		}
		if n := len(out); n > 0 && out[n-1].lang == lang {
			out[n-1].text += t
			return
		}
		out = append(out, langSeg{lang: lang, text: t})
	}
	emit := func(t string) {
		if s.cur != "" {
			add(googleLang(s.cur), t)
			return
		}
		for _, seg := range scriptSegments(t, googleLang(s.def)) {
			add(seg.lang, seg.text)
		}
	}
	last := 0
	for _, m := range langTagRe.FindAllStringSubmatchIndex(text, -1) {
		emit(text[last:m[0]])
		if strings.HasPrefix(text[m[0]:m[1]], "</") {
			s.cur = ""
		} else {
			s.cur = strings.ToLower(text[m[2]:m[3]])
		}
		last = m[1]
	}
	emit(text[last:])

	kept := out[:0]
	for _, seg := range out {
		seg.text = strings.TrimSpace(seg.text)
		if hasSpeakable(seg.text) {
			kept = append(kept, seg)
		}
	}
	return kept
}

// scriptSegments tags Han/Kana/Hangul runs; spaces and punctuation stay with
// the current run.
func scriptSegments(text, def string) []langSeg {
	var out []langSeg
	cur := def
	var b strings.Builder
	for _, r := range text {
		lang := ""
		switch {
		case unicode.Is(unicode.Hiragana, r) || unicode.Is(unicode.Katakana, r):
			lang = "ja"
		case unicode.Is(unicode.Hangul, r):
			lang = "ko"
		case unicode.Is(unicode.Han, r):
			lang = "zh-CN"
			if def == "ja" || def == "zh-TW" {
				lang = def
			}
		case unicode.IsLetter(r):
			lang = def
		}
		if lang != "" && lang != cur {
			if b.Len() > 0 {
				out = append(out, langSeg{lang: cur, text: b.String()})
				b.Reset()
			}
			cur = lang
		}
		b.WriteRune(r)
	}
	if b.Len() > 0 {
		out = append(out, langSeg{lang: cur, text: b.String()})
	}
	return out
}

func hasSpeakable(s string) bool {
	for _, r := range s {
		if unicode.IsLetter(r) || unicode.IsDigit(r) {
			return true
		}
	}
	return false
}

func googleLang(code string) string {
	code = strings.ToLower(strings.TrimSpace(code))
	switch code {
	case "", OpenAILanguageAuto:
		return "vi"
	case "zh", "zh-cn", "cn":
		return "zh-CN"
	case "zh-tw":
		return "zh-TW"
	}
	return code
}

func googleSpeakSegments(ctx context.Context, segs []langSeg, write func([]byte) error) error {
	for _, seg := range segs {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		pcm, err := fetchGoogleTranslatePCM16kMulti(seg.text, seg.lang)
		if err != nil {
			return fmt.Errorf("google %s: %w", seg.lang, err)
		}
		if err := oaiWritePCM(ctx, ApplyGain(pcm, 1.7), write); err != nil {
			return err
		}
	}
	return nil
}

func oaiWritePCM(ctx context.Context, pcm []byte, write func([]byte) error) error {
	for off := 0; off < len(pcm); off += 4096 {
		end := off + 4096
		if end > len(pcm) {
			end = len(pcm)
		}
		if ctx.Err() != nil {
			return ctx.Err()
		}
		if err := write(pcm[off:end]); err != nil {
			return err
		}
	}
	return nil
}

// --- engine selection ---

// ttsCooldown skips an engine for a while after rate limits/outages so each
// sentence does not wait for the same failure.
var ttsCooldown = struct {
	sync.Mutex
	until map[string]time.Time
}{until: map[string]time.Time{}}

func ttsCooling(engine string) bool {
	ttsCooldown.Lock()
	defer ttsCooldown.Unlock()
	return time.Now().Before(ttsCooldown.until[engine])
}

func ttsCool(engine string, d time.Duration) {
	ttsCooldown.Lock()
	ttsCooldown.until[engine] = time.Now().Add(d)
	ttsCooldown.Unlock()
}

var errTTSRateLimited = errors.New("rate limited")

// ttsTurn is the speech state of one reply: the Google language carried
// across sentences and an optional pre-dialed Edge connection.
type ttsTurn struct {
	seg  langSegmenter
	edge *edgePrewarm
}

func newTTSTurn(ctx context.Context, ocfg OpenAIConfig) *ttsTurn {
	tt := &ttsTurn{seg: langSegmenter{def: ocfg.speechLanguage()}}
	if ocfg.TTSEngine == TTSEngineEdge && !ttsCooling(TTSEngineEdge) {
		tt.edge = startEdgePrewarm(ctx)
	}
	return tt
}

func (tt *ttsTurn) close() {
	tt.edge.close()
}

// oaiSpeak synthesizes text and streams 16 kHz PCM to write.
func oaiSpeak(ctx context.Context, ocfg OpenAIConfig, text string, tt *ttsTurn, write func([]byte) error) error {
	var err error
	switch ocfg.TTSEngine {
	case TTSEngineGoogle:
		return googleSpeakSegments(ctx, tt.seg.split(text), write)
	case TTSEngineEdge:
		ws := tt.edge.take(ctx)
		if ttsCooling(TTSEngineEdge) {
			if ws != nil {
				_ = ws.Close()
			}
			break
		}
		var pcm []byte
		if pcm, err = edgeSpeakPCM16k(ctx, ws, ocfg.EdgeVoice, stripLangTags(text)); err == nil {
			return oaiWritePCM(ctx, pcm, write)
		}
		if ctx.Err() == nil {
			ttsCool(TTSEngineEdge, 20*time.Second)
		}
	case TTSEngineGemini:
		key := ocfg.geminiKey()
		if key == "" {
			err = errors.New("gemini API key not set")
			break
		}
		if ttsCooling(TTSEngineGemini) {
			break
		}
		var pcm []byte
		if pcm, err = geminiSpeakPCM16k(ctx, ocfg, key, stripLangTags(text)); err == nil {
			return oaiWritePCM(ctx, pcm, write)
		}
		if errors.Is(err, errTTSRateLimited) {
			ttsCool(TTSEngineGemini, 60*time.Second)
		}
	default:
		if ocfg.APIKey == "" {
			break
		}
		err = oaiSpeakOpenAI(ctx, ocfg, stripLangTags(text), write)
		if err == nil || errors.Is(err, errSpeakerCapped) {
			return err
		}
	}
	if ctx.Err() != nil {
		return ctx.Err()
	}
	if err != nil {
		log.Printf("[OpenAI][TTS] %s failed, Google fallback: %v", ocfg.TTSEngine, err)
	}
	fallback := &langSegmenter{def: ocfg.speechLanguage()}
	return googleSpeakSegments(ctx, fallback.split(stripLangTags(text)), write)
}

// oaiTTSBatches reports whether queued sentences should be merged into one
// request (per-request latency/rate limits dominate for these engines).
func oaiTTSBatches(engine string) bool {
	return engine == TTSEngineEdge || engine == TTSEngineGemini
}

// --- Microsoft Edge Read Aloud ---

var edgeTTSURL = "wss://speech.platform.bing.com/consumer/speech/synthesize/readaloud/edge/v1"

const (
	edgeTrustedToken = "6A5AA1D4EAFF4E9FB37E23D68491D6F4"
	edgeChromium     = "143.0.3650.75"
	edgeMaxMP3Bytes  = 4 << 20
)

// edgeSkew is the server-minus-local clock offset learned from a 403.
var edgeSkew struct {
	sync.Mutex
	secs int64
}

// edgeSecMSGEC is the Sec-MS-GEC token: SHA-256 of the Windows file time
// rounded down to 5 minutes plus the trusted client token.
func edgeSecMSGEC(now time.Time) string {
	secs := now.Unix() + 11644473600
	secs -= secs % 300
	sum := sha256.Sum256([]byte(strconv.FormatInt(secs, 10) + "0000000" + edgeTrustedToken))
	return strings.ToUpper(hex.EncodeToString(sum[:]))
}

// edgeVoiceName expands "en-US-EmmaMultilingualNeural" to the service name.
func edgeVoiceName(short string) (name, locale string) {
	parts := strings.SplitN(strings.TrimSpace(short), "-", 3)
	if len(parts) != 3 {
		return short, "en-US"
	}
	locale = parts[0] + "-" + parts[1]
	return fmt.Sprintf("Microsoft Server Speech Text to Speech Voice (%s, %s)", locale, parts[2]), locale
}

func edgeRandomHex() string {
	b := make([]byte, 16)
	_, _ = rand.Read(b)
	return hex.EncodeToString(b)
}

func edgeDial(ctx context.Context) (*websocket.Conn, error) {
	major := strings.SplitN(edgeChromium, ".", 2)[0]
	h := http.Header{}
	h.Set("Pragma", "no-cache")
	h.Set("Cache-Control", "no-cache")
	h.Set("Origin", "chrome-extension://jdiccldimpdaibmpdkjnbmckianbfold")
	h.Set("User-Agent", "Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/"+
		major+".0.0.0 Safari/537.36 Edg/"+major+".0.0.0")
	h.Set("Accept-Language", "en-US,en;q=0.9")
	h.Set("Cookie", "muid="+strings.ToUpper(edgeRandomHex())+";")
	d := websocket.Dialer{HandshakeTimeout: 10 * time.Second, Proxy: http.ProxyFromEnvironment}

	for attempt := 0; ; attempt++ {
		edgeSkew.Lock()
		now := time.Now().Add(time.Duration(edgeSkew.secs) * time.Second)
		edgeSkew.Unlock()
		u := edgeTTSURL + "?TrustedClientToken=" + edgeTrustedToken + "&ConnectionId=" + edgeRandomHex() +
			"&Sec-MS-GEC=" + edgeSecMSGEC(now) + "&Sec-MS-GEC-Version=1-" + edgeChromium
		ws, resp, err := d.DialContext(ctx, u, h)
		if err == nil {
			return ws, nil
		}
		if resp != nil && resp.StatusCode == http.StatusForbidden && attempt == 0 {
			if srv, perr := http.ParseTime(resp.Header.Get("Date")); perr == nil {
				edgeSkew.Lock()
				edgeSkew.secs = srv.Unix() - time.Now().Unix()
				edgeSkew.Unlock()
				continue
			}
		}
		if resp != nil {
			return nil, fmt.Errorf("edge dial: %s", resp.Status)
		}
		return nil, fmt.Errorf("edge dial: %w", err)
	}
}

// edgePrewarm holds a connection dialed while the LLM is still thinking; the
// cold TLS handshake from the robot takes seconds.
type edgePrewarm struct {
	ch      chan *websocket.Conn
	pending bool
}

func startEdgePrewarm(ctx context.Context) *edgePrewarm {
	p := &edgePrewarm{ch: make(chan *websocket.Conn, 1), pending: true}
	go func() {
		ws, err := edgeDial(ctx)
		if err != nil {
			ws = nil
		}
		p.ch <- ws
	}()
	return p
}

// take returns the pre-dialed connection once (nil if it failed or was used).
func (p *edgePrewarm) take(ctx context.Context) *websocket.Conn {
	if p == nil || !p.pending {
		return nil
	}
	p.pending = false
	select {
	case ws := <-p.ch:
		return ws
	case <-ctx.Done():
		go p.discard()
		return nil
	}
}

func (p *edgePrewarm) close() {
	if p != nil && p.pending {
		p.pending = false
		go p.discard()
	}
}

func (p *edgePrewarm) discard() {
	if ws := <-p.ch; ws != nil {
		_ = ws.Close()
	}
}

// edgeSynthesizeMP3 returns the MP3 for text (the service rejects raw PCM).
// ws is an optional pre-dialed connection.
func edgeSynthesizeMP3(ctx context.Context, ws *websocket.Conn, voice, text string) ([]byte, error) {
	text = strings.TrimSpace(text)
	if text == "" {
		if ws != nil {
			_ = ws.Close()
		}
		return nil, errors.New("empty text")
	}
	if ws == nil {
		var err error
		if ws, err = edgeDial(ctx); err != nil {
			return nil, err
		}
	}
	defer ws.Close()
	stop := context.AfterFunc(ctx, func() { _ = ws.Close() })
	defer stop()

	ts := time.Now().UTC().Format("Mon Jan 02 2006 15:04:05 GMT+0000 (Coordinated Universal Time)")
	cfgMsg := "X-Timestamp:" + ts + "\r\nContent-Type:application/json; charset=utf-8\r\nPath:speech.config\r\n\r\n" +
		`{"context":{"synthesis":{"audio":{"metadataoptions":{"sentenceBoundaryEnabled":"false","wordBoundaryEnabled":"false"},` +
		`"outputFormat":"audio-24khz-48kbitrate-mono-mp3"}}}}` + "\r\n"
	name, locale := edgeVoiceName(voice)
	var esc bytes.Buffer
	_ = xml.EscapeText(&esc, []byte(text))
	var attr bytes.Buffer
	_ = xml.EscapeText(&attr, []byte(name))
	ssml := "<speak version='1.0' xmlns='http://www.w3.org/2001/10/synthesis' xml:lang='" + locale + "'>" +
		"<voice name='" + attr.String() + "'><prosody pitch='+0Hz' rate='+0%' volume='+0%'>" +
		esc.String() + "</prosody></voice></speak>"
	ssmlMsg := "X-RequestId:" + edgeRandomHex() + "\r\nContent-Type:application/ssml+xml\r\nX-Timestamp:" + ts +
		"Z\r\nPath:ssml\r\n\r\n" + ssml

	_ = ws.SetWriteDeadline(time.Now().Add(10 * time.Second))
	if err := ws.WriteMessage(websocket.TextMessage, []byte(cfgMsg)); err != nil {
		return nil, err
	}
	if err := ws.WriteMessage(websocket.TextMessage, []byte(ssmlMsg)); err != nil {
		return nil, err
	}
	var mp3 []byte
	for {
		_ = ws.SetReadDeadline(time.Now().Add(15 * time.Second))
		mt, data, err := ws.ReadMessage()
		if err != nil {
			if ctx.Err() != nil {
				return nil, ctx.Err()
			}
			return nil, fmt.Errorf("edge read: %w", err)
		}
		if mt == websocket.BinaryMessage {
			if len(data) < 2 {
				continue
			}
			hl := int(binary.BigEndian.Uint16(data[:2]))
			if 2+hl > len(data) || !bytes.Contains(data[2:2+hl], []byte("Path:audio")) {
				continue
			}
			mp3 = append(mp3, data[2+hl:]...)
			if len(mp3) > edgeMaxMP3Bytes {
				return nil, errors.New("edge audio too long")
			}
			continue
		}
		if bytes.Contains(data, []byte("Path:turn.end")) {
			break
		}
	}
	if len(mp3) == 0 {
		return nil, errors.New("edge returned no audio")
	}
	return mp3, nil
}

func edgeSpeakPCM16k(ctx context.Context, ws *websocket.Conn, voice, text string) ([]byte, error) {
	mp3, err := edgeSynthesizeMP3(ctx, ws, voice, text)
	if err != nil {
		return nil, err
	}
	return decodeMP3To16k(mp3)
}

// --- Gemini TTS ---

const (
	geminiAPIHost = "generativelanguage.googleapis.com"
	groqAPIHost   = "api.groq.com"
)

var geminiAPIBase = "https://" + geminiAPIHost + "/v1beta"

func geminiSpeakPCM16k(ctx context.Context, ocfg OpenAIConfig, key, text string) ([]byte, error) {
	text = strings.TrimSpace(text)
	if text == "" {
		return nil, errors.New("empty text")
	}
	payload := map[string]interface{}{
		"contents": []interface{}{map[string]interface{}{
			"parts": []interface{}{map[string]interface{}{"text": text}},
		}},
		"generationConfig": map[string]interface{}{
			"responseModalities": []string{"AUDIO"},
			"speechConfig": map[string]interface{}{
				"voiceConfig": map[string]interface{}{
					"prebuiltVoiceConfig": map[string]interface{}{"voiceName": ocfg.GeminiVoice},
				},
			},
		},
	}
	data, _ := json.Marshal(payload)
	rctx, cancel := context.WithTimeout(ctx, 45*time.Second)
	defer cancel()
	u := geminiAPIBase + "/models/" + ocfg.GeminiTTSModel + ":generateContent"
	req, err := http.NewRequestWithContext(rctx, http.MethodPost, u, bytes.NewReader(data))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("x-goog-api-key", key)
	resp, err := oaiHTTP.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode == http.StatusTooManyRequests {
		return nil, fmt.Errorf("gemini tts: %w (%v)", errTTSRateLimited, oaiAPIError(resp))
	}
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("gemini tts: %w", oaiAPIError(resp))
	}
	var out struct {
		Candidates []struct {
			Content struct {
				Parts []struct {
					InlineData struct {
						MimeType string `json:"mimeType"`
						Data     string `json:"data"`
					} `json:"inlineData"`
				} `json:"parts"`
			} `json:"content"`
		} `json:"candidates"`
	}
	if err := json.NewDecoder(io.LimitReader(resp.Body, 16<<20)).Decode(&out); err != nil {
		return nil, fmt.Errorf("gemini tts decode: %w", err)
	}
	for _, c := range out.Candidates {
		for _, p := range c.Content.Parts {
			if p.InlineData.Data == "" {
				continue
			}
			pcm, err := base64.StdEncoding.DecodeString(p.InlineData.Data)
			if err != nil {
				return nil, fmt.Errorf("gemini tts base64: %w", err)
			}
			rate := pcmRateFromMime(p.InlineData.MimeType, 24000)
			if rate != 16000 {
				pcm = resampleS16(pcm, rate, 16000)
			}
			return pcm, nil
		}
	}
	return nil, errors.New("gemini tts returned no audio")
}

// pcmRateFromMime reads "audio/L16;codec=pcm;rate=24000".
func pcmRateFromMime(mime string, def int) int {
	for _, p := range strings.Split(mime, ";") {
		kv := strings.SplitN(strings.TrimSpace(p), "=", 2)
		if len(kv) == 2 && strings.EqualFold(kv[0], "rate") {
			if n, err := strconv.Atoi(kv[1]); err == nil && n >= 8000 {
				return n
			}
		}
	}
	return def
}
