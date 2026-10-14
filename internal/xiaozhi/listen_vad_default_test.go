package xiaozhi

import "testing"

func TestVADListenModeDefault(t *testing.T) {
	if !VADListenMode(Config{}) || !VADListenMode(Config{ListenMode: ListenModeVAD}) {
		t.Fatal("empty or vad listen_mode must listen until end of speech")
	}
	if VADListenMode(Config{ListenMode: ListenModeFixed}) {
		t.Fatal("explicit fixed must keep the ~6s window")
	}
	if !VADListenMode(Config{ListenMode: ListenModeFixed, Backend: BackendGemini}) {
		t.Fatal("direct-API backends always use end-of-speech")
	}
}
