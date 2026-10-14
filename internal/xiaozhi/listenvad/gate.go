package listenvad

import (
	"encoding/binary"
	"math"
	"time"
)

const (
	gateSampleRate   = 16000
	gateFrameSamples = gateSampleRate / 50 // 20ms
	// The wake earcon and trigger tail land in the first few hundred ms.
	gateIgnoreHeadSamples = gateSampleRate * 3 / 10
	// RMS floor below which a frame is never voice (s16 scale).
	gateMinRMS = 200.0
	// Voice must be this many times the tracked noise floor (~12 dB).
	gateFloorRatio = 4.0
	// Floor drifts up ~10%/s so rising fan/motor noise is not voice forever.
	gateFloorRise = 1.002
	// ~160ms of net voiced frames.
	gateHotFrames = 8
	// After speech started, a frame above this (vs floor) still counts as
	// talking for end-of-speech; lower than the start ratio so soft word
	// endings do not read as silence.
	gateTalkFloorRatio = 2.5
	gateTalkMinRMS     = 150.0
	// Frames this far below the loudest voice (~-18 dB) read as silence, so
	// steady room noise (fan, TV, motors) above the floor cannot hold the mic.
	gateTalkPeakRatio = 0.12
	// Talking resumes only after ~60ms of loud frames; lone clicks and motor
	// ticks do not restart the end-of-speech count.
	gateTalkRunFrames = 3
)

// SpeechGate answers "has anyone started talking" for 16kHz s16le mono PCM.
// The noise floor follows the quietest recent frame, so speech during
// calibration (quick reply after TTS) does not raise the threshold.
type SpeechGate struct {
	pending []byte
	samples int
	floor   float64
	peak    float64
	hot     int
	started bool
	// quiet counts non-talking samples since speech last resumed.
	quiet int
	// voice is the loudest frame since the first voiced frame.
	voice   float64
	talkRun int
}

func NewSpeechGate() *SpeechGate {
	return &SpeechGate{}
}

func (g *SpeechGate) Feed(pcm []byte) {
	g.pending = append(g.pending, pcm...)
	n := gateFrameSamples * 2
	for len(g.pending) >= n {
		g.frame(g.pending[:n])
		g.pending = g.pending[n:]
	}
}

func (g *SpeechGate) Started() bool  { return g.started }
func (g *SpeechGate) Floor() float64 { return g.floor }
func (g *SpeechGate) Peak() float64  { return g.peak }

// TalkThreshold is the RMS a frame must exceed to count as talking once
// speech has started.
func (g *SpeechGate) TalkThreshold() float64 {
	return math.Max(math.Max(g.floor*gateTalkFloorRatio, gateTalkMinRMS), g.voice*gateTalkPeakRatio)
}

// SilenceAfterSpeech is how long the input has been quiet since speech
// started (0 before speech or while talking).
func (g *SpeechGate) SilenceAfterSpeech() time.Duration {
	if !g.started {
		return 0
	}
	return time.Duration(g.quiet) * time.Second / gateSampleRate
}

func (g *SpeechGate) frame(b []byte) {
	var sum float64
	count := len(b) / 2
	for i := 0; i+1 < len(b); i += 2 {
		s := float64(int16(binary.LittleEndian.Uint16(b[i:])))
		sum += s * s
	}
	rms := math.Sqrt(sum / float64(count))
	g.samples += count

	if g.floor == 0 || rms < g.floor {
		g.floor = rms
	} else {
		g.floor *= gateFloorRise
	}

	if g.started {
		if rms > g.voice {
			g.voice = rms
		}
		if rms > g.TalkThreshold() {
			g.talkRun++
			if g.talkRun >= gateTalkRunFrames {
				g.quiet = 0
				return
			}
		} else {
			g.talkRun = 0
		}
		g.quiet += count
		return
	}
	if g.samples <= gateIgnoreHeadSamples {
		return
	}
	if rms > g.peak {
		g.peak = rms
	}
	if rms > math.Max(g.floor*gateFloorRatio, gateMinRMS) {
		g.hot++
	} else if g.hot > 0 {
		g.hot--
	}
	if g.hot >= gateHotFrames {
		g.started = true
		g.voice = g.peak
	}
}
