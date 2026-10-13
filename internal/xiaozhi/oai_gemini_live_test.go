package xiaozhi

import (
	"context"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/gorilla/websocket"
)

func TestGLFunctionDecls(t *testing.T) {
	decls := glFunctionDecls(defaultConfig())
	byName := map[string]map[string]interface{}{}
	for _, d := range decls {
		if d["behavior"] != "BLOCKING" {
			t.Fatalf("%v not blocking", d["name"])
		}
		byName[d["name"].(string)] = d
	}
	if _, ok := byName["self__vector__stop"]["parameters"]; ok {
		t.Fatal("argument-less tool must omit parameters")
	}
	vol, ok := byName["self__audio_speaker__set_volume"]["parameters"].(map[string]interface{})
	if !ok {
		t.Fatal("set_volume lost its parameters")
	}
	prop := vol["properties"].(map[string]interface{})["volume"].(map[string]interface{})
	if prop["maximum"] != 100 || prop["type"] != "number" {
		t.Fatalf("volume schema %v", prop)
	}
	if _, ok := byName["self__conversation__end"]; !ok {
		t.Fatal("end_conversation tool missing")
	}
}

func TestGLSchemaStripsUnsupported(t *testing.T) {
	in := map[string]interface{}{
		"type": "object", "$schema": "x", "additionalProperties": false,
		"properties": map[string]interface{}{
			"a": map[string]interface{}{"type": "array", "default": 1,
				"items": map[string]interface{}{"type": "string", "examples": []string{"x"}}},
		},
	}
	got, _ := json.Marshal(glSchema(in))
	want := `{"properties":{"a":{"items":{"type":"string"},"type":"array"}},"type":"object"}`
	if string(got) != want {
		t.Fatalf("got %s", got)
	}
}

func TestVisionEndpoint(t *testing.T) {
	ocfg := defaultOpenAIConfig()
	ocfg.APIKey = "sk-openai"
	if base, key, model := visionEndpoint(BackendOpenAI, ocfg); key != "sk-openai" || model != "gpt-4o-mini" || base != ocfg.BaseURL {
		t.Fatalf("openai vision %s %s %s", base, key, model)
	}
	ocfg.GeminiAPIKey = "AIza-g"
	if _, key, _ := visionEndpoint(BackendOpenAI, ocfg); key != "sk-openai" {
		t.Fatal("openai backend with an OpenAI key must keep OpenAI vision")
	}
	if base, key, model := visionEndpoint(BackendGemini, ocfg); key != "AIza-g" || model != geminiVisionModel || !strings.HasSuffix(base, "/v1beta/openai") {
		t.Fatalf("gemini vision %s %s %s", base, key, model)
	}
	ocfg.APIKey = ""
	ocfg.GeminiAPIKey = "AIza-llm"
	ocfg.LLMBaseURL = "https://" + geminiAPIHost + "/v1beta/openai"
	if _, key, _ := visionEndpoint(BackendOpenAI, ocfg); key != "AIza-llm" {
		t.Fatalf("free pipeline vision key %q", key)
	}
}

func TestIsDirectAPIBackend(t *testing.T) {
	if !IsDirectAPIBackend(BackendOpenAI) || !IsDirectAPIBackend(BackendGemini) || IsDirectAPIBackend("") || IsDirectAPIBackend(BackendXiaozhi) {
		t.Fatal("direct backend classification")
	}
	if !VADListenMode(Config{Backend: BackendGemini}) {
		t.Fatal("gemini must use end-of-speech listens")
	}
}

// fakeGeminiLive records client messages; handle reacts to each one.
type fakeGeminiLive struct {
	mu     sync.Mutex
	setups []map[string]interface{}
	seen   []string
	handle func(send func(string), kind string, m map[string]interface{})
}

func (f *fakeGeminiLive) kinds() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.seen...)
}

func (f *fakeGeminiLive) start(t *testing.T) func() {
	up := websocket.Upgrader{}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("x-goog-api-key") != "AIza-test" || r.URL.Query().Get("key") != "" {
			http.Error(w, "auth", 401)
			return
		}
		c, err := up.Upgrade(w, r, nil)
		if err != nil {
			return
		}
		defer c.Close()
		send := func(s string) { _ = c.WriteMessage(websocket.BinaryMessage, []byte(s)) }
		for {
			_, data, err := c.ReadMessage()
			if err != nil {
				return
			}
			var m map[string]interface{}
			_ = json.Unmarshal(data, &m)
			kind := ""
			for k := range m {
				kind = k
			}
			if ri, ok := m["realtimeInput"].(map[string]interface{}); ok {
				for k := range ri {
					kind = k
				}
			}
			f.mu.Lock()
			if kind == "setup" {
				f.setups = append(f.setups, m["setup"].(map[string]interface{}))
			}
			if n := len(f.seen); kind != "audio" || n == 0 || f.seen[n-1] != "audio" {
				f.seen = append(f.seen, kind)
			}
			f.mu.Unlock()
			if kind == "setup" {
				send(`{"setupComplete":{}}`)
				send(`{"sessionResumptionUpdate":{"newHandle":"h1","resumable":true}}`)
			}
			f.handle(send, kind, m)
		}
	}))
	old := geminiLiveURL
	geminiLiveURL = "ws" + strings.TrimPrefix(srv.URL, "http")
	return func() {
		geminiLiveURL = old
		srv.Close()
	}
}

func glTestConfig() OpenAIConfig {
	ocfg := defaultOpenAIConfig()
	ocfg.GeminiAPIKey = "AIza-test"
	return ocfg
}

func TestGeminiLiveDetectToolFlow(t *testing.T) {
	ClearPendingMCPIntent("test")
	defer ClearPendingMCPIntent("test")
	defer oaiCloseSession("test")

	gotResp := make(chan map[string]interface{}, 1)
	f := &fakeGeminiLive{handle: func(send func(string), kind string, m map[string]interface{}) {
		switch kind {
		case "clientContent":
			send(`{"toolCall":{"functionCalls":[{"id":"fc1","name":"self__vector__stop","args":{}}]}}`)
			// Some servers close the turn before the tool result.
			send(`{"serverContent":{"turnComplete":true}}`)
		case "toolResponse":
			fr := m["toolResponse"].(map[string]interface{})["functionResponses"].([]interface{})
			gotResp <- fr[0].(map[string]interface{})
			send(`{"serverContent":{"outputTranscription":{"text":"Mình dừng "}}}`)
			send(`{"serverContent":{"outputTranscription":{"text":"lại rồi."}}}`)
			send(`{"serverContent":{"turnComplete":true,"interactionStatus":"IDLE"}}`)
		}
	}}
	defer f.start(t)()

	st := &oaiTurnState{}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := runGeminiLiveTurn(ctx, nil, defaultConfig(), glTestConfig(), nil, "dừng lại đi", st); err != nil {
		t.Fatal(err)
	}
	select {
	case r := <-gotResp:
		if r["id"] != "fc1" || r["name"] != "self__vector__stop" {
			t.Fatalf("tool response %v", r)
		}
	default:
		t.Fatal("no toolResponse")
	}
	if st.reply.String() != "Mình dừng lại rồi." || st.stt != "dừng lại đi" {
		t.Fatalf("reply %q stt %q", st.reply.String(), st.stt)
	}
	if intent, _, ok := PeekMCPIntentQueued(); !ok || intent != "intent_imperative_quiet" {
		t.Fatalf("intent %q ok=%v", intent, ok)
	}

	f.mu.Lock()
	setup := f.setups[0]
	f.mu.Unlock()
	if setup["model"] != "models/gemini-3.8-live" {
		t.Fatalf("model %v", setup["model"])
	}
	gen := setup["generationConfig"].(map[string]interface{})
	if _, ok := gen["thinkingConfig"]; ok {
		t.Fatal("Gemini 3 Live rejects thinking settings")
	}
	if !glThinkingOff("gemini-2.5-flash-native-audio-latest") {
		t.Fatal("2.5 native audio must run with thinking off")
	}
	aad := setup["realtimeInputConfig"].(map[string]interface{})["automaticActivityDetection"].(map[string]interface{})
	if aad["disabled"] != true {
		t.Fatal("server VAD must be off")
	}
	voice := gen["speechConfig"].(map[string]interface{})["voiceConfig"].(map[string]interface{})["prebuiltVoiceConfig"].(map[string]interface{})["voiceName"]
	if voice != "Kore" {
		t.Fatalf("voice %v", voice)
	}
	if ins := setup["systemInstruction"].(map[string]interface{})["parts"].([]interface{})[0].(map[string]interface{})["text"].(string); !strings.Contains(ins, "Vector") {
		t.Fatal("system instruction missing persona")
	}
}

func TestGeminiLiveResumeAfterGoAway(t *testing.T) {
	defer oaiCloseSession("test")
	f := &fakeGeminiLive{handle: func(send func(string), kind string, m map[string]interface{}) {
		if kind == "clientContent" {
			send(`{"goAway":{"timeLeft":"5s"}}`)
			send(`{"serverContent":{"outputTranscription":{"text":"ok"}}}`)
			send(`{"serverContent":{"turnComplete":true}}`)
		}
	}}
	defer f.start(t)()

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	for i := 0; i < 2; i++ {
		if err := runGeminiLiveTurn(ctx, nil, defaultConfig(), glTestConfig(), nil, "xin chào", &oaiTurnState{}); err != nil {
			t.Fatal(err)
		}
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	if len(f.setups) != 2 {
		t.Fatalf("setups %d, want a redial after goAway", len(f.setups))
	}
	if h := f.setups[1]["sessionResumption"].(map[string]interface{})["handle"]; h != "h1" {
		t.Fatalf("resume handle %v", h)
	}
	if _, ok := f.setups[0]["sessionResumption"].(map[string]interface{})["handle"]; ok {
		t.Fatal("first setup must not resume")
	}
}

// pcmFrames: 120 ms 16 kHz frames of a tone (amp>0) or faint noise.
func pcmFrames(n int, amp float64) [][]byte {
	out := make([][]byte, n)
	for f := range out {
		b := make([]byte, oaiMicFrameBytes)
		for i := 0; i < len(b)/2; i++ {
			v := 20 * math.Sin(float64(i)*1.7)
			if amp > 0 {
				v = amp * math.Sin(2*math.Pi*300*float64(f*len(b)/2+i)/16000)
			}
			binary.LittleEndian.PutUint16(b[i*2:], uint16(int16(v)))
		}
		out[f] = b
	}
	return out
}

func TestGeminiLiveMicTurn(t *testing.T) {
	defer oaiCloseSession("test")
	f := &fakeGeminiLive{handle: func(send func(string), kind string, m map[string]interface{}) {
		if kind == "activityEnd" {
			send(`{"serverContent":{"inputTranscription":{"text":" xin"}}}`)
			send(`{"serverContent":{"inputTranscription":{"text":" chào"}}}`)
			send(`{"serverContent":{"outputTranscription":{"text":"Chào bạn!"}}}`)
			send(`{"serverContent":{"turnComplete":true}}`)
		}
	}}
	defer f.start(t)()

	audio := make(chan []byte, 64)
	for _, fr := range pcmFrames(10, 0) {
		audio <- fr
	}
	for _, fr := range pcmFrames(10, 6000) {
		audio <- fr
	}
	for _, fr := range pcmFrames(12, 0) {
		audio <- fr
	}
	close(audio)

	st := &oaiTurnState{}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := runGeminiLiveTurn(ctx, audio, defaultConfig(), glTestConfig(), nil, "", st); err != nil {
		t.Fatal(err)
	}
	if got := strings.Join(f.kinds(), ","); got != "setup,activityStart,audio,activityEnd" {
		t.Fatalf("client messages %s", got)
	}
	if st.stt != "xin chào" || st.reply.String() != "Chào bạn!" {
		t.Fatalf("stt %q reply %q", st.stt, st.reply.String())
	}
}

func TestGeminiLiveSilentWakeSendsNothing(t *testing.T) {
	defer oaiCloseSession("test")
	f := &fakeGeminiLive{handle: func(func(string), string, map[string]interface{}) {}}
	defer f.start(t)()

	audio := make(chan []byte, 16)
	for _, fr := range pcmFrames(8, 0) {
		audio <- fr
	}
	close(audio)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	err := runGeminiLiveTurn(ctx, audio, defaultConfig(), glTestConfig(), nil, "", &oaiTurnState{})
	if !errors.Is(err, ErrNoSpeech) {
		t.Fatalf("err %v, want ErrNoSpeech", err)
	}
	time.Sleep(100 * time.Millisecond)
	if got := strings.Join(f.kinds(), ","); got != "setup" {
		t.Fatalf("client messages %s", got)
	}
}

// TestLiveGeminiLive talks to the real Gemini Live API with the robot's
// openai.json key: spoken questions (Edge TTS as the "mic") on one session,
// one of which needs a tool. The key is never printed.
func TestLiveGeminiLive(t *testing.T) {
	if os.Getenv("OAI_LIVE_GEMINI") != "1" {
		t.Skip("set OAI_LIVE_GEMINI=1 to call the real Gemini Live API")
	}
	ocfg := LoadOpenAIConfig()
	if m := os.Getenv("GEMINI_LIVE_MODEL"); m != "" {
		ocfg.GeminiLiveModel = m
	}
	if ocfg.geminiKey() == "" {
		t.Skip("no Gemini key in openai.json")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()

	var questions [][]byte
	for _, q := range []string{"Xin chào, bạn khỏe không?", "Pin của robot còn bao nhiêu phần trăm?"} {
		ews, err := edgeDial(ctx)
		if err != nil {
			t.Fatal("edge dial:", err)
		}
		pcm, err := edgeSpeakPCM16k(ctx, ews, "vi-VN-HoaiMyNeural", q)
		ews.Close()
		if err != nil {
			t.Fatal("edge tts:", err)
		}
		questions = append(questions, pcm)
	}

	start := time.Now()
	c, err := dialGeminiLive(ctx, defaultConfig(), ocfg, "")
	if err != nil {
		t.Fatal("dial:", err)
	}
	defer c.close()
	t.Logf("setup %v model=%s", time.Since(start), ocfg.GeminiLiveModel)

	for _, question := range questions {
		// Real-time pacing, like the mic.
		if err := c.sendActivity("activityStart"); err != nil {
			t.Fatal(err)
		}
		for off := 0; off < len(question); off += oaiMicFrameBytes {
			end := off + oaiMicFrameBytes
			if end > len(question) {
				end = len(question)
			}
			if err := c.sendAudio(question[off:end]); err != nil {
				t.Fatal(err)
			}
			time.Sleep(120 * time.Millisecond)
		}
		sent := time.Now()
		if err := c.sendActivity("activityEnd"); err != nil {
			t.Fatal(err)
		}
		liveCollect(t, ctx, c, sent)
	}
}

func liveCollect(t *testing.T, ctx context.Context, c *glConn, sent time.Time) {
	var heard, said strings.Builder
	audioBytes, tools := 0, 0
	var firstAudio time.Duration
	for {
		select {
		case <-ctx.Done():
			t.Fatal("timeout")
		case <-c.dead:
			t.Fatal("closed:", c.deadErr)
		case data := <-c.msgs:
			var m glServerMsg
			if json.Unmarshal(data, &m) != nil {
				continue
			}
			if tc := m.ToolCall; tc != nil {
				var resp []map[string]interface{}
				for _, fc := range tc.FunctionCalls {
					tools++
					t.Logf("tool %s %s (+%v)", fc.Name, fc.Args, time.Since(sent).Round(time.Millisecond))
					resp = append(resp, map[string]interface{}{"id": fc.ID, "name": fc.Name,
						"response": map[string]interface{}{"result": `{"percent":76,"charging":false,"summary":"Pin còn 76%, không sạc."}`}})
				}
				_ = c.send(map[string]interface{}{"toolResponse": map[string]interface{}{"functionResponses": resp}})
				continue
			}
			sc := m.ServerContent
			if sc == nil {
				continue
			}
			if sc.InputTranscription != nil {
				heard.WriteString(sc.InputTranscription.Text)
			}
			if sc.OutputTranscription != nil {
				said.WriteString(sc.OutputTranscription.Text)
			}
			if sc.ModelTurn != nil {
				for _, p := range sc.ModelTurn.Parts {
					if p.InlineData != nil {
						if audioBytes == 0 {
							firstAudio = time.Since(sent)
						}
						audioBytes += len(p.InlineData.Data) * 3 / 4
					}
				}
			}
			if sc.TurnComplete && sc.InteractionStatus != "IN_PROGRESS" && (tools == 0 || audioBytes > 0) {
				t.Logf("heard %q", strings.TrimSpace(heard.String()))
				t.Logf("said %q", strings.TrimSpace(said.String()))
				t.Logf("first audio +%v, %.1fs of audio, tools=%d", firstAudio.Round(time.Millisecond),
					float64(audioBytes)/(glOutputRate*2), tools)
				if audioBytes == 0 {
					t.Fatal("no audio reply")
				}
				return
			}
		}
	}
}

// A key without Google Search quota (free tier, Gemini 3 Live) is refused at
// setup; the session reconnects without the search tool and remembers it.
func TestGeminiLiveDropsSearchOnQuota(t *testing.T) {
	defer oaiCloseSession("test")
	defer glNoSearch.Delete("gemini-3.8-live")
	up := websocket.Upgrader{}
	var mu sync.Mutex
	var withSearch []bool
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		c, err := up.Upgrade(w, r, nil)
		if err != nil {
			return
		}
		defer c.Close()
		_, data, err := c.ReadMessage()
		if err != nil {
			return
		}
		search := strings.Contains(string(data), "googleSearch")
		mu.Lock()
		withSearch = append(withSearch, search)
		mu.Unlock()
		if search {
			_ = c.WriteMessage(websocket.CloseMessage, websocket.FormatCloseMessage(websocket.CloseInternalServerErr,
				"You exceeded your current quota, please check your plan and billing details."))
			return
		}
		_ = c.WriteMessage(websocket.TextMessage, []byte(`{"setupComplete":{}}`))
		for {
			if _, _, err := c.ReadMessage(); err != nil {
				return
			}
		}
	}))
	defer srv.Close()
	old := geminiLiveURL
	geminiLiveURL = "ws" + strings.TrimPrefix(srv.URL, "http")
	defer func() { geminiLiveURL = old }()

	ocfg := glTestConfig()
	ocfg.GeminiLiveModel = "gemini-3.8-live"
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	for i := 0; i < 2; i++ {
		c, err := dialGeminiLive(ctx, defaultConfig(), ocfg, "")
		if err != nil {
			t.Fatal(err)
		}
		c.close()
	}
	mu.Lock()
	defer mu.Unlock()
	if fmt.Sprint(withSearch) != "[true false false]" {
		t.Fatalf("setups with search %v", withSearch)
	}
}

func TestGeminiLiveSetupRejected(t *testing.T) {
	defer oaiCloseSession("test")
	up := websocket.Upgrader{}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		c, err := up.Upgrade(w, r, nil)
		if err != nil {
			return
		}
		_, _, _ = c.ReadMessage()
		_ = c.WriteMessage(websocket.CloseMessage, websocket.FormatCloseMessage(websocket.ClosePolicyViolation, "API key not valid"))
		c.Close()
	}))
	defer srv.Close()
	old := geminiLiveURL
	geminiLiveURL = "ws" + strings.TrimPrefix(srv.URL, "http")
	defer func() { geminiLiveURL = old }()

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	err := runGeminiLiveTurn(ctx, nil, defaultConfig(), glTestConfig(), nil, "xin chào", &oaiTurnState{})
	if err == nil || !strings.Contains(err.Error(), "API key not valid") {
		t.Fatalf("err %v", err)
	}
}
