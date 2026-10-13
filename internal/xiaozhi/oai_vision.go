package xiaozhi

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"

	"github.com/digital-dream-labs/vector-cloud/internal/log"
)

// geminiVisionModel / grokVisionModel answer photo questions when vision goes
// through Gemini or xAI.
const (
	geminiVisionModel = "gemini-3.5-flash-lite"
	grokVisionModel   = "grok-4.7"
	xaiAPIBase        = "https://api.x.ai/v1"
)

// visionEndpoint: OpenAI (local LLMs rarely accept images), Gemini's
// OpenAI-compatible API for the Gemini Live backend / when only a Gemini key
// is set, else xAI for the Grok backend with only an xAI key.
func visionEndpoint(backend string, ocfg OpenAIConfig) (base, key, model string) {
	if gk := ocfg.geminiKey(); gk != "" && (backend == BackendGemini || ocfg.APIKey == "") {
		return geminiAPIBase + "/openai", gk, geminiVisionModel
	}
	if backend == BackendGrok && ocfg.APIKey == "" && ocfg.XAIAPIKey != "" {
		return xaiAPIBase, ocfg.XAIAPIKey, grokVisionModel
	}
	return ocfg.BaseURL, ocfg.APIKey, ocfg.VisionModel
}

// oaiAnalyzeScene answers self.camera.analyze_photo with a vision model.
func oaiAnalyzeScene(ctx context.Context, question string) (string, error) {
	ocfg := LoadOpenAIConfig()
	base, key, model := visionEndpoint(GetConfig().Backend, ocfg)
	if key == "" {
		return "", fmt.Errorf("vision API key not set")
	}
	jpeg, err := captureJPEGForAnalysis(ctx)
	NoteAnalyzeAttempt()
	if err != nil {
		return "", fmt.Errorf("capture: %w", err)
	}
	log.Printf("[OpenAI] analyze_photo captured %d bytes JPEG", len(jpeg))
	go playPhotoShutter(context.Background())

	if strings.TrimSpace(question) == "" {
		question = "Mô tả ngắn gọn những gì bạn thấy."
	}
	payload := map[string]interface{}{
		"model": model,
		"messages": []map[string]interface{}{
			{"role": "system", "content": "Bạn là đôi mắt của robot Vector. Đây là ảnh từ camera của nó. Trả lời ngắn gọn, đúng sự thật, bằng ngôn ngữ của câu hỏi."},
			{"role": "user", "content": []map[string]interface{}{
				{"type": "text", "text": question},
				{"type": "image_url", "image_url": map[string]interface{}{
					"url":    "data:image/jpeg;base64," + base64.StdEncoding.EncodeToString(jpeg),
					"detail": "low",
				}},
			}},
		},
		"max_tokens": 300,
	}
	data, _ := json.Marshal(payload)
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, base+"/chat/completions", bytes.NewReader(data))
	if err != nil {
		return "", err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+key)
	resp, err := oaiHTTP.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return "", oaiAPIError(resp)
	}
	var out struct {
		Choices []struct {
			Message struct {
				Content string `json:"content"`
			} `json:"message"`
		} `json:"choices"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return "", err
	}
	if len(out.Choices) == 0 || strings.TrimSpace(out.Choices[0].Message.Content) == "" {
		return "", fmt.Errorf("empty vision reply")
	}
	text := strings.TrimSpace(out.Choices[0].Message.Content)
	log.Printf("[OpenAI] analyze_photo OK (%d chars): %s", len(text), truncate(text, 120))
	return text, nil
}
