package xiaozhi

import (
	"encoding/binary"
	"math"
	"sync"
	"sync/atomic"
)

const (
	ttsGainMinPct = 100
	ttsGainMaxPct = 1000
)

// ttsGainPct is the user's AI voice boost (tts_gain_pct), refreshed on every
// LoadConfig so a change applies from the next turn.
var ttsGainPct atomic.Int32

func setTTSGain(pct int) {
	ttsGainPct.Store(int32(clampTTSGain(pct)))
}

func clampTTSGain(pct int) int {
	switch {
	case pct <= 0:
		return ttsGainMinPct
	case pct < ttsGainMinPct:
		return ttsGainMinPct
	case pct > ttsGainMaxPct:
		return ttsGainMaxPct
	}
	return pct
}

const (
	// ~-1 dBFS: boosted peaks are held under this instead of clipping.
	ttsLimitCeiling = 0.89 * 32767
	// 2ms blocks at 16kHz; each block's gain also covers the next block's
	// peak, so the ramp between two block gains never overshoots.
	ttsLimitBlock = 32
	// Gain recovers with a ~250ms time constant after a loud syllable. A
	// faster release lifts the decay between words to full boost, which
	// sounds like a glassy room echo.
	ttsLimitReleaseMs = 250.0
)

var ttsLimitRelease = 1 - math.Exp(-float64(ttsLimitBlock)/16.0/ttsLimitReleaseMs)

const (
	// Sibilants (s/x/ch, ~4–8 kHz) sit above this split. They have low peaks,
	// so the limiter would let them take the full boost while vowels are held
	// back, turning word endings into a harsh hiss on the small speaker.
	ttsDeEssHz   = 3500
	ttsDeEssTaps = 63
	// The band above the split is boosted at most this much (de-esser).
	ttsDeEssMaxGain = 1.6
)

var ttsDeEssLow = lowpassTaps(ttsDeEssTaps, ttsDeEssHz/16000.0)

const (
	// Vector's tiny speaker barely reproduces the voice below this, yet that
	// band holds a large share of the peaks the limiter must keep under full
	// scale. It stays at source level; only the band the speaker plays well
	// is boosted, so the same ceiling gives louder speech.
	ttsBassHz   = 450
	ttsBassTaps = 127
)

var ttsBassLow = lowpassTaps(ttsBassTaps, ttsBassHz/16000.0)

// ttsLimiter keeps the gain reduction and filter history between
// StreamAppend chunks.
var ttsLimiter struct {
	mu     sync.Mutex
	reduce float64
	hist   []float64
}

// applyTTSGain leaves 100% untouched (the source as-is). Above that it boosts
// the speaker's voice band linearly and limits only the peaks that would
// clip, so loud boosts stay clean (no waveshaping distortion).
func applyTTSGain(pcm []byte) []byte {
	pct := ttsGainPct.Load()
	if pct <= ttsGainMinPct || len(pcm) < 2 {
		return pcm
	}
	ttsLimiter.mu.Lock()
	defer ttsLimiter.mu.Unlock()
	if ttsLimiter.reduce <= 0 {
		ttsLimiter.reduce = 1
	}
	var in []float64
	in, ttsLimiter.hist = bandGain(s16Samples(pcm), float64(pct)/100, ttsLimiter.hist)
	out, reduce := limitSamples(in, ttsLimiter.reduce)
	ttsLimiter.reduce = reduce
	return out
}

func s16Samples(pcm []byte) []float64 {
	x := make([]float64, len(pcm)/2)
	for i := range x {
		x[i] = float64(int16(binary.LittleEndian.Uint16(pcm[i*2:])))
	}
	return x
}

// bandGain keeps the bass (below ttsBassHz) at source level, applies gain up
// to ttsDeEssHz and at most ttsDeEssMaxGain above it (de-esser). Bands are
// differences of linear-phase low-passes sharing one centre, so they sum back
// without phase smear — an IIR split rotates harmonic phases, which raises the
// peaks and costs loudness under the limiter. Output lags input by ~4ms;
// hist carries the last ttsBassTaps-1 input samples.
func bandGain(x []float64, gain float64, hist []float64) ([]float64, []float64) {
	bass, mid := ttsBassLow, ttsDeEssLow
	delay := len(bass) / 2
	skew := delay - len(mid)/2
	highGain := math.Min(gain, ttsDeEssMaxGain)
	if len(hist) != len(bass)-1 {
		hist = make([]float64, len(bass)-1)
	}
	win := make([]float64, 0, len(hist)+len(x))
	win = append(win, hist...)
	win = append(win, x...)
	out := make([]float64, len(x))
	for i := range out {
		lo := symFIR(bass, win[i:])
		lm := symFIR(mid, win[i+skew:])
		out[i] = lo + gain*(lm-lo) + highGain*(win[i+delay]-lm)
	}
	hist = append(hist[:0], win[len(win)-len(bass)+1:]...)
	return out, hist
}

// symFIR applies odd-length symmetric taps to w[0:len(taps)], folding the
// mirrored pairs to halve the multiplies.
func symFIR(taps, w []float64) float64 {
	n := len(taps) - 1
	y := taps[n/2] * w[n/2]
	for k := 0; k < n/2; k++ {
		y += taps[k] * (w[k] + w[n-k])
	}
	return y
}

// cleanGain is limitGain for one whole clip.
func cleanGain(pcm []byte, gain float64) []byte {
	if gain <= 1 || len(pcm) < 2 {
		return pcm
	}
	out, _ := limitGain(pcm, gain, 1)
	return out
}

// limitGain multiplies s16le PCM by gain with a look-ahead peak limiter.
// reduce is the limiter state (1 = no reduction) carried across calls.
func limitGain(pcm []byte, gain, reduce float64) ([]byte, float64) {
	in := make([]float64, len(pcm)/2)
	for i := range in {
		in[i] = float64(int16(binary.LittleEndian.Uint16(pcm[i*2:]))) * gain
	}
	return limitSamples(in, reduce)
}

// limitSamples holds already-boosted samples under ttsLimitCeiling and
// converts them to s16le.
func limitSamples(in []float64, reduce float64) ([]byte, float64) {
	n := len(in)
	blockPeak := func(b int) float64 {
		peak := 0.0
		for i := b * ttsLimitBlock; i < n && i < (b+1)*ttsLimitBlock; i++ {
			peak = math.Max(peak, math.Abs(in[i]))
		}
		return peak
	}
	need := func(peak float64) float64 {
		if peak <= ttsLimitCeiling {
			return 1
		}
		return ttsLimitCeiling / peak
	}

	out := make([]byte, n*2)
	blocks := (n + ttsLimitBlock - 1) / ttsLimitBlock
	cur := blockPeak(0)
	start := math.Min(reduce, need(cur))
	for b := 0; b < blocks; b++ {
		next := 0.0
		if b+1 < blocks {
			next = blockPeak(b + 1)
		}
		target := need(math.Max(cur, next))
		end := target
		if target > start {
			end = start + (target-start)*ttsLimitRelease
		}
		lo, hi := b*ttsLimitBlock, min((b+1)*ttsLimitBlock, n)
		for i := lo; i < hi; i++ {
			g := start + (end-start)*float64(i-lo+1)/float64(hi-lo)
			v := math.Round(in[i] * g)
			v = math.Max(math.MinInt16, math.Min(math.MaxInt16, v))
			binary.LittleEndian.PutUint16(out[i*2:], uint16(int16(v)))
		}
		start, cur = end, next
	}
	return out, start
}
