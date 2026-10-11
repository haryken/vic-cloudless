package listenvad

import (
	"encoding/binary"
	"math"
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

	if g.started || g.samples <= gateIgnoreHeadSamples {
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
	}
}
