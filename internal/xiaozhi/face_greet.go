package xiaozhi

import (
	"sync"
	"time"

	"github.com/digital-dream-labs/vector-cloud/internal/log"
)

// Greeting when Vector sees a person (config face_greet). Engine face events
// are sparse (often one every few seconds, some minutes apart), so a person is
// treated as "arrived" only after no face was seen for faceAbsentGap.
const (
	faceAbsentGap  = 60 * time.Second
	faceGreetEvery = 5 * time.Minute
	faceQuietAfter = 30 * time.Second
	faceGreetTag   = "[Xiaozhi][Face]"
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
	defer face.mu.Unlock()
	arrived := face.lastSeen.IsZero() || now.Sub(face.lastSeen) >= faceAbsentGap
	face.lastSeen = now
	switch {
	case !arrived, face.greeting:
		return false
	case !face.lastGreet.IsZero() && now.Sub(face.lastGreet) < faceGreetEvery:
		return false
	case !face.lastTalk.IsZero() && now.Sub(face.lastTalk) < faceQuietAfter:
		return false
	}
	face.greeting = true
	return true
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
	if reason := faceGreetBlocked(); reason != "" {
		log.Printf("%s face %d seen — skip greeting (%s)", faceGreetTag, faceID, reason)
		return
	}
	log.Printf("%s face %d arrived — greeting", faceGreetTag, faceID)
	greeted = true
	runWakeDetect(faceGreetTag, GreetDetectText)
}

func faceGreetBlocked() string {
	switch {
	case IsBusy():
		return "turn/playback active"
	case SessionAlive():
		return "conversation open"
	case MusicPlaying():
		return "music playing"
	case InBlackjackGameMode():
		return "blackjack"
	}
	return ""
}
