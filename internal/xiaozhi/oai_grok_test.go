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

func grokTestConfig() OpenAIConfig {
	ocfg := defaultOpenAIConfig()
	ocfg.XAIAPIKey = "xai-test"
	return ocfg
}

func TestGrokSessionUpdate(t *testing.T) {
	ocfg := grokTestConfig()
	tg := rtTargetFor(BackendGrok, ocfg)
	if !tg.grok || tg.key != "xai-test" || tg.voice != "eve" || !strings.HasSuffix(tg.url, "?model=grok-voice-latest") {
		t.Fatalf("target %+v", tg)
	}
	sess := grokSessionUpdate(defaultConfig(), ocfg, tg, "inst", true)["session"].(map[string]interface{})
	if sess["voice"] != "eve" || sess["instructions"] != "inst" || sess["type"] != nil || sess["output_modalities"] != nil {
		t.Fatalf("session %v", sess)
	}
	if r := sess["reasoning"].(map[string]interface{}); r["effort"] != "none" {
		t.Fatalf("reasoning %v", r)
	}
	if td := sess["turn_detection"].(map[string]interface{}); td["type"] != "server_vad" {
		t.Fatalf("turn detection %v", td)
	}
	in := sess["audio"].(map[string]interface{})["input"].(map[string]interface{})
	if f := in["format"].(map[string]interface{}); f["type"] != "audio/pcm" || f["rate"] != rtSampleRate {
		t.Fatalf("input format %v", f)
	}
	if tr := in["transcription"].(map[string]interface{}); tr["language_hint"] != "vi" {
		t.Fatalf("transcription %v", tr)
	}
	tools := sess["tools"].([]map[string]interface{})
	if tools[len(tools)-1]["type"] != "web_search" || tools[0]["type"] != "function" {
		t.Fatalf("tools %v", tools)
	}
	noSearch := grokSessionUpdate(defaultConfig(), ocfg, tg, "inst", false)["session"].(map[string]interface{})
	if strings.Contains(fmt.Sprint(noSearch["tools"]), "web_search") {
		t.Fatal("web_search must be dropped")
	}

	ocfg.Language = OpenAILanguageAuto
	in = grokSessionUpdate(defaultConfig(), ocfg, tg, "inst", false)["session"].(map[string]interface{})["audio"].(map[string]interface{})["input"].(map[string]interface{})
	if in["transcription"] != nil {
		t.Fatal("auto language must not send a hint")
	}

	if rtTargetFor(BackendOpenAI, ocfg).grok || rtSignature(defaultConfig(), ocfg, tg) == rtSignature(defaultConfig(), ocfg, rtTargetFor(BackendOpenAI, ocfg)) {
		t.Fatal("openai and grok sessions must not share a socket")
	}
}

func TestRTAudioDeltaField(t *testing.T) {
	for _, raw := range []string{`{"delta":"AAEC"}`, `{"audio":"AAEC"}`} {
		if pcm, ok := rtAudioDelta([]byte(raw)); !ok || len(pcm) != 3 {
			t.Fatalf("%s -> %v %v", raw, pcm, ok)
		}
	}
	if _, ok := rtAudioDelta([]byte(`{}`)); ok {
		t.Fatal("empty delta")
	}
}

func TestGrokVisionEndpoint(t *testing.T) {
	ocfg := grokTestConfig()
	if base, key, model := visionEndpoint(BackendGrok, ocfg); base != xaiAPIBase || key != "xai-test" || model != grokVisionModel {
		t.Fatalf("grok vision %s %s %s", base, key, model)
	}
	ocfg.GeminiAPIKey = "AIza-g"
	if _, key, _ := visionEndpoint(BackendGrok, ocfg); key != "AIza-g" {
		t.Fatal("a free Gemini key wins for photos")
	}
	if _, key, _ := visionEndpoint(BackendOpenAI, grokTestConfig()); key == "xai-test" {
		t.Fatal("xAI key must only be used by the grok backend")
	}
	if !IsDirectAPIBackend(BackendGrok) || !VADListenMode(Config{Backend: BackendGrok}) {
		t.Fatal("grok is a direct-API backend with end-of-speech listens")
	}
}

// fakeGrok answers like the xAI Voice Agent; rejectSearch refuses web_search.
func fakeGrok(t *testing.T, rejectSearch bool, updates *[]map[string]interface{}, mu *sync.Mutex, gotOutput chan<- string) *httptest.Server {
	up := websocket.Upgrader{}
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer xai-test" || r.Header.Get("OpenAI-Beta") != "" ||
			r.URL.Query().Get("model") != "grok-voice-latest" {
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
		responses := 0
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
				*updates = append(*updates, m["session"].(map[string]interface{}))
				mu.Unlock()
				if rejectSearch && strings.Contains(string(data), "web_search") {
					send(`{"type":"error","error":{"type":"invalid_request_error","message":"web_search not enabled for this team"}}`)
					continue
				}
				send(`{"type":"session.updated"}`)
			case "conversation.item.create":
				item := m["item"].(map[string]interface{})
				if item["type"] == "function_call_output" && gotOutput != nil {
					gotOutput <- fmt.Sprint(item["call_id"], ":", item["output"])
				}
			case "response.create":
				responses++
				send(`{"type":"response.created"}`)
				if responses == 1 {
					send(`{"type":"response.function_call_arguments.done","call_id":"g1","name":"self__vector__stop","arguments":"{}"}`)
				} else {
					send(`{"type":"response.output_audio_transcript.delta","delta":"Dừng rồi nha."}`)
				}
				send(`{"type":"response.done","response":{"status":"completed"}}`)
			}
		}
	}))
}

func TestGrokDetectTurnToolFlow(t *testing.T) {
	ClearPendingMCPIntent("test")
	defer ClearPendingMCPIntent("test")
	defer oaiCloseSession("test")
	defer grokNoSearch.Delete("grok-voice-latest")

	var mu sync.Mutex
	var updates []map[string]interface{}
	gotOutput := make(chan string, 1)
	srv := fakeGrok(t, false, &updates, &mu, gotOutput)
	defer srv.Close()
	old := grokRealtimeURL
	grokRealtimeURL = "ws" + strings.TrimPrefix(srv.URL, "http")
	defer func() { grokRealtimeURL = old }()

	cfg := defaultConfig()
	cfg.Backend = BackendGrok
	st := &oaiTurnState{}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := runRealtimeTurn(ctx, nil, cfg, grokTestConfig(), nil, "dừng lại đi", st); err != nil {
		t.Fatal(err)
	}
	select {
	case out := <-gotOutput:
		if !strings.HasPrefix(out, "g1:") {
			t.Fatalf("function output %q", out)
		}
	default:
		t.Fatal("no function_call_output sent")
	}
	if st.reply.String() != "Dừng rồi nha." || st.stt != "dừng lại đi" {
		t.Fatalf("reply %q stt %q", st.reply.String(), st.stt)
	}
	mu.Lock()
	defer mu.Unlock()
	if len(updates) != 1 || updates[0]["voice"] != "eve" || !strings.Contains(fmt.Sprint(updates[0]["tools"]), "web_search") {
		t.Fatalf("session updates %v", updates)
	}
}

func TestGrokDropsRejectedSearch(t *testing.T) {
	defer oaiCloseSession("test")
	defer grokNoSearch.Delete("grok-voice-latest")

	var mu sync.Mutex
	var updates []map[string]interface{}
	srv := fakeGrok(t, true, &updates, &mu, nil)
	defer srv.Close()
	old := grokRealtimeURL
	grokRealtimeURL = "ws" + strings.TrimPrefix(srv.URL, "http")
	defer func() { grokRealtimeURL = old }()

	ocfg := grokTestConfig()
	tg := rtTargetFor(BackendGrok, ocfg)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	for i := 0; i < 2; i++ {
		c, err := dialRealtime(ctx, defaultConfig(), ocfg, tg)
		if err != nil {
			t.Fatal(err)
		}
		c.close()
	}
	mu.Lock()
	defer mu.Unlock()
	var search []bool
	for _, u := range updates {
		search = append(search, strings.Contains(fmt.Sprint(u["tools"]), "web_search"))
	}
	if fmt.Sprint(search) != "[true false false]" {
		t.Fatalf("setups with search %v", search)
	}
}

func TestGrokDefaults(t *testing.T) {
	ocfg := defaultOpenAIConfig()
	if ocfg.XAIAPIKey != "" || ocfg.GrokModel != "grok-voice-latest" || ocfg.GrokVoice != "eve" {
		t.Fatalf("defaults %+v", ocfg)
	}
}
