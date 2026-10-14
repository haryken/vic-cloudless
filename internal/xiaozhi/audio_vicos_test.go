//go:build vicos

package xiaozhi

import (
	"math"
	"testing"
)

func TestDecodeFrameLimitsFullScaleSpeech(t *testing.T) {
	enc, err := NewEncoder()
	if err != nil {
		t.Fatal(err)
	}
	dec, err := NewDecoder()
	if err != nil {
		t.Fatal(err)
	}
	in := tonePCM(EncSampleRate, EncFrameSamples*20, 300, 32767)
	peak, samples := 0.0, 0
	for off := 0; off+EncFrameSamples*2 <= len(in); off += EncFrameSamples * 2 {
		frame, err := enc.EncodeFrame(in[off : off+EncFrameSamples*2])
		if err != nil {
			t.Fatal(err)
		}
		pcm, err := dec.DecodeFrame(frame)
		if err != nil {
			t.Fatal(err)
		}
		for i := 0; i < len(pcm)/2; i++ {
			peak = math.Max(peak, math.Abs(pcmSample(pcm, i)))
		}
		samples += len(pcm) / 2
	}
	if samples == 0 {
		t.Fatal("no audio decoded")
	}
	if peak > ttsLimitCeiling+1 {
		t.Fatalf("decoded peak %v above limiter ceiling", peak)
	}
	if peak < ttsLimitCeiling*0.7 {
		t.Fatalf("decoded peak %v — loud speech came out too quiet", peak)
	}
}
