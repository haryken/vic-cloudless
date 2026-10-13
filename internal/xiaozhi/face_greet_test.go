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
	if faceArrivalDue(t0.Add(2 * time.Minute)) {
		t.Fatal("returned within cooldown must not greet")
	}
	if !faceArrivalDue(t0.Add(faceGreetEvery + 2*time.Minute)) {
		t.Fatal("returned after cooldown and absence should greet")
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
