package xiaozhi

import (
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/digital-dream-labs/vector-cloud/internal/log"
)

// AI self-control completion → detect "xong hành động" (same pattern as face
// greet / music end). Only armed when cloud delivers a voice self-control
// intent with config self_control_resume; UI actions never arm pending.
// Paths are vars so tests can redirect to a temp dir.
var (
	AISelfControlPendingPath = "/run/vic-cloud/ai-self-control-pending"
	IntentDonePath           = "/run/vic-cloud/intent-done"
)

const (
	// IntentDoneDetectText is sent as listen/detect so the AI speaks then
	// finishDetectTurn reopens the mic like a face greeting.
	IntentDoneDetectText = "xong hành động"
	intentDoneTag        = "[Xiaozhi][IntentDone]"
	intentDonePoll       = 250 * time.Millisecond
	// Drop a pending arm that never finished (behavior never deactivated).
	intentDonePendingMaxAge = 10 * time.Minute
)

var intentDoneOnce sync.Once

// StartIntentDoneWatcher polls the engine intent-done flag.
func StartIntentDoneWatcher() {
	intentDoneOnce.Do(func() {
		if err := os.MkdirAll(filepath.Dir(IntentDonePath), 0775); err != nil {
			log.Println(intentDoneTag, "mkdir:", err)
		}
		go watchIntentDone()
	})
}

// ArmAISelfControlDone marks that the next engine DeactivateUserIntent should
// raise intent-done. No-op when the resume feature is off (old behavior).
func ArmAISelfControlDone(intent string) {
	cfg := LoadConfig()
	if !cfg.Enabled || !cfg.SelfControlResume {
		return
	}
	intent = strings.TrimSpace(intent)
	if intent == "" {
		intent = "self_control"
	}
	if err := os.MkdirAll(filepath.Dir(AISelfControlPendingPath), 0775); err != nil {
		log.Println(intentDoneTag, "mkdir pending:", err)
		return
	}
	if err := os.WriteFile(AISelfControlPendingPath, []byte(intent), 0644); err != nil {
		log.Println(intentDoneTag, "arm pending:", err)
		return
	}
	log.Println(intentDoneTag, "armed for", intent)
}

// DisarmAISelfControlDone clears pending/done flags (feature toggled off).
func DisarmAISelfControlDone() {
	_ = os.Remove(AISelfControlPendingPath)
	_ = os.Remove(IntentDonePath)
}

func watchIntentDone() {
	t := time.NewTicker(intentDonePoll)
	defer t.Stop()
	for range t.C {
		expireStaleSelfControlPending()
		data, err := os.ReadFile(IntentDonePath)
		if err != nil {
			continue
		}
		_ = os.Remove(IntentDonePath)
		tag := strings.TrimSpace(string(data))
		cfg := LoadConfig()
		if !cfg.Enabled || !cfg.SelfControlResume {
			log.Printf("%s got done %q — feature off, ignore", intentDoneTag, tag)
			continue
		}
		log.Printf("%s engine finished %q — detect %q", intentDoneTag, tag, IntentDoneDetectText)
		go runWakeDetect(intentDoneTag, IntentDoneDetectText)
	}
}

func expireStaleSelfControlPending() {
	info, err := os.Stat(AISelfControlPendingPath)
	if err != nil {
		return
	}
	if time.Since(info.ModTime()) < intentDonePendingMaxAge {
		return
	}
	log.Println(intentDoneTag, "expire stale pending")
	_ = os.Remove(AISelfControlPendingPath)
}
