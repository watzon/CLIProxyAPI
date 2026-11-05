package extract

import (
	"sort"
	"strings"

	"github.com/MoeForge/moego/ojson"
)

// Reply is the normalized assistant reply of one turn.
type Reply struct {
	Messages   []Message
	StopReason string
	Model      string
	ErrorText  string
}

// ToolNames lists the tool calls of the reply in order.
func (r *Reply) ToolNames() []string {
	var names []string
	if r == nil {
		return names
	}
	for _, m := range r.Messages {
		for _, b := range m.Blocks {
			if b.Kind == KindToolCall && b.ToolName != "" {
				names = append(names, b.ToolName)
			}
		}
	}
	return names
}

// ParseReply rebuilds the reply from what was written to the client.
// kind "responses-output" means body is a Responses output array.
func ParseReply(format, kind, contentType string, body []byte) *Reply {
	reply := &Reply{}
	if len(body) == 0 {
		return reply
	}
	if kind == "responses-output" {
		parseResponsesOutput(reply, ojson.ParseBytes(body))
		return reply
	}
	events := sseEvents(body)
	isSSE := strings.Contains(strings.ToLower(contentType), "event-stream") || (len(events) > 0 && !ojson.ValidBytes(trimSpaceBytes(body)))
	switch detectReplyFormat(format, body, isSSE) {
	case "claude":
		if isSSE {
			parseClaudeStream(reply, events)
		} else {
			parseClaudeMessage(reply, ojson.ParseBytes(trimSpaceBytes(body)))
		}
	case "openai":
		if isSSE {
			parseChatStream(reply, events)
		} else {
			parseChatMessage(reply, ojson.ParseBytes(trimSpaceBytes(body)))
		}
	case "openai-response":
		if isSSE {
			parseResponsesStream(reply, events)
		} else {
			root := ojson.ParseBytes(trimSpaceBytes(body))
			if r := root.Get("response"); r.IsObject() {
				root = r
			}
			parseResponsesObject(reply, root)
		}
	case "gemini":
		if isSSE {
			for _, ev := range events {
				parseGeminiChunk(reply, ojson.Parse(ev.data))
			}
		} else {
			root := ojson.ParseBytes(trimSpaceBytes(body))
			if root.IsArray() {
				for _, chunk := range root.Array() {
					parseGeminiChunk(reply, chunk)
				}
			} else {
				parseGeminiChunk(reply, root)
			}
		}
		mergeGeminiText(reply)
	}
	if reply.ErrorText == "" && !isSSE {
		reply.ErrorText = errorTextOf(ojson.ParseBytes(trimSpaceBytes(body)))
	}
	return reply
}

// ErrorText extracts a readable error message from an error body.
func ErrorText(body []byte) string {
	body = trimSpaceBytes(body)
	if len(body) == 0 {
		return ""
	}
	if ojson.ValidBytes(body) {
		if text := errorTextOf(ojson.ParseBytes(body)); text != "" {
			return text
		}
	}
	for _, ev := range sseEvents(body) {
		if text := errorTextOf(ojson.Parse(ev.data)); text != "" {
			return text
		}
	}
	text := strings.TrimSpace(string(body))
	if len(text) > 500 {
		text = text[:500]
	}
	return text
}

func errorTextOf(root ojson.Result) string {
	if !root.IsObject() {
		return ""
	}
	if e := root.Get("error"); e.Exists() {
		if e.Type == ojson.String {
			return e.String()
		}
		if m := e.Get("message").String(); m != "" {
			return m
		}
	}
	if r := root.Get("response.error.message").String(); r != "" {
		return r
	}
	if root.Get("type").String() == "error" {
		return root.Get("message").String()
	}
	return ""
}

func detectReplyFormat(format string, body []byte, isSSE bool) string {
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
	text := string(body)
	switch {
	case strings.Contains(text, "message_start") || strings.Contains(text, `"type":"message"`):
		return "claude"
	case strings.Contains(text, "response.created") || strings.Contains(text, `"object":"response"`):
		return "openai-response"
	case strings.Contains(text, "candidates"):
		return "gemini"
	case strings.Contains(text, "choices"):
		return "openai"
	}
	return ""
}

type sseEvent struct {
	event string
	data  string
}

// sseEvents splits an SSE body into events. Keep-alive comments are skipped.
func sseEvents(body []byte) []sseEvent {
	var out []sseEvent
	var cur sseEvent
	var data []string
	flush := func() {
		if len(data) > 0 {
			cur.data = strings.Join(data, "\n")
			out = append(out, cur)
		}
		cur = sseEvent{}
		data = nil
	}
	for _, line := range strings.Split(string(body), "\n") {
		line = strings.TrimRight(line, "\r")
		switch {
		case line == "":
			flush()
		case strings.HasPrefix(line, ":"):
		case strings.HasPrefix(line, "event:"):
			cur.event = strings.TrimSpace(strings.TrimPrefix(line, "event:"))
		case strings.HasPrefix(line, "data:"):
			data = append(data, strings.TrimPrefix(strings.TrimPrefix(line, "data:"), " "))
		}
	}
	flush()
	return out
}

func trimSpaceBytes(b []byte) []byte {
	return []byte(strings.TrimSpace(string(b)))
}

// --- Claude Messages ---

func parseClaudeMessage(reply *Reply, root ojson.Result) {
	if root.Get("type").String() == "error" {
		reply.ErrorText = errorTextOf(root)
		return
	}
	reply.Model = root.Get("model").String()
	reply.StopReason = root.Get("stop_reason").String()
	unit := Message{Role: "assistant", Blocks: claudeBlocks("assistant", root.Get("content"))}
	if !unit.Empty() {
		reply.Messages = append(reply.Messages, unit)
	}
}

func parseClaudeStream(reply *Reply, events []sseEvent) {
	type partial struct {
		block Block
		input strings.Builder
	}
	blocks := map[int64]*partial{}
	var order []int64
	for _, ev := range events {
		data := ojson.Parse(ev.data)
		switch data.Get("type").String() {
		case "message_start":
			reply.Model = data.Get("message.model").String()
		case "content_block_start":
			idx := data.Get("index").Int()
			cb := data.Get("content_block")
			p := &partial{}
			switch typ := cb.Get("type").String(); {
			case typ == "text":
				p.block = Block{Kind: KindAssistant, Text: cb.Get("text").String()}
			case typ == "thinking":
				p.block = Block{Kind: KindThinking, Text: cb.Get("thinking").String()}
			case typ == "redacted_thinking":
				p.block = Block{Kind: KindThinking, Text: "[redacted thinking]"}
			case typ == "tool_use" || typ == "server_tool_use" || typ == "mcp_tool_use":
				p.block = Block{Kind: KindToolCall, ToolName: cb.Get("name").String(), ToolCallID: cb.Get("id").String()}
				if in := cb.Get("input"); in.IsObject() && len(in.Map()) > 0 {
					p.input.WriteString(in.Raw)
				}
			case strings.HasSuffix(typ, "_tool_result"):
				p.block = Block{Kind: KindToolResult, ToolCallID: cb.Get("tool_use_id").String(), Text: textOf(cb.Get("content"))}
			default:
				continue
			}
			if _, seen := blocks[idx]; !seen {
				order = append(order, idx)
			}
			blocks[idx] = p
		case "content_block_delta":
			p := blocks[data.Get("index").Int()]
			if p == nil {
				continue
			}
			delta := data.Get("delta")
			switch delta.Get("type").String() {
			case "text_delta":
				p.block.Text += delta.Get("text").String()
			case "thinking_delta":
				p.block.Text += delta.Get("thinking").String()
			case "input_json_delta":
				p.input.WriteString(delta.Get("partial_json").String())
			}
		case "message_delta":
			if sr := data.Get("delta.stop_reason").String(); sr != "" {
				reply.StopReason = sr
			}
		case "error":
			reply.ErrorText = errorTextOf(data)
		}
	}
	unit := Message{Role: "assistant"}
	for _, idx := range order {
		p := blocks[idx]
		if p.block.Kind == KindToolCall {
			p.block.Text = canonicalArguments(p.input.String())
			if p.block.Text == "" {
				p.block.Text = "{}"
			}
		}
		unit.Blocks = append(unit.Blocks, p.block)
	}
	if !unit.Empty() {
		reply.Messages = append(reply.Messages, unit)
	}
}

// --- OpenAI Chat Completions ---

func parseChatMessage(reply *Reply, root ojson.Result) {
	reply.Model = root.Get("model").String()
	choice := root.Get("choices.0")
	reply.StopReason = choice.Get("finish_reason").String()
	msg := choice.Get("message")
	unit := Message{Role: "assistant"}
	if r := firstString(msg, "reasoning_content", "reasoning"); r != "" {
		unit.Blocks = append(unit.Blocks, Block{Kind: KindThinking, Text: r})
	}
	if text := textOf(msg.Get("content")); text != "" {
		unit.Blocks = append(unit.Blocks, Block{Kind: KindAssistant, Text: text})
	}
	for _, call := range msg.Get("tool_calls").Array() {
		unit.Blocks = append(unit.Blocks, Block{
			Kind:       KindToolCall,
			ToolName:   call.Get("function.name").String(),
			ToolCallID: call.Get("id").String(),
			Text:       canonicalArguments(call.Get("function.arguments").String()),
		})
	}
	if !unit.Empty() {
		reply.Messages = append(reply.Messages, unit)
	}
}

func parseChatStream(reply *Reply, events []sseEvent) {
	var reasoning, content strings.Builder
	type call struct {
		id, name string
		args     strings.Builder
	}
	calls := map[int64]*call{}
	var order []int64
	for _, ev := range events {
		if strings.TrimSpace(ev.data) == "[DONE]" {
			continue
		}
		data := ojson.Parse(ev.data)
		if e := errorTextOf(data); e != "" {
			reply.ErrorText = e
		}
		if m := data.Get("model").String(); m != "" {
			reply.Model = m
		}
		choice := data.Get("choices.0")
		if fr := choice.Get("finish_reason").String(); fr != "" {
			reply.StopReason = fr
		}
		delta := choice.Get("delta")
		reasoning.WriteString(firstString(delta, "reasoning_content", "reasoning"))
		content.WriteString(delta.Get("content").String())
		for _, tc := range delta.Get("tool_calls").Array() {
			idx := tc.Get("index").Int()
			c := calls[idx]
			if c == nil {
				c = &call{}
				calls[idx] = c
				order = append(order, idx)
			}
			if id := tc.Get("id").String(); id != "" {
				c.id = id
			}
			if name := tc.Get("function.name").String(); name != "" {
				c.name = name
			}
			c.args.WriteString(tc.Get("function.arguments").String())
		}
	}
	unit := Message{Role: "assistant"}
	if reasoning.Len() > 0 {
		unit.Blocks = append(unit.Blocks, Block{Kind: KindThinking, Text: reasoning.String()})
	}
	if content.Len() > 0 {
		unit.Blocks = append(unit.Blocks, Block{Kind: KindAssistant, Text: content.String()})
	}
	for _, idx := range order {
		c := calls[idx]
		unit.Blocks = append(unit.Blocks, Block{Kind: KindToolCall, ToolName: c.name, ToolCallID: c.id, Text: canonicalArguments(c.args.String())})
	}
	if !unit.Empty() {
		reply.Messages = append(reply.Messages, unit)
	}
}

// --- OpenAI Responses ---

func parseResponsesObject(reply *Reply, resp ojson.Result) {
	reply.Model = resp.Get("model").String()
	reply.StopReason = resp.Get("status").String()
	if reason := resp.Get("incomplete_details.reason").String(); reason != "" {
		reply.StopReason = reason
	}
	if e := resp.Get("error.message").String(); e != "" {
		reply.ErrorText = e
	}
	parseResponsesOutput(reply, resp.Get("output"))
}

func parseResponsesOutput(reply *Reply, output ojson.Result) {
	for _, item := range output.Array() {
		if unit, ok := responsesItem(item); ok && !unit.Empty() {
			reply.Messages = append(reply.Messages, unit)
		}
	}
}

func parseResponsesStream(reply *Reply, events []sseEvent) {
	items := map[int64]ojson.Result{}
	var text strings.Builder
	for _, ev := range events {
		data := ojson.Parse(ev.data)
		typ := data.Get("type").String()
		if typ == "" {
			typ = ev.event
		}
		switch typ {
		case "response.completed", "response.incomplete", "response.failed", "response.done":
			resp := data.Get("response")
			if resp.Get("output").IsArray() && len(resp.Get("output").Array()) > 0 {
				*reply = Reply{}
				parseResponsesObject(reply, resp)
				return
			}
			reply.Model = resp.Get("model").String()
			reply.StopReason = resp.Get("status").String()
			if e := resp.Get("error.message").String(); e != "" {
				reply.ErrorText = e
			}
		case "response.output_item.done":
			items[data.Get("output_index").Int()] = data.Get("item")
		case "response.output_text.delta":
			text.WriteString(data.Get("delta").String())
		case "error":
			reply.ErrorText = errorTextOf(data)
			if reply.ErrorText == "" {
				reply.ErrorText = data.Get("message").String()
			}
		}
	}
	if len(items) > 0 {
		keys := make([]int64, 0, len(items))
		for k := range items {
			keys = append(keys, k)
		}
		sort.Slice(keys, func(i, j int) bool { return keys[i] < keys[j] })
		for _, k := range keys {
			if unit, ok := responsesItem(items[k]); ok && !unit.Empty() {
				reply.Messages = append(reply.Messages, unit)
			}
		}
		return
	}
	if text.Len() > 0 {
		reply.Messages = append(reply.Messages, Message{Role: "assistant", Blocks: []Block{{Kind: KindAssistant, Text: text.String()}}})
	}
}

// --- Gemini ---

func parseGeminiChunk(reply *Reply, root ojson.Result) {
	if root.Get("response").IsObject() {
		root = root.Get("response")
	}
	if e := errorTextOf(root); e != "" {
		reply.ErrorText = e
	}
	if m := root.Get("modelVersion").String(); m != "" {
		reply.Model = m
	}
	cand := root.Get("candidates.0")
	if fr := cand.Get("finishReason").String(); fr != "" {
		reply.StopReason = fr
	}
	blocks := geminiBlocks("model", cand.Get("content.parts"))
	if len(blocks) == 0 {
		return
	}
	if len(reply.Messages) == 0 {
		reply.Messages = append(reply.Messages, Message{Role: "assistant"})
	}
	reply.Messages[0].Blocks = append(reply.Messages[0].Blocks, blocks...)
}

// mergeGeminiText joins adjacent text blocks of the same kind that streaming split.
func mergeGeminiText(reply *Reply) {
	for i := range reply.Messages {
		var merged []Block
		for _, b := range reply.Messages[i].Blocks {
			n := len(merged)
			if n > 0 && (b.Kind == KindAssistant || b.Kind == KindThinking) && merged[n-1].Kind == b.Kind {
				merged[n-1].Text += b.Text
				continue
			}
			merged = append(merged, b)
		}
		reply.Messages[i].Blocks = merged
	}
}
