package xiaozhi

import (
	"errors"
	"fmt"
	"strings"
	"sync"
	"time"

	"github.com/digital-dream-labs/vector-cloud/internal/log"
	"github.com/digital-dream-labs/vector-cloud/internal/xiaozhi/music"
)

// Device ("YouTube") music, ubt_speech_demo style:
//  1. self.music.play searches and opens the stream while the server waits,
//     then returns text the LLM speaks ("found X" / "couldn't find X").
//  2. After that TTS the WSS is closed and the song plays (music-only mode).
//  3. Song over → reopen WSS, send listen/detect "phát hết nhạc"; the reply
//     TTS is followed by the normal relisten. Hey Vector stops the song with
//     no detect.

const (
	MusicSourceServer  = "server"
	MusicSourceYouTube = "youtube"

	musicPlayTool = "self.music.play"

	// MusicDetectFinished / MusicDetectFailed are sent as listen/detect text.
	MusicDetectFinished = "phát hết nhạc"
	MusicDetectFailed   = "nhạc thất bại"

	musicPrepareTimeout = 10 * time.Second
	// A found song waits this long for its confirmation TTS to finish.
	musicPendingTTL  = 60 * time.Second
	musicFIFOPath    = "/run/vic-cloud/xiaozhi_music.fifo"
	musicDetectTries = 4
)

// MusicToolsEnabled is true when the device music tool overrides server music.
func MusicToolsEnabled(cfg Config) bool {
	return strings.EqualFold(strings.TrimSpace(cfg.MusicSource), MusicSourceYouTube)
}

func musicToolDefs() []map[string]interface{} {
	return []map[string]interface{}{
		toolDef(musicPlayTool,
			"Play a song on Vector's speaker. ALWAYS use this tool for every request to play music: mở nhạc, phát nhạc, bật bài, nghe bài hát, hát bài, play song/music — do NOT use any other music tool. "+
				"Pass the song name (and artist if said) as `query`. The call waits while the song is searched (a few seconds) and returns text. "+
				"On success: tell the user in ONE short sentence which song was found; the robot starts playing it by itself right after you finish speaking and the conversation pauses until the song ends. "+
				"On error: tell the user the song could not be found and ask if they want to try another song. Never claim a song is playing unless this tool succeeded.",
			map[string]interface{}{
				"query": map[string]interface{}{
					"type":        "string",
					"description": "Song name and/or artist, e.g. Lạc Trôi Sơn Tùng",
				},
			},
			[]string{"query"}),
	}
}

type musicState struct {
	mu           sync.Mutex
	pending      *music.Prepared
	pendingTimer *time.Timer
	playing      *music.Playback
	// hold is set for the whole song, including the moment before playing is
	// assigned, so face greet treats music-only mode as a conversation.
	hold         bool
	title        string
	toolInFlight bool
	toolDoneAt   time.Time
}

var mus musicState

// MusicToolHoldsTurn is true while self.music.play runs or finished less than
// grace ago, so the TTS wait does not time out on the server's tool pause.
func MusicToolHoldsTurn(grace time.Duration) bool {
	mus.mu.Lock()
	defer mus.mu.Unlock()
	return mus.toolInFlight || (!mus.toolDoneAt.IsZero() && time.Since(mus.toolDoneAt) < grace)
}

// MusicToolInFlight is true while self.music.play is still searching.
func MusicToolInFlight() bool {
	mus.mu.Lock()
	defer mus.mu.Unlock()
	return mus.toolInFlight
}

func waitMusicTool(max time.Duration) {
	deadline := time.Now().Add(max)
	for MusicToolInFlight() && time.Now().Before(deadline) {
		time.Sleep(100 * time.Millisecond)
	}
}

// isBlockedMusicQuery: our own detect texts must never become a song search.
func isBlockedMusicQuery(q string) bool {
	switch strings.ToLower(strings.TrimSpace(q)) {
	case MusicDetectFinished, MusicDetectFailed, "phat het nhac", "nhac that bai":
		return true
	}
	return false
}

// handleMusicPlay runs self.music.play and returns the MCP result text.
func handleMusicPlay(query string) (string, bool) {
	query = strings.TrimSpace(query)
	if query == "" {
		return "Thiếu tên bài hát. Hãy hỏi người dùng muốn nghe bài gì.", true
	}
	if isBlockedMusicQuery(query) {
		return "Đây không phải yêu cầu phát nhạc. Không phát bài nào.", true
	}
	cfg := GetConfig()
	if !MusicToolsEnabled(cfg) {
		return "Chế độ nhạc YouTube đang tắt trên robot.", true
	}

	mus.mu.Lock()
	mus.toolInFlight = true
	mus.mu.Unlock()
	defer func() {
		mus.mu.Lock()
		mus.toolInFlight = false
		mus.toolDoneAt = time.Now()
		mus.mu.Unlock()
	}()

	base := music.NormalizeServerURL(cfg.MusicServerURL)
	start := time.Now()
	log.Printf("[Xiaozhi][Music] play %q via %s", query, base)
	p, err := music.Prepare(base, query, musicPrepareTimeout)
	if err != nil {
		log.Printf("[Xiaozhi][Music] %q not playable after %v: %v",
			query, time.Since(start).Round(time.Millisecond), err)
		return fmt.Sprintf("Không tìm được bài %q (%s). Hãy nói với người dùng là không tìm được bài này và hỏi họ có muốn thử bài khác không.",
			query, musicErrorReason(err)), true
	}
	log.Printf("[Xiaozhi][Music] ready %q (id=%s, %d Hz) in %v",
		p.Track.Title, p.Track.ID, p.Rate, time.Since(start).Round(time.Millisecond))
	setPendingMusic(p)
	return fmt.Sprintf("Đã tìm thấy bài %q. Hãy nói thật ngắn với người dùng là đã tìm thấy bài này và sắp phát; robot sẽ tự phát nhạc ngay sau khi bạn nói xong.",
		p.Track.Title), false
}

func musicErrorReason(err error) string {
	msg := err.Error()
	switch {
	case strings.Contains(msg, "deadline exceeded"), strings.Contains(msg, "context canceled"):
		return "máy chủ nhạc phản hồi quá chậm"
	case strings.Contains(msg, "no results"):
		return "không có kết quả"
	case strings.Contains(msg, "HTTP 5"):
		return "máy chủ nhạc báo lỗi"
	}
	return "lỗi kết nối máy chủ nhạc"
}

func setPendingMusic(p *music.Prepared) {
	mus.mu.Lock()
	old := mus.pending
	if mus.pendingTimer != nil {
		mus.pendingTimer.Stop()
	}
	mus.pending = p
	mus.pendingTimer = time.AfterFunc(musicPendingTTL, func() {
		mus.mu.Lock()
		stale := mus.pending == p
		if stale {
			mus.pending = nil
			mus.pendingTimer = nil
		}
		mus.mu.Unlock()
		if stale {
			log.Println("[Xiaozhi][Music] found song never started (no TTS end) — dropped:", p.Track.Title)
			p.Discard()
		}
	})
	mus.mu.Unlock()
	if old != nil {
		old.Discard()
	}
}

func takePendingMusic() *music.Prepared {
	mus.mu.Lock()
	defer mus.mu.Unlock()
	p := mus.pending
	mus.pending = nil
	if mus.pendingTimer != nil {
		mus.pendingTimer.Stop()
		mus.pendingTimer = nil
	}
	return p
}

// HasPendingMusic reports a found song waiting for its confirmation TTS.
func HasPendingMusic() bool {
	mus.mu.Lock()
	defer mus.mu.Unlock()
	return mus.pending != nil
}

// StartPendingMusic plays a song found during the turn that just finished
// speaking: closes the WSS (music-only mode) and starts the speaker. Returns
// false when nothing was pending; the caller then continues as usual.
func StartPendingMusic(reason string) bool {
	if MusicToolInFlight() {
		log.Println("[Xiaozhi][Music] turn ended while still searching — waiting for the result")
		waitMusicTool(musicPrepareTimeout + 2*time.Second)
	}
	p := takePendingMusic()
	if p == nil {
		return false
	}
	log.Printf("[Xiaozhi][Music] start %q (%s) — closing WSS for music-only mode", p.Track.Title, reason)
	mus.mu.Lock()
	mus.hold = true
	mus.mu.Unlock()
	noteConversation()
	DisarmRelistenPending()
	ClearPendingMCPIntent("music start")
	CloseSession()
	if AlsaActive() {
		AlsaCancel()
		time.Sleep(100 * time.Millisecond)
	}
	ensureMM2Mixer()

	pb, err := p.Play(musicFIFOPath)
	if err != nil {
		log.Println("[Xiaozhi][Music] player start failed:", err)
		mus.mu.Lock()
		mus.hold = false
		mus.mu.Unlock()
		go runMusicDetect(MusicDetectFailed)
		return true
	}
	mus.mu.Lock()
	mus.playing = pb
	mus.title = p.Track.Title
	mus.mu.Unlock()
	go watchMusic(pb, p.Track.Title)
	return true
}

func watchMusic(pb *music.Playback, title string) {
	start := time.Now()
	err := pb.Wait()
	mus.mu.Lock()
	if mus.playing == pb {
		mus.playing = nil
		mus.title = ""
		mus.hold = false
	}
	mus.mu.Unlock()
	played := time.Since(start).Round(time.Second)
	switch {
	case errors.Is(err, music.ErrStopped):
		log.Printf("[Xiaozhi][Music] %q stopped after %v — no detect", title, played)
	case err != nil:
		log.Printf("[Xiaozhi][Music] %q failed after %v: %v", title, played, err)
		runMusicDetect(MusicDetectFailed)
	default:
		log.Printf("[Xiaozhi][Music] %q finished (%v)", title, played)
		runMusicDetect(MusicDetectFinished)
	}
}

// MusicPlaying reports whether a device song is on the speaker (or about to be).
// Face greet uses this as "conversation open": the WSS is closed on purpose
// during music-only mode, but a greeting must not start and cut the song.
func MusicPlaying() bool {
	mus.mu.Lock()
	defer mus.mu.Unlock()
	return mus.hold || mus.playing != nil
}

// StopMusic stops the device song and drops any found-but-unplayed one.
// No detect is sent. Returns true if anything was stopped.
func StopMusic(reason string) bool {
	p := takePendingMusic()
	mus.mu.Lock()
	pb, title := mus.playing, mus.title
	mus.playing = nil
	mus.title = ""
	mus.hold = false
	mus.mu.Unlock()
	if p != nil {
		log.Printf("[Xiaozhi][Music] dropped found song %q (%s)", p.Track.Title, reason)
		p.Discard()
	}
	if pb != nil {
		log.Printf("[Xiaozhi][Music] stop %q (%s)", title, reason)
		pb.Stop()
		_ = pb.Wait()
	}
	return p != nil || pb != nil
}

func runMusicDetect(text string) { runWakeDetect("[Xiaozhi][Music]", text) }

// runWakeDetect reopens the WSS and sends listen/detect text; the reply TTS
// plays and the normal continuous relisten opens the mic afterwards.
func runWakeDetect(tag, text string) {
	if IsBusy() {
		log.Printf("%s skip detect %q — a turn is already active", tag, text)
		return
	}
	// Song-end detect runs only after watchMusic clears the hold. A face
	// greeting (or a reload greet) must not open a turn while the song is up:
	// that turn's speaker steals MultiMedia2 and cuts the music.
	if MusicPlaying() {
		log.Printf("%s skip detect %q — music playing", tag, text)
		return
	}
	var result *TurnResult
	var err error
	for attempt := 1; attempt <= musicDetectTries; attempt++ {
		cfg := LoadConfig()
		if !cfg.Enabled {
			return
		}
		result, err = RunDetectTurn(cfg, text)
		if err == nil || errors.Is(err, ErrAborted) {
			break
		}
		log.Printf("%s detect %q attempt %d/%d: %v", tag, text, attempt, musicDetectTries, err)
		// Only retry when the server never answered (dial / send failures).
		if !strings.Contains(err.Error(), "dial") && !strings.Contains(err.Error(), "listen detect") {
			break
		}
		time.Sleep(time.Duration(1500*attempt) * time.Millisecond)
		if IsBusy() || MusicPlaying() {
			return
		}
	}
	if err != nil {
		if !errors.Is(err, ErrAborted) {
			log.Printf("%s detect %q gave up: %v", tag, text, err)
		}
		return
	}
	finishDetectTurn(tag, result)
}

func finishDetectTurn(tag string, result *TurnResult) {
	if result.StreamedAudio {
		DisarmRelistenPending()
		idleOK := WaitPlaybackIdle(result.PCMBytes, result.AudioStartedAt, 120*time.Second)
		CleanupPlaybackFiles()
		SetPlaying(false)
		if !idleOK {
			_ = RequestCancelPlayback()
			ClearCancelFlags()
			time.Sleep(250 * time.Millisecond)
		}
	}
	// The reply may itself have queued another song.
	if StartPendingMusic("after detect TTS") {
		return
	}
	ClearPendingMCPIntent("detect done")
	if result.EndConversation || !SessionAlive() || InBlackjackGameMode() ||
		(!ContinuousMode() && !AmbientHold() && !OpenMic()) {
		log.Println(tag, "detect reply done — mic closed until Hey Vector")
		return
	}
	time.Sleep(350 * time.Millisecond)
	ReopenMicAfterChat("after detect reply")
}
