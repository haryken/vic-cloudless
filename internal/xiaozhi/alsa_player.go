package xiaozhi

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
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
	// Silence drip only once the 512ms ring is about to run dry. Sentence
	// gaps are often ~400ms and still fit in that ring; dripping at 400ms
	// inserted a hitch (the "vấp") before the next chunk arrived.
	alsaKeepaliveEvery = 8 * time.Millisecond
	alsaKeepaliveBytes = 256 // 128 frames * 2 bytes = 8ms @ 16kHz mono
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
)

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

func ensureMM2Mixer() {
	if mixerReady.Load() {
		return
	}
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
	alsaCmd = cmd
	alsaWriter = w
	alsaPCMCh = pcmCh
	alsaDone = done
	alsaBytes = 0
	alsaRealBytes = 0
	alsaPendingBytes = 0
	alsaReady.Store(true)
	// Pad only once the 512ms ring is nearly empty. A ~400ms server pause
	// still has audio queued; dripping there was the audible hitch.
	alsaKeepalive.Store(true)

	go alsaPump(w, pcmCh, done, cmd)

	log.Printf("[Xiaozhi][ALSA] player started (%s, period 1024 x8, silence after 460ms)", player)
	return nil
}

func alsaPump(w *os.File, pcmCh <-chan []byte, done chan struct{}, cmd *exec.Cmd) {
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
	ticker := time.NewTicker(alsaKeepaliveEvery)
	defer ticker.Stop()

	var lastRealPCM time.Time
	var gapLogged bool
	// Start just before the 512ms ring runs dry. Shorter idle is still
	// covered by audio already queued, so it must not be overwritten
	// with silence.
	const (
		minGapBeforePad = 460 * time.Millisecond
		maxGapPad       = 60 * time.Second
	)

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

	for {
		select {
		case pcm, ok := <-pcmCh:
			if !ok {
				// Brief ring fill only (~300ms) so tinyplay doesn't underrun on
				// EOF. Longer pads (2.5s) made users wait in silence before mic
				// reopened after TTS finished.
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
			if err := writeFIFO(pcm, true); err != nil {
				log.Println("[Xiaozhi][ALSA] write:", err)
				return
			}
		case <-ticker.C:
			if !alsaKeepalive.Load() || len(pcmCh) > 0 {
				continue
			}
			if lastRealPCM.IsZero() {
				continue
			}
			idle := time.Since(lastRealPCM)
			if idle < minGapBeforePad || idle > maxGapPad {
				continue
			}
			if !gapLogged {
				log.Printf("[Xiaozhi][ALSA] gap silence after %v idle — holding the 512ms ring",
					idle.Round(time.Millisecond))
				gapLogged = true
			}
			// realSpeech=false: drips must not keep the mic closed after the last word.
			if err := writeFIFO(silence, false); err != nil {
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
