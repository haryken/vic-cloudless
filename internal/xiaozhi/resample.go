package xiaozhi

import (
	"encoding/binary"
	"math"
)

// lowpassTaps is a Blackman-windowed sinc low-pass (odd length, unity DC
// gain); fc is the cutoff in cycles per input sample.
func lowpassTaps(n int, fc float64) []float64 {
	if n%2 == 0 {
		n++
	}
	taps := make([]float64, n)
	mid := float64(n-1) / 2
	sum := 0.0
	for i := range taps {
		x := float64(i) - mid
		h := 2 * fc
		if x != 0 {
			h = math.Sin(2*math.Pi*fc*x) / (math.Pi * x)
		}
		w := 0.42 - 0.5*math.Cos(2*math.Pi*float64(i)/float64(n-1)) + 0.08*math.Cos(4*math.Pi*float64(i)/float64(n-1))
		taps[i] = h * w
		sum += taps[i]
	}
	for i := range taps {
		taps[i] /= sum
	}
	return taps
}

// antiAliasTaps removes what cannot exist at toRate before downsampling:
// ~3 kHz transition centred on the new Nyquist (16 kHz out: flat to ~6.5 kHz,
// gone by ~9.5 kHz). Without it, sibilants above 8 kHz fold back as a harsh
// hiss that the TTS boost makes loud.
func antiAliasTaps(fromRate, toRate int) []float64 {
	n := int(math.Ceil(29.3 * float64(fromRate) / float64(toRate)))
	return lowpassTaps(min(n, 255), 0.5*float64(toRate)/float64(fromRate))
}

var taps24to16 = antiAliasTaps(24000, 16000)

func clampS16(v float64) int16 {
	return int16(math.Max(math.MinInt16, math.Min(math.MaxInt16, math.Round(v))))
}

// down24to16 converts streamed 24 kHz s16le mono to 16 kHz (3 in → 2 out),
// keeping odd bytes, filter history and a partial group between chunks.
type down24to16 struct {
	odd  []byte
	hist []float64
	grp  []float64
}

func (d *down24to16) Process(in []byte) []byte {
	buf := append(d.odd, in...)
	n := len(buf) / 2
	d.odd = append([]byte(nil), buf[n*2:]...)
	taps := taps24to16
	if d.hist == nil {
		d.hist = make([]float64, len(taps)-1)
	}
	win := make([]float64, 0, len(d.hist)+n)
	win = append(win, d.hist...)
	for i := 0; i < n; i++ {
		win = append(win, float64(int16(binary.LittleEndian.Uint16(buf[i*2:]))))
	}
	for i := 0; i < n; i++ {
		y := 0.0
		for k, c := range taps {
			y += c * win[i+k]
		}
		d.grp = append(d.grp, y)
	}
	d.hist = append(d.hist[:0], win[len(win)-len(taps)+1:]...)

	groups := len(d.grp) / 3
	out := make([]byte, groups*4)
	for g := 0; g < groups; g++ {
		s0, s1, s2 := d.grp[g*3], d.grp[g*3+1], d.grp[g*3+2]
		binary.LittleEndian.PutUint16(out[g*4:], uint16(clampS16(s0)))
		binary.LittleEndian.PutUint16(out[g*4+2:], uint16(clampS16((s1+s2)/2)))
	}
	d.grp = append(d.grp[:0], d.grp[groups*3:]...)
	return out
}

// lowpassS16 filters a whole s16le mono clip (zero-padded edges).
func lowpassS16(in []byte, taps []float64) []byte {
	n := len(in) / 2
	half := len(taps) / 2
	x := make([]float64, n)
	for i := range x {
		x[i] = float64(int16(binary.LittleEndian.Uint16(in[i*2:])))
	}
	out := make([]byte, n*2)
	for i := 0; i < n; i++ {
		y := 0.0
		for k, c := range taps {
			if j := i + k - half; j >= 0 && j < n {
				y += c * x[j]
			}
		}
		binary.LittleEndian.PutUint16(out[i*2:], uint16(clampS16(y)))
	}
	return out
}
