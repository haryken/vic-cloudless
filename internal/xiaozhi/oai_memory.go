package xiaozhi

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

// OpenAIMemoryPath keeps the last exchanges so a new session (after idle,
// reboot or reload) still knows what was said before.
const OpenAIMemoryPath = "/data/data/com.anki.victor/persistent/xiaozhi/openai_memory.json"

type oaiExchange struct {
	User      string `json:"user"`
	Assistant string `json:"assistant"`
	At        int64  `json:"at"`
}

var oaiMemMu sync.Mutex

func loadOAIMemory() []oaiExchange {
	oaiMemMu.Lock()
	defer oaiMemMu.Unlock()
	return readOAIMemoryLocked()
}

func readOAIMemoryLocked() []oaiExchange {
	data, err := os.ReadFile(OpenAIMemoryPath)
	if err != nil {
		return nil
	}
	var out []oaiExchange
	if json.Unmarshal(data, &out) != nil {
		return nil
	}
	return out
}

// rememberOAIExchange appends one user/assistant pair, keeping the newest max.
func rememberOAIExchange(user, assistant string, max int) {
	user = strings.TrimSpace(user)
	assistant = strings.TrimSpace(assistant)
	if max <= 0 || (user == "" && assistant == "") {
		return
	}
	oaiMemMu.Lock()
	defer oaiMemMu.Unlock()
	mem := append(readOAIMemoryLocked(), oaiExchange{User: user, Assistant: assistant, At: time.Now().Unix()})
	if len(mem) > max {
		mem = mem[len(mem)-max:]
	}
	data, err := json.Marshal(mem)
	if err != nil {
		return
	}
	_ = os.MkdirAll(filepath.Dir(OpenAIMemoryPath), 0775)
	tmp := OpenAIMemoryPath + ".tmp"
	if os.WriteFile(tmp, data, 0664) == nil {
		_ = os.Rename(tmp, OpenAIMemoryPath)
	}
}

// oaiMemoryTail returns at most max newest exchanges.
func oaiMemoryTail(mem []oaiExchange, max int) []oaiExchange {
	if max <= 0 {
		return nil
	}
	if len(mem) > max {
		return mem[len(mem)-max:]
	}
	return mem
}

// oaiMemoryText renders exchanges for the Realtime instructions.
func oaiMemoryText(mem []oaiExchange) string {
	if len(mem) == 0 {
		return ""
	}
	var b strings.Builder
	b.WriteString("Những lượt trò chuyện gần đây với người dùng (cũ → mới), dùng để nhớ ngữ cảnh:\n")
	for _, e := range mem {
		if e.User != "" {
			b.WriteString("Người dùng: ")
			b.WriteString(e.User)
			b.WriteString("\n")
		}
		if e.Assistant != "" {
			b.WriteString("Vector: ")
			b.WriteString(e.Assistant)
			b.WriteString("\n")
		}
	}
	return b.String()
}
