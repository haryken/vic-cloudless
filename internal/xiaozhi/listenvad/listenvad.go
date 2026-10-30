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
	ReasonServerSTT     = "server_stt"
	ReasonNoSpeech      = "no_speech"
	ReasonMaxListen     = "max_listen"
	ReasonMicClosed     = "mic_closed"
	ReasonEndSpeech     = "end_of_speech"
	ReasonSilencePrompt = "silence_prompt"
)

var (
	ErrNoSpeech      = errors.New("no speech after listen start")
	ErrSilencePrompt = errors.New("silence prompt — nudge the user")
	ErrNoUplink      = errors.New("no mic uplink after listen start")
	ErrSend          = errors.New("send audio")
	ErrServer        = errors.New("WSS died during listen")
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
	// EndOfSpeech > 0 ends the listen locally after that much silence
	// following speech (uplinks without server-side end-of-speech).
	EndOfSpeech time.Duration
	// MuteHead uplinks silence for this long after listen start so the wake
	// earcon cannot trip a server-side VAD (which would answer it).
	MuteHead time.Duration
	// MinRMS raises the "someone started talking" floor. 0 keeps the default.
	// Ambient (robot walking) sets this above wheel noise.
	MinRMS float64
	// WaitForSpeech holds the listen open with no silence timeout until the
	// gate hears a voice. Audio before that is not uploaded; a short pre-roll
	// is sent when speech starts.
	WaitForSpeech bool
	// SilencePrompt (WaitForSpeech only): if nobody clears the gate by then,
	// end with ErrSilencePrompt so the caller can nudge the user.
	SilencePrompt time.Duration
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
	// TalkThr is the end-of-speech talking threshold when the listen ended.
	TalkThr float64
	// StopErr is the ListenStop send error (logged by caller, not fatal).
	StopErr error
}

// Capture streams audio until the server returns STT, nobody speaks within
// NoSpeech, MaxListen elapses, or anim closes the mic. ListenStop is sent on
// every normal exit; ErrNoSpeech is returned when nobody spoke.
func Capture(ctx context.Context, up Uplink, audio <-chan []byte, opts Options) (Result, error) {
	res := Result{StartedAt: time.Now()}
	gate := NewSpeechGate()
	if opts.MinRMS > 0 {
		gate.SetMinRMS(opts.MinRMS)
	}
	encBuf := make([]byte, 0, opts.FrameBytes*4)
	muteBytes := int(opts.MuteHead.Seconds()*gateSampleRate) * 2
	sentBytes := 0

	// A zero duration leaves that timer off. Open mode uses that so the mic
	// stays up until the server returns a sentence, instead of the 10s/30s cuts.
	var noSpeechC <-chan time.Time
	var maxListenC <-chan time.Time
	var silencePromptC <-chan time.Time
	var maxTimer *time.Timer
	if !opts.WaitForSpeech && opts.NoSpeech > 0 {
		noSpeech := time.NewTimer(opts.NoSpeech)
		defer noSpeech.Stop()
		noSpeechC = noSpeech.C
	}
	if !opts.WaitForSpeech && opts.MaxListen > 0 {
		maxListen := time.NewTimer(opts.MaxListen)
		defer maxListen.Stop()
		maxListenC = maxListen.C
	}
	if opts.WaitForSpeech && opts.SilencePrompt > 0 {
		sil := time.NewTimer(opts.SilencePrompt)
		defer sil.Stop()
		silencePromptC = sil.C
	}
	defer func() {
		if maxTimer != nil {
			maxTimer.Stop()
		}
	}()
	micWait := time.After(opts.MicWait)
	gotMic := false
	// ~400ms of PCM so the start of the word is not cut off.
	const preMax = 16000 * 2 * 400 / 1000
	pre := make([]byte, 0, preMax)
	flushed := false
	muteLeft := int(opts.MuteHead.Seconds()*gateSampleRate) * 2

	finish := func() {
		res.Duration = time.Since(res.StartedAt)
		res.Speech = gate.Started()
		res.Floor = gate.Floor()
		res.Peak = gate.Peak()
		res.TalkThr = gate.TalkThreshold()
	}

loop:
	for {
		select {
		case chunk, ok := <-audio:
			if !ok {
				res.Reason = ReasonMicClosed
				break loop
			}
			gotMic = true
			feed := chunk
			if muteLeft > 0 {
				n := len(chunk)
				if n > muteLeft {
					n = muteLeft
				}
				silenced := append([]byte(nil), chunk...)
				for i := 0; i < n; i++ {
					silenced[i] = 0
				}
				feed = silenced
				muteLeft -= n
			}
			gate.Feed(feed)
			// Hold the uplink until a voice is actually there. Wheel noise stays local.
			if opts.WaitForSpeech && !gate.Started() {
				pre = append(pre, feed...)
				if len(pre) > preMax {
					pre = pre[len(pre)-preMax:]
				}
				continue
			}
			if opts.WaitForSpeech && maxTimer == nil && opts.MaxListen > 0 {
				maxTimer = time.NewTimer(opts.MaxListen)
				maxListenC = maxTimer.C
			}
			if opts.WaitForSpeech && !flushed {
				encBuf = append(encBuf, pre...)
				pre = nil
				flushed = true
			}
			endSpeech := opts.EndOfSpeech > 0 && gate.SilenceAfterSpeech() >= opts.EndOfSpeech
			if opts.Encode == nil || opts.FrameBytes <= 0 {
				if endSpeech {
					res.Reason = ReasonEndSpeech
					break loop
				}
				continue
			}
			encBuf = append(encBuf, feed...)
			for len(encBuf) >= opts.FrameBytes {
				frame := encBuf[:opts.FrameBytes]
				encBuf = encBuf[opts.FrameBytes:]
				if sentBytes < muteBytes {
					frame = make([]byte, opts.FrameBytes)
				}
				sentBytes += opts.FrameBytes
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
			if endSpeech {
				res.Reason = ReasonEndSpeech
				break loop
			}
		case stt := <-up.STTCh():
			res.STT = stt
			res.Reason = ReasonServerSTT
			break loop
		case <-noSpeechC:
			// No uplink yet is MicWait's job (caller relistens on ErrNoUplink).
			if res.Frames > 0 && !gate.Started() {
				res.Reason = ReasonNoSpeech
				break loop
			}
		case <-silencePromptC:
			if !gate.Started() {
				res.Reason = ReasonSilencePrompt
				break loop
			}
		case <-maxListenC:
			res.Reason = ReasonMaxListen
			break loop
		case <-micWait:
			if !gotMic && res.Frames == 0 {
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
	switch res.Reason {
	case ReasonNoSpeech:
		return res, ErrNoSpeech
	case ReasonSilencePrompt:
		return res, ErrSilencePrompt
	}
	return res, nil
}
