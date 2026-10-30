package xiaozhi

import (
	"strings"
	"time"

	"github.com/digital-dream-labs/vector-cloud/internal/xiaozhi/listenvad"
)

const (
	// SilencePromptDetectText is the short listen/detect cue (ESP32-compatible).
	// Xiaozhi cloud treats it as a wake-style detect; Direct AI expands it.
	SilencePromptDetectText = "khơi gợi lại"
	// SilencePromptNudgeText is what Direct AI backends hear so the model
	// gently invites the user to keep talking instead of ending the chat.
	SilencePromptNudgeText = "(Bạn im lặng nãy giờ. Mình hỏi han hoặc gợi chuyện bằng một câu thật ngắn. Đừng kết thúc.)"

	SilencePromptMinSec = 10
	SilencePromptMaxSec = 50
)

// ErrSilencePrompt: quiet past silence_prompt_sec while the mic was open —
// caller should nudge with listen/detect instead of closing the channel.
var ErrSilencePrompt = listenvad.ErrSilencePrompt

// ClampSilencePromptSec: 0 = off; otherwise snap into 10..50.
func ClampSilencePromptSec(sec int) int {
	if sec <= 0 {
		return 0
	}
	if sec < SilencePromptMinSec {
		return SilencePromptMinSec
	}
	if sec > SilencePromptMaxSec {
		return SilencePromptMaxSec
	}
	return sec
}

// SilencePromptSec is the configured quiet nudge interval (0 = off).
func SilencePromptSec() int {
	return ClampSilencePromptSec(GetConfig().SilencePromptSec)
}

// SilencePromptDuration is SilencePromptSec as a duration, or 0 when off.
func SilencePromptDuration() time.Duration {
	if sec := SilencePromptSec(); sec > 0 {
		return time.Duration(sec) * time.Second
	}
	return 0
}

// ExpandSilencePromptDetect rewrites the short cue for Direct AI backends.
func ExpandSilencePromptDetect(text string) string {
	if strings.TrimSpace(text) != SilencePromptDetectText {
		return text
	}
	return SilencePromptNudgeText
}

// RunSilencePrompt sends the silence-nudge detect and (on continuous / ambient /
// open / gate) reopens the mic after the reply, same as a face greeting.
func RunSilencePrompt() {
	runWakeDetect("[Xiaozhi][Silence]", SilencePromptDetectText)
}
