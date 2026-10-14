package xiaozhi

import (
	"encoding/binary"
	"math"
	"testing"
)

func tonePCM(rate, samples int, freq, amp float64) []byte {
	b := make([]byte, samples*2)
	for i := 0; i < samples; i++ {
		v := amp * math.Sin(2*math.Pi*freq*float64(i)/float64(rate))
		binary.LittleEndian.PutUint16(b[i*2:], uint16(int16(math.Round(v))))
	}
	return b
}

// rmsS16 skips the edges where the filter ramps in.
func rmsS16(b []byte) float64 {
	n := len(b) / 2
	sum, cnt := 0.0, 0
	for i := n / 10; i < n-n/10; i++ {
		v := float64(int16(binary.LittleEndian.Uint16(b[i*2:])))
		sum += v * v
		cnt++
	}
	return math.Sqrt(sum / float64(cnt))
}

func TestDown24to16PassesSpeechBand(t *testing.T) {
	in := tonePCM(24000, 24000, 1000, 10000)
	var d down24to16
	out := d.Process(in)
	if len(out) < 15900*2 {
		t.Fatalf("got %d samples, want ~16000", len(out)/2)
	}
	if r := rmsS16(out) / rmsS16(in); r < 0.97 || r > 1.03 {
		t.Fatalf("1 kHz level ratio %.3f, want ~1", r)
	}
}

func TestDown24to16RemovesAliasingHiss(t *testing.T) {
	in := tonePCM(24000, 24000, 10000, 10000)
	var d down24to16
	out := d.Process(in)
	if r := rmsS16(out) / rmsS16(in); r > 0.01 {
		t.Fatalf("10 kHz leaked at ratio %.4f — would fold back to 6 kHz as hiss", r)
	}
}

func TestDown24to16ChunkingMatchesOneShot(t *testing.T) {
	in := tonePCM(24000, 4800, 440, 8000)
	var whole down24to16
	want := whole.Process(in)

	var parts down24to16
	var got []byte
	prev := 0
	// Byte offsets, including odd ones that split a sample.
	for _, cut := range []int{1, 5, 6, 777, 2001, 4801, len(in)} {
		got = append(got, parts.Process(in[prev:cut])...)
		prev = cut
	}
	if string(got) != string(want) {
		t.Fatalf("chunked output differs (%d vs %d bytes)", len(got), len(want))
	}
}

func TestResampleS16DownsampleRemovesAliasing(t *testing.T) {
	speech := resampleS16(tonePCM(24000, 24000, 1000, 10000), 24000, 16000)
	if r := rmsS16(speech) / (10000 / math.Sqrt2); r < 0.95 || r > 1.05 {
		t.Fatalf("1 kHz level ratio %.3f, want ~1", r)
	}
	hiss := resampleS16(tonePCM(24000, 24000, 10000, 10000), 24000, 16000)
	if r := rmsS16(hiss) / (10000 / math.Sqrt2); r > 0.01 {
		t.Fatalf("10 kHz leaked at ratio %.4f", r)
	}
}
