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
	// ListenMode: "fixed" (default) = Vector's ~6s mic window; "vad" = keep the
	// mic open until the server returns STT (see listen_vad.go).
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
	globalCfg = cfg
	SyncListenVADFlag(cfg)
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
	return nil
}

// Enabled returns true if Xiaozhi integration is enabled in the current config.
func Enabled() bool {
	globalCfgMu.RLock()
	defer globalCfgMu.RUnlock()
	return globalCfg.Enabled
}

// ContinuousMode returns true when conversation_mode == "continuous".
func ContinuousMode() bool {
	globalCfgMu.RLock()
	defer globalCfgMu.RUnlock()
	return globalCfg.ConversationMode == "continuous"
}
