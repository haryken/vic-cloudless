package listenvad

import (
	"context"
	"encoding/binary"
	"errors"
	"math"
	"math/rand"
	"sync"
	"testing"
	"time"
)

const chunkSamples = 160 // anim sends 10ms blocks

func pcm(samples int, amp float64, rng *rand.Rand) []byte {
	b := make([]byte, samples*2)
	for i := 0; i < samples; i++ {
		v := amp * math.Sin(2*math.Pi*220*float64(i)/16000)
		v += (rng.Float64()*2 - 1) * 60 // background hiss
		binary.LittleEndian.PutUint16(b[i*2:], uint16(int16(v)))
	}
	return b
}

func feedMs(g *SpeechGate, ms int, amp float64, rng *rand.Rand) {
	for i := 0; i < ms/10; i++ {
		g.Feed(pcm(chunkSamples, amp, rng))
	}
}

func TestSpeechGateSilenceNeverStarts(t *testing.T) {
	rng := rand.New(rand.NewSource(1))
	g := NewSpeechGate()
	feedMs(g, 10000, 0, rng)
	if g.Started() {
		t.Fatalf("hiss detected as speech (floor %.0f peak %.0f)", g.Floor(), g.Peak())
	}
}

func TestSpeechGateDetectsVoice(t *testing.T) {
	rng := rand.New(rand.NewSource(2))
	g := NewSpeechGate()
	feedMs(g, 1000, 0, rng)
	feedMs(g, 400, 3000, rng)
	if !g.Started() {
		t.Fatalf("voice not detected (floor %.0f peak %.0f)", g.Floor(), g.Peak())
	}
}

func TestSpeechGateVoiceRightAwayAfterHead(t *testing.T) {
	rng := rand.New(rand.NewSource(3))
	g := NewSpeechGate()
	feedMs(g, 300, 0, rng)
	// Quick reply with short pauses between words.
	for i := 0; i < 4; i++ {
		feedMs(g, 250, 3000, rng)
		feedMs(g, 80, 0, rng)
	}
	if !g.Started() {
		t.Fatalf("immediate speech not detected (floor %.0f peak %.0f)", g.Floor(), g.Peak())
	}
}

func TestSpeechGateIgnoresEarconHead(t *testing.T) {
	rng := rand.New(rand.NewSource(4))
	g := NewSpeechGate()
	feedMs(g, 250, 5000, rng)
	feedMs(g, 5000, 0, rng)
	if g.Started() {
		t.Fatal("earcon in first 300ms counted as speech")
	}
}

type fakeUplink struct {
	mu      sync.Mutex
	sent    int
	stopped int
	stt     chan string
	errs    chan error
}

func newFakeUplink() *fakeUplink {
	return &fakeUplink{stt: make(chan string, 8), errs: make(chan error, 1)}
}

func (f *fakeUplink) SendAudio([]byte) error {
	f.mu.Lock()
	f.sent++
	f.mu.Unlock()
	return nil
}

func (f *fakeUplink) ListenStop() error {
	f.mu.Lock()
	f.stopped++
	f.mu.Unlock()
	return nil
}

func (f *fakeUplink) STTCh() <-chan string { return f.stt }
func (f *fakeUplink) ErrCh() <-chan error  { return f.errs }

func (f *fakeUplink) stops() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.stopped
}

func testOpts() Options {
	return Options{
		NoSpeech:   time.Second,
		MaxListen:  3 * time.Second,
		MicWait:    time.Second,
		FrameBytes: 960 * 2,
		Encode:     func(p []byte) ([]byte, error) { return p[:2], nil },
	}
}

// micFeed streams 10ms chunks as fast as Capture reads them until stop.
func micFeed(audio chan<- []byte, stop <-chan struct{}, amp func(elapsed int) float64) {
	rng := rand.New(rand.NewSource(9))
	for ms := 0; ; ms += 10 {
		select {
		case audio <- pcm(chunkSamples, amp(ms), rng):
		case <-stop:
			return
		}
	}
}

// talking: quiet head, then 250ms words separated by 80ms gaps.
func talking(ms int) float64 {
	if ms < 300 || (ms-300)%330 >= 250 {
		return 0
	}
	return 3000
}

func TestCaptureEndsOnServerSTT(t *testing.T) {
	up := newFakeUplink()
	audio := make(chan []byte, 10)
	stop := make(chan struct{})
	defer close(stop)
	go micFeed(audio, stop, talking)
	go func() {
		time.Sleep(600 * time.Millisecond)
		up.stt <- "hôm nay trời thế nào"
	}()

	res, err := Capture(context.Background(), up, audio, testOpts())
	if err != nil {
		t.Fatal(err)
	}
	if res.Reason != ReasonServerSTT || res.STT != "hôm nay trời thế nào" {
		t.Fatalf("reason %q stt %q", res.Reason, res.STT)
	}
	if !res.Speech || res.Frames == 0 || up.stops() != 1 {
		t.Fatalf("speech %v frames %d stops %d", res.Speech, res.Frames, up.stops())
	}
}

type recordUplink struct {
	*fakeUplink
	packets [][]byte
}

func (r *recordUplink) SendAudio(p []byte) error {
	r.packets = append(r.packets, append([]byte(nil), p...))
	return nil
}

func TestCaptureMuteHeadSendsSilenceThenAudio(t *testing.T) {
	up := &recordUplink{fakeUplink: newFakeUplink()}
	audio := make(chan []byte, 10)
	stop := make(chan struct{})
	defer close(stop)
	go micFeed(audio, stop, func(int) float64 { return 5000 })
	go func() {
		time.Sleep(300 * time.Millisecond)
		up.stt <- "x"
	}()

	opts := testOpts()
	opts.Encode = func(p []byte) ([]byte, error) { return append([]byte(nil), p...), nil }
	opts.MuteHead = 120 * time.Millisecond // 960-sample frames: first two muted
	if _, err := Capture(context.Background(), up, audio, opts); err != nil {
		t.Fatal(err)
	}
	if len(up.packets) < 4 {
		t.Fatalf("only %d packets", len(up.packets))
	}
	loud := func(p []byte) bool {
		for _, b := range p {
			if b != 0 {
				return true
			}
		}
		return false
	}
	for i, p := range up.packets {
		if want := i >= 2; loud(p) != want {
			t.Fatalf("packet %d loud=%v, want %v", i, loud(p), want)
		}
	}
}

func TestCaptureLongSpeechNotCutAtNoSpeechTimeout(t *testing.T) {
	up := newFakeUplink()
	audio := make(chan []byte, 10)
	stop := make(chan struct{})
	defer close(stop)
	go micFeed(audio, stop, talking)

	opts := testOpts()
	opts.MaxListen = 1500 * time.Millisecond
	res, err := Capture(context.Background(), up, audio, opts)
	if err != nil {
		t.Fatalf("%v (frames %d floor %.0f peak %.0f)", err, res.Frames, res.Floor, res.Peak)
	}
	if res.Reason != ReasonMaxListen {
		t.Fatalf("reason %q, want max_listen", res.Reason)
	}
	if res.Duration < opts.MaxListen {
		t.Fatalf("closed after %v, before MaxListen", res.Duration)
	}
}

func TestCaptureNoSpeechClosesEarly(t *testing.T) {
	up := newFakeUplink()
	audio := make(chan []byte, 10)
	stop := make(chan struct{})
	defer close(stop)
	go micFeed(audio, stop, func(int) float64 { return 0 })

	opts := testOpts()
	opts.NoSpeech = 300 * time.Millisecond
	res, err := Capture(context.Background(), up, audio, opts)
	if !errors.Is(err, ErrNoSpeech) {
		t.Fatalf("err %v, want ErrNoSpeech", err)
	}
	if res.Reason != ReasonNoSpeech || up.stops() != 1 {
		t.Fatalf("reason %q stops %d", res.Reason, up.stops())
	}
}

func TestCaptureMicClosedByAnim(t *testing.T) {
	up := newFakeUplink()
	audio := make(chan []byte, 10)
	rng := rand.New(rand.NewSource(5))
	go func() {
		for i := 0; i < 50; i++ {
			audio <- pcm(chunkSamples, 3000, rng)
		}
		close(audio)
	}()

	res, err := Capture(context.Background(), up, audio, testOpts())
	if err != nil {
		t.Fatal(err)
	}
	if res.Reason != ReasonMicClosed || up.stops() != 1 {
		t.Fatalf("reason %q stops %d", res.Reason, up.stops())
	}
}

func TestCaptureNoUplink(t *testing.T) {
	up := newFakeUplink()
	audio := make(chan []byte)
	opts := testOpts()
	opts.MicWait = 200 * time.Millisecond
	opts.NoSpeech = 100 * time.Millisecond

	_, err := Capture(context.Background(), up, audio, opts)
	if !errors.Is(err, ErrNoUplink) {
		t.Fatalf("err %v, want ErrNoUplink", err)
	}
	if up.stops() != 0 {
		t.Fatal("ListenStop sent on uplink failure")
	}
}

func TestSpeechGateSilenceAfterSpeech(t *testing.T) {
	rng := rand.New(rand.NewSource(6))
	g := NewSpeechGate()
	feedMs(g, 300, 0, rng)
	for i := 0; i < 4; i++ {
		feedMs(g, 250, 3000, rng)
		feedMs(g, 80, 0, rng)
	}
	if !g.Started() {
		t.Fatal("speech not detected")
	}
	if s := g.SilenceAfterSpeech(); s > 200*time.Millisecond {
		t.Fatalf("word gap reads as %v silence", s)
	}
	feedMs(g, 900, 0, rng)
	if s := g.SilenceAfterSpeech(); s < 850*time.Millisecond {
		t.Fatalf("silence %v after 900ms quiet", s)
	}
	feedMs(g, 100, 3000, rng)
	if s := g.SilenceAfterSpeech(); s != 0 {
		t.Fatalf("silence %v after talking resumed", s)
	}
}

func TestCaptureEndOfSpeech(t *testing.T) {
	up := newFakeUplink()
	audio := make(chan []byte, 10)
	stop := make(chan struct{})
	defer close(stop)
	go micFeed(audio, stop, func(ms int) float64 {
		if ms < 1300 {
			return talking(ms)
		}
		return 0
	})

	opts := testOpts()
	opts.MaxListen = 5 * time.Second
	opts.EndOfSpeech = 600 * time.Millisecond
	res, err := Capture(context.Background(), up, audio, opts)
	if err != nil {
		t.Fatal(err)
	}
	if res.Reason != ReasonEndSpeech || !res.Speech || up.stops() != 1 {
		t.Fatalf("reason %q speech %v stops %d", res.Reason, res.Speech, up.stops())
	}
}

func TestCaptureZeroTimersStayOpenUntilSTT(t *testing.T) {
	up := newFakeUplink()
	audio := make(chan []byte, 10)
	stop := make(chan struct{})
	defer close(stop)
	go micFeed(audio, stop, func(int) float64 { return 0 })
	go func() {
		time.Sleep(250 * time.Millisecond)
		up.stt <- "nói nhỏ"
	}()

	opts := testOpts()
	opts.NoSpeech = 0
	opts.MaxListen = 0
	res, err := Capture(context.Background(), up, audio, opts)
	if err != nil {
		t.Fatal(err)
	}
	if res.Reason != ReasonServerSTT || res.STT != "nói nhỏ" || res.Frames == 0 {
		t.Fatalf("reason %q stt %q frames %d", res.Reason, res.STT, res.Frames)
	}
}

func TestCaptureCancelled(t *testing.T) {
	up := newFakeUplink()
	audio := make(chan []byte)
	ctx, cancel := context.WithCancel(context.Background())
	go func() {
		time.Sleep(50 * time.Millisecond)
		cancel()
	}()
	_, err := Capture(ctx, up, audio, testOpts())
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("err %v, want context.Canceled", err)
	}
}

func TestSpeechGateEndsOverSteadyRoomNoise(t *testing.T) {
	rng := rand.New(rand.NewSource(7))
	g := NewSpeechGate()
	feedMs(g, 300, 0, rng)
	feedMs(g, 1500, 6000, rng)
	if !g.Started() {
		t.Fatal("speech not detected")
	}
	// Fan / TV well above the quiet floor but far below the voice.
	feedMs(g, 900, 500, rng)
	if s := g.SilenceAfterSpeech(); s < 850*time.Millisecond {
		t.Fatalf("room noise held the mic: silence %v thr %.0f", s, g.TalkThreshold())
	}
}

func TestSpeechGateIgnoresLoneClicks(t *testing.T) {
	rng := rand.New(rand.NewSource(8))
	g := NewSpeechGate()
	feedMs(g, 300, 0, rng)
	feedMs(g, 1000, 3000, rng)
	for i := 0; i < 5; i++ {
		feedMs(g, 180, 0, rng)
		feedMs(g, 20, 3000, rng) // one loud 20ms frame
	}
	if s := g.SilenceAfterSpeech(); s < 900*time.Millisecond {
		t.Fatalf("lone clicks restarted the silence count: %v", s)
	}
}

func TestSpeechGateWordGapsOverNoiseKeepTalking(t *testing.T) {
	rng := rand.New(rand.NewSource(9))
	g := NewSpeechGate()
	feedMs(g, 300, 0, rng)
	for i := 0; i < 6; i++ {
		feedMs(g, 300, 5000, rng)
		feedMs(g, 250, 400, rng)
	}
	if s := g.SilenceAfterSpeech(); s > 300*time.Millisecond {
		t.Fatalf("word gaps read as %v silence", s)
	}
}
