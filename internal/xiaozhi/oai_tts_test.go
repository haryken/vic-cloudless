package xiaozhi

import (
	"context"
	"encoding/base64"
	"encoding/binary"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/gorilla/websocket"
)

func TestOAIKeyRouting(t *testing.T) {
	c := OpenAIConfig{APIKey: "openai", BaseURL: "https://api.openai.com/v1", Mode: OpenAIModePipeline}
	if c.sttKey() != "openai" || c.llmKey() != "openai" || c.sttBase() != c.BaseURL {
		t.Fatal("default endpoints should use the OpenAI key")
	}
	c.STTBaseURL = "https://api.groq.com/openai/v1"
	c.LLMBaseURL = "https://generativelanguage.googleapis.com/v1beta/openai"
	c.GroqAPIKey, c.GeminiAPIKey, c.STTAPIKey = "groq", "gem", "custom"
	if c.sttKey() != "groq" || c.llmKey() != "gem" || c.geminiKey() != "gem" {
		t.Fatalf("provider keys: stt %q llm %q", c.sttKey(), c.llmKey())
	}
	c.LLMBaseURL = "https://api.groq.com/openai/v1"
	if c.llmKey() != "groq" {
		t.Fatal("switching the LLM to Groq must use the Groq key")
	}
	c.STTBaseURL, c.LLMBaseURL = "http://192.168.1.5:11434/v1", "http://192.168.1.5:11434/v1"
	if c.sttKey() != "custom" || c.llmKey() != "custom" {
		t.Fatal("same custom host should share the step key")
	}
	c.LLMBaseURL = "http://192.168.1.6:11434/v1"
	if c.llmKey() != "" {
		t.Fatal("a different LLM host must not get any other key")
	}
}

func TestOAILegacyKeysMigrate(t *testing.T) {
	c := parseOpenAIConfig([]byte(`{"mode":"pipeline","stt_base_url":"https://api.groq.com/openai/v1","stt_api_key":"gsk",
		"llm_base_url":"https://generativelanguage.googleapis.com/v1beta/openai","llm_api_key":"AIza"}`))
	if c.sttKey() != "gsk" || c.llmKey() != "AIza" || c.geminiKey() != "AIza" || c.STTAPIKey != "" {
		t.Fatalf("legacy %+v", c)
	}
	c = parseOpenAIConfig([]byte(`{"mode":"pipeline","stt_base_url":"https://api.groq.com/openai/v1","stt_api_key":"srv","groq_api_key":""}`))
	if c.sttKey() != "" || c.STTAPIKey != "srv" {
		t.Fatalf("new layout must not move a custom-server key: %+v", c)
	}
}

func TestOAINeedsKey(t *testing.T) {
	c := OpenAIConfig{BaseURL: "https://api.openai.com/v1", Mode: OpenAIModeRealtime}
	if !c.needsKey() {
		t.Fatal("realtime without key")
	}
	c.Mode = OpenAIModePipeline
	c.STTBaseURL = "https://api.groq.com/openai/v1"
	c.GroqAPIKey = "groq"
	c.LLMBaseURL = "https://api.groq.com/openai/v1"
	if c.needsKey() {
		t.Fatal("all-free pipeline must not need the OpenAI key")
	}
	c.LLMBaseURL = ""
	if !c.needsKey() {
		t.Fatal("OpenAI LLM without key")
	}
}

func TestOAILanguageAuto(t *testing.T) {
	c := OpenAIConfig{Language: OpenAILanguageAuto}
	if c.sttLanguage() != "" || c.speechLanguage() != "vi" {
		t.Fatalf("auto: stt %q speech %q", c.sttLanguage(), c.speechLanguage())
	}
	c.Language = "en"
	if c.sttLanguage() != "en" || c.speechLanguage() != "en" {
		t.Fatal("explicit language")
	}
}

func TestOAITranscribeSTTOverrideAuto(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer groq" {
			http.Error(w, "wrong key "+r.Header.Get("Authorization"), 401)
			return
		}
		if err := r.ParseMultipartForm(4 << 20); err != nil {
			http.Error(w, err.Error(), 400)
			return
		}
		if _, ok := r.MultipartForm.Value["language"]; ok {
			http.Error(w, "auto must omit language", 400)
			return
		}
		fmt.Fprint(w, `{"text":"hello Vector"}`)
	}))
	defer srv.Close()
	ocfg := OpenAIConfig{BaseURL: "http://unused.invalid/v1", APIKey: "openai-key", STTBaseURL: srv.URL,
		STTAPIKey: "groq", STTModel: "whisper-large-v3-turbo", Language: OpenAILanguageAuto}
	text, err := oaiTranscribe(context.Background(), ocfg, make([]byte, 32000))
	if err != nil || text != "hello Vector" {
		t.Fatalf("text %q err %v", text, err)
	}
}

func TestLangSegmenterTags(t *testing.T) {
	s := &langSegmenter{def: "vi"}
	got := s.split("Mình thích <en>machine learning</en> lắm, còn <zh>你好</zh> nghĩa là xin chào.")
	want := []langSeg{{"vi", "Mình thích"}, {"en", "machine learning"}, {"vi", "lắm, còn"}, {"zh-CN", "你好"}, {"vi", "nghĩa là xin chào."}}
	if fmt.Sprint(got) != fmt.Sprint(want) {
		t.Fatalf("got %v\nwant %v", got, want)
	}
}

func TestLangSegmenterCarriesOpenTag(t *testing.T) {
	s := &langSegmenter{def: "vi"}
	a := s.split("<en>Hello there.")
	b := s.split("How are you?</en> Mình khỏe.")
	if len(a) != 1 || a[0].lang != "en" || len(b) != 2 || b[0].lang != "en" || b[1].lang != "vi" {
		t.Fatalf("a %v b %v", a, b)
	}
}

func TestLangSegmenterScriptDetect(t *testing.T) {
	s := &langSegmenter{def: "vi"}
	got := s.split("Món này tiếng Trung là 火锅 đó, tiếng Nhật là ありがとう.")
	langs := []string{}
	for _, g := range got {
		langs = append(langs, g.lang)
	}
	if strings.Join(langs, ",") != "vi,zh-CN,vi,ja" {
		t.Fatalf("segments %v", got)
	}
}

func TestStripLangTags(t *testing.T) {
	if got := stripLangTags("Mình thích <en>AI</en> và <zh-CN>你好</zh-CN>."); got != "Mình thích AI và 你好." {
		t.Fatalf("got %q", got)
	}
}

func TestOAIInstructionsLangTags(t *testing.T) {
	ocfg := defaultOpenAIConfig()
	if strings.Contains(oaiInstructions(ocfg, nil), "<en>") {
		t.Fatal("tags only for the Google pipeline voice")
	}
	ocfg.Mode = OpenAIModePipeline
	ocfg.TTSEngine = TTSEngineGoogle
	if !strings.Contains(oaiInstructions(ocfg, nil), "<en>") {
		t.Fatal("google pipeline should ask for language tags")
	}
}

func TestEdgeSecMSGEC(t *testing.T) {
	// 1791928800 + 11644473600 is a multiple of 300: a window boundary.
	start := time.Unix(1791928800, 0)
	a := edgeSecMSGEC(start)
	if len(a) != 64 || strings.ToUpper(a) != a {
		t.Fatalf("token %q", a)
	}
	if edgeSecMSGEC(start.Add(299*time.Second)) != a {
		t.Fatal("token should be stable within a 5-minute window")
	}
	if edgeSecMSGEC(start.Add(300*time.Second)) == a {
		t.Fatal("token should change every 5 minutes")
	}
	name, loc := edgeVoiceName("en-US-EmmaMultilingualNeural")
	if name != "Microsoft Server Speech Text to Speech Voice (en-US, EmmaMultilingualNeural)" || loc != "en-US" {
		t.Fatalf("voice %q %q", name, loc)
	}
}

func TestEdgeSynthesizeFake(t *testing.T) {
	up := websocket.Upgrader{CheckOrigin: func(*http.Request) bool { return true }}
	var gotSSML string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Query().Get("Sec-MS-GEC") == "" || r.URL.Query().Get("TrustedClientToken") != edgeTrustedToken {
			http.Error(w, "no token", 403)
			return
		}
		c, err := up.Upgrade(w, r, nil)
		if err != nil {
			return
		}
		defer c.Close()
		for i := 0; i < 2; i++ {
			_, msg, err := c.ReadMessage()
			if err != nil {
				return
			}
			if strings.Contains(string(msg), "Path:ssml") {
				gotSSML = string(msg)
			}
		}
		_ = c.WriteMessage(websocket.TextMessage, []byte("Path:turn.start\r\n\r\n{}"))
		for _, chunk := range []string{"ID3a", "bcd"} {
			hdr := "Path:audio\r\n"
			frame := make([]byte, 2)
			binary.BigEndian.PutUint16(frame, uint16(len(hdr)))
			frame = append(frame, hdr...)
			frame = append(frame, chunk...)
			_ = c.WriteMessage(websocket.BinaryMessage, frame)
		}
		_ = c.WriteMessage(websocket.TextMessage, []byte("Path:turn.end\r\n\r\n{}"))
	}))
	defer srv.Close()
	old := edgeTTSURL
	edgeTTSURL = "ws" + strings.TrimPrefix(srv.URL, "http")
	defer func() { edgeTTSURL = old }()

	mp3, err := edgeSynthesizeMP3(context.Background(), nil, "vi-VN-HoaiMyNeural", "Tom & Jerry <3")
	if err != nil || string(mp3) != "ID3abcd" {
		t.Fatalf("mp3 %q err %v", mp3, err)
	}
	if !strings.Contains(gotSSML, "Tom &amp; Jerry &lt;3") || !strings.Contains(gotSSML, "(vi-VN, HoaiMyNeural)") {
		t.Fatalf("ssml %q", gotSSML)
	}
}

func TestGeminiTTSFake(t *testing.T) {
	pcm24 := make([]byte, 24000*2/10)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("x-goog-api-key") != "gem" || !strings.HasSuffix(r.URL.Path, "/models/tts-model:generateContent") {
			http.Error(w, `{"error":{"message":"bad request"}}`, 400)
			return
		}
		var body struct {
			Contents []struct {
				Parts []struct {
					Text string `json:"text"`
				} `json:"parts"`
			} `json:"contents"`
			GenerationConfig struct {
				ResponseModalities []string `json:"responseModalities"`
				SpeechConfig       struct {
					VoiceConfig struct {
						PrebuiltVoiceConfig struct {
							VoiceName string `json:"voiceName"`
						} `json:"prebuiltVoiceConfig"`
					} `json:"voiceConfig"`
				} `json:"speechConfig"`
			} `json:"generationConfig"`
		}
		_ = json.NewDecoder(r.Body).Decode(&body)
		if body.Contents[0].Parts[0].Text != "Xin chào" || body.GenerationConfig.SpeechConfig.VoiceConfig.PrebuiltVoiceConfig.VoiceName != "Kore" {
			http.Error(w, `{"error":{"message":"bad body"}}`, 400)
			return
		}
		fmt.Fprintf(w, `{"candidates":[{"content":{"parts":[{"inlineData":{"mimeType":"audio/L16;codec=pcm;rate=24000","data":%q}}]}}]}`,
			base64.StdEncoding.EncodeToString(pcm24))
	}))
	defer srv.Close()
	old := geminiAPIBase
	geminiAPIBase = srv.URL
	defer func() { geminiAPIBase = old }()

	ocfg := OpenAIConfig{GeminiTTSModel: "tts-model", GeminiVoice: "Kore"}
	pcm, err := geminiSpeakPCM16k(context.Background(), ocfg, "gem", "Xin chào")
	if err != nil {
		t.Fatal(err)
	}
	if n := len(pcm); n < 3000 || n > 3400 {
		t.Fatalf("expected ~100 ms of 16 kHz PCM, got %d bytes", n)
	}
}

func TestGeminiTTSRateLimit(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, `{"error":{"message":"quota"}}`, 429)
	}))
	defer srv.Close()
	old := geminiAPIBase
	geminiAPIBase = srv.URL
	defer func() { geminiAPIBase = old }()
	_, err := geminiSpeakPCM16k(context.Background(), OpenAIConfig{GeminiTTSModel: "m", GeminiVoice: "Kore"}, "k", "hi")
	if err == nil || !strings.Contains(err.Error(), "rate limited") {
		t.Fatalf("err %v", err)
	}
}

// Gemini sends complete calls sharing index 0 and a thought signature that
// must be echoed back.
func TestOAIChatStreamGeminiCalls(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, `data: {"choices":[{"delta":{"role":"assistant","tool_calls":[`+
			`{"index":0,"id":"a","type":"function","function":{"name":"self__vector__action","arguments":"{\"action\":\"dance\"}"},"extra_content":{"google":{"thought_signature":"sig1"}}},`+
			`{"index":0,"id":"b","type":"function","function":{"name":"self__get_device_status","arguments":"{}"}}]}}]}`+"\n\ndata: [DONE]\n\n")
	}))
	defer srv.Close()
	_, calls, err := oaiChatStream(context.Background(), OpenAIConfig{LLMBaseURL: srv.URL}, nil, nil, func(string) {})
	if err != nil || len(calls) != 2 || calls[0].ID != "a" || calls[1].ID != "b" {
		t.Fatalf("calls %+v err %v", calls, err)
	}
	if calls[1].Function.Name != "self__get_device_status" || calls[0].Function.Arguments != `{"action":"dance"}` {
		t.Fatalf("calls %+v", calls)
	}
	out, _ := json.Marshal(chatMsg{Role: "assistant", ToolCalls: calls})
	if !strings.Contains(string(out), `"extra_content":{"google":{"thought_signature":"sig1"}}`) ||
		strings.Count(string(out), "extra_content") != 1 {
		t.Fatalf("assistant msg %s", out)
	}
}

func TestPCMRateFromMime(t *testing.T) {
	if pcmRateFromMime("audio/L16;codec=pcm;rate=24000", 1) != 24000 || pcmRateFromMime("audio/L16", 22050) != 22050 {
		t.Fatal("mime rate")
	}
}

func TestPLTTSDrainBatches(t *testing.T) {
	tt := &plTTS{ch: make(chan string, 8)}
	tt.ch <- "Câu hai."
	tt.ch <- "Câu ba."
	if got := tt.drainInto("Câu một."); got != "Câu một. Câu hai. Câu ba." {
		t.Fatalf("got %q", got)
	}
}

// TestLiveEdgeTTS calls the real Edge Read Aloud service (OAI_LIVE_EDGE=1).
func TestLiveEdgeTTS(t *testing.T) {
	if os.Getenv("OAI_LIVE_EDGE") != "1" {
		t.Skip("set OAI_LIVE_EDGE=1 to call the real Edge TTS")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	start := time.Now()
	pre := startEdgePrewarm(ctx)
	ws := pre.take(ctx)
	if ws == nil {
		t.Fatal("prewarm dial failed")
	}
	t.Logf("Edge dial %v", time.Since(start))
	start = time.Now()
	mp3, err := edgeSynthesizeMP3(ctx, ws, "en-US-EmmaMultilingualNeural", "Xin chào, mình là Vector. Hello, nice to meet you. 你好。")
	if err != nil {
		t.Fatal(err)
	}
	t.Logf("Edge synth on pre-dialed conn: %d mp3 bytes in %v", len(mp3), time.Since(start))
	start = time.Now()
	pcm, err := decodeMP3To16k(mp3)
	if err != nil {
		t.Fatal(err)
	}
	t.Logf("decode %d bytes (%v audio) in %v", len(pcm), time.Duration(len(pcm)/32)*time.Millisecond, time.Since(start))
	if len(pcm) < 32000 {
		t.Fatal("too little audio")
	}
}

// TestLiveGoogleSplit speaks a tagged mixed reply via Google (OAI_LIVE_EDGE=1).
func TestLiveGoogleSplit(t *testing.T) {
	if os.Getenv("OAI_LIVE_EDGE") != "1" {
		t.Skip("set OAI_LIVE_EDGE=1 to call the real Google TTS")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	tt := &ttsTurn{seg: langSegmenter{def: "vi"}}
	var pcm []byte
	start := time.Now()
	err := oaiSpeak(ctx, OpenAIConfig{TTSEngine: TTSEngineGoogle}, "Mình thích <en>machine learning</en>, tiếng Trung là 机器学习.",
		tt, func(b []byte) error { pcm = append(pcm, b...); return nil })
	if err != nil {
		t.Fatal(err)
	}
	t.Logf("Google split %v audio in %v", time.Duration(len(pcm)/32)*time.Millisecond, time.Since(start))
}
