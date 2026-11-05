package extract

import (
	"strings"

	"github.com/MoeForge/moego/ojson"
)

// Request is the normalized client request.
type Request struct {
	Format string
	Model  string
	// System is the system prompt unit, or nil.
	System *Message
	// Tools is the canonical JSON of the tool list, or "".
	Tools string
	// Messages are the conversation units in client order.
	Messages []Message
	// ClaudeUserID is metadata.user_id of a Claude Messages request.
	ClaudeUserID string
	// LiveCwd is safeguards[0].classifier_context.live_cwd when present.
	LiveCwd string
}

// SystemText returns the system prompt text.
func (r *Request) SystemText() string {
	if r == nil || r.System == nil {
		return ""
	}
	var parts []string
	for _, b := range r.System.Blocks {
		parts = append(parts, b.Text)
	}
	return strings.Join(parts, "\n")
}

// ParseRequest normalizes a client request body of the given entry format
// ("claude", "openai", "openai-response", "gemini", ...). Unknown formats are
// detected from the body shape.
func ParseRequest(format string, body []byte) *Request {
	req := &Request{Format: format}
	if len(body) == 0 || !ojson.ValidBytes(body) {
		return req
	}
	root := ojson.ParseBytes(body)
	if inner := root.Get("request"); inner.IsObject() && !root.Get("contents").Exists() {
		// Gemini CLI wraps the request.
		root = inner
	}
	req.Model = root.Get("model").String()
	switch detectRequestFormat(format, root) {
	case "claude":
		parseClaudeRequest(req, root)
	case "openai":
		parseChatRequest(req, root)
	case "openai-response":
		parseResponsesRequest(req, root)
	case "gemini":
		parseGeminiRequest(req, root)
	}
	if tools := root.Get("tools"); tools.IsArray() && len(tools.Array()) > 0 {
		req.Tools = canonicalJSON(tools)
	}
	return req
}

func detectRequestFormat(format string, root ojson.Result) string {
	switch strings.ToLower(strings.TrimSpace(format)) {
	case "claude":
		return "claude"
	case "openai":
		return "openai"
	case "openai-response", "codex":
		return "openai-response"
	case "gemini", "gemini-cli", "antigravity":
		return "gemini"
	}
	switch {
	case root.Get("contents").Exists():
		return "gemini"
	case root.Get("input").Exists() || root.Get("instructions").Exists():
		return "openai-response"
	case root.Get("system").Exists() || root.Get("anthropic_version").Exists():
		return "claude"
	case root.Get("messages").Exists():
		return "openai"
	}
	return ""
}

func parseClaudeRequest(req *Request, root ojson.Result) {
	if sys := root.Get("system"); sys.Exists() {
		if text := textOf(sys); strings.TrimSpace(text) != "" {
			req.System = &Message{Role: KindSystem, Blocks: []Block{{Kind: KindSystem, Text: text}}}
		}
	}
	req.ClaudeUserID = root.Get("metadata.user_id").String()
	req.LiveCwd = root.Get("safeguards.0.classifier_context.live_cwd").String()
	for _, msg := range root.Get("messages").Array() {
		role := msg.Get("role").String()
		unit := Message{Role: role, Blocks: claudeBlocks(role, msg.Get("content"))}
		if !unit.Empty() {
			req.Messages = append(req.Messages, unit)
		}
	}
}

// claudeBlocks normalizes Claude content (string or block list).
func claudeBlocks(role string, content ojson.Result) []Block {
	textKind := KindUser
	if role == "assistant" {
		textKind = KindAssistant
	}
	if content.Type == ojson.String {
		return []Block{{Kind: textKind, Text: content.String()}}
	}
	var blocks []Block
	for _, part := range content.Array() {
		switch typ := part.Get("type").String(); {
		case typ == "text":
			blocks = append(blocks, Block{Kind: textKind, Text: part.Get("text").String()})
		case typ == "thinking":
			blocks = append(blocks, Block{Kind: KindThinking, Text: part.Get("thinking").String()})
		case typ == "redacted_thinking":
			blocks = append(blocks, Block{Kind: KindThinking, Text: "[redacted thinking]"})
		case typ == "tool_use" || typ == "server_tool_use" || typ == "mcp_tool_use":
			blocks = append(blocks, Block{
				Kind:       KindToolCall,
				ToolName:   part.Get("name").String(),
				ToolCallID: part.Get("id").String(),
				Text:       canonicalJSON(part.Get("input")),
			})
		case typ == "tool_result" || strings.HasSuffix(typ, "_tool_result"):
			blocks = append(blocks, Block{
				Kind:       KindToolResult,
				ToolCallID: part.Get("tool_use_id").String(),
				Text:       textOf(part.Get("content")),
				IsError:    part.Get("is_error").Bool(),
			})
		case typ == "image":
			blocks = append(blocks, Block{Kind: textKind, Text: "[image]"})
		case typ == "document":
			blocks = append(blocks, Block{Kind: textKind, Text: "[document]"})
		}
	}
	return blocks
}

func parseChatRequest(req *Request, root ojson.Result) {
	var systemParts []string
	for _, msg := range root.Get("messages").Array() {
		role := msg.Get("role").String()
		var blocks []Block
		switch role {
		case "system", "developer":
			text := textOf(msg.Get("content"))
			systemParts = append(systemParts, text)
			blocks = append(blocks, Block{Kind: KindSystem, Text: text})
		case "tool", "function":
			blocks = append(blocks, Block{
				Kind:       KindToolResult,
				ToolCallID: firstString(msg, "tool_call_id", "name"),
				Text:       textOf(msg.Get("content")),
			})
		case "assistant":
			if r := firstString(msg, "reasoning_content", "reasoning"); r != "" {
				blocks = append(blocks, Block{Kind: KindThinking, Text: r})
			}
			if text := textOf(msg.Get("content")); text != "" {
				blocks = append(blocks, Block{Kind: KindAssistant, Text: text})
			}
			for _, call := range msg.Get("tool_calls").Array() {
				blocks = append(blocks, Block{
					Kind:       KindToolCall,
					ToolName:   call.Get("function.name").String(),
					ToolCallID: call.Get("id").String(),
					Text:       canonicalArguments(call.Get("function.arguments").String()),
				})
			}
		default:
			blocks = append(blocks, Block{Kind: KindUser, Text: chatContentText(msg.Get("content"))})
		}
		unit := Message{Role: role, Blocks: blocks}
		if !unit.Empty() {
			req.Messages = append(req.Messages, unit)
		}
	}
	if text := strings.Join(systemParts, "\n\n"); strings.TrimSpace(text) != "" {
		req.System = &Message{Role: KindSystem, Blocks: []Block{{Kind: KindSystem, Text: text}}}
	}
}

func chatContentText(content ojson.Result) string {
	if content.Type == ojson.String {
		return content.String()
	}
	var parts []string
	for _, part := range content.Array() {
		switch part.Get("type").String() {
		case "text", "input_text", "output_text", "refusal":
			parts = append(parts, part.Get("text").String())
		case "image_url", "input_image":
			parts = append(parts, "[image]")
		case "input_audio":
			parts = append(parts, "[audio]")
		case "file", "input_file":
			parts = append(parts, "[file]")
		}
	}
	return strings.Join(parts, "\n")
}

func parseResponsesRequest(req *Request, root ojson.Result) {
	if instr := root.Get("instructions"); instr.Exists() {
		if text := textOf(instr); strings.TrimSpace(text) != "" {
			req.System = &Message{Role: KindSystem, Blocks: []Block{{Kind: KindSystem, Text: text}}}
		}
	}
	input := root.Get("input")
	if input.Type == ojson.String {
		req.Messages = append(req.Messages, Message{Role: "user", Blocks: []Block{{Kind: KindUser, Text: input.String()}}})
		return
	}
	for _, item := range input.Array() {
		if unit, ok := responsesItem(item); ok && !unit.Empty() {
			req.Messages = append(req.Messages, unit)
		}
	}
}

// responsesItem normalizes one Responses input or output item.
func responsesItem(item ojson.Result) (Message, bool) {
	typ := item.Get("type").String()
	switch {
	case typ == "message" || (typ == "" && item.Get("role").Exists()):
		role := item.Get("role").String()
		kind := KindUser
		switch role {
		case "assistant":
			kind = KindAssistant
		case "system", "developer":
			kind = KindSystem
		}
		return Message{Role: role, Blocks: []Block{{Kind: kind, Text: chatContentText(item.Get("content"))}}}, true
	case typ == "reasoning":
		var parts []string
		for _, s := range item.Get("summary").Array() {
			if t := s.Get("text").String(); t != "" {
				parts = append(parts, t)
			}
		}
		if len(parts) == 0 {
			for _, s := range item.Get("content").Array() {
				if t := s.Get("text").String(); t != "" {
					parts = append(parts, t)
				}
			}
		}
		if len(parts) == 0 {
			return Message{}, false
		}
		return Message{Role: "assistant", Blocks: []Block{{Kind: KindThinking, Text: strings.Join(parts, "\n\n")}}}, true
	case typ == "function_call":
		return Message{Role: "assistant", Blocks: []Block{{
			Kind:       KindToolCall,
			ToolName:   item.Get("name").String(),
			ToolCallID: item.Get("call_id").String(),
			Text:       canonicalArguments(item.Get("arguments").String()),
		}}}, true
	case typ == "custom_tool_call":
		return Message{Role: "assistant", Blocks: []Block{{
			Kind:       KindToolCall,
			ToolName:   item.Get("name").String(),
			ToolCallID: item.Get("call_id").String(),
			Text:       item.Get("input").String(),
		}}}, true
	case strings.HasSuffix(typ, "_call_output") || strings.HasSuffix(typ, "_output"):
		return Message{Role: "tool", Blocks: []Block{{
			Kind:       KindToolResult,
			ToolCallID: item.Get("call_id").String(),
			Text:       textOf(item.Get("output")),
		}}}, true
	case strings.HasSuffix(typ, "_call"):
		name := item.Get("name").String()
		if name == "" {
			name = strings.TrimSuffix(typ, "_call")
		}
		input := item.Get("action")
		if !input.Exists() {
			input = item.Get("arguments")
		}
		text := canonicalJSON(input)
		if input.Type == ojson.String {
			text = canonicalArguments(input.String())
		}
		return Message{Role: "assistant", Blocks: []Block{{
			Kind:       KindToolCall,
			ToolName:   name,
			ToolCallID: firstString(item, "call_id", "id"),
			Text:       text,
		}}}, true
	}
	return Message{}, false
}

func parseGeminiRequest(req *Request, root ojson.Result) {
	sys := root.Get("systemInstruction")
	if !sys.Exists() {
		sys = root.Get("system_instruction")
	}
	if sys.Exists() {
		if text := geminiPartsText(sys.Get("parts")); strings.TrimSpace(text) != "" {
			req.System = &Message{Role: KindSystem, Blocks: []Block{{Kind: KindSystem, Text: text}}}
		}
	}
	for _, content := range root.Get("contents").Array() {
		role := content.Get("role").String()
		unit := Message{Role: role, Blocks: geminiBlocks(role, content.Get("parts"))}
		if !unit.Empty() {
			req.Messages = append(req.Messages, unit)
		}
	}
}

func geminiPartsText(parts ojson.Result) string {
	var out []string
	for _, p := range parts.Array() {
		if t := p.Get("text").String(); t != "" {
			out = append(out, t)
		}
	}
	return strings.Join(out, "\n")
}

func geminiBlocks(role string, parts ojson.Result) []Block {
	textKind := KindUser
	if role == "model" {
		textKind = KindAssistant
	}
	var blocks []Block
	for i, p := range parts.Array() {
		switch {
		case p.Get("functionCall").Exists():
			call := p.Get("functionCall")
			id := call.Get("id").String()
			if id == "" {
				id = call.Get("name").String() + "#" + itoa(i)
			}
			blocks = append(blocks, Block{Kind: KindToolCall, ToolName: call.Get("name").String(), ToolCallID: id, Text: canonicalJSON(call.Get("args"))})
		case p.Get("functionResponse").Exists():
			resp := p.Get("functionResponse")
			id := resp.Get("id").String()
			if id == "" {
				id = resp.Get("name").String() + "#" + itoa(i)
			}
			blocks = append(blocks, Block{Kind: KindToolResult, ToolName: resp.Get("name").String(), ToolCallID: id, Text: canonicalJSON(resp.Get("response"))})
		case p.Get("thought").Bool():
			blocks = append(blocks, Block{Kind: KindThinking, Text: p.Get("text").String()})
		case p.Get("text").Exists():
			blocks = append(blocks, Block{Kind: textKind, Text: p.Get("text").String()})
		case p.Get("inlineData").Exists() || p.Get("fileData").Exists():
			blocks = append(blocks, Block{Kind: textKind, Text: "[file]"})
		}
	}
	return blocks
}

func firstString(r ojson.Result, keys ...string) string {
	for _, key := range keys {
		if v := r.Get(key); v.Exists() && v.String() != "" {
			return v.String()
		}
	}
	return ""
}
