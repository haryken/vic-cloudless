package xiaozhi

import "testing"

func TestVADListenModeAlways(t *testing.T) {
	if !VADListenMode(Config{}) || !VADListenMode(Config{ListenMode: ListenModeVAD}) {
		t.Fatal("empty or vad listen_mode must listen until end of speech")
	}
	if !VADListenMode(Config{ListenMode: ListenModeFixed}) {
		t.Fatal("legacy fixed must also use end-of-speech (6s window removed)")
	}
	if !VADListenMode(Config{ListenMode: ListenModeFixed, Backend: BackendGemini}) {
		t.Fatal("direct-API backends always use end-of-speech")
	}
}
