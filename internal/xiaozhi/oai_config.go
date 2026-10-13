package xiaozhi

import (
	"encoding/json"
	"net/url"
	"os"
	"strings"
)

// OpenAI backend (AI giao tiếp → OpenAI). Settings and the API key live in a
// separate file written by wired (root:anki 0640) and only read here, so the
// key never round-trips through xiaozhi.json.
const (
	OpenAIConfigPath = "/data/data/com.anki.victor/persistent/xiaozhi/openai.json"

	BackendXiaozhi = "xiaozhi"
	BackendOpenAI  = "openai"
	// BackendGemini is Gemini Live; it shares openai.json, memory and tools.
	BackendGemini = "gemini"
	// BackendGrok is the xAI Grok Voice Agent (OpenAI Realtime compatible).
	BackendGrok = "grok"

	OpenAIModeRealtime = "realtime"
	OpenAIModePipeline = "pipeline"

	TTSEngineOpenAI = "openai"
	TTSEngineGoogle = "google"
	TTSEngineEdge   = "edge"
	TTSEngineGemini = "gemini"

	OpenAILanguageAuto = "auto"
)

// OpenAIConfig mirrors wired's openai.json.
type OpenAIConfig struct {
	APIKey  string `json:"api_key"`
	Mode    string `json:"mode"`
	BaseURL string `json:"base_url"`
	// Realtime (mode A).
	RealtimeModel string `json:"realtime_model"`
	// Pipeline (mode B, wire-pod style). STT/LLM base URL overrides the
	// OpenAI endpoint per stage (Groq / Gemini / Ollama / LM Studio / …);
	// the stage keys are only for custom servers (see stageKey).
	STTModel   string `json:"stt_model"`
	STTBaseURL string `json:"stt_base_url"`
	STTAPIKey  string `json:"stt_api_key"`
	LLMModel   string `json:"llm_model"`
	LLMBaseURL string `json:"llm_base_url"`
	LLMAPIKey  string `json:"llm_api_key"`
	TTSModel   string `json:"tts_model"`
	// TTSEngine: openai | google (free, per-language segments) | edge (free
	// Microsoft neural voices) | gemini (Gemini API TTS).
	TTSEngine      string `json:"tts_engine"`
	EdgeVoice      string `json:"edge_voice"`
	GeminiAPIKey   string `json:"gemini_api_key"`
	GroqAPIKey     string `json:"groq_api_key"`
	GeminiTTSModel string `json:"gemini_tts_model"`
	GeminiVoice    string `json:"gemini_voice"`
	// Gemini Live backend (key: gemini_api_key).
	GeminiLiveModel string `json:"gemini_live_model"`
	GeminiLiveVoice string `json:"gemini_live_voice"`
	// Grok Voice backend (xAI).
	XAIAPIKey string `json:"xai_api_key"`
	GrokModel string `json:"grok_model"`
	GrokVoice string `json:"grok_voice"`
	// Shared. Language "auto" lets STT detect the spoken language.
	Voice        string `json:"voice"`
	Language     string `json:"language"`
	Instructions string `json:"instructions"`
	MemoryTurns  int    `json:"memory_turns"`
	VisionModel  string `json:"vision_model"`
}

func defaultOpenAIConfig() OpenAIConfig {
	return OpenAIConfig{
		Mode:            OpenAIModeRealtime,
		BaseURL:         "https://api.openai.com/v1",
		RealtimeModel:   "gpt-realtime-mini",
		STTModel:        "gpt-4o-mini-transcribe",
		LLMModel:        "gpt-4o-mini",
		TTSModel:        "gpt-4o-mini-tts",
		TTSEngine:       TTSEngineOpenAI,
		EdgeVoice:       "en-US-EmmaMultilingualNeural",
		GeminiTTSModel:  "gemini-2.5-flash-preview-tts",
		GeminiVoice:     "Kore",
		GeminiLiveModel: "gemini-3.8-live",
		GeminiLiveVoice: "Kore",
		GrokModel:       "grok-voice-latest",
		GrokVoice:       "eve",
		Voice:           "coral",
		Language:        "vi",
		MemoryTurns:     10,
		VisionModel:     "gpt-4o-mini",
	}
}

// LoadOpenAIConfig reads openai.json, filling blanks with defaults.
func LoadOpenAIConfig() OpenAIConfig {
	data, _ := os.ReadFile(OpenAIConfigPath)
	return parseOpenAIConfig(data)
}

func parseOpenAIConfig(data []byte) OpenAIConfig {
	cfg := defaultOpenAIConfig()
	_ = json.Unmarshal(data, &cfg)
	legacy := isLegacyKeyLayout(data)
	d := defaultOpenAIConfig()
	fill := func(v *string, def string) {
		if strings.TrimSpace(*v) == "" {
			*v = def
		}
		*v = strings.TrimSpace(*v)
	}
	fill(&cfg.Mode, d.Mode)
	fill(&cfg.BaseURL, d.BaseURL)
	fill(&cfg.RealtimeModel, d.RealtimeModel)
	fill(&cfg.STTModel, d.STTModel)
	fill(&cfg.LLMModel, d.LLMModel)
	fill(&cfg.TTSModel, d.TTSModel)
	fill(&cfg.TTSEngine, d.TTSEngine)
	fill(&cfg.EdgeVoice, d.EdgeVoice)
	fill(&cfg.GeminiTTSModel, d.GeminiTTSModel)
	fill(&cfg.GeminiVoice, d.GeminiVoice)
	fill(&cfg.GeminiLiveModel, d.GeminiLiveModel)
	fill(&cfg.GeminiLiveVoice, d.GeminiLiveVoice)
	fill(&cfg.GrokModel, d.GrokModel)
	fill(&cfg.GrokVoice, d.GrokVoice)
	fill(&cfg.Voice, d.Voice)
	fill(&cfg.Language, d.Language)
	fill(&cfg.VisionModel, d.VisionModel)
	cfg.Language = strings.ToLower(cfg.Language)
	cfg.APIKey = strings.TrimSpace(cfg.APIKey)
	cfg.STTAPIKey = strings.TrimSpace(cfg.STTAPIKey)
	cfg.LLMAPIKey = strings.TrimSpace(cfg.LLMAPIKey)
	cfg.GeminiAPIKey = strings.TrimSpace(cfg.GeminiAPIKey)
	cfg.GroqAPIKey = strings.TrimSpace(cfg.GroqAPIKey)
	cfg.XAIAPIKey = strings.TrimSpace(cfg.XAIAPIKey)
	cfg.BaseURL = strings.TrimRight(cfg.BaseURL, "/")
	cfg.STTBaseURL = strings.TrimRight(strings.TrimSpace(cfg.STTBaseURL), "/")
	cfg.LLMBaseURL = strings.TrimRight(strings.TrimSpace(cfg.LLMBaseURL), "/")
	if legacy {
		normalizeProviderKeys(&cfg)
	}
	if cfg.MemoryTurns < 0 {
		cfg.MemoryTurns = 0
	}
	if cfg.Mode != OpenAIModePipeline {
		cfg.Mode = OpenAIModeRealtime
	}
	return cfg
}

func (c OpenAIConfig) llmBase() string {
	if c.LLMBaseURL != "" {
		return c.LLMBaseURL
	}
	return c.BaseURL
}

func (c OpenAIConfig) sttBase() string {
	if c.STTBaseURL != "" {
		return c.STTBaseURL
	}
	return c.BaseURL
}

// stageKey: Groq and Gemini have one key each, used by whichever step points
// at them; a custom server only ever gets its own step key (shared when STT
// and LLM use the same custom host), never the OpenAI one.
func (c OpenAIConfig) stageKey(base, own, otherBase, otherOwn string) string {
	switch {
	case base == "":
		return c.APIKey
	case urlHost(base) == groqAPIHost:
		return c.GroqAPIKey
	case urlHost(base) == geminiAPIHost:
		return c.GeminiAPIKey
	case own == "" && otherBase != "" && urlHost(base) == urlHost(otherBase):
		return otherOwn
	}
	return own
}

func (c OpenAIConfig) llmKey() string {
	return c.stageKey(c.LLMBaseURL, c.LLMAPIKey, c.STTBaseURL, c.STTAPIKey)
}

func (c OpenAIConfig) sttKey() string {
	return c.stageKey(c.STTBaseURL, c.STTAPIKey, c.LLMBaseURL, c.LLMAPIKey)
}

func (c OpenAIConfig) geminiKey() string { return c.GeminiAPIKey }

// normalizeProviderKeys moves Groq / Gemini keys that older versions stored
// per step into the provider's own field (wired does the same on load).
func normalizeProviderKeys(c *OpenAIConfig) {
	for _, s := range []struct {
		base string
		key  *string
	}{{c.STTBaseURL, &c.STTAPIKey}, {c.LLMBaseURL, &c.LLMAPIKey}} {
		var dst *string
		switch urlHost(s.base) {
		case groqAPIHost:
			dst = &c.GroqAPIKey
		case geminiAPIHost:
			dst = &c.GeminiAPIKey
		default:
			continue
		}
		if *dst == "" {
			*dst = *s.key
		}
		*s.key = ""
	}
}

// isLegacyKeyLayout: files written before per-provider keys have no
// groq_api_key field (wired always writes it now, even empty).
func isLegacyKeyLayout(data []byte) bool {
	var raw map[string]json.RawMessage
	if json.Unmarshal(data, &raw) != nil {
		return false
	}
	_, ok := raw["groq_api_key"]
	return !ok
}

// sttLanguage is the ISO hint for STT, empty when auto-detecting.
func (c OpenAIConfig) sttLanguage() string {
	if c.Language == OpenAILanguageAuto {
		return ""
	}
	return c.Language
}

// speechLanguage is the default language for single-language TTS voices.
func (c OpenAIConfig) speechLanguage() string {
	if c.Language == "" || c.Language == OpenAILanguageAuto {
		return "vi"
	}
	return c.Language
}

func urlHost(raw string) string {
	u, err := url.Parse(strings.TrimSpace(raw))
	if err != nil {
		return ""
	}
	return strings.ToLower(u.Host)
}

func isOfficialOpenAI(base string) bool {
	return strings.Contains(base, "api.openai.com")
}

// needsKey: some stage would call the official endpoint without a key.
// Realtime is OpenAI-only; the pipeline checks STT and LLM (TTS falls back
// to Google, vision just reports the error).
func (c OpenAIConfig) needsKey() bool {
	if c.Mode != OpenAIModePipeline {
		return c.APIKey == "" && isOfficialOpenAI(c.BaseURL)
	}
	if isOfficialOpenAI(c.sttBase()) && c.sttKey() == "" {
		return true
	}
	return isOfficialOpenAI(c.llmBase()) && c.llmKey() == ""
}

// IsDirectAPIBackend: the robot talks to the AI API itself (OpenAI, Gemini
// Live or Grok) instead of the Xiaozhi server — no OTA, always end-of-speech listens.
func IsDirectAPIBackend(backend string) bool {
	return backend == BackendOpenAI || backend == BackendGemini || backend == BackendGrok
}

// UsingOpenAI reports whether AI giao tiếp is on with a direct-API backend.
func UsingOpenAI() bool {
	cfg := GetConfig()
	return cfg.Enabled && IsDirectAPIBackend(cfg.Backend)
}
