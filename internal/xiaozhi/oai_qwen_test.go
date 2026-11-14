package xiaozhi

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/gorilla/websocket"
)

func qwenTestConfig() OpenAIConfig {
	ocfg := defaultOpenAIConfig()
	ocfg.DashScopeAPIKey = "sk-qwen-test"
	ocfg.QwenRegion = "intl"
	return ocfg
}

func TestQwenSessionUpdate(t *testing.T) {
	ocfg := qwenTestConfig()
	tg := rtTargetFor(BackendQwen, ocfg)
	if !tg.qwen || tg.grok || tg.key != "sk-qwen-test" || tg.voice != "longanqian_v3.1" ||
		!strings.Contains(tg.url, "dashscope-intl") || !strings.Contains(tg.url, "qwen-audio-3.1-realtime-flash") {
		t.Fatalf("target %+v", tg)
	}
	ocfg.QwenRegion = "cn"
	if !strings.Contains(rtTargetFor(BackendQwen, ocfg).url, "dashscope.aliyuncs.com/api-ws") {
		t.Fatalf("cn url %s", rtTargetFor(BackendQwen, ocfg).url)
	}

	sess := qwenSessionUpdate(defaultConfig(), qwenTestConfig(), tg, "inst")["session"].(map[string]interface{})
	if sess["voice"] != "longanqian_v3.1" || sess["instructions"] != "inst" ||
		sess["input_audio_format"] != "pcm" || sess["output_audio_format"] != "pcm" {
		t.Fatalf("session %v", sess)
	}
	if mods, _ := sess["modalities"].([]string); len(mods) != 2 {
		t.Fatalf("modalities %v", sess["modalities"])
	}
	if td := sess["turn_detection"].(map[string]interface{}); td["type"] != "server_vad" {
		t.Fatalf("turn detection %v", td)
	}
	tools := sess["tools"].([]map[string]interface{})
	if len(tools) == 0 || tools[0]["type"] != "function" || tools[0]["function"] == nil {
		t.Fatalf("tools must use nested function shape: %v", tools)
	}
	if tr := sess["input_audio_transcription"].(map[string]interface{}); tr["language"] != "vi" {
		t.Fatalf("transcription %v", tr)
	}

	ocfg = qwenTestConfig()
	ocfg.Language = OpenAILanguageAuto
	sess = qwenSessionUpdate(defaultConfig(), ocfg, tg, "inst")["session"].(map[string]interface{})
	if sess["input_audio_transcription"] != nil {
		t.Fatal("auto language must not send transcription language")
	}

	if rtTargetFor(BackendOpenAI, ocfg).qwen ||
		rtSignature(defaultConfig(), ocfg, tg) == rtSignature(defaultConfig(), ocfg, rtTargetFor(BackendOpenAI, ocfg)) {
		t.Fatal("openai and qwen sessions must not share a socket")
	}
	if !IsDirectAPIBackend(BackendQwen) || !VADListenMode(Config{Backend: BackendQwen}) {
		t.Fatal("qwen is a direct-API backend with end-of-speech listens")
	}
}

func TestQwenVisionEndpoint(t *testing.T) {
	ocfg := qwenTestConfig()
	if base, key, model := visionEndpoint(BackendQwen, ocfg); base != dashscopeAPIIntl || key != "sk-qwen-test" || model != qwenVisionModel {
		t.Fatalf("qwen vision %s %s %s", base, key, model)
	}
	ocfg.QwenRegion = "cn"
	if base, _, _ := visionEndpoint(BackendQwen, ocfg); base != dashscopeAPICN {
		t.Fatalf("cn vision base %s", base)
	}
	ocfg = qwenTestConfig()
	ocfg.GeminiAPIKey = "AIza-g"
	if _, key, _ := visionEndpoint(BackendQwen, ocfg); key != "AIza-g" {
		t.Fatal("a free Gemini key wins for photos")
	}
}

func fakeQwen(t *testing.T, updates *[]map[string]interface{}, mu *sync.Mutex, gotOutput chan<- string) *httptest.Server {
	up := websocket.Upgrader{}
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer sk-qwen-test" ||
			r.Header.Get("OpenAI-Beta") != "" ||
			r.URL.Query().Get("model") != "qwen-audio-3.1-realtime-flash" {
			http.Error(w, "auth", 401)
			return
		}
		c, err := up.Upgrade(w, r, nil)
		if err != nil {
			return
		}
		defer c.Close()
		send := func(s string) { _ = c.WriteMessage(websocket.TextMessage, []byte(s)) }
		send(`{"type":"session.created"}`)
		for {
			_, data, err := c.ReadMessage()
			if err != nil {
				return
			}
			var m map[string]interface{}
			_ = json.Unmarshal(data, &m)
			switch m["type"] {
			case "session.update":
				mu.Lock()
				*updates = append(*updates, m)
				mu.Unlock()
				send(`{"type":"session.updated"}`)
			case "input_audio_buffer.append":
				send(`{"type":"input_audio_buffer.speech_stopped"}`)
				send(`{"type":"response.created"}`)
				send(`{"type":"response.audio.delta","delta":"AAEC"}`)
				send(`{"type":"response.audio_transcript.delta","delta":"xin chào"}`)
				send(`{"type":"response.done"}`)
				select {
				case gotOutput <- "ok":
				default:
				}
			}
		}
	}))
}

func TestQwenDialSession(t *testing.T) {
	var updates []map[string]interface{}
	var mu sync.Mutex
	got := make(chan string, 1)
	srv := fakeQwen(t, &updates, &mu, got)
	defer srv.Close()

	oldIntl, oldCN := qwenRealtimeURLIntl, qwenRealtimeURLCN
	qwenRealtimeURLIntl = "ws" + strings.TrimPrefix(srv.URL, "http")
	qwenRealtimeURLCN = qwenRealtimeURLIntl
	defer func() {
		qwenRealtimeURLIntl, qwenRealtimeURLCN = oldIntl, oldCN
	}()

	ocfg := qwenTestConfig()
	cfg := defaultConfig()
	cfg.Backend = BackendQwen
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	tg := rtTargetFor(BackendQwen, ocfg)
	c, err := dialRealtime(ctx, cfg, ocfg, tg)
	if err != nil {
		t.Fatal(err)
	}
	defer c.close()
	if c.inRate != qwenInputRate {
		t.Fatalf("inRate %d", c.inRate)
	}
	mu.Lock()
	defer mu.Unlock()
	if len(updates) == 0 {
		t.Fatal("no session.update")
	}
	sess := updates[0]["session"].(map[string]interface{})
	if fmt.Sprint(sess["input_audio_format"]) != "pcm" || fmt.Sprint(sess["voice"]) != "longanqian_v3.1" {
		t.Fatalf("session %v", sess)
	}
}
