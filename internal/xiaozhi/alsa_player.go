package xiaozhi

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"golang.org/x/sys/unix"

	"github.com/digital-dream-labs/vector-cloud/internal/log"
)

// Plan B: play Xiaozhi TTS via ALSA MultiMedia2 from vic-cloud, bypassing
// ExternalAudio / Wwise in vic-anim. Goal: keep anim RSS flat during talk.
//
// Device: hw:0,1 (pcmC0D1p) — MultiMedia1 stays with Wwise.
// Format: 16 kHz mono s16le (matches Opus decode).
//
// Do NOT tinymix-mute MultiMedia1 during TTS: Wwise keeps writing MM1 and hits
// AkAlsaSink Broken pipe → all anim/SFX (fireworks, getout, etc.) go silent
// until anim restart.
//
// Default ON. Set XIAOZHI_ALSA=0 to restore ExternalAudio.
//
// Writes go through a queue so the TTS select loop is not blocked by realtime
// ALSA backpressure (that race let TTSStateStop close the FIFO while Opus was
// still queued → speech cut mid-sentence).

const (
	alsaFIFOPath = "/run/vic-cloud/xiaozhi_alsa.fifo"
	alsaCard     = "0"
	alsaDevice   = "1"
	alsaRate     = "16000"
	alsaChannels = "1"
	alsaPeriod   = "1024"
	// tinyplay rejects -n, so the driver ring stays 2 * period (~128ms at
	// 1024). Do not raise -p: 2048 plays too fast and 8192 crackles.
	alsaPeriodCount = "40"
	alsaMixerMM2    = "PRI_MI2S_RX Audio Mixer MultiMedia2"
	alsaQueueCap    = 512
	// Silence drip for long server stalls, but NOT into Xiaozhi inter-sentence
	// holes. Measured vấp (goodbye): pad@460ms fired at 463ms idle between
	// "Chào bạn!" and "Hẹn gặp lại nhé!" — next Opus still in flight.
	// Pad@900ms left 512→900ms underrun (giật). Sweet spot: just above those
	// ~450–470ms sentence holes, just under the 512ms ring; first pad write =
	// one full period (2048B) so silence hits ALSA immediately (drips alone
	// need ~64ms to coalesce in xiaozhi-play). ×10/×16 fail pcm_open on MM2.
	alsaKeepaliveEvery = 8 * time.Millisecond
	alsaKeepaliveBytes = 256 // 128 frames * 2 bytes = 8ms @ 16kHz mono
	// One period of s16le mono @16kHz — first pad write flushes xiaozhi-play immediately.
	alsaKeepalivePrime  = 2048
	alsaMinGapBeforePad = 500 * time.Millisecond
	alsaMaxGapPad       = 60 * time.Second
	// Smooth ≈ ESP audio_service: PCM queue → blocking FIFO write (fills the
	// kernel pipe + 512ms HW ring). Never pace-throttle writes — that starved
	// ALSA and crackled ("rè") the whole utterance. Prefill ~960ms once, then
	// write as fast as the pipe accepts. No gap-silence drip (that also rè).
	alsaSmoothPrefillBytes = 30720 // ~960ms = ESP MAX_PLAYBACK_TASKS_IN_QUEUE×60ms
	alsaSmoothMaxSoftBytes = 48000 // ~1.5s cap
	alsaSmoothPeriodBytes  = 2048  // one ALSA period (matches xiaozhi-play)
	alsaSmoothPrefillWait  = 3 * time.Second

	TTSPlaybackClassic = "classic"
	TTSPlaybackSmooth  = "smooth"
)

var (
	alsaMu           sync.Mutex
	alsaCmd          *exec.Cmd
	alsaWriter       *os.File
	alsaPCMCh        chan []byte
	alsaDone         chan struct{}
	alsaBytes        int64
	alsaRealBytes    int64 // speech only — excludes gap-keepalive silence
	alsaPendingBytes int64 // PCM enqueued but not yet written to FIFO
	alsaReady        atomic.Bool
	mixerReady       atomic.Bool
	alsaKeepalive    atomic.Bool
	alsaSmoothMode   atomic.Bool
)

// SetTTSPlaybackMode selects classic (gap keepalive) or smooth (prefill).
// Empty / unknown → classic. Safe to call from LoadConfig on every turn.
func SetTTSPlaybackMode(mode string) {
	m := strings.ToLower(strings.TrimSpace(mode))
	on := m == TTSPlaybackSmooth || m == "new" || m == "esp"
	alsaSmoothMode.Store(on)
}

// TTSPlaybackMode returns the active mode string for UI/status.
func TTSPlaybackMode() string {
	if alsaSmoothMode.Load() {
		return TTSPlaybackSmooth
	}
	return TTSPlaybackClassic
}

// UseAlsaPlayback reports whether Xiaozhi TTS should go to ALSA (Plan B).
func UseAlsaPlayback() bool {
	v := os.Getenv("XIAOZHI_ALSA")
	if v == "" {
		return true
	}
	return v != "0" && v != "false" && v != "off" && v != "no"
}

func tinymixSet(control, value string) {
	cmd := exec.Command("tinymix", "set", control, value)
	if out, err := cmd.CombinedOutput(); err != nil {
		log.Printf("[Xiaozhi][ALSA] tinymix set %q=%s failed: %v — %s", control, value, err, string(out))
	}
}

func tinymixGet(control string) string {
	out, err := exec.Command("tinymix", "get", control).CombinedOutput()
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(out))
}

// EnsureCodecGainsSane repairs debug leftovers that make Wwise/intent SFX
// (fireworks, dance, …) ignore Âm lượng hệ thống. Seen on-device: RX3 Digital
// Volume stuck at 330 while tinymix range is 0–124 — system "Thấp" still screamed.
// Does not change in-range gains or master_volume; only clamps illegal values
// back to the stock baseline (84).
func EnsureCodecGainsSane() {
	// Never disable MultiMedia1 for mute — Wwise AkAlsaSink then fails with
	// Device busy and system SFX stay dead after raising volume. Only repair
	// illegal RX gains; keep MM1 on.
	_ = exec.Command("sudo", "-n", "/anki/bin/wireos-speaker-gain", "84").Run()
	const stock = "84"
	for _, ctl := range []string{"RX1 Digital Volume", "RX2 Digital Volume", "RX3 Digital Volume"} {
		raw := tinymixGet(ctl)
		if raw == "" {
			continue
		}
		// tinymix get → "84 (range 0->124)"
		field := strings.Fields(raw)
		if len(field) == 0 {
			continue
		}
		n, err := strconv.Atoi(field[0])
		if err != nil {
			continue
		}
		if n > 124 || n < 0 {
			log.Printf("[Xiaozhi][ALSA] codec %s out of range (%q) — reset to %s (Wwise/system SFX)", ctl, raw, stock)
			tinymixSet(ctl, stock)
		}
	}
}

func ensureMM2Mixer() {
	if mixerReady.Load() {
		return
	}
	// Only enable MultiMedia2 for AI TTS. Never touch MultiMedia1, RX1/RX2,
	// Speaker Boost, or master_volume — those are the Wwise/system path so
	// UI Mute must keep muting footsteps/power beeps while Xiaozhi still talks
	// via MM2 + tts_gain_pct on PCM.
	tinymixSet(alsaMixerMM2, "1")
	mixerReady.Store(true)
	log.Println("[Xiaozhi][ALSA] MultiMedia2 mixer enabled")
}

func alsaKillLocked() {
	alsaKeepalive.Store(false)
	ch := alsaPCMCh
	alsaPCMCh = nil
	if ch != nil {
		func() {
			defer func() { _ = recover() }()
			close(ch)
		}()
	}
	if alsaCmd != nil && alsaCmd.Process != nil {
		_ = alsaCmd.Process.Kill()
	}
	// The FIFO is opened O_RDWR, so with tinyplay gone a full pipe blocks the
	// pump's Write forever (done never closes). Closing unblocks it.
	if alsaWriter != nil {
		_ = alsaWriter.Close()
	}
	// Pump owns Wait(); just wait for it to finish.
	done := alsaDone
	if done != nil {
		alsaMu.Unlock()
		select {
		case <-done:
		case <-time.After(2 * time.Second):
		}
		alsaMu.Lock()
	}
	alsaCmd = nil
	alsaWriter = nil
	alsaDone = nil
	alsaBytes = 0
	alsaRealBytes = 0
	alsaPendingBytes = 0
	alsaReady.Store(false)
	_ = os.Remove(alsaFIFOPath)
}

// AlsaCancel stops ALSA playback immediately (barge-in / cleanup).
func AlsaCancel() {
	if !UseAlsaPlayback() {
		return
	}
	alsaMu.Lock()
	defer alsaMu.Unlock()
	alsaKillLocked()
}

func alsaStart() error {
	alsaMu.Lock()
	defer alsaMu.Unlock()
	alsaKillLocked()
	ensureMM2Mixer()

	if err := os.MkdirAll(filepath.Dir(alsaFIFOPath), 0775); err != nil {
		return fmt.Errorf("alsa mkdir: %w", err)
	}
	_ = os.Remove(alsaFIFOPath)
	if err := unix.Mkfifo(alsaFIFOPath, 0666); err != nil {
		return fmt.Errorf("alsa mkfifo: %w", err)
	}

	w, err := os.OpenFile(alsaFIFOPath, os.O_RDWR, 0)
	if err != nil {
		_ = os.Remove(alsaFIFOPath)
		return fmt.Errorf("alsa open fifo: %w", err)
	}

	// xiaozhi-play asks libtinyalsa for 8 periods (512ms) at period 1024.
	// tinyplay cannot; its ring is 128ms and sentence gaps underrun.
	player := "/anki/bin/xiaozhi-play"
	if _, err := os.Stat(player); err != nil {
		player = "tinyplay"
	}
	cmd := exec.Command(player, alsaFIFOPath,
		"-D", alsaCard, "-d", alsaDevice,
		"-i", "raw", "-c", alsaChannels, "-r", alsaRate,
		"-p", alsaPeriod, "-n", alsaPeriodCount)
	cmd.Stdout = nil
	cmd.Stderr = nil
	if err := cmd.Start(); err != nil {
		_ = w.Close()
		_ = os.Remove(alsaFIFOPath)
		return fmt.Errorf("tinyplay start: %w", err)
	}

	pcmCh := make(chan []byte, alsaQueueCap)
	done := make(chan struct{})
	smooth := alsaSmoothMode.Load()
	alsaCmd = cmd
	alsaWriter = w
	alsaPCMCh = pcmCh
	alsaDone = done
	alsaBytes = 0
	alsaRealBytes = 0
	alsaPendingBytes = 0
	alsaReady.Store(true)
	// Classic + smooth both may pad on long idle; thresholds differ in the pump.
	alsaKeepalive.Store(true)

	go alsaPump(w, pcmCh, done, cmd, smooth)

	if smooth {
		log.Printf("[Xiaozhi][ALSA] player started (%s, mode=smooth, prefill≈%dms queue≤%dms)",
			player,
			alsaSmoothPrefillBytes*1000/(16000*2),
			alsaSmoothMaxSoftBytes*1000/(16000*2))
	} else {
		log.Printf("[Xiaozhi][ALSA] player started (%s, mode=classic, silence after %v)",
			player, alsaMinGapBeforePad)
	}
	return nil
}

func alsaPump(w *os.File, pcmCh <-chan []byte, done chan struct{}, cmd *exec.Cmd, smooth bool) {
	defer func() {
		alsaKeepalive.Store(false)
		_ = w.Close()
		_, _ = cmd.Process.Wait()
		alsaMu.Lock()
		if alsaCmd == cmd {
			alsaCmd = nil
			alsaWriter = nil
			alsaPCMCh = nil
			alsaDone = nil
		}
		alsaMu.Unlock()
		alsaReady.Store(false)
		_ = os.Remove(alsaFIFOPath)
		close(done)
		log.Println("[Xiaozhi][ALSA] tinyplay finished")
		_ = os.Remove(BusyPath)
		_ = os.Remove("/run/xiaozhi-busy")
	}()

	silence := make([]byte, alsaKeepaliveBytes)
	primeSilence := make([]byte, alsaKeepalivePrime)
	ticker := time.NewTicker(alsaKeepaliveEvery)
	defer ticker.Stop()

	var lastRealPCM time.Time
	var gapLogged bool
	var gapPrimed bool

	writeFIFO := func(pcm []byte, realSpeech bool) error {
		if len(pcm) == 0 {
			return nil
		}
		n, err := w.Write(pcm)
		if n > 0 {
			alsaMu.Lock()
			alsaBytes += int64(n)
			if realSpeech {
				alsaRealBytes += int64(n)
			}
			if alsaPendingBytes >= int64(n) {
				alsaPendingBytes -= int64(n)
			} else {
				alsaPendingBytes = 0
			}
			alsaMu.Unlock()
		}
		return err
	}

	if smooth {
		// ESP-like: one goroutine fills soft[], another drains to FIFO as fast
		// as the pipe/HW accepts (blocking Write). Prefill once, then no silence
		// drip — pacing + early PLC were what made the whole sentence crackle.
		var softMu sync.Mutex
		soft := make([]byte, 0, alsaSmoothMaxSoftBytes)
		softClosed := false
		softCV := sync.NewCond(&softMu)
		prefillDeadline := time.Now().Add(alsaSmoothPrefillWait)
		writerDone := make(chan struct{})

		go func() {
			defer close(writerDone)
			started := false
			for {
				softMu.Lock()
				for {
					if softClosed && len(soft) == 0 {
						softMu.Unlock()
						return
					}
					if !started {
						ready := len(soft) >= alsaSmoothPrefillBytes ||
							(len(soft) > 0 && time.Now().After(prefillDeadline)) ||
							(softClosed && len(soft) > 0)
						if !ready {
							softCV.Wait()
							continue
						}
						started = true
						log.Printf("[Xiaozhi][ALSA] smooth prefill start %dB (%.0fms)",
							len(soft), float64(len(soft))*1000/(16000*2))
					}
					if len(soft) >= alsaSmoothPeriodBytes || (softClosed && len(soft) > 0) {
						break
					}
					softCV.Wait()
				}
				n := alsaSmoothPeriodBytes
				if len(soft) < n {
					n = len(soft)
				}
				// Always write full periods — short pcm_write crackles on MM2.
				chunk := make([]byte, alsaSmoothPeriodBytes)
				copy(chunk, soft[:n])
				soft = soft[n:]
				softMu.Unlock()
				if err := writeFIFO(chunk, true); err != nil {
					log.Println("[Xiaozhi][ALSA] smooth write:", err)
					softMu.Lock()
					softClosed = true
					soft = soft[:0]
					softCV.Broadcast()
					softMu.Unlock()
					return
				}
			}
		}()

		appendSoft := func(pcm []byte) {
			softMu.Lock()
			defer softMu.Unlock()
			if softClosed {
				return
			}
			soft = append(soft, pcm...)
			if len(soft) > alsaSmoothMaxSoftBytes {
				overflow := len(soft) - alsaSmoothMaxSoftBytes
				overflow -= overflow % alsaSmoothPeriodBytes
				if overflow > 0 {
					soft = soft[overflow:]
				}
			}
			softCV.Signal()
		}

		for {
			select {
			case pcm, ok := <-pcmCh:
				if !ok {
					softMu.Lock()
					softClosed = true
					softCV.Broadcast()
					softMu.Unlock()
					<-writerDone
					tail := make([]byte, 1024*2*5)
					_, _ = w.Write(tail)
					return
				}
				if len(pcm) > 0 {
					appendSoft(pcm)
				}
			case <-ticker.C:
				// Smooth: no gap silence. Pipe + prefill absorb holes.
			}
		}
	}

	// Classic path: write-through + gap keepalive.
	for {
		select {
		case pcm, ok := <-pcmCh:
			if !ok {
				tail := make([]byte, 1024*2*5) // ~320ms @ 16kHz s16le mono
				if _, err := w.Write(tail); err != nil {
					_ = err
				}
				return
			}
			if len(pcm) == 0 {
				continue
			}
			lastRealPCM = time.Now()
			gapLogged = false
			gapPrimed = false
			if err := writeFIFO(pcm, true); err != nil {
				log.Println("[Xiaozhi][ALSA] write:", err)
				return
			}
		case <-ticker.C:
			if len(pcmCh) > 0 || !alsaKeepalive.Load() {
				continue
			}
			if lastRealPCM.IsZero() {
				continue
			}
			idle := time.Since(lastRealPCM)
			if idle < alsaMinGapBeforePad || idle > alsaMaxGapPad {
				continue
			}
			if !gapLogged {
				log.Printf("[Xiaozhi][ALSA] gap silence after %v idle — holding ring (pad≥%v, mode=classic)",
					idle.Round(time.Millisecond), alsaMinGapBeforePad)
				gapLogged = true
			}
			chunk := silence
			if !gapPrimed {
				chunk = primeSilence
				gapPrimed = true
			}
			if err := writeFIFO(chunk, false); err != nil {
				log.Println("[Xiaozhi][ALSA] keepalive write:", err)
				return
			}
		}
	}
}

func alsaWrite(pcm []byte) (written int64, err error) {
	if len(pcm) == 0 {
		return alsaBytesWritten(), nil
	}
	cp := make([]byte, len(pcm))
	copy(cp, pcm)

	alsaMu.Lock()
	ch := alsaPCMCh
	alsaMu.Unlock()
	if ch == nil {
		return -1, fmt.Errorf("alsa not started")
	}
	defer func() {
		if recover() != nil {
			err = fmt.Errorf("alsa closed")
		}
	}()
	// Count bytes when actually written in the pump (avoids “fed ahead of ears”
	// drift for WaitPlaybackIdle). Here only enqueue.
	select {
	case ch <- cp:
		alsaMu.Lock()
		alsaPendingBytes += int64(len(cp))
		alsaMu.Unlock()
	case <-time.After(8 * time.Second):
		return -1, fmt.Errorf("alsa queue blocked")
	}
	return alsaBytesWritten() + int64(len(cp)), nil
}

func alsaBytesWritten() int64 {
	alsaMu.Lock()
	defer alsaMu.Unlock()
	return alsaBytes
}

// alsaRealBytesWritten is speech PCM only (excludes gap-keepalive silence).
func alsaRealBytesWritten() int64 {
	alsaMu.Lock()
	defer alsaMu.Unlock()
	return alsaRealBytes
}

// alsaEnd closes the PCM queue so the pump drains to tinyplay then exits.
// Waits for the pump (tinyplay drain) so callers do not open the mic or kill
// mid-tail — early return used to leave hardware still playing → giật rè kill.
func alsaEnd() error {
	alsaKeepalive.Store(false)
	alsaMu.Lock()
	ch := alsaPCMCh
	done := alsaDone
	alsaMu.Unlock()
	if ch != nil {
		func() {
			defer func() { _ = recover() }()
			close(ch)
		}()
	}
	alsaMu.Lock()
	if alsaPCMCh == ch {
		alsaPCMCh = nil
	}
	alsaMu.Unlock()
	if done == nil {
		alsaReady.Store(false)
		return nil
	}
	// Drain remaining queue + HW ring (~2.5s) + tinyplay exit.
	// A fixed 3s cap was killing Google VI / longer comments mid-sentence
	// (log: "drain wait timed out — killing tinyplay" after ~3s).
	// Scale wait with how much PCM still pending in the pump queue.
	alsaMu.Lock()
	pending := alsaPendingBytes
	alsaMu.Unlock()

	// 16 kHz mono s16le → 32000 bytes/sec. Add HW buffer (~2.5s) + slop.
	est := time.Duration(pending)*time.Second/32000 + 5*time.Second
	if est < 10*time.Second {
		est = 10 * time.Second
	}
	if est > 60*time.Second {
		est = 60 * time.Second
	}
	select {
	case <-done:
	case <-time.After(est):
		log.Printf("[Xiaozhi][ALSA] drain wait timed out after %v (pending≈%dB) — killing tinyplay",
			est.Round(time.Millisecond), pending)
		AlsaCancel()
	}
	return nil
}

// AlsaActive is true while tinyplay / pump is still running.
func AlsaActive() bool {
	return alsaReady.Load()
}
