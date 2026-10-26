package xiaozhi

import (
	"testing"
	"time"
)

func resetFaceState() {
	face.mu.Lock()
	face.lastSeen, face.lastGreet, face.lastTalk = time.Time{}, time.Time{}, time.Time{}
	face.greeting = false
	face.mu.Unlock()
}

func finishGreet(at time.Time) {
	face.mu.Lock()
	face.greeting = false
	face.lastGreet = at
	face.mu.Unlock()
}

func TestFaceArrivalFirstSightingGreets(t *testing.T) {
	resetFaceState()
	t0 := time.Now()
	if !faceArrivalDue(t0) {
		t.Fatal("first face should greet")
	}
	if faceArrivalDue(t0.Add(time.Second)) {
		t.Fatal("must not start a second greeting while one is running")
	}
}

func TestFaceStillPresentDoesNotRegreet(t *testing.T) {
	resetFaceState()
	t0 := time.Now()
	faceArrivalDue(t0)
	finishGreet(t0)
	// Person stays in view: sightings every 40s for 20 minutes.
	for at := t0.Add(40 * time.Second); at.Before(t0.Add(20 * time.Minute)); at = at.Add(40 * time.Second) {
		if faceArrivalDue(at) {
			t.Fatalf("re-greeted a person who never left (at +%v)", at.Sub(t0))
		}
	}
}

func TestFaceReturnRespectsCooldown(t *testing.T) {
	resetFaceState()
	t0 := time.Now()
	faceArrivalDue(t0)
	finishGreet(t0)
	back := t0.Add(2 * time.Minute)
	if !faceArrivalDue(back) {
		t.Fatal("re-arrival after absence should reach the cooldown check")
	}
	if faceGreetCooldownLeft(back, faceGreetEveryDefault) <= 0 {
		t.Fatal("returned within the default 5 min must not greet")
	}
	if faceGreetCooldownLeft(back, time.Minute) > 0 {
		t.Fatal("a 1 min interval must allow greeting after 2 min")
	}
	if faceGreetCooldownLeft(t0.Add(faceGreetEveryDefault+time.Second), faceGreetEveryDefault) > 0 {
		t.Fatal("returned after the cooldown should greet")
	}
}

func TestFaceGreetInterval(t *testing.T) {
	if faceGreetInterval(Config{}) != faceGreetEveryDefault {
		t.Fatal("unset interval must default to 5 min")
	}
	fifteen, zero := 15, 0
	if faceGreetInterval(Config{FaceGreetEveryMin: &fifteen}) != 15*time.Minute {
		t.Fatal("configured minutes not applied")
	}
	if faceGreetInterval(Config{FaceGreetEveryMin: &zero}) != 0 {
		t.Fatal("0 must mean no wait")
	}
}

func TestFaceGreetNoWaitStillNeedsArrival(t *testing.T) {
	resetFaceState()
	t0 := time.Now()
	faceArrivalDue(t0)
	finishGreet(t0)
	if faceArrivalDue(t0.Add(20 * time.Second)) {
		t.Fatal("a person still in view must not be greeted again")
	}
	back := t0.Add(20*time.Second + faceAbsentGap)
	if !faceArrivalDue(back) || faceGreetCooldownLeft(back, 0) > 0 {
		t.Fatal("with no wait, a returning person should be greeted")
	}
}

func TestFaceArrivalSkipsWhileMusicPlaying(t *testing.T) {
	resetFaceState()
	mus.mu.Lock()
	mus.hold = true
	mus.mu.Unlock()
	t.Cleanup(func() {
		mus.mu.Lock()
		mus.hold = false
		mus.playing = nil
		mus.mu.Unlock()
	})
	if faceGreetBlocked() != "music playing" {
		t.Fatalf("music should count as busy, got %q", faceGreetBlocked())
	}
	if faceArrivalDue(time.Now()) {
		t.Fatal("must not greet while a song is playing")
	}
}

func TestFaceSkipsRightAfterConversation(t *testing.T) {
	resetFaceState()
	t0 := time.Now()
	face.mu.Lock()
	face.lastTalk = t0
	face.mu.Unlock()
	if faceArrivalDue(t0.Add(10 * time.Second)) {
		t.Fatal("must not greet right after a conversation")
	}
	if !faceArrivalDue(t0.Add(10*time.Second + faceAbsentGap)) {
		t.Fatal("should greet once the quiet window passed and the person re-arrives")
	}
}
