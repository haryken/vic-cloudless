package xiaozhi

import (
	"crypto/rand"
	"os"
	"strings"
)

// SkipRotatePath: wired writes this before restarting vic-cloud after a
// manual pool renew so boot-time rotation does not run a second time.
const SkipRotatePath = "/run/xiaozhi-skip-rotate"

func ConsumeSkipRotate() bool {
	if _, err := os.Stat(SkipRotatePath); err != nil {
		return false
	}
	_ = os.Remove(SkipRotatePath)
	return true
}

// IdentityViPool is the shared Vietnamese Xiaozhi preset: rotate a
// pre-registered MAC + new client UUID on every vic-cloud start.
const IdentityViPool = "vi_pool"
const IdentityCustom = "custom"

const TenclassOTA = "https://api.tenclass.net/"
const TenclassWSS = "wss://api.tenclass.net/xiaozhi/v1/"

// Pre-registered tenclass device MACs (each account slot is short-lived).
var viPoolMACs = []string{
	"1c:db:d4:b5:73:3c",
	"58:a0:23:a6:fe:31",
	"a8:b5:44:dd:e3:cf",
	"1c:db:d4:b5:74:7c",
	"1c:db:d4:b5:6a:d8",
	"1c:db:d4:b5:74:54",
	"1c:db:d4:b5:72:ec",
	"1c:db:d4:b5:71:d4",
	"1c:db:d4:b5:74:d4",
	"1c:db:d4:a9:48:84",
	"1c:db:d4:a9:59:b4",
	"1c:db:d4:a9:5b:a0",
	"28:df:eb:02:6c:7d",
	"bc:fc:e7:8a:d8:06",
	"dc:b4:d9:0c:a4:9c",
	"dc:b4:d9:0c:a4:80",
	"dc:b4:d9:0c:a6:00",
	"dc:b4:d9:03:4e:f4",
	"dc:b4:d9:0c:a5:38",
	"dc:b4:d9:03:43:38",
}

func macInViPool(mac string) bool {
	mac = strings.ToLower(strings.TrimSpace(mac))
	for _, m := range viPoolMACs {
		if m == mac {
			return true
		}
	}
	return false
}

func IsViPool(cfg Config) bool {
	mode := strings.ToLower(strings.TrimSpace(cfg.IdentityMode))
	if mode == IdentityCustom {
		return false
	}
	if mode == IdentityViPool {
		return true
	}
	// Existing tenclass robots using a shared pool MAC, before identity_mode existed.
	return mode == "" && macInViPool(cfg.DeviceID)
}

// ApplyViPoolDefaults sets tenclass OTA/WSS and identity_mode. Does not rotate IDs.
func ApplyViPoolDefaults(cfg *Config) {
	cfg.IdentityMode = IdentityViPool
	cfg.OTABaseURL = TenclassOTA
	cfg.Endpoint = TenclassWSS
	cfg.AutoApplyOTAWebsocket = true
	cfg.TTSMode = "xiaozhi"
	if cfg.ConversationMode == "" {
		cfg.ConversationMode = "continuous"
	}
}

// RotateViPoolIdentity picks a pool MAC (not the previous one) and a new UUID.
func RotateViPoolIdentity(cfg *Config) {
	ApplyViPoolDefaults(cfg)
	cfg.DeviceID = pickPoolMAC(cfg.DeviceID)
	cfg.ClientID = genUUIDv4()
	cfg.Token = ""
}

func pickPoolMAC(avoid string) string {
	avoid = strings.ToLower(strings.TrimSpace(avoid))
	n := len(viPoolMACs)
	if n == 0 {
		return genRandomMAC()
	}
	var b [1]byte
	_, _ = rand.Read(b[:])
	start := int(b[0]) % n
	for i := 0; i < n; i++ {
		mac := viPoolMACs[(start+i)%n]
		if mac != avoid {
			return mac
		}
	}
	return viPoolMACs[start]
}
