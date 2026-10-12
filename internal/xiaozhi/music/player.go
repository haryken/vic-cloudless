// Package music streams a song from a YouTube-MP3 music server (same HTTP API as
// the Otto / ubt_speech_demo player) and plays it on Vector's speaker. It knows
// nothing about Xiaozhi; the xiaozhi package decides when to start and stop it.
package music

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"sync"
	"time"

	"golang.org/x/sys/unix"
)

// ErrStopped is returned by Wait when Stop ended playback early.
var ErrStopped = errors.New("music stopped")

const (
	// ~5s of 44.1kHz MP3 frames (1152 samples each) decoded ahead of the speaker.
	pcmQueueFrames    = 200
	streamReadBuf     = 128 * 1024
	playerOpenTimeout = 3 * time.Second
)

// PlayerCommand builds the process that reads raw mono s16le PCM from fifo.
// MultiMedia2 (hw:0,1) is the same path Xiaozhi TTS uses; it accepts 44.1/48kHz.
// Period must stay 1024: with -p 2048 the DSP consumes ~3-4x faster than real
// time (sped-up, distorted music).
var PlayerCommand = func(fifo string, rate int) *exec.Cmd {
	return exec.Command("tinyplay", fifo, "-D", "0", "-d", "1",
		"-i", "raw", "-c", "1", "-r", strconv.Itoa(rate), "-p", "1024")
}

// Prepared is a found track whose stream is open and already decoding.
type Prepared struct {
	Track Track
	Rate  int

	cancel context.CancelFunc
	pcm    chan []byte
	decErr chan error // decoder's final error (nil at clean EOF), buffered 1

	mu     sync.Mutex
	played bool
}

// Prepare searches for query, opens its MP3 stream and waits for the first
// decoded audio. It fails if that does not happen within timeout.
func Prepare(base, query string, timeout time.Duration) (*Prepared, error) {
	deadline := time.Now().Add(timeout)
	searchCtx, cancelSearch := context.WithDeadline(context.Background(), deadline)
	track, err := Search(searchCtx, base, query)
	cancelSearch()
	if err != nil {
		return nil, fmt.Errorf("search: %w", err)
	}

	// The body outlives Prepare, so it gets its own context; the timer only
	// guards the wait for the first frame.
	streamCtx, cancel := context.WithCancel(context.Background())
	guard := time.AfterFunc(time.Until(deadline), cancel)
	body, err := OpenStream(streamCtx, base, track.ID)
	if err != nil {
		guard.Stop()
		cancel()
		return nil, err
	}

	p := &Prepared{
		Track:  track,
		cancel: cancel,
		pcm:    make(chan []byte, pcmQueueFrames),
		decErr: make(chan error, 1),
	}
	rateCh := make(chan int, 1)
	go p.decode(body, rateCh)

	select {
	case rate := <-rateCh:
		guard.Stop()
		if !supportedRate(rate) {
			p.Discard()
			return nil, fmt.Errorf("unsupported sample rate %d Hz", rate)
		}
		p.Rate = rate
		return p, nil
	case err := <-p.decErr:
		guard.Stop()
		cancel()
		if err == nil {
			err = errors.New("stream ended before any audio")
		}
		return nil, fmt.Errorf("decode %s: %w", track.ID, err)
	}
}

// supportedRate: MultiMedia2 plays 44.1/48kHz; tinyplay hangs at 16kHz mono.
var supportedRate = func(rate int) bool {
	return rate == 44100 || rate == 48000
}

func (p *Prepared) decode(body io.ReadCloser, rateCh chan<- int) {
	defer body.Close()
	defer close(p.pcm)
	dec := newMP3Decoder(bufio.NewReaderSize(body, streamReadBuf))
	first := true
	for {
		frame, err := dec.Next()
		if err != nil {
			if err == io.EOF {
				err = nil
			}
			p.decErr <- err
			return
		}
		if first {
			first = false
			rateCh <- dec.Rate
		}
		p.pcm <- frame
	}
}

// Discard drops a prepared track that will never be played.
func (p *Prepared) Discard() {
	p.cancel()
	go func() {
		for range p.pcm {
		}
	}()
}

// Playback is a running song.
type Playback struct {
	cmd      *exec.Cmd
	w        *os.File
	cancel   context.CancelFunc
	stopOnce sync.Once
	stopped  chan struct{}
	done     chan struct{}
	err      error
}

// Play starts the speaker. fifo is a path in a writable tmpfs directory.
func (p *Prepared) Play(fifo string) (*Playback, error) {
	p.mu.Lock()
	if p.played {
		p.mu.Unlock()
		return nil, errors.New("already played")
	}
	p.played = true
	p.mu.Unlock()

	_ = os.MkdirAll(filepath.Dir(fifo), 0775)
	_ = os.Remove(fifo)
	if err := unix.Mkfifo(fifo, 0660); err != nil {
		p.Discard()
		return nil, fmt.Errorf("mkfifo: %w", err)
	}
	cmd := PlayerCommand(fifo, p.Rate)
	if err := cmd.Start(); err != nil {
		_ = os.Remove(fifo)
		p.Discard()
		return nil, fmt.Errorf("start player: %w", err)
	}
	w, err := openFIFOWriter(fifo, playerOpenTimeout)
	if err != nil {
		_ = cmd.Process.Kill()
		_ = cmd.Wait()
		_ = os.Remove(fifo)
		p.Discard()
		return nil, err
	}

	pb := &Playback{
		cmd:     cmd,
		w:       w,
		cancel:  p.cancel,
		stopped: make(chan struct{}),
		done:    make(chan struct{}),
	}
	go func() {
		defer close(pb.done)
		defer os.Remove(fifo)
		writeErr := pb.pump(w, p.pcm)
		_ = w.Close()
		go func() {
			for range p.pcm {
			}
		}()
		waitErr := cmd.Wait()
		select {
		case <-pb.stopped:
			pb.err = ErrStopped
			return
		default:
		}
		switch {
		case writeErr != nil:
			pb.err = writeErr
		case waitErr != nil:
			pb.err = fmt.Errorf("player exited: %w", waitErr)
		default:
			pb.err = <-p.decErr
		}
	}()
	return pb, nil
}

// openFIFOWriter waits for the player to open the read side. Only then is a
// later Close a clean EOF: data written before the reader opens is dropped
// when the last writer closes, and the player would wait forever.
func openFIFOWriter(fifo string, timeout time.Duration) (*os.File, error) {
	type result struct {
		f   *os.File
		err error
	}
	ch := make(chan result, 1)
	go func() {
		f, err := os.OpenFile(fifo, os.O_WRONLY, 0)
		ch <- result{f, err}
	}()
	select {
	case r := <-ch:
		if r.err != nil {
			return nil, fmt.Errorf("open fifo: %w", r.err)
		}
		return r.f, nil
	case <-time.After(timeout):
		// Open a reader ourselves so the blocked open returns, then drop both.
		if rd, err := os.OpenFile(fifo, os.O_RDONLY|unix.O_NONBLOCK, 0); err == nil {
			if r := <-ch; r.f != nil {
				_ = r.f.Close()
			}
			_ = rd.Close()
		}
		return nil, errors.New("player did not open the audio pipe")
	}
}

func (pb *Playback) pump(w *os.File, pcm <-chan []byte) error {
	for {
		select {
		case <-pb.stopped:
			return nil
		case frame, ok := <-pcm:
			if !ok {
				return nil
			}
			if _, err := w.Write(frame); err != nil {
				return fmt.Errorf("write player: %w", err)
			}
		}
	}
}

// Stop ends playback early; Wait then returns ErrStopped.
func (pb *Playback) Stop() {
	pb.stopOnce.Do(func() {
		close(pb.stopped)
		pb.cancel()
		if pb.cmd.Process != nil {
			_ = pb.cmd.Process.Kill()
		}
		_ = pb.w.Close()
	})
}

// Wait blocks until the song ends: nil after the whole track played,
// ErrStopped after Stop, or the stream/player error.
func (pb *Playback) Wait() error {
	<-pb.done
	return pb.err
}
