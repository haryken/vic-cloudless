package xiaozhi

import (
	"context"
	"fmt"
	"time"

	"github.com/digital-dream-labs/vector-cloud/internal/log"
)

// micCapture is the mic uplink outcome shared by the fixed and vad listen modes.
type micCapture struct {
	frames     int
	startedAt  time.Time
	forceClose bool
}

// captureFixed streams mic audio until Vector closes the stream (anim's ~6s
// window → AudioDone), then sends ListenStop.
func captureFixed(turnCtx context.Context, client *Client, audioStream <-chan []byte, enc *Encoder, encErr error) (micCapture, error) {
	// Accumulate opus-encoding buffer (we need EncFrameSamples*2 bytes per frame).
	encBuf := make([]byte, 0, EncFrameSamples*2*4)
	audioSendCount := 0
	// If FakeTrigger opened cloud-listen but anim never streams mic, do not hang forever.
	var micWaitDeadline <-chan time.Time = time.After(12 * time.Second)
	listenStartAt := time.Now()

streamLoop:
	for {
		select {
		case chunk, ok := <-audioStream:
			if !ok {
				MarkMicListenEnded()
				break streamLoop
			}
			if encErr != nil || enc == nil {
				// No opus encoder — send raw PCM as a fallback note; server will see silence.
				// This path only happens in non-vicos builds (dev).
				continue
			}
			encBuf = append(encBuf, chunk...)
			// Drain complete Opus frames from buffer.
			for len(encBuf) >= EncFrameSamples*2 {
				frame := encBuf[:EncFrameSamples*2]
				encBuf = encBuf[EncFrameSamples*2:]
				opusFrame, err := enc.EncodeFrame(frame)
				if err != nil {
					log.Println("[Xiaozhi] encode error:", err)
					continue
				}
				if serr := client.SendAudio(opusFrame); serr != nil {
					log.Println("[Xiaozhi] send audio error:", serr)
					InvalidateSession("send_audio_failed")
					return micCapture{frames: audioSendCount, startedAt: listenStartAt, forceClose: true},
						fmt.Errorf("send audio: %w", serr)
				}
				audioSendCount++
				if audioSendCount == 1 {
					MarkMicUplink()
					log.Printf("[Xiaozhi][Mic] first uplink frame (+%v after ListenStart)",
						time.Since(listenStartAt).Round(time.Millisecond))
				}
			}
		// Do NOT drain TTS/LLM/Audio here. Tenclass often starts TTS as soon as STT
		// is ready — while mic uplink is still running. Discarding = missing head.
		case <-micWaitDeadline:
			if audioSendCount == 0 {
				log.Printf("[Xiaozhi] no mic audio for %v after listen start — aborting stuck listen",
					time.Since(listenStartAt).Round(time.Millisecond))
				return micCapture{startedAt: listenStartAt}, fmt.Errorf("no mic uplink after listen start")
			}
			// Uplink already alive; disable further mic-wait wakes by draining to nil channel.
			micWaitDeadline = nil
		case err := <-client.ErrCh():
			return micCapture{frames: audioSendCount, startedAt: listenStartAt, forceClose: true},
				fmt.Errorf("WSS died during listen: %w", err)
		case <-turnCtx.Done():
			if turnCtx.Err() == context.Canceled {
				return micCapture{frames: audioSendCount, startedAt: listenStartAt}, ErrAborted
			}
			return micCapture{frames: audioSendCount, startedAt: listenStartAt}, turnCtx.Err()
		}
	}

	if err := client.ListenStop(); err != nil {
		log.Println("[Xiaozhi] listen stop error:", err)
	}
	MarkMicListenEnded()
	listenDur := time.Since(listenStartAt).Round(time.Millisecond)
	log.Printf("[Xiaozhi][Mic] ListenStop — sent %d opus frames, uplink %v", audioSendCount, listenDur)
	return micCapture{frames: audioSendCount, startedAt: listenStartAt}, nil
}
