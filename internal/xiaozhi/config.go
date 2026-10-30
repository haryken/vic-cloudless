// Package xiaozhi implements the Xiaozhi AI voice assistant integration for vic-cloudless.
// Config is shared with wired via /data/data/com.anki.victor/persistent/xiaozhi/xiaozhi.json.
package xiaozhi

import (
	"encoding/json"
	"os"
	"path/filepath"
	"sync"
)

const ConfigPath = "/data/data/com.anki.victor/persistent/xiaozhi/xiaozhi.json"

// Config mirrors the shared xiaozhi.json configuration.
type Config struct {
	Enabled               bool   `json:"enabled"`
	OTABaseURL            string `json:"ota_base_url"`
	Endpoint              string `json:"endpoint"`
	DeviceID              string `json:"device_id"`
	ClientID              string `json:"client_id"`
	Token                 string `json:"token"`
	ProtocolVersion       int    `json:"protocol_version"`
	AutoApplyOTAWebsocket bool   `json:"auto_apply_ota_websocket"`
	ConversationMode      string `json:"conversation_mode"`
	IdleTimeoutSec        int    `json:"idle_timeout_sec"`
	SessionIdleSec        int    `json:"session_idle_sec"`
	TTSMode               string `json:"tts_mode"`
	GameGoogleTTSVI       bool   `json:"game_google_tts_vi"`
	GameGoogleTTSLang     string `json:"game_google_tts_lang,omitempty"`
	// ListenMode: "vad" (default, also when empty) = keep the mic open until the
	// server returns STT (see listen_vad.go); "fixed" = Vector's ~6s mic window.
	ListenMode string `json:"listen_mode,omitempty"`
	// MusicSource: "server" (default) leaves music to the Xiaozhi server;
	// "youtube" lists self.music.play, which streams from MusicServerURL.
	MusicSource    string `json:"music_source,omitempty"`
	MusicServerURL string `json:"music_server_url,omitempty"`
	// IdentityMode: "vi_pool" = shared Vietnamese preset (rotate MAC/UUID on boot);
	// "course" = English-teaching course pool (see course package);
	// "custom" = user-paired xiaozhi.me device.
	IdentityMode string `json:"identity_mode,omitempty"`
	// English-teaching course (identity_mode "course"). Maps are keyed by the
	// course index ("1".."5"); CourseUnit by "<course>.<book>" (unit 1-based).
	CourseIdx   int            `json:"course_idx,omitempty"`
	CourseVoice map[string]int `json:"course_voice,omitempty"`
	CourseBook  map[string]int `json:"course_book,omitempty"`
	CourseUnit  map[string]int `json:"course_unit,omitempty"`
	StudentName string         `json:"student_name,omitempty"`
	// FaceGreet sends the greeting detect when Vector sees a person arrive.
	FaceGreet bool `json:"face_greet,omitempty"`
	// FaceGreetEveryMin: minimum minutes between face greetings; unset = 5,
	// 0 = no wait (greet whenever someone arrives and no chat is running).
	FaceGreetEveryMin *int `json:"face_greet_every_min,omitempty"`
	// TTSGainPct boosts every AI voice before the speaker (100 = as received).
	TTSGainPct int `json:"tts_gain_pct,omitempty"`
	// GateRMS is the "gate" mode floor. Audio stays on the robot until a
	// voice stays above this RMS. 0 means the default (800).
	GateRMS int `json:"gate_rms,omitempty"`
	// SilencePromptSec: 0 = off (default). When 10..50, after that many
	// seconds of quiet while the mic is open, send listen/detect
	// "khơi gợi lại" so the AI nudges the user to keep talking.
	SilencePromptSec int `json:"silence_prompt_sec,omitempty"`
	// Backend: "xiaozhi" (default), "openai", "gemini" (Gemini Live) or
	// "grok" (xAI Voice); the direct-API ones read openai.json.
	Backend string `json:"backend,omitempty"`
}

func defaultConfig() Config {
	return Config{
		Enabled:               true,
		OTABaseURL:            "https://api.tenclass.net/",
		Endpoint:              "wss://api.tenclass.net/xiaozhi/v1/",
		ProtocolVersion:       1,
		AutoApplyOTAWebsocket: true,
		ConversationMode:      "continuous",
		IdleTimeoutSec:        20,
		SessionIdleSec:        30,
		TTSMode:               "xiaozhi",
		GameGoogleTTSVI:       true,
		GameGoogleTTSLang:     "vi",
		IdentityMode:          IdentityViPool,
	}
}

var (
	globalCfg     Config
	globalCfgOnce sync.Once
	globalCfgMu   sync.RWMutex
)

// LoadConfig reads config from disk (or returns default on error).
// It replaces any previously cached config so it's safe to call multiple times.
func LoadConfig() Config {
	globalCfgMu.Lock()
	defer globalCfgMu.Unlock()

	cfg := defaultConfig()
	data, err := os.ReadFile(ConfigPath)
	if err != nil {
		// Keep previous in-memory config if disk is temporarily unreadable.
		if globalCfg.Endpoint != "" || globalCfg.Enabled {
			return globalCfg
		}
		globalCfg = cfg
		return cfg
	}
	if err := json.Unmarshal(data, &cfg); err != nil {
		if globalCfg.Endpoint != "" || globalCfg.Enabled {
			return globalCfg
		}
		globalCfg = cfg
		return cfg
	}
	if cfg.ProtocolVersion == 0 {
		cfg.ProtocolVersion = 1
	}
	// Shared UI default: blank conversation_mode = Liên tục (all AI backends).
	if cfg.ConversationMode == "" {
		cfg.ConversationMode = "continuous"
	}
	globalCfg = cfg
	SyncListenVADFlag(cfg)
	SyncOpenMicModeFlag(cfg)
	setTTSGain(cfg.TTSGainPct)
	return cfg
}

// GetConfig returns the last loaded config (or the default if never loaded).
func GetConfig() Config {
	globalCfgMu.RLock()
	defer globalCfgMu.RUnlock()
	return globalCfg
}

// SaveConfig writes the config atomically to disk and updates the in-memory cache.
func SaveConfig(cfg Config) error {
	if err := os.MkdirAll(filepath.Dir(ConfigPath), 0777); err != nil {
		return err
	}
	data, err := json.MarshalIndent(cfg, "", "  ")
	if err != nil {
		return err
	}
	if err := os.WriteFile(ConfigPath, data, 0644); err != nil {
		return err
	}
	globalCfgMu.Lock()
	globalCfg = cfg
	globalCfgMu.Unlock()
	SyncListenVADFlag(cfg)
	SyncOpenMicModeFlag(cfg)
	return nil
}

// Enabled returns true if Xiaozhi integration is enabled in the current config.
func Enabled() bool {
	globalCfgMu.RLock()
	defer globalCfgMu.RUnlock()
	return globalCfg.Enabled
}

// ContinuousMode returns true when conversation_mode is "continuous", or empty.
// Empty means continuous: Direct AI (OpenAI / Gemini / Grok / pipeline) and
// older configs already kept multi-turn chat by default — that maps to Liên tục.
// Ambient hold is a different mode: it must not FakeTrigger (that freezes Vector).
func ContinuousMode() bool {
	globalCfgMu.RLock()
	defer globalCfgMu.RUnlock()
	m := globalCfg.ConversationMode
	return m == "" || m == "continuous"
}

// SingleTurnMode is conversation_mode "single": one Q→A then mic off until Hey Vector.
func SingleTurnMode() bool {
	globalCfgMu.RLock()
	defer globalCfgMu.RUnlock()
	return globalCfg.ConversationMode == "single"
}

// AmbientHold is conversation_mode "ambient": after the standing listen ends,
// keep the mic open while Vector walks. It does not include "continuous".
func AmbientHold() bool {
	globalCfgMu.RLock()
	defer globalCfgMu.RUnlock()
	return globalCfg.ConversationMode == "ambient"
}

// OpenMic is conversation_mode "open" or "gate": ting and a cyan backpack on
// wake and on each new turn, without BehaviorReactToVoiceCommand. The mic
// stays open until the utterance has been sent, then it closes so Vector can
// speak. "gate" adds a loudness floor; "open" uploads every frame.
func OpenMic() bool {
	globalCfgMu.RLock()
	defer globalCfgMu.RUnlock()
	m := globalCfg.ConversationMode
	return m == "open" || m == "gate"
}

// GateMic is conversation_mode "gate": same wake as open, but nothing is
// uploaded until the voice clears GateRMS.
func GateMic() bool {
	globalCfgMu.RLock()
	defer globalCfgMu.RUnlock()
	return globalCfg.ConversationMode == "gate"
}

const (
	gateRMSDefault = 800
	gateRMSMin     = 100
	gateRMSMax     = 4000
)

// GateRMS is the configured floor for gate mode, clamped to 100..4000.
// Unset uses 800, the old open-mic hold.
func GateRMS() float64 {
	globalCfgMu.RLock()
	n := globalCfg.GateRMS
	globalCfgMu.RUnlock()
	if n <= 0 {
		return gateRMSDefault
	}
	if n < gateRMSMin {
		return gateRMSMin
	}
	if n > gateRMSMax {
		return gateRMSMax
	}
	return float64(n)
}

// PersistentSession keeps the AI channel (Xiaozhi WSS or Direct-API socket)
// up across turns for continuous / ambient / open / gate. Single closes it.
func PersistentSession() bool {
	return ContinuousMode() || AmbientHold() || OpenMic()
}

// SyncOpenMicModeFlag mirrors conversation_mode "open" into /run for vic-anim.
// The one-shot arm file is removed when leaving the mode so a saved ting
// cannot fire after the user picks another style.
func SyncOpenMicModeFlag(cfg Config) {
	if cfg.Enabled && (cfg.ConversationMode == "open" || cfg.ConversationMode == "gate") {
		_ = os.MkdirAll(filepath.Dir(OpenMicModeFlagPath), 0775)
		_ = os.WriteFile(OpenMicModeFlagPath, []byte("1"), 0664)
		return
	}
	_ = os.Remove(OpenMicModeFlagPath)
	_ = os.Remove(OpenMicArmPath)
}
