package xiaozhi

import (
	"encoding/json"
	"strings"

	"github.com/digital-dream-labs/vector-cloud/internal/log"
)

// OpenAI function names must match ^[a-zA-Z0-9_-]{1,64}$; self.* names use
// dots, so "." is carried as "__" on the wire.
func oaiToolName(mcpName string) string {
	return strings.ReplaceAll(mcpName, ".", "__")
}

func mcpToolName(oaiName string) string {
	return strings.ReplaceAll(oaiName, "__", ".")
}

// oaiEndConversationTool lets the model close the conversation (goodbye) the
// way Xiaozhi's server goodbye does: no continuous relisten afterwards.
const oaiEndConversationTool = "self.conversation.end"

func oaiExtraToolDefs() []map[string]interface{} {
	return []map[string]interface{}{
		toolDef(oaiEndConversationTool,
			"Call when the user says goodbye or wants to stop talking (tạm biệt, bye, thôi nhé, dừng nói chuyện, không nói nữa, đi ngủ đi). Say a short goodbye; Vector then stops listening until the wake word.",
			nil, nil),
	}
}

type oaiToolSpec struct {
	Name        string
	Description string
	Parameters  map[string]interface{}
}

func oaiToolSpecs(cfg Config) []oaiToolSpec {
	defs := append(ToolDefs(cfg), oaiExtraToolDefs()...)
	out := make([]oaiToolSpec, 0, len(defs))
	for _, d := range defs {
		name, _ := d["name"].(string)
		if name == "" {
			continue
		}
		desc, _ := d["description"].(string)
		params, _ := d["inputSchema"].(map[string]interface{})
		if params == nil {
			params = map[string]interface{}{"type": "object", "properties": map[string]interface{}{}}
		}
		out = append(out, oaiToolSpec{Name: oaiToolName(name), Description: desc, Parameters: params})
	}
	return out
}

// oaiChatTools is the Chat Completions "tools" array.
func oaiChatTools(cfg Config) []map[string]interface{} {
	specs := oaiToolSpecs(cfg)
	out := make([]map[string]interface{}, 0, len(specs))
	for _, s := range specs {
		out = append(out, map[string]interface{}{
			"type": "function",
			"function": map[string]interface{}{
				"name":        s.Name,
				"description": s.Description,
				"parameters":  s.Parameters,
			},
		})
	}
	return out
}

// oaiRealtimeTools is the Realtime session "tools" array (flat shape).
func oaiRealtimeTools(cfg Config) []map[string]interface{} {
	specs := oaiToolSpecs(cfg)
	out := make([]map[string]interface{}, 0, len(specs))
	for _, s := range specs {
		out = append(out, map[string]interface{}{
			"type":        "function",
			"name":        s.Name,
			"description": s.Description,
			"parameters":  s.Parameters,
		})
	}
	return out
}

// oaiToolResult is what one model function call did.
type oaiToolResult struct {
	Output string
	// End: the model asked to end the conversation.
	End bool
}

// runOAITool executes a model function call. Robot intents go through the
// same queue as Xiaozhi tools/call, so delivery after TTS is shared.
func runOAITool(oaiName, argsJSON string) oaiToolResult {
	name := mcpToolName(oaiName)
	if name == oaiEndConversationTool {
		log.Println("[OpenAI] tool end_conversation")
		return oaiToolResult{Output: `{"ok":true}`, End: true}
	}
	args := map[string]interface{}{}
	if s := strings.TrimSpace(argsJSON); s != "" {
		if err := json.Unmarshal([]byte(s), &args); err != nil {
			log.Printf("[OpenAI] tool %s bad arguments %q: %v", name, truncate(s, 120), err)
		}
	}
	log.Printf("[OpenAI] tool call %s %s", name, truncate(argsJSON, 160))
	out := ExecuteTool(name, args)
	if out.Intent != "" && !out.IsError {
		NoteMCPIntentQueued(out.Intent, out.Params)
		log.Println("[OpenAI] action queued (await TTS confirmation):", out.Intent)
	}
	text := out.Text
	if out.IsError && text == "" {
		text = "error"
	}
	if text == "" {
		text = `{"ok":true}`
	}
	return oaiToolResult{Output: text}
}
