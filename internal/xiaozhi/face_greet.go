package xiaozhi

import (
	"os"
	"sync"
	"time"

	"github.com/digital-dream-labs/vector-cloud/internal/log"
)

// Greeting when Vector sees a person (config face_greet). Engine face events
// are sparse (often one every few seconds, some minutes apart), so a person is
// treated as "arrived" only after no face was seen for faceAbsentGap.
const (
	faceAbsentGap         = 60 * time.Second
	faceGreetEveryDefault = 5 * time.Minute
	faceQuietAfter        = 30 * time.Second
	faceGreetTag          = "[Xiaozhi][Face]"
)

var face struct {
	mu        sync.Mutex
	lastSeen  time.Time
	lastGreet time.Time
	greeting  bool
	// lastTalk is the last time a conversation turn started or the WSS closed.
	lastTalk time.Time
}

// noteConversation marks conversation activity so a face greeting does not
// follow right after a chat.
func noteConversation() {
	face.mu.Lock()
	face.lastTalk = time.Now()
	face.mu.Unlock()
}

// NoteConversation is the exported wake/listen marker. Call as soon as Hey
// Vector (or open-mic arm) starts — before BindTurn / WSS — so face greet
// cannot race into the quiet window.
func NoteConversation() {
	noteConversation()
}

// OnFaceObserved is called for every RobotObservedFace from vic-engine.
// It must not block (runs on the engine message loop).
func OnFaceObserved(faceID int32) {
	if faceArrivalDue(time.Now()) {
		go greetFace(faceID)
	}
}

// faceArrivalDue records a sighting and reports whether it should start a
// greeting (claims the in-progress slot when it does).
func faceArrivalDue(now time.Time) bool {
	face.mu.Lock()
	arrived := face.lastSeen.IsZero() || now.Sub(face.lastSeen) >= faceAbsentGap
	face.lastSeen = now
	skip := !arrived || face.greeting ||
		(!face.lastTalk.IsZero() && now.Sub(face.lastTalk) < faceQuietAfter)
	face.mu.Unlock()
	if skip {
		return false
	}
	// IsBusy/SessionAlive/openMic take other locks — check outside face.mu.
	if faceGreetBusyNow() != "" {
		return false
	}
	face.mu.Lock()
	defer face.mu.Unlock()
	if face.greeting || (!face.lastTalk.IsZero() && now.Sub(face.lastTalk) < faceQuietAfter) {
		return false
	}
	face.greeting = true
	return true
}

// faceGreetBusyNow is the non-quiet half of faceGreetBlocked (no face.mu).
func faceGreetBusyNow() string {
	switch {
	case IsBusy():
		return "turn/playback active"
	case SessionAlive():
		return "conversation open"
	case openMicArmed():
		return "open mic armed"
	case MusicPlaying():
		return "music playing"
	case InBlackjackGameMode():
		return "blackjack"
	}
	return ""
}

func greetFace(faceID int32) {
	greeted := false
	defer func() {
		face.mu.Lock()
		face.greeting = false
		if greeted {
			face.lastGreet = time.Now()
		}
		face.mu.Unlock()
	}()

	cfg := LoadConfig()
	if !cfg.Enabled || !cfg.FaceGreet {
		return
	}
	if left := faceGreetCooldownLeft(time.Now(), faceGreetInterval(cfg)); left > 0 {
		log.Printf("%s face %d seen — skip greeting (next allowed in %v)", faceGreetTag, faceID, left.Round(time.Second))
		return
	}
	if reason := faceGreetBlocked(); reason != "" {
		log.Printf("%s face %d seen — skip greeting (%s)", faceGreetTag, faceID, reason)
		return
	}
	log.Printf("%s face %d arrived — greeting", faceGreetTag, faceID)
	greeted = true
	runWakeDetect(faceGreetTag, GreetDetectText)
}

// faceGreetInterval is the user-chosen gap between greetings (face_greet_every_min).
func faceGreetInterval(cfg Config) time.Duration {
	switch {
	case cfg.FaceGreetEveryMin == nil:
		return faceGreetEveryDefault
	case *cfg.FaceGreetEveryMin <= 0:
		return 0
	}
	return time.Duration(*cfg.FaceGreetEveryMin) * time.Minute
}

// faceGreetCooldownLeft is how long until the next greeting is allowed (<= 0: now).
func faceGreetCooldownLeft(now time.Time, every time.Duration) time.Duration {
	face.mu.Lock()
	defer face.mu.Unlock()
	if face.lastGreet.IsZero() {
		return 0
	}
	return every - now.Sub(face.lastGreet)
}

func faceGreetBlocked() string {
	if reason := faceGreetBusyNow(); reason != "" {
		return reason
	}
	// Covers the wake→BindTurn gap: Hotword already noted activity but
	// IsBusy/SessionAlive are still false. Also catches a face claim that
	// raced ahead of NoteConversation before greetFace runs.
	if conversationQuietActive(time.Now()) {
		return "recent conversation"
	}
	return ""
}

func openMicArmed() bool {
	_, err := os.Stat(OpenMicArmPath)
	return err == nil
}

func conversationQuietActive(now time.Time) bool {
	face.mu.Lock()
	defer face.mu.Unlock()
	return !face.lastTalk.IsZero() && now.Sub(face.lastTalk) < faceQuietAfter
}
