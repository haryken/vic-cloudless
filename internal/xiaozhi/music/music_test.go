package music

import (
	"bytes"
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"testing/iotest"
	"time"
)

func readFixture(t *testing.T) []byte {
	t.Helper()
	b, err := os.ReadFile("testdata/short.mp3")
	if err != nil {
		t.Fatal(err)
	}
	return b
}

func decodeAll(t *testing.T, r io.Reader) (pcm []byte, rate int) {
	t.Helper()
	d := newMP3Decoder(r)
	for {
		f, err := d.Next()
		if err == io.EOF {
			return pcm, d.Rate
		}
		if err != nil {
			t.Fatal(err)
		}
		pcm = append(pcm, f...)
	}
}

func TestDecoderFixture(t *testing.T) {
	mp3 := readFixture(t)
	pcm, rate := decodeAll(t, bytes.NewReader(mp3))
	if len(pcm) == 0 || rate == 0 {
		t.Fatalf("decoded %d bytes at %d Hz", len(pcm), rate)
	}
	// Tiny reads must not lose frames split across buffer refills.
	pcm2, _ := decodeAll(t, iotest.OneByteReader(bytes.NewReader(mp3)))
	if len(pcm2) != len(pcm) {
		t.Fatalf("one-byte reader decoded %d bytes, want %d", len(pcm2), len(pcm))
	}
}

func TestDecoderSkipsLeadingJunk(t *testing.T) {
	mp3 := readFixture(t)
	want, _ := decodeAll(t, bytes.NewReader(mp3))
	junk := append(bytes.Repeat([]byte{0}, 5000), mp3...)
	got, _ := decodeAll(t, bytes.NewReader(junk))
	if len(got) != len(want) {
		t.Fatalf("decoded %d bytes after junk, want %d", len(got), len(want))
	}
}

func TestDecoderRejectsNonMP3(t *testing.T) {
	d := newMP3Decoder(bytes.NewReader(bytes.Repeat([]byte("not an mp3 "), 40000)))
	if _, err := d.Next(); err == nil || err == io.EOF {
		t.Fatalf("want errNotMP3, got %v", err)
	}
}

func TestDownmixStereo(t *testing.T) {
	out := downmixS16([]int16{100, 300, -100, -300}, 2)
	if len(out) != 4 || int16(uint16(out[0])|uint16(out[1])<<8) != 200 || int16(uint16(out[2])|uint16(out[3])<<8) != -200 {
		t.Fatalf("downmix = %v", out)
	}
}

func TestSearchCandidates(t *testing.T) {
	got := searchCandidates("Lạc Trôi Sơn Tùng M-TP")
	want := []string{
		"Lạc Trôi Sơn Tùng M-TP",
		"Lac Troi Son Tung M-TP",
		"Lac Troi Son Tung",
		"nhac Lac Troi Son Tung",
		"Lac Troi Son",
		"Lac Troi",
		"nhac Lac Troi",
	}
	if strings.Join(got, "|") != strings.Join(want, "|") {
		t.Fatalf("candidates:\n got %q\nwant %q", got, want)
	}
}

func TestScoreTitlePrefersFullMatch(t *testing.T) {
	full := scoreTitle("lạc trôi", "Sơn Tùng M-TP | LẠC TRÔI | Official MV")
	partial := scoreTitle("lạc trôi", "Trôi theo dòng tập 5")
	if full < goodEnoughScore || partial >= full {
		t.Fatalf("full=%d partial=%d", full, partial)
	}
}

func TestNormalizeServerURL(t *testing.T) {
	cases := map[string]string{
		"":                           DefaultServerURL,
		"  music.local:5005/ ":       "https://music.local:5005",
		"http://192.168.1.2:8000//":  "http://192.168.1.2:8000",
		"https://youtube.kytuoi.com": "https://youtube.kytuoi.com",
	}
	for in, want := range cases {
		if got := NormalizeServerURL(in); got != want {
			t.Errorf("NormalizeServerURL(%q) = %q, want %q", in, got, want)
		}
	}
}

type fakeServer struct {
	mp3        []byte
	streamCode int
	queries    []string
}

func (f *fakeServer) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	switch r.URL.Path {
	case "/api/search":
		q := r.URL.Query().Get("q")
		f.queries = append(f.queries, q)
		if strings.Contains(q, "M-TP") {
			w.WriteHeader(500)
			_, _ = io.WriteString(w, `{"success":false,"error":"boom"}`)
			return
		}
		if strings.Contains(q, "khong co") {
			_, _ = io.WriteString(w, `{"success":true,"data":[]}`)
			return
		}
		_, _ = io.WriteString(w, `{"success":true,"data":[{"id":"x1","title":"Tập 5 trôi"},{"id":"abc","title":"LẠC TRÔI | Official MV"}]}`)
	case "/api/stream/mp3":
		if f.streamCode != 0 {
			w.WriteHeader(f.streamCode)
			_, _ = io.WriteString(w, `{"error":"No supported JavaScript runtime"}`)
			return
		}
		if r.URL.Query().Get("id") != "abc" {
			w.WriteHeader(404)
			return
		}
		w.Header().Set("Content-Type", "audio/mpeg")
		_, _ = w.Write(f.mp3)
	default:
		w.WriteHeader(404)
	}
}

func TestSearchFallsBackPastServerError(t *testing.T) {
	fs := &fakeServer{}
	srv := httptest.NewServer(fs)
	defer srv.Close()
	tr, err := Search(context.Background(), srv.URL, "Lạc Trôi M-TP")
	if err != nil {
		t.Fatal(err)
	}
	if tr.ID != "abc" {
		t.Fatalf("picked %+v", tr)
	}
	if len(fs.queries) < 2 {
		t.Fatalf("expected a fallback query, got %q", fs.queries)
	}
}

func TestPrepareReportsStreamError(t *testing.T) {
	srv := httptest.NewServer(&fakeServer{streamCode: 500})
	defer srv.Close()
	_, err := Prepare(srv.URL, "lac troi", 5*time.Second)
	if err == nil || !strings.Contains(err.Error(), "500") || !strings.Contains(err.Error(), "JavaScript runtime") {
		t.Fatalf("err = %v", err)
	}
}

func TestPrepareNotFound(t *testing.T) {
	srv := httptest.NewServer(&fakeServer{})
	defer srv.Close()
	if _, err := Prepare(srv.URL, "khong co", 5*time.Second); err == nil {
		t.Fatal("expected not found")
	}
}

// The fixture is 8kHz; the speaker-rate check is covered separately.
func allowAnyRate(t *testing.T) {
	t.Helper()
	old := supportedRate
	supportedRate = func(int) bool { return true }
	t.Cleanup(func() { supportedRate = old })
}

func TestPrepareRejectsUnplayableRate(t *testing.T) {
	srv := httptest.NewServer(&fakeServer{mp3: readFixture(t)})
	defer srv.Close()
	_, err := Prepare(srv.URL, "lac troi", 5*time.Second)
	if err == nil || !strings.Contains(err.Error(), "8000 Hz") {
		t.Fatalf("err = %v", err)
	}
}

func usePlayerCat(t *testing.T, out string) {
	t.Helper()
	allowAnyRate(t)
	old := PlayerCommand
	PlayerCommand = func(fifo string, rate int) *exec.Cmd {
		return exec.Command("sh", "-c", `cat "$0" > "$1"`, fifo, out)
	}
	t.Cleanup(func() { PlayerCommand = old })
}

func TestPrepareAndPlayWholeTrack(t *testing.T) {
	mp3 := readFixture(t)
	want, wantRate := decodeAll(t, bytes.NewReader(mp3))
	srv := httptest.NewServer(&fakeServer{mp3: mp3})
	defer srv.Close()

	dir := t.TempDir()
	out := filepath.Join(dir, "out.pcm")
	usePlayerCat(t, out)

	p, err := Prepare(srv.URL, "lạc trôi", 5*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	if p.Track.Title != "LẠC TRÔI | Official MV" || p.Rate != wantRate {
		t.Fatalf("prepared %+v rate %d", p.Track, p.Rate)
	}
	pb, err := p.Play(filepath.Join(dir, "music.fifo"))
	if err != nil {
		t.Fatal(err)
	}
	if err := pb.Wait(); err != nil {
		t.Fatalf("Wait = %v", err)
	}
	got, _ := os.ReadFile(out)
	if len(got) != len(want) {
		t.Fatalf("player got %d bytes, want %d", len(got), len(want))
	}
	if _, err := os.Stat(filepath.Join(dir, "music.fifo")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("fifo not removed: %v", err)
	}
}

func TestStopUnblocksStalledPlayer(t *testing.T) {
	mp3 := readFixture(t)
	// Endless stream: keep repeating the fixture until the client goes away.
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/api/search" {
			_, _ = io.WriteString(w, `{"success":true,"data":[{"id":"abc","title":"loop"}]}`)
			return
		}
		for r.Context().Err() == nil {
			if _, err := w.Write(mp3); err != nil {
				return
			}
		}
	}))
	defer srv.Close()

	allowAnyRate(t)
	old := PlayerCommand
	// A player that opens the pipe but never reads: the FIFO fills and Write blocks.
	PlayerCommand = func(fifo string, rate int) *exec.Cmd {
		return exec.Command("sh", "-c", `exec 3<"$0"; exec sleep 30`, fifo)
	}
	defer func() { PlayerCommand = old }()

	p, err := Prepare(srv.URL, "loop", 5*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	pb, err := p.Play(filepath.Join(t.TempDir(), "music.fifo"))
	if err != nil {
		t.Fatal(err)
	}
	time.Sleep(300 * time.Millisecond)
	pb.Stop()
	done := make(chan error, 1)
	go func() { done <- pb.Wait() }()
	select {
	case err := <-done:
		if !errors.Is(err, ErrStopped) {
			t.Fatalf("Wait = %v, want ErrStopped", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("Stop did not unblock playback")
	}
}
