package xiaozhi

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"time"

	"github.com/digital-dream-labs/vector-cloud/internal/log"
	"github.com/digital-dream-labs/vector-cloud/internal/xiaozhi/listenvad"
)

const (
	ListenModeFixed = "fixed"
	ListenModeVAD   = "vad"

	// vic-anim and vic-engine lift their ~6s / 10s listen caps while this exists.
	ListenVADFlagPath = "/run/vic-cloud/xiaozhi-listen-vad"

	vadNoSpeechTimeout = 10 * time.Second
	// Must stay <= the anim/engine caps used when ListenVADFlagPath exists.
	vadMaxListen = 30 * time.Second

	// ChannelIdleTimeout matches xiaozhi-esp32 Protocol::IsTimeout
	// (main/protocols/protocol.cc kTimeoutSeconds = 120): no useful
	// conversation traffic for this long → close the audio channel / WSS.
	ChannelIdleTimeout = 120 * time.Second

	// VADStreamTimeout replaces the 9s Vector stream timeout for standing
	// VAD listens (continuous / ambient). Open/gate use Timeout=0 instead —
	// a 34s deadline was killing quiet open-mic streams (mic_closed @ ~562 frames).
	VADStreamTimeout = vadMaxListen + 4*time.Second
)

// ErrNoSpeech: "vad" listen closed because nobody spoke; caller ends quietly.
var ErrNoSpeech = listenvad.ErrNoSpeech

// VADListenMode: end-of-speech listening is the default; only an explicit
// "fixed" keeps Vector's ~6s window. Direct-API backends always use VAD.
func VADListenMode(cfg Config) bool {
	return cfg.ListenMode != ListenModeFixed || IsDirectAPIBackend(cfg.Backend)
}

// ListenVADActive reports the /run flag that vic-anim latched for this stream.
// wired updates it on save, before vic-cloud reloads xiaozhi.json.
func ListenVADActive() bool {
	_, err := os.Stat(ListenVADFlagPath)
	return err == nil
}

// SyncListenVADFlag mirrors listen_mode into /run for vic-anim / vic-engine.
func SyncListenVADFlag(cfg Config) {
	if cfg.Enabled && VADListenMode(cfg) {
		_ = os.MkdirAll(filepath.Dir(ListenVADFlagPath), 0775)
		_ = os.WriteFile(ListenVADFlagPath, []byte("1"), 0664)
		return
	}
	_ = os.Remove(ListenVADFlagPath)
}

// listenTune is how this conversation mode feeds the server.
type listenTune struct {
	noSpeech      time.Duration
	maxListen     time.Duration
	muteHead      time.Duration
	minRMS        float64
	waitSpeech    bool
	silencePrompt time.Duration
}

// tuneListen matches continuous unless the mode says otherwise.
// Open uploads every frame and does not close on the 10s/30s cuts.
// Gate keeps that wake, but holds audio on the robot until GateRMS.
// When silence_prompt_sec is on, NoSpeech fires the nudge before MaxListen
// closes the channel (ESP32 CheckSilencePrompt).
func tuneListen() listenTune {
	t := listenTune{noSpeech: vadNoSpeechTimeout, maxListen: vadMaxListen}
	sp := SilencePromptDuration()
	if GateMic() {
		t.minRMS = GateRMS()
		t.waitSpeech = true
		t.muteHead = 400 * time.Millisecond
		t.maxListen = 3 * time.Minute
		t.silencePrompt = sp
		return t
	}
	if OpenMic() {
		// Stay open while quiet (ESP32-style), then end listen + close WSS.
		// MaxListen is the hard stop even if ambient noise tripped the gate.
		t.noSpeech = ChannelIdleTimeout
		t.maxListen = ChannelIdleTimeout
		if sp > 0 {
			t.noSpeech = sp
		}
		return t
	}
	if AmbientActive() {
		// Wheel noise sits above the standing-still floor (200).
		t.minRMS = 500
	}
	if sp > 0 {
		t.noSpeech = sp
		if t.maxListen < ChannelIdleTimeout {
			t.maxListen = ChannelIdleTimeout
		}
	}
	return t
}

// captureVAD keeps the mic open until the server returns STT (ESP32 auto mode).
// An STT consumed here is put back on the client so the shared STT wait sees it.
func captureVAD(ctx context.Context, client *Client, audioStream <-chan []byte, enc *Encoder, encErr error) (micCapture, error) {
	var encode func([]byte) ([]byte, error)
	if encErr == nil && enc != nil {
		encode = enc.EncodeFrame
	}
	tune := tuneListen()
	if tune.waitSpeech {
		log.Printf("[Xiaozhi][VAD] holding uplink until RMS %.0f", tune.minRMS)
	}
	res, err := listenvad.Capture(ctx, client, audioStream, listenvad.Options{
		NoSpeech:      tune.noSpeech,
		MaxListen:     tune.maxListen,
		MicWait:       12 * time.Second,
		FrameBytes:    EncFrameSamples * 2,
		MinRMS:        tune.minRMS,
		WaitForSpeech: tune.waitSpeech,
		MuteHead:      tune.muteHead,
		SilencePrompt: tune.silencePrompt,
		Encode:        encode,
		OnFirstFrame: func() {
			MarkMicUplink()
			log.Println("[Xiaozhi][VAD] first uplink frame")
		},
	})
	out := micCapture{frames: res.Frames, startedAt: res.StartedAt}

	switch {
	case errors.Is(err, listenvad.ErrSilencePrompt):
		MarkMicListenEnded()
		log.Printf("[Xiaozhi][VAD] silence prompt (%s) — %d frames, %v",
			res.Reason, res.Frames, res.Duration.Round(time.Millisecond))
		return out, ErrSilencePrompt
	case err == nil, errors.Is(err, listenvad.ErrNoSpeech):
		MarkMicListenEnded()
		if res.StopErr != nil {
			log.Println("[Xiaozhi][VAD] listen stop error:", res.StopErr)
		}
		log.Printf("[Xiaozhi][VAD] ListenStop (%s) — sent %d opus frames, uplink %v, speech=%v floor=%.0f peak=%.0f",
			res.Reason, res.Frames, res.Duration.Round(time.Millisecond), res.Speech, res.Floor, res.Peak)
		if res.STT != "" {
			client.requeueSTT(res.STT)
		}
		// Quiet past silence_prompt_sec (NoSpeech timer) → nudge, not close.
		if SilencePromptSec() > 0 && res.STT == "" &&
			(res.Reason == listenvad.ReasonNoSpeech || res.Reason == listenvad.ReasonSilencePrompt) {
			return out, ErrSilencePrompt
		}
		// Open idle: max_listen/no_speech with no STT → ErrNoSpeech so we close
		// WSS immediately (skip the 30s STT wait).
		if err == nil && OpenMic() && res.STT == "" &&
			(res.Reason == listenvad.ReasonMaxListen || res.Reason == listenvad.ReasonNoSpeech) {
			return out, listenvad.ErrNoSpeech
		}
		return out, err
	case errors.Is(err, listenvad.ErrNoUplink):
		log.Printf("[Xiaozhi] no mic audio for %v after listen start — aborting stuck listen",
			res.Duration.Round(time.Millisecond))
		return out, fmt.Errorf("no mic uplink after listen start")
	case errors.Is(err, listenvad.ErrSend):
		log.Println("[Xiaozhi] send audio error:", err)
		InvalidateSession("send_audio_failed")
		out.forceClose = true
		return out, err
	case errors.Is(err, listenvad.ErrServer):
		out.forceClose = true
		return out, err
	case errors.Is(err, context.Canceled):
		return out, ErrAborted
	default:
		return out, err
	}
}
