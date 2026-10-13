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

	// VADStreamTimeout replaces the 9s Vector stream timeout so the stream
	// outlives the open mic and the intent that closes the listen UI.
	VADStreamTimeout = vadMaxListen + 4*time.Second
)

// ErrNoSpeech: "vad" listen closed because nobody spoke; caller ends quietly.
var ErrNoSpeech = listenvad.ErrNoSpeech

// VADListenMode: the OpenAI backend always ends the listen on end-of-speech.
func VADListenMode(cfg Config) bool {
	return cfg.ListenMode == ListenModeVAD || IsDirectAPIBackend(cfg.Backend)
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

// captureVAD keeps the mic open until the server returns STT (ESP32 auto mode).
// An STT consumed here is put back on the client so the shared STT wait sees it.
func captureVAD(ctx context.Context, client *Client, audioStream <-chan []byte, enc *Encoder, encErr error) (micCapture, error) {
	var encode func([]byte) ([]byte, error)
	if encErr == nil && enc != nil {
		encode = enc.EncodeFrame
	}
	res, err := listenvad.Capture(ctx, client, audioStream, listenvad.Options{
		NoSpeech:   vadNoSpeechTimeout,
		MaxListen:  vadMaxListen,
		MicWait:    12 * time.Second,
		FrameBytes: EncFrameSamples * 2,
		Encode:     encode,
		OnFirstFrame: func() {
			MarkMicUplink()
			log.Println("[Xiaozhi][VAD] first uplink frame")
		},
	})
	out := micCapture{frames: res.Frames, startedAt: res.StartedAt}

	switch {
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
