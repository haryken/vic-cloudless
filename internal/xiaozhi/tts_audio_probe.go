package xiaozhi

import (
	"encoding/binary"
	"math"
	"sync/atomic"

	"github.com/digital-dream-labs/vector-cloud/internal/log"
)

// Per-turn TTS path probe: locate harsh/rè audio (server peak, downsample,
// gain/limiter, or ALSA gap keepalive).
type ttsAudioProbe struct {
	frames       int
	peakDecode   float64 // after Opus→PCM limit (24k)
	peakDown     float64 // after 24k→16k
	peakGain     float64 // after applyTTSGain
	overFSFrames int
	maxOverDB    float64
	minReduce    float64
	gainPct      int32
}

var (
	alsaGapCount  atomic.Uint32
	curTTSProbe   atomic.Pointer[ttsAudioProbe]
)

func bindTTSProbe(p *ttsAudioProbe) {
	if p != nil {
		p.reset()
	}
	curTTSProbe.Store(p)
}

func noteAlsaGap() {
	alsaGapCount.Add(1)
}

func resetAlsaGapCount() {
	alsaGapCount.Store(0)
}

func pcmPeakAbs(pcm []byte) float64 {
	peak := 0.0
	for i := 0; i+1 < len(pcm); i += 2 {
		s := math.Abs(float64(int16(binary.LittleEndian.Uint16(pcm[i:]))))
		if s > peak {
			peak = s
		}
	}
	return peak
}

func peakDBFS(peak float64) float64 {
	if peak <= 1 {
		return -99
	}
	return 20 * math.Log10(peak/32767)
}

func (p *ttsAudioProbe) reset() {
	*p = ttsAudioProbe{minReduce: 1, gainPct: ttsGainPct.Load()}
	resetAlsaGapCount()
	resetTTSLimiter()
}

func (p *ttsAudioProbe) noteDecode(peak float64, overFS bool, reduce float64) {
	p.frames++
	if peak > p.peakDecode {
		p.peakDecode = peak
	}
	if overFS {
		p.overFSFrames++
		db := peakDBFS(peak)
		if db > p.maxOverDB {
			p.maxOverDB = db
		}
	}
	if reduce > 0 && reduce < p.minReduce {
		p.minReduce = reduce
	}
}

func (p *ttsAudioProbe) noteDownsample(pcm []byte) {
	if pk := pcmPeakAbs(pcm); pk > p.peakDown {
		p.peakDown = pk
	}
}

func (p *ttsAudioProbe) noteGain(pcm []byte) {
	if pk := pcmPeakAbs(pcm); pk > p.peakGain {
		p.peakGain = pk
	}
}

func (p *ttsAudioProbe) logSummary(tag string) {
	if p.frames == 0 {
		return
	}
	gaps := alsaGapCount.Load()
	log.Printf("[Xiaozhi][TTS][Audio] %s frames=%d gain=%d%% "+
		"peak_decode=%.1fdBFS peak_down=%.1fdBFS peak_out=%.1fdBFS "+
		"overFS_frames=%d max_over=%.2fdB min_limit_gain=%.3f alsa_gaps=%d",
		tag, p.frames, p.gainPct,
		peakDBFS(p.peakDecode), peakDBFS(p.peakDown), peakDBFS(p.peakGain),
		p.overFSFrames, p.maxOverDB, p.minReduce, gaps)
	switch {
	case p.overFSFrames > p.frames/5:
		log.Println("[Xiaozhi][TTS][Audio] hint: many server frames over full scale — rè often from cloud mastering / Opus overshoot")
	case p.gainPct > 150 && p.minReduce < 0.7:
		log.Println("[Xiaozhi][TTS][Audio] hint: high tts_gain_pct + heavy limiting — try lower AI voice boost")
	case gaps > 3:
		log.Println("[Xiaozhi][TTS][Audio] hint: many ALSA gap pads — choppy/rè can be server Opus arriving in bursts")
	case p.peakDown > 0 && p.peakDecode > 0 && peakDBFS(p.peakDown) > peakDBFS(p.peakDecode)+0.5:
		log.Println("[Xiaozhi][TTS][Audio] hint: downsample raised peaks — linear 24→16 may alias (harsh)")
	}
}

func resetTTSLimiter() {
	ttsLimiter.mu.Lock()
	ttsLimiter.reduce = 1
	ttsLimiter.hist = nil
	ttsLimiter.mu.Unlock()
}
