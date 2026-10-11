// Package listenvad is the "vad" listen mode uplink: keep the mic open until the
// Xiaozhi server returns STT (server-side end-of-speech, like xiaozhi-esp32 auto
// mode). The local SpeechGate only decides whether anyone started talking, so a
// silent wake can close early; it never decides when speech ended.
package listenvad

import (
	"context"
	"errors"
	"fmt"
	"time"
)

// End reasons reported in Result.Reason.
const (
	ReasonServerSTT = "server_stt"
	ReasonNoSpeech  = "no_speech"
	ReasonMaxListen = "max_listen"
	ReasonMicClosed = "mic_closed"
)

var (
	ErrNoSpeech = errors.New("no speech after listen start")
	ErrNoUplink = errors.New("no mic uplink after listen start")
	ErrSend     = errors.New("send audio")
	ErrServer   = errors.New("WSS died during listen")
)

// Uplink is the subset of the Xiaozhi client used while the mic is open.
type Uplink interface {
	SendAudio(opus []byte) error
	ListenStop() error
	STTCh() <-chan string
	ErrCh() <-chan error
}

type Options struct {
	// NoSpeech closes the listen if SpeechGate saw no voice by then.
	NoSpeech time.Duration
	// MaxListen is the hard cap even while the user keeps talking.
	MaxListen time.Duration
	// MicWait fails the turn if anim never streams any audio.
	MicWait time.Duration
	// FrameBytes is the PCM size of one encoder frame (s16le).
	FrameBytes int
	// Encode turns one PCM frame into an Opus packet; nil drops audio (dev builds).
	Encode func(pcm []byte) ([]byte, error)
	// OnFirstFrame runs once after the first Opus frame is sent.
	OnFirstFrame func()
}

type Result struct {
	Frames    int
	StartedAt time.Time
	Duration  time.Duration
	// STT is set when the server finished recognition while the mic was open.
	STT    string
	Reason string
	Speech bool
	Floor  float64
	Peak   float64
	// StopErr is the ListenStop send error (logged by caller, not fatal).
	StopErr error
}

// Capture streams audio until the server returns STT, nobody speaks within
// NoSpeech, MaxListen elapses, or anim closes the mic. ListenStop is sent on
// every normal exit; ErrNoSpeech is returned when nobody spoke.
func Capture(ctx context.Context, up Uplink, audio <-chan []byte, opts Options) (Result, error) {
	res := Result{StartedAt: time.Now()}
	gate := NewSpeechGate()
	encBuf := make([]byte, 0, opts.FrameBytes*4)

	noSpeech := time.NewTimer(opts.NoSpeech)
	defer noSpeech.Stop()
	maxListen := time.NewTimer(opts.MaxListen)
	defer maxListen.Stop()
	micWait := time.After(opts.MicWait)

	finish := func() {
		res.Duration = time.Since(res.StartedAt)
		res.Speech = gate.Started()
		res.Floor = gate.Floor()
		res.Peak = gate.Peak()
	}

loop:
	for {
		select {
		case chunk, ok := <-audio:
			if !ok {
				res.Reason = ReasonMicClosed
				break loop
			}
			gate.Feed(chunk)
			if opts.Encode == nil || opts.FrameBytes <= 0 {
				continue
			}
			encBuf = append(encBuf, chunk...)
			for len(encBuf) >= opts.FrameBytes {
				frame := encBuf[:opts.FrameBytes]
				encBuf = encBuf[opts.FrameBytes:]
				packet, err := opts.Encode(frame)
				if err != nil {
					continue
				}
				if err := up.SendAudio(packet); err != nil {
					finish()
					return res, fmt.Errorf("%w: %v", ErrSend, err)
				}
				res.Frames++
				if res.Frames == 1 && opts.OnFirstFrame != nil {
					opts.OnFirstFrame()
				}
			}
		case stt := <-up.STTCh():
			res.STT = stt
			res.Reason = ReasonServerSTT
			break loop
		case <-noSpeech.C:
			// No uplink yet is MicWait's job (caller relistens on ErrNoUplink).
			if res.Frames > 0 && !gate.Started() {
				res.Reason = ReasonNoSpeech
				break loop
			}
		case <-maxListen.C:
			res.Reason = ReasonMaxListen
			break loop
		case <-micWait:
			if res.Frames == 0 {
				finish()
				return res, ErrNoUplink
			}
			micWait = nil
		case err := <-up.ErrCh():
			finish()
			return res, fmt.Errorf("%w: %v", ErrServer, err)
		case <-ctx.Done():
			finish()
			return res, ctx.Err()
		}
	}

	finish()
	res.StopErr = up.ListenStop()
	if res.Reason == ReasonNoSpeech {
		return res, ErrNoSpeech
	}
	return res, nil
}
