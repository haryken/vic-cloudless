package xiaozhi

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"time"

	"github.com/digital-dream-labs/vector-cloud/internal/log"
	"github.com/digital-dream-labs/vector-cloud/internal/xiaozhi/listenvad"
)

// OpenAI conversation session: alive between turns of one continuous
// conversation (like the persistent Xiaozhi WSS) and closed after
// SessionIdleSec without a turn, on goodbye, or on config reload.
var oaiSess struct {
	mu    sync.Mutex
	alive bool
	rt    *rtConn
	gl    *glConn
	// history is the pipeline (mode B) chat transcript, system prompt excluded.
	history []chatMsg
	idle    *time.Timer
}

func oaiSessionAlive() bool {
	oaiSess.mu.Lock()
	defer oaiSess.mu.Unlock()
	return oaiSess.alive
}

// oaiCloseSession drops the Realtime / Gemini Live socket and the in-session
// chat history.
func oaiCloseSession(reason string) {
	oaiSess.mu.Lock()
	rt := oaiSess.rt
	gl := oaiSess.gl
	was := oaiSess.alive
	oaiSess.rt = nil
	oaiSess.gl = nil
	oaiSess.alive = false
	oaiSess.history = nil
	if oaiSess.idle != nil {
		oaiSess.idle.Stop()
		oaiSess.idle = nil
	}
	oaiSess.mu.Unlock()
	if rt != nil {
		rt.close()
	}
	if gl != nil {
		gl.close()
	}
	if was || rt != nil || gl != nil {
		noteConversation()
		log.Println("[OpenAI] session closed; reason:", reason)
	}
}

// oaiBeginTurn marks the session alive and stops the idle timer.
func oaiBeginTurn() {
	oaiSess.mu.Lock()
	oaiSess.alive = true
	if oaiSess.idle != nil {
		oaiSess.idle.Stop()
		oaiSess.idle = nil
	}
	oaiSess.mu.Unlock()
}

// oaiArmIdle closes the session after SessionIdleSec without a new turn.
// Only for persistent conversation styles (continuous / ambient / open / gate).
func oaiArmIdle(cfg Config) {
	idle := time.Duration(cfg.SessionIdleSec) * time.Second
	if idle < 10*time.Second {
		idle = 10 * time.Second
	}
	oaiSess.mu.Lock()
	defer oaiSess.mu.Unlock()
	if !oaiSess.alive {
		return
	}
	if oaiSess.idle != nil {
		oaiSess.idle.Stop()
	}
	var t *time.Timer
	t = time.AfterFunc(idle, func() {
		oaiSess.mu.Lock()
		current := oaiSess.idle == t
		oaiSess.mu.Unlock()
		if !current {
			return
		}
		if IsBusy() {
			oaiArmIdle(GetConfig())
			return
		}
		oaiCloseSession("idle")
	})
	oaiSess.idle = t
}

// oaiKeepOrCloseAfterTurn mirrors Xiaozhi PersistentSession: keep the Direct-API
// socket for continuous/ambient/open/gate; close it for single-turn.
func oaiKeepOrCloseAfterTurn(cfg Config, reason string) {
	if PersistentSession() {
		oaiArmIdle(cfg)
		return
	}
	oaiCloseSession(reason)
}

const oaiBaseInstructions = `Bạn là Vector, chú robot nhỏ thông minh, tinh nghịch và tốt bụng của người dùng.
Mặc định trả lời bằng tiếng Việt; nếu người dùng nói ngôn ngữ khác thì trả lời bằng ngôn ngữ đó.
Câu trả lời sẽ được đọc thành tiếng: nói tự nhiên, ngắn gọn (thường 1-3 câu), không emoji, không markdown, không gạch đầu dòng.
Bạn có các công cụ (function) điều khiển thân robot và đọc thông tin robot. Khi người dùng muốn robot LÀM gì đó (nhảy, bắn pháo hoa, về sạc, đổi âm lượng, chụp ảnh, nhìn xem có gì, mở nhạc, chơi game, kiểm tra pin, ...), hãy hiểu theo ý nghĩa câu nói chứ không cần đúng từ khóa, BẮT BUỘC gọi công cụ phù hợp, rồi nói một câu xác nhận ngắn. Không được nói là đã làm nếu chưa gọi công cụ.
Không bịa thông tin mà công cụ có thể cung cấp (pin, trạng thái, hình ảnh camera).
Khi người dùng chào tạm biệt hoặc muốn dừng nói chuyện, nói lời tạm biệt ngắn và gọi công cụ self__conversation__end.`

// oaiInstructions builds the system prompt: persona, user extras, language
// and (for new Realtime sessions) remembered exchanges.
func oaiInstructions(ocfg OpenAIConfig, memory []oaiExchange) string {
	var b strings.Builder
	b.WriteString(oaiBaseInstructions)
	if lang := strings.TrimSpace(ocfg.Language); lang != "" && lang != "vi" && lang != OpenAILanguageAuto {
		b.WriteString("\nNgôn ngữ mặc định của người dùng: " + lang + ".")
	}
	if ocfg.Mode == OpenAIModePipeline && ocfg.TTSEngine == TTSEngineGoogle {
		b.WriteString("\n" + oaiLangTagInstructions(ocfg.speechLanguage()))
	}
	if extra := strings.TrimSpace(ocfg.Instructions); extra != "" {
		b.WriteString("\n\nYêu cầu thêm của chủ robot:\n")
		b.WriteString(extra)
	}
	if mem := oaiMemoryText(memory); mem != "" {
		b.WriteString("\n\n")
		b.WriteString(mem)
	}
	return b.String()
}

// oaiSpeaker streams 16 kHz PCM to Vector's speaker for one turn.
type oaiSpeaker struct {
	started   bool
	ended     bool
	failed    bool
	capped    bool
	bytes     int
	startedAt time.Time
}

var errSpeakerCapped = errors.New("speaker pcm cap")

// oaiSoftCapPCMBytes matches the Xiaozhi turn cap (~250 s of 16 kHz s16le).
const oaiSoftCapPCMBytes = 250 * 16000 * 2

func (s *oaiSpeaker) write(pcm16k []byte) error {
	if s.failed || s.ended {
		return fmt.Errorf("speaker closed")
	}
	if len(pcm16k) == 0 {
		return nil
	}
	if !s.started {
		prime, err := ArmStreamWithSilencePrime()
		if err != nil {
			s.failed = true
			return fmt.Errorf("stream start: %w", err)
		}
		s.started = true
		s.startedAt = time.Now()
		s.bytes = prime
		SetPlaying(true)
	}
	if s.bytes > oaiSoftCapPCMBytes {
		s.capped = true
		s.end()
		return errSpeakerCapped
	}
	if _, err := StreamAppend(pcm16k); err != nil {
		if err == ErrPCMHardCap {
			s.capped = true
			s.end()
			return errSpeakerCapped
		}
		s.failed = true
		_ = StreamEnd()
		SetPlaying(false)
		s.started = false
		return err
	}
	s.bytes += len(pcm16k)
	return nil
}

func (s *oaiSpeaker) end() {
	if s.started && !s.ended {
		_ = StreamEnd()
		s.ended = true
	}
}

// oaiMicFrameBytes is one anim mic chunk (120 ms at 16 kHz).
const oaiMicFrameBytes = 3840

// oaiMicMuteHead covers the relisten earcon; realtime server VAD otherwise
// hears it as a short utterance and auto-replies to an empty transcript.
const oaiMicMuteHead = 600 * time.Millisecond

// oaiCapture keeps the mic open until end-of-speech (server VAD for Realtime,
// local silence for the pipeline). Returns listenvad.ErrNoSpeech on silence,
// ErrSilencePrompt when quiet past silence_prompt_sec.
func oaiCapture(ctx context.Context, up listenvad.Uplink, audio <-chan []byte, endOfSpeech time.Duration) (listenvad.Result, error) {
	tune := tuneListen()
	mute := oaiMicMuteHead
	if tune.muteHead > mute {
		mute = tune.muteHead
	}
	res, err := listenvad.Capture(ctx, up, audio, listenvad.Options{
		NoSpeech:      tune.noSpeech,
		MaxListen:     tune.maxListen,
		MicWait:       12 * time.Second,
		FrameBytes:    oaiMicFrameBytes,
		MinRMS:        tune.minRMS,
		WaitForSpeech: tune.waitSpeech,
		SilencePrompt: tune.silencePrompt,
		Encode:        func(p []byte) ([]byte, error) { return append([]byte(nil), p...), nil },
		EndOfSpeech:   endOfSpeech,
		MuteHead:      mute,
		OnFirstFrame: func() {
			MarkMicUplink()
			log.Println("[OpenAI][Mic] first uplink frame")
		},
	})
	switch {
	case errors.Is(err, listenvad.ErrSilencePrompt):
		MarkMicListenEnded()
		log.Printf("[OpenAI][Mic] silence prompt (%s) — %d frames, %v",
			res.Reason, res.Frames, res.Duration.Round(time.Millisecond))
		return res, ErrSilencePrompt
	case err == nil, errors.Is(err, listenvad.ErrNoSpeech):
		MarkMicListenEnded()
		log.Printf("[OpenAI][Mic] listen end (%s) — %d frames, %v, speech=%v floor=%.0f peak=%.0f talk_thr=%.0f",
			res.Reason, res.Frames, res.Duration.Round(time.Millisecond), res.Speech, res.Floor, res.Peak, res.TalkThr)
		if SilencePromptSec() > 0 &&
			(res.Reason == listenvad.ReasonNoSpeech || res.Reason == listenvad.ReasonSilencePrompt) {
			return res, ErrSilencePrompt
		}
		if err == nil && OpenMic() &&
			(res.Reason == listenvad.ReasonMaxListen || res.Reason == listenvad.ReasonNoSpeech) {
			return res, ErrNoSpeech
		}
		return res, err
	case errors.Is(err, listenvad.ErrNoUplink):
		log.Printf("[OpenAI] no mic audio for %v after listen start", res.Duration.Round(time.Millisecond))
		return res, fmt.Errorf("no mic uplink after listen start")
	case errors.Is(err, context.Canceled):
		return res, ErrAborted
	default:
		return res, err
	}
}

// oaiTurnState collects what one OpenAI turn produced.
type oaiTurnState struct {
	stt     string
	reply   strings.Builder
	end     bool
	speaker oaiSpeaker
}

// replyText is the spoken reply without language tags.
func (st *oaiTurnState) replyText() string {
	return strings.TrimSpace(stripLangTags(st.reply.String()))
}

func (st *oaiTurnState) result() *TurnResult {
	r := &TurnResult{
		STTText:         st.stt,
		ResponseText:    st.replyText(),
		StreamedAudio:   st.speaker.started,
		PCMBytes:        st.speaker.bytes,
		AudioStartedAt:  st.speaker.startedAt,
		EndConversation: st.end,
	}
	if intent, params, ok := PeekMCPIntentQueued(); ok {
		r.RobotIntent = intent
		r.RobotIntentParams = params
		log.Println("[OpenAI] turn complete with queued action:", intent)
	}
	return r
}

// runOpenAITurn is runTurn for the direct-API backends (OpenAI both modes,
// Gemini Live, Grok).
func runOpenAITurn(audioStream <-chan []byte, cfg Config, afterSTT func(stt string), detectText string) (*TurnResult, error) {
	ocfg := LoadOpenAIConfig()
	gemini := cfg.Backend == BackendGemini
	grok := cfg.Backend == BackendGrok
	switch {
	case gemini && ocfg.geminiKey() == "":
		return nil, fmt.Errorf("gemini: API key not set")
	case grok && ocfg.XAIAPIKey == "":
		return nil, fmt.Errorf("grok: xAI API key not set")
	case !gemini && !grok && ocfg.needsKey():
		return nil, fmt.Errorf("openai: API key not set")
	}

	turnCtx, turnCancel := context.WithTimeout(context.Background(), 15*time.Minute)
	defer turnCancel()

	if detectText == "" {
		StopMusic("new listen")
	}
	turnGen := BindTurn(turnCancel)
	oaiBeginTurn()
	ResetMicUplink()

	st := &oaiTurnState{}
	var err error
	switch {
	case gemini:
		err = runGeminiLiveTurn(turnCtx, audioStream, cfg, ocfg, afterSTT, detectText, st)
	case grok:
		err = runRealtimeTurn(turnCtx, audioStream, cfg, ocfg, afterSTT, detectText, st)
	case ocfg.Mode == OpenAIModePipeline:
		err = runPipelineTurn(turnCtx, audioStream, cfg, ocfg, afterSTT, detectText, st)
	default:
		err = runRealtimeTurn(turnCtx, audioStream, cfg, ocfg, afterSTT, detectText, st)
	}

	// Drain while still bound (like turn.go) so a wake-word barge-in during
	// the tail of the reply cancels this turn instead of racing the new one.
	st.speaker.end()
	UnbindTurn(turnGen)
	if turnCtx.Err() == context.Canceled {
		oaiKeepOrCloseAfterTurn(cfg, "aborted")
		return nil, ErrAborted
	}

	if err != nil && !st.speaker.started {
		if errors.Is(err, ErrSilencePrompt) {
			// Caller runs the nudge detect immediately; keep the session.
			return nil, err
		}
		if !errors.Is(err, ErrNoSpeech) {
			log.Println("[OpenAI] turn error:", err)
		}
		if errors.Is(err, errOAISessionBroken) {
			oaiCloseSession("error")
		} else {
			oaiKeepOrCloseAfterTurn(cfg, "no_speech")
		}
		return nil, err
	}
	if err != nil {
		log.Println("[OpenAI] turn ended early (partial reply played):", err)
	}

	if st.stt != "" || st.reply.Len() > 0 {
		rememberOAIExchange(st.stt, st.replyText(), ocfg.MemoryTurns)
	}
	log.Printf("[OpenAI] turn done STT=%q reply=%q streamed=%v pcmBytes=%d end=%v",
		st.stt, truncate(st.replyText(), 200), st.speaker.started, st.speaker.bytes, st.end)

	if st.end {
		oaiCloseSession("end_conversation")
	} else {
		oaiKeepOrCloseAfterTurn(cfg, "single")
	}
	return st.result(), nil
}

var errOAISessionBroken = errors.New("openai session broken")
