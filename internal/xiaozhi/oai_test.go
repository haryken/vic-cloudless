package xiaozhi

import (
	"context"
	"encoding/binary"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/gorilla/websocket"
)

func TestOAIToolNameRoundTrip(t *testing.T) {
	for _, n := range []string{"self.vector.fireworks", "self.camera.analyze_photo", "self.get_device_status"} {
		o := oaiToolName(n)
		if strings.Contains(o, ".") {
			t.Fatalf("%s → %s still has a dot", n, o)
		}
		if back := mcpToolName(o); back != n {
			t.Fatalf("%s → %s → %s", n, o, back)
		}
	}
}

func TestOAIToolsShape(t *testing.T) {
	cfg := defaultConfig()
	chat := oaiChatTools(cfg)
	rt := oaiRealtimeTools(cfg)
	if len(chat) == 0 || len(chat) != len(rt) {
		t.Fatalf("chat %d realtime %d", len(chat), len(rt))
	}
	seenEnd := false
	for i, c := range chat {
		fn, _ := c["function"].(map[string]interface{})
		name, _ := fn["name"].(string)
		if name == "" || name != rt[i]["name"] {
			t.Fatalf("tool %d name %q vs %v", i, name, rt[i]["name"])
		}
		for _, r := range name {
			if !(r == '_' || r == '-' || r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' || r >= '0' && r <= '9') {
				t.Fatalf("invalid OpenAI function name %q", name)
			}
		}
		if len(name) > 64 {
			t.Fatalf("function name too long: %q", name)
		}
		if p, _ := fn["parameters"].(map[string]interface{}); p["type"] != "object" {
			t.Fatalf("%s parameters %v", name, p)
		}
		if name == oaiToolName(oaiEndConversationTool) {
			seenEnd = true
		}
	}
	if !seenEnd {
		t.Fatal("end_conversation tool missing")
	}
}

func TestRunOAIToolQueuesIntent(t *testing.T) {
	ClearPendingMCPIntent("test")
	defer ClearPendingMCPIntent("test")
	r := runOAITool(oaiToolName("self.vector.stop"), "{}")
	if r.End {
		t.Fatal("stop flagged as end")
	}
	intent, _, ok := PeekMCPIntentQueued()
	if !ok || intent != "intent_imperative_quiet" {
		t.Fatalf("queued %q ok=%v", intent, ok)
	}
	if !runOAITool(oaiToolName(oaiEndConversationTool), "").End {
		t.Fatal("end tool not flagged")
	}
}

func s16(vals ...int16) []byte {
	b := make([]byte, len(vals)*2)
	for i, v := range vals {
		binary.LittleEndian.PutUint16(b[i*2:], uint16(v))
	}
	return b
}

func TestDown24to16Carry(t *testing.T) {
	var carry []byte
	// 7 samples split oddly (incl. a half sample) → 2 groups out, 1 sample carried.
	in := s16(100, 200, 400, 1000, 2000, 4000, 7)
	out := down24to16(&carry, in[:5])
	out = append(out, down24to16(&carry, in[5:])...)
	want := s16(100, 300, 1000, 3000)
	if string(out) != string(want) {
		t.Fatalf("out %v want %v", out, want)
	}
	if len(carry) != 2 {
		t.Fatalf("carry %d bytes", len(carry))
	}
}

func TestSentenceSplitter(t *testing.T) {
	var s sentenceSplitter
	var got []string
	for _, d := range []string{"Chào bạn", "! Pin còn 3", ".5 vôn nhé. ", "**Mình** sẽ bắn", " pháo hoa"} {
		got = append(got, s.push(d)...)
	}
	got = append(got, s.flush()...)
	want := []string{"Chào bạn!", "Pin còn 3.5 vôn nhé.", "Mình sẽ bắn pháo hoa"}
	if strings.Join(got, "|") != strings.Join(want, "|") {
		t.Fatalf("got %q", got)
	}
}

func TestSentenceSplitterLongClause(t *testing.T) {
	var s sentenceSplitter
	long := strings.Repeat("một hai ba, ", 30)
	out := s.push(long)
	if len(out) == 0 {
		t.Fatal("long unpunctuated text never split")
	}
	for _, o := range out {
		if n := len([]rune(o)); n > plMaxSentenceRunes {
			t.Fatalf("chunk %d runes", n)
		}
	}
}

func TestPLSaveHistoryKeepsWholeExchanges(t *testing.T) {
	defer oaiCloseSession("test")
	msgs := []chatMsg{{Role: "system", Content: "sys"}}
	for i := 0; i < 5; i++ {
		msgs = append(msgs,
			chatMsg{Role: "user", Content: fmt.Sprint("u", i)},
			chatMsg{Role: "assistant", ToolCalls: []chatToolCall{{ID: "c"}}},
			chatMsg{Role: "tool", ToolCallID: "c", Content: "{}"},
			chatMsg{Role: "assistant", Content: fmt.Sprint("a", i)})
	}
	plSaveHistory(msgs, OpenAIConfig{MemoryTurns: 2})
	h := oaiSess.history
	if len(h) != 8 || h[0].Role != "user" || h[0].Content != "u3" {
		t.Fatalf("history %d first %+v", len(h), h[0])
	}
}

func TestOAIChatStreamToolCalls(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/chat/completions" || r.Header.Get("Authorization") != "Bearer k" {
			http.Error(w, "bad", 400)
			return
		}
		var body map[string]interface{}
		_ = json.NewDecoder(r.Body).Decode(&body)
		if body["stream"] != true || body["tools"] == nil {
			http.Error(w, "no stream/tools", 400)
			return
		}
		w.Header().Set("Content-Type", "text/event-stream")
		for _, l := range []string{
			`{"choices":[{"delta":{"content":"Mình bắn "}}]}`,
			`{"choices":[{"delta":{"content":"pháo hoa nhé."}}]}`,
			`{"choices":[{"delta":{"tool_calls":[{"index":0,"id":"call_1","type":"function","function":{"name":"self__vector__fire","arguments":""}}]}}]}`,
			`{"choices":[{"delta":{"tool_calls":[{"index":0,"function":{"name":"works","arguments":"{\"a\":"}}]}}]}`,
			`{"choices":[{"delta":{"tool_calls":[{"index":0,"function":{"arguments":"1}"}}]}}]}`,
		} {
			fmt.Fprintf(w, "data: %s\n\n", l)
		}
		fmt.Fprint(w, "data: [DONE]\n\n")
	}))
	defer srv.Close()

	ocfg := OpenAIConfig{BaseURL: srv.URL + "/v1", APIKey: "k", LLMModel: "m"}
	var streamed strings.Builder
	content, calls, err := oaiChatStream(context.Background(), ocfg, []chatMsg{{Role: "user", Content: "hi"}},
		oaiChatTools(defaultConfig()), func(d string) { streamed.WriteString(d) })
	if err != nil {
		t.Fatal(err)
	}
	if content != "Mình bắn pháo hoa nhé." || streamed.String() != content {
		t.Fatalf("content %q streamed %q", content, streamed.String())
	}
	if len(calls) != 1 || calls[0].ID != "call_1" || calls[0].Function.Name != "self__vector__fireworks" ||
		calls[0].Function.Arguments != `{"a":1}` {
		t.Fatalf("calls %+v", calls)
	}
}

func TestOAIChatStreamLocalOverride(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "" {
			http.Error(w, "local LLM must not get the OpenAI key", 400)
			return
		}
		fmt.Fprint(w, "data: {\"choices\":[{\"delta\":{\"content\":\"ok\"}}]}\n\ndata: [DONE]\n\n")
	}))
	defer srv.Close()
	ocfg := OpenAIConfig{BaseURL: "http://unused.invalid/v1", APIKey: "openai-key", LLMBaseURL: srv.URL}
	content, _, err := oaiChatStream(context.Background(), ocfg, nil, nil, func(string) {})
	if err != nil || content != "ok" {
		t.Fatalf("content %q err %v", content, err)
	}
}

func TestOAITranscribe(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if err := r.ParseMultipartForm(4 << 20); err != nil {
			http.Error(w, err.Error(), 400)
			return
		}
		f, _, err := r.FormFile("file")
		if err != nil {
			http.Error(w, err.Error(), 400)
			return
		}
		head := make([]byte, 4)
		_, _ = io.ReadFull(f, head)
		if string(head) != "RIFF" || r.FormValue("model") != "stt" || r.FormValue("language") != "vi" {
			http.Error(w, "bad form", 400)
			return
		}
		fmt.Fprint(w, `{"text":" bắn pháo hoa đi "}`)
	}))
	defer srv.Close()
	ocfg := OpenAIConfig{BaseURL: srv.URL, APIKey: "k", STTModel: "stt", Language: "vi"}
	text, err := oaiTranscribe(context.Background(), ocfg, make([]byte, 32000))
	if err != nil || strings.TrimSpace(text) != "bắn pháo hoa đi" {
		t.Fatalf("text %q err %v", text, err)
	}
}

func TestSTTHallucination(t *testing.T) {
	if !sttHallucination("Hãy subscribe cho kênh Ghiền Mì Gõ") || sttHallucination("bắn pháo hoa") {
		t.Fatal("hallucination filter")
	}
}

func TestOAIMemoryText(t *testing.T) {
	txt := oaiMemoryText([]oaiExchange{{User: "tên tôi là Linh", Assistant: "Chào Linh!"}})
	if !strings.Contains(txt, "Người dùng: tên tôi là Linh") || !strings.Contains(txt, "Vector: Chào Linh!") {
		t.Fatalf("memory text %q", txt)
	}
	if got := oaiMemoryTail(make([]oaiExchange, 12), 10); len(got) != 10 {
		t.Fatalf("tail %d", len(got))
	}
}

func TestRTSessionUpdateGA(t *testing.T) {
	ocfg := defaultOpenAIConfig()
	u := rtSessionUpdate(defaultConfig(), ocfg, false, "inst")
	sess := u["session"].(map[string]interface{})
	if sess["type"] != "realtime" || sess["instructions"] != "inst" {
		t.Fatalf("session %v", sess)
	}
	in := sess["audio"].(map[string]interface{})["input"].(map[string]interface{})
	td := in["turn_detection"].(map[string]interface{})
	if td["type"] != "server_vad" || td["interrupt_response"] != false || td["create_response"] != true {
		t.Fatalf("turn detection %v", td)
	}
	b := rtSessionUpdate(defaultConfig(), ocfg, true, "inst")["session"].(map[string]interface{})
	if b["input_audio_format"] != "pcm16" || b["type"] != nil {
		t.Fatalf("beta session %v", b)
	}
}

// TestLivePipeline hits the real API with the robot's openai.json (OAI_LIVE=1).
func TestLivePipeline(t *testing.T) {
	if os.Getenv("OAI_LIVE") != "1" {
		t.Skip("set OAI_LIVE=1 to call the real API")
	}
	ocfg := LoadOpenAIConfig()
	if ocfg.APIKey == "" {
		t.Skip("no API key in openai.json")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	start := time.Now()
	var pcm []byte
	if err := oaiSpeakOpenAI(ctx, ocfg, "Xin chào, mình là Vector.", func(b []byte) error {
		pcm = append(pcm, b...)
		return nil
	}); err != nil {
		t.Fatal("tts:", err)
	}
	t.Logf("TTS %d bytes 16k PCM (%v audio) in %v", len(pcm), time.Duration(len(pcm)/32)*time.Millisecond, time.Since(start))

	start = time.Now()
	text, err := oaiTranscribe(ctx, ocfg, pcm)
	if err != nil {
		t.Fatal("stt:", err)
	}
	t.Logf("STT %q in %v", text, time.Since(start))
	if !strings.Contains(strings.ToLower(text), "chào") {
		t.Fatalf("STT did not hear the TTS: %q", text)
	}

	start = time.Now()
	msgs := []chatMsg{
		{Role: "system", Content: oaiInstructions(ocfg, nil)},
		{Role: "user", Content: "bắn pháo hoa cho mình xem đi"},
	}
	content, calls, err := oaiChatStream(ctx, ocfg, msgs, oaiChatTools(defaultConfig()), func(string) {})
	if err != nil {
		t.Fatal("llm:", err)
	}
	t.Logf("LLM %q calls %+v in %v", content, calls, time.Since(start))
	if len(calls) == 0 {
		t.Fatal("LLM did not call a tool for fireworks")
	}
}

// Fake Realtime server: one detect turn whose response calls a tool, then a
// follow-up response with a transcript.
func TestRealtimeDetectTurnToolFlow(t *testing.T) {
	ClearPendingMCPIntent("test")
	defer ClearPendingMCPIntent("test")
	defer oaiCloseSession("test")

	up := websocket.Upgrader{}
	gotOutput := make(chan string, 1)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer k" || r.URL.Query().Get("model") != "gpt-realtime-mini" {
			http.Error(w, "auth", 401)
			return
		}
		c, err := up.Upgrade(w, r, nil)
		if err != nil {
			return
		}
		defer c.Close()
		send := func(s string) { _ = c.WriteMessage(websocket.TextMessage, []byte(s)) }
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
				send(`{"type":"session.updated"}`)
			case "conversation.item.create":
				item := m["item"].(map[string]interface{})
				if item["type"] == "function_call_output" {
					gotOutput <- fmt.Sprint(item["call_id"], ":", item["output"])
				}
			case "response.create":
				responses++
				send(`{"type":"response.created"}`)
				if responses == 1 {
					send(`{"type":"response.function_call_arguments.done","call_id":"c1","name":"self__vector__stop","arguments":"{}"}`)
				} else {
					send(`{"type":"response.output_audio_transcript.delta","delta":"Mình dừng lại rồi."}`)
				}
				send(`{"type":"response.done","response":{"status":"completed"}}`)
			}
		}
	}))
	defer srv.Close()

	ocfg := defaultOpenAIConfig()
	ocfg.APIKey = "k"
	ocfg.BaseURL = "http" + strings.TrimPrefix(srv.URL, "http")
	st := &oaiTurnState{}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := runRealtimeTurn(ctx, nil, defaultConfig(), ocfg, nil, "dừng lại đi", st); err != nil {
		t.Fatal(err)
	}
	select {
	case out := <-gotOutput:
		if !strings.HasPrefix(out, "c1:") {
			t.Fatalf("function output %q", out)
		}
	default:
		t.Fatal("no function_call_output sent")
	}
	if st.reply.String() != "Mình dừng lại rồi." || st.stt != "dừng lại đi" {
		t.Fatalf("reply %q stt %q", st.reply.String(), st.stt)
	}
	if intent, _, ok := PeekMCPIntentQueued(); !ok || intent != "intent_imperative_quiet" {
		t.Fatalf("intent %q ok=%v", intent, ok)
	}
}
