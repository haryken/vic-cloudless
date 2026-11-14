package xiaozhi

import (
	"time"

	"github.com/digital-dream-labs/vector-cloud/internal/log"
)

// StandHoldMode is conversation_mode "stand".
//
// Separate from ContinuousMode on purpose: Liên tục keeps the hard ~10s
// no-speech cut. Stand keeps the same FakeTrigger path (ting + stand still)
// but holds the listen / cyan until the WSS idle timer fires or goodbye
// closes the session.
func StandHoldMode() bool {
	globalCfgMu.RLock()
	defer globalCfgMu.RUnlock()
	return globalCfg.ConversationMode == "stand"
}

// StandHoldIdle is how long a quiet stand-listen (and the WSS idle timer)
// may stay up. Matches xiaozhi-esp32 channel idle (120s); config may raise it.
func StandHoldIdle() time.Duration {
	cfg := GetConfig()
	idle := time.Duration(cfg.SessionIdleSec) * time.Second
	if idle <= 0 {
		idle = time.Duration(cfg.IdleTimeoutSec) * time.Second
	}
	if idle < ChannelIdleTimeout {
		return ChannelIdleTimeout
	}
	return idle
}

// StandHoldRelisten FakeTriggers the next standing listen (ting + stand).
// Own helper so continuous TriggerRelisten call sites stay untouched.
func StandHoldRelisten(reason string) {
	if InBlackjackGameMode() {
		return
	}
	if err := TriggerRelisten(); err != nil {
		log.Println("[Xiaozhi][Stand] relisten:", reason, err)
		return
	}
	ClearReopenMicAfterBlackjack()
	log.Println("[Xiaozhi][Stand] relisten —", reason)
}
