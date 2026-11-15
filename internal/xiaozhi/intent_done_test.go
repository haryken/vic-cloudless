package xiaozhi

import (
	"os"
	"path/filepath"
	"testing"
)

func TestDisarmAISelfControlDoneClearsFlags(t *testing.T) {
	dir := t.TempDir()
	pending := filepath.Join(dir, "ai-self-control-pending")
	done := filepath.Join(dir, "intent-done")
	if err := os.WriteFile(pending, []byte("intent_test"), 0644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(done, []byte("test"), 0644); err != nil {
		t.Fatal(err)
	}
	oldPending, oldDone := AISelfControlPendingPath, IntentDonePath
	AISelfControlPendingPath = pending
	IntentDonePath = done
	defer func() {
		AISelfControlPendingPath = oldPending
		IntentDonePath = oldDone
	}()

	DisarmAISelfControlDone()
	if _, err := os.Stat(pending); !os.IsNotExist(err) {
		t.Fatalf("pending still present: %v", err)
	}
	if _, err := os.Stat(done); !os.IsNotExist(err) {
		t.Fatalf("done still present: %v", err)
	}
}
