package xiaozhi

import (
	"os"
	"path/filepath"
	"runtime"
	"runtime/debug"
	"time"

	"github.com/digital-dream-labs/vector-cloud/internal/log"
	"github.com/digital-dream-labs/vector-cloud/internal/voice/vtr"
)

// In-place config apply (ubt_speech_demo ApplyDeviceIdentity): wired saves
// xiaozhi.json and touches ReloadFlagPath instead of restarting vic-cloud.
// ReloadSupportedPath tells wired this build handles the flag; without it
// wired falls back to a (debounced) service restart.
const (
	ReloadFlagPath      = "/run/vic-cloud/xiaozhi-reload"
	ReloadSupportedPath = "/run/vic-cloud/xiaozhi-reload-supported"
	// GreetDetectText is sent as a wake detect after a reload so the new
	// agent greets the user (ubt DETECT_GREET_TEXT).
	GreetDetectText = "xin chào"
)

const (
	reloadPoll = 400 * time.Millisecond
	// The web UI often saves twice in a row (save + generate_code); only the
	// last request within this window is applied.
	reloadSettle = 2 * time.Second
)

// StartConfigReloadWatcher advertises reload support and applies requests.
func StartConfigReloadWatcher() {
	if err := os.MkdirAll(filepath.Dir(ReloadSupportedPath), 0775); err != nil {
		log.Println("[Xiaozhi][Reload] mkdir:", err)
	}
	if err := os.WriteFile(ReloadSupportedPath, []byte("1"), 0644); err != nil {
		log.Println("[Xiaozhi][Reload] advertise support:", err)
	}
	go watchConfigReload()
}

func watchConfigReload() {
	t := time.NewTicker(reloadPoll)
	defer t.Stop()
	var requestedAt time.Time
	for range t.C {
		if _, err := os.Stat(ReloadFlagPath); err == nil {
			_ = os.Remove(ReloadFlagPath)
			if requestedAt.IsZero() {
				log.Println("[Xiaozhi][Reload] config change requested")
			}
			requestedAt = time.Now()
			continue
		}
		if !requestedAt.IsZero() && time.Since(requestedAt) >= reloadSettle {
			requestedAt = time.Time{}
			applyConfigReload()
		}
	}
}

func applyConfigReload() {
	prev := GetConfig()
	cfg := LoadConfig()
	log.Printf("[Xiaozhi][Reload] applying: enabled %v→%v identity %q→%q device %s→%s",
		prev.Enabled, cfg.Enabled, prev.IdentityMode, cfg.IdentityMode, prev.DeviceID, cfg.DeviceID)

	StopMusic("config reload")
	InterruptTurn("config reload")
	AlsaCancel()
	DisarmRelistenPending()
	ClearPendingMCPIntent("config reload")
	CloseSession()

	if InBlackjackGameMode() {
		log.Println("[Xiaozhi][Reload] blackjack active — new config applies after the game")
		return
	}
	if !cfg.Enabled {
		log.Println("[Xiaozhi][Reload] Xiaozhi off — Vosk loads on the next wake")
		return
	}
	if vtr.VoskLoaded() {
		vtr.UnloadVosk()
		runtime.GC()
		debug.FreeOSMemory()
	}
	if cfg.DeviceID != "" && cfg.ClientID != "" {
		if err := OtaCheck(&cfg); err != nil {
			log.Println("[Xiaozhi][Reload] OTA check:", err)
		}
	}
	// The interrupted turn unbinds asynchronously; greet only once it is gone.
	deadline := time.Now().Add(5 * time.Second)
	for IsBusy() && time.Now().Before(deadline) {
		time.Sleep(100 * time.Millisecond)
	}
	runWakeDetect("[Xiaozhi][Reload]", GreetDetectText)
}
