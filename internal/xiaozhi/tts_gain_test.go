package xiaozhi

import (
	"encoding/binary"
	"math"
	"testing"
)

func sinePCM(samples int, amp float64) []byte {
	return tonePCM(16000, samples, 220, amp)
}

func bandPeak(t *testing.T, hz, amp, gain float64) float64 {
	t.Helper()
	out, _ := bandGain(s16Samples(tonePCM(16000, 4000, hz, amp)), gain, nil)
	peak := 0.0
	for _, v := range out[1000:] {
		peak = math.Max(peak, math.Abs(v))
	}
	return peak / amp
}

func TestBandGainBoostsVoiceOnly(t *testing.T) {
	if g := bandPeak(t, 1500, 1000, 10); g < 9.8 || g > 10.2 {
		t.Fatalf("1.5 kHz boost = %.2f, want ~10", g)
	}
	if g := bandPeak(t, 150, 1000, 10); g < 0.97 || g > 1.03 {
		t.Fatalf("150 Hz bass = %.2f, want source level", g)
	}
	if g := bandPeak(t, 6000, 1000, 10); g > ttsDeEssMaxGain*1.03 {
		t.Fatalf("6 kHz boost = %.2f, want <= %.1f", g, ttsDeEssMaxGain)
	}
	if g := bandPeak(t, 6000, 1000, 1.3); g < 1.27 || g > 1.33 {
		t.Fatalf("small boost must stay flat: 6 kHz = %.2f", g)
	}
}

func TestBandGainChunkingMatchesOneShot(t *testing.T) {
	in := s16Samples(tonePCM(16000, 5000, 1234, 8000))
	whole, _ := bandGain(in, 5, nil)
	var hist, joined []float64
	for off := 0; off < len(in); off += 321 {
		var part []float64
		part, hist = bandGain(in[off:min(off+321, len(in))], 5, hist)
		joined = append(joined, part...)
	}
	for i := range whole {
		if math.Abs(whole[i]-joined[i]) > 1e-6 {
			t.Fatalf("sample %d: %v vs %v", i, joined[i], whole[i])
		}
	}
}

func pcmSample(b []byte, i int) float64 {
	return float64(int16(binary.LittleEndian.Uint16(b[i*2:])))
}

func TestLimitGainQuietSpeechIsExactlyLinear(t *testing.T) {
	in := sinePCM(1600, 3000)
	out, _ := limitGain(in, 3, 1)
	for i := 0; i < 1600; i++ {
		if got, want := pcmSample(out, i), math.Round(pcmSample(in, i)*3); got != want {
			t.Fatalf("sample %d = %v, want %v (no limiting needed)", i, got, want)
		}
	}
}

func TestLimitGainLoudSpeechKeepsShapeUnderCeiling(t *testing.T) {
	in := sinePCM(16000, 20000)
	reduce := 1.0
	var out []byte
	for off := 0; off < len(in); off += 1920 {
		var chunk []byte
		chunk, reduce = limitGain(in[off:min(off+1920, len(in))], 3, reduce)
		out = append(out, chunk...)
	}
	var rmsIn, rmsOut float64
	for i := 0; i < 16000; i++ {
		x, y := pcmSample(in, i), pcmSample(out, i)
		if math.Abs(y) > ttsLimitCeiling+1 {
			t.Fatalf("sample %d = %v above ceiling", i, y)
		}
		rmsIn += x * x
		rmsOut += y * y
	}
	if rmsOut < rmsIn*1.5 {
		t.Fatalf("not louder: rms ratio %.2f", math.Sqrt(rmsOut/rmsIn))
	}
	// Steady tone: one constant gain, so the waveform is a scaled copy.
	ratio := 0.0
	for i := 8000; i < 16000; i++ {
		x, y := pcmSample(in, i), pcmSample(out, i)
		if math.Abs(x) < 5000 {
			continue
		}
		if ratio == 0 {
			ratio = y / x
		} else if math.Abs(y/x-ratio) > 0.02*ratio {
			t.Fatalf("sample %d gain %.3f vs %.3f — waveform distorted", i, y/x, ratio)
		}
	}
}

func TestLimitGainMaxBoostStaysUnderCeiling(t *testing.T) {
	in := sinePCM(8000, 30000)
	out, _ := limitGain(in, float64(ttsGainMaxPct)/100, 1)
	for i := 0; i < 8000; i++ {
		if y := pcmSample(out, i); math.Abs(y) > ttsLimitCeiling+1 {
			t.Fatalf("sample %d = %v above ceiling at max gain", i, y)
		}
	}
}

func TestApplyTTSGainBoostsSpeech(t *testing.T) {
	defer setTTSGain(0)
	setTTSGain(200)
	out := applyTTSGain(tonePCM(16000, 3200, 1000, 4000))
	peak := 0.0
	for i := 1600; i < 3200; i++ {
		peak = math.Max(peak, math.Abs(pcmSample(out, i)))
	}
	if peak < 7900 || peak > 8100 {
		t.Fatalf("200%% gain on a 4000 peak at 1 kHz gave %v", peak)
	}
}

func TestApplyTTSGainFullPercentIsSource(t *testing.T) {
	defer setTTSGain(0)
	setTTSGain(100)
	pcm := tonePCM(16000, 320, 150, 30000)
	if out := applyTTSGain(pcm); &out[0] != &pcm[0] {
		t.Fatal("100% must play the source untouched")
	}
}

func TestClampTTSGain(t *testing.T) {
	cases := map[int]int{0: 100, -5: 100, 50: 100, 100: 100, 160: 160, 300: 300, 400: 400, 1000: 1000, 1500: 1000}
	for in, want := range cases {
		if got := clampTTSGain(in); got != want {
			t.Fatalf("clampTTSGain(%d) = %d, want %d", in, got, want)
		}
	}
}

func TestApplyTTSGainDefaultIsPassthrough(t *testing.T) {
	setTTSGain(0)
	pcm := []byte{0x10, 0x01, 0xf0, 0xfe}
	if out := applyTTSGain(pcm); &out[0] != &pcm[0] {
		t.Fatal("unset gain must not touch the PCM")
	}
}
