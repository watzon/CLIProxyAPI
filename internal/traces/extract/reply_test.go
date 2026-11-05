package extract

import (
	"strings"
	"testing"
)

func kinds(msgs []Message) string {
	var out []string
	for _, m := range msgs {
		for _, b := range m.Blocks {
			out = append(out, b.Kind)
		}
	}
	return strings.Join(out, ",")
}

func TestParseReplyPerFormat(t *testing.T) {
	tests := []struct {
		name, format, kind, contentType, body string
		wantKinds, wantStop, wantTools        string
	}{
		{
			name: "claude sse", format: "claude", contentType: "text/event-stream",
			body: "event: message_start\ndata: {\"type\":\"message_start\",\"message\":{\"model\":\"m\"}}\n\n" +
				": keep-alive\n\n" +
				"data: {\"type\":\"content_block_start\",\"index\":0,\"content_block\":{\"type\":\"thinking\",\"thinking\":\"\"}}\n\n" +
				"data: {\"type\":\"content_block_delta\",\"index\":0,\"delta\":{\"type\":\"thinking_delta\",\"thinking\":\"hmm\"}}\n\n" +
				"data: {\"type\":\"content_block_start\",\"index\":1,\"content_block\":{\"type\":\"text\",\"text\":\"\"}}\n\n" +
				"data: {\"type\":\"content_block_delta\",\"index\":1,\"delta\":{\"type\":\"text_delta\",\"text\":\"Hi\"}}\n\n" +
				"data: {\"type\":\"content_block_start\",\"index\":2,\"content_block\":{\"type\":\"tool_use\",\"id\":\"t1\",\"name\":\"Bash\",\"input\":{}}}\n\n" +
				"data: {\"type\":\"content_block_delta\",\"index\":2,\"delta\":{\"type\":\"input_json_delta\",\"partial_json\":\"{\\\"command\\\":\"}}\n\n" +
				"data: {\"type\":\"content_block_delta\",\"index\":2,\"delta\":{\"type\":\"input_json_delta\",\"partial_json\":\"\\\"ls\\\"}\"}}\n\n" +
				"data: {\"type\":\"message_delta\",\"delta\":{\"stop_reason\":\"tool_use\"}}\n\n",
			wantKinds: "thinking,assistant,tool_call", wantStop: "tool_use", wantTools: "Bash",
		},
		{
			name: "claude json", format: "claude", contentType: "application/json",
			body:      `{"type":"message","model":"m","stop_reason":"end_turn","content":[{"type":"text","text":"ok"}]}`,
			wantKinds: "assistant", wantStop: "end_turn",
		},
		{
			name: "chat sse", format: "openai", contentType: "text/event-stream",
			body: "data: {\"choices\":[{\"delta\":{\"reasoning_content\":\"r\"}}]}\n\n" +
				"data: {\"choices\":[{\"delta\":{\"content\":\"Hel\"}}]}\n\n" +
				"data: {\"choices\":[{\"delta\":{\"content\":\"lo\",\"tool_calls\":[{\"index\":0,\"id\":\"c1\",\"function\":{\"name\":\"read\",\"arguments\":\"{\\\"p\\\":\"}}]}}]}\n\n" +
				"data: {\"choices\":[{\"delta\":{\"tool_calls\":[{\"index\":0,\"function\":{\"arguments\":\"1}\"}}]},\"finish_reason\":\"tool_calls\"}]}\n\n" +
				"data: [DONE]\n\n",
			wantKinds: "thinking,assistant,tool_call", wantStop: "tool_calls", wantTools: "read",
		},
		{
			name: "chat json with keep-alive padding", format: "openai", contentType: "application/json",
			body:      "\n\n" + `{"choices":[{"message":{"content":"x","tool_calls":[{"id":"c","function":{"name":"f","arguments":"{}"}}]},"finish_reason":"stop"}]}`,
			wantKinds: "assistant,tool_call", wantStop: "stop", wantTools: "f",
		},
		{
			name: "responses sse completed", format: "openai-response", contentType: "text/event-stream",
			body: "event: response.output_text.delta\ndata: {\"type\":\"response.output_text.delta\",\"delta\":\"partial\"}\n\n" +
				"event: response.completed\ndata: {\"type\":\"response.completed\",\"response\":{\"status\":\"completed\",\"output\":[" +
				"{\"type\":\"reasoning\",\"summary\":[{\"text\":\"plan\"}]},{\"type\":\"message\",\"role\":\"assistant\",\"content\":[{\"type\":\"output_text\",\"text\":\"done\"}]}," +
				"{\"type\":\"custom_tool_call\",\"call_id\":\"c\",\"name\":\"apply_patch\",\"input\":\"*** Begin Patch\"}]}}\n\n",
			wantKinds: "thinking,assistant,tool_call", wantStop: "completed", wantTools: "apply_patch",
		},
		{
			name: "responses sse text only", format: "openai-response", contentType: "text/event-stream",
			body:      "data: {\"type\":\"response.output_text.delta\",\"delta\":\"a\"}\n\ndata: {\"type\":\"response.output_text.delta\",\"delta\":\"b\"}\n\n",
			wantKinds: "assistant",
		},
		{
			name: "responses websocket output", format: "openai-response", kind: "responses-output",
			body:      `[{"type":"function_call","call_id":"c","name":"shell","arguments":"{\"command\":[\"ls\"]}"}]`,
			wantKinds: "tool_call", wantTools: "shell",
		},
		{
			name: "gemini sse", format: "gemini", contentType: "text/event-stream",
			body: "data: {\"candidates\":[{\"content\":{\"parts\":[{\"text\":\"He\"}]}}]}\n\n" +
				"data: {\"candidates\":[{\"content\":{\"parts\":[{\"text\":\"llo\"},{\"functionCall\":{\"name\":\"f\",\"args\":{\"a\":1}}}]},\"finishReason\":\"STOP\"}]}\n\n",
			wantKinds: "assistant,tool_call", wantStop: "STOP", wantTools: "f",
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			kind := tc.kind
			if kind == "" {
				kind = "http"
			}
			r := ParseReply(tc.format, kind, tc.contentType, []byte(tc.body))
			if got := kinds(r.Messages); got != tc.wantKinds {
				t.Errorf("kinds = %q, want %q (%+v)", got, tc.wantKinds, r.Messages)
			}
			if r.StopReason != tc.wantStop {
				t.Errorf("stop = %q, want %q", r.StopReason, tc.wantStop)
			}
			if got := strings.Join(r.ToolNames(), ","); got != tc.wantTools {
				t.Errorf("tools = %q, want %q", got, tc.wantTools)
			}
		})
	}
}

// The reply rebuilt from a stream must hash like the same assistant message
// resent by the client, so a resent conversation stores each message once.
func TestReplyMatchesResentAssistantMessage(t *testing.T) {
	stream := "data: {\"type\":\"content_block_start\",\"index\":0,\"content_block\":{\"type\":\"text\",\"text\":\"\"}}\n\n" +
		"data: {\"type\":\"content_block_delta\",\"index\":0,\"delta\":{\"type\":\"text_delta\",\"text\":\"Edit it\"}}\n\n" +
		"data: {\"type\":\"content_block_start\",\"index\":1,\"content_block\":{\"type\":\"tool_use\",\"id\":\"t\",\"name\":\"Edit\",\"input\":{}}}\n\n" +
		"data: {\"type\":\"content_block_delta\",\"index\":1,\"delta\":{\"type\":\"input_json_delta\",\"partial_json\":\"{\\\"new_string\\\":\\\"b\\\",\\\"file_path\\\":\\\"f\\\"}\"}}\n\n"
	reply := ParseReply("claude", "http", "text/event-stream", []byte(stream))
	req := ParseRequest("claude", []byte(`{"messages":[{"role":"assistant","content":[{"type":"text","text":"Edit it","cache_control":{"type":"ephemeral"}},{"type":"tool_use","id":"t","name":"Edit","input":{"file_path":"f","new_string":"b"}}]}]}`))
	if len(reply.Messages) != 1 || len(req.Messages) != 1 {
		t.Fatalf("messages: reply %d request %d", len(reply.Messages), len(req.Messages))
	}
	if reply.Messages[0].Hash() != req.Messages[0].Hash() {
		t.Errorf("hash mismatch:\nreply   %s\nrequest %s", reply.Messages[0].Canonical(), req.Messages[0].Canonical())
	}
}

func TestErrorText(t *testing.T) {
	for body, want := range map[string]string{
		`{"error":{"type":"rate_limit_error","message":"slow down"}}`: "slow down",
		`{"error":"bad key"}`: "bad key",
		"event: error\ndata: {\"type\":\"error\",\"error\":{\"message\":\"overloaded\"}}\n\n": "overloaded",
		"plain failure": "plain failure",
	} {
		if got := ErrorText([]byte(body)); got != want {
			t.Errorf("ErrorText(%q) = %q, want %q", body, got, want)
		}
	}
}

func TestParseRequestFormats(t *testing.T) {
	chat := ParseRequest("openai", []byte(`{"messages":[{"role":"developer","content":"rules"},{"role":"user","content":[{"type":"text","text":"q"},{"type":"image_url"}]},{"role":"assistant","tool_calls":[{"id":"c","function":{"name":"f","arguments":"{\"b\":2,\"a\":1}"}}]},{"role":"tool","tool_call_id":"c","content":"out"}],"tools":[{"type":"function"}]}`))
	if got := kinds(chat.Messages); got != "system,user,tool_call,tool_result" {
		t.Errorf("chat kinds = %q", got)
	}
	if chat.Messages[2].Blocks[0].Text != `{"a":1,"b":2}` {
		t.Errorf("arguments are not canonical: %s", chat.Messages[2].Blocks[0].Text)
	}
	if chat.SystemText() != "rules" || chat.Tools == "" {
		t.Errorf("system %q tools %q", chat.SystemText(), chat.Tools)
	}
	gem := ParseRequest("gemini", []byte(`{"request":{"systemInstruction":{"parts":[{"text":"sys"}]},"contents":[{"role":"user","parts":[{"text":"hi"}]},{"role":"model","parts":[{"thought":true,"text":"t"},{"functionCall":{"name":"f","args":{}}}]},{"role":"function","parts":[{"functionResponse":{"name":"f","response":{"ok":true}}}]}]}}`))
	if got := kinds(gem.Messages); got != "user,thinking,tool_call,tool_result" || gem.SystemText() != "sys" {
		t.Errorf("gemini kinds = %q system %q", got, gem.SystemText())
	}
	resp := ParseRequest("openai-response", []byte(`{"instructions":"be brief","input":[{"role":"user","content":"q"},{"type":"function_call_output","call_id":"c","output":"Exit code: 1\nboom"}]}`))
	if got := kinds(resp.Messages); got != "user,tool_result" {
		t.Errorf("responses kinds = %q", got)
	}
	if !ResultFailed(resp.Messages[1].Blocks[0].Text, false) {
		t.Errorf("non-zero exit code was not detected as a failure")
	}
	if ResultFailed("Exit code: 0\nok", false) || !ResultFailed("anything", true) {
		t.Errorf("ResultFailed misclassified")
	}
	if p := ParseRequest("claude", []byte("not json")); len(p.Messages) != 0 {
		t.Errorf("invalid body produced messages")
	}
}

func TestDiffFromToolCall(t *testing.T) {
	tests := []struct {
		name, tool, input string
		file              string
		added, removed    int
		patch             bool
	}{
		{"claude edit", "Edit", `{"file_path":"a.go","old_string":"x\ny\nz","new_string":"x\nY\nz\nw"}`, "a.go", 2, 1, false},
		{"claude write", "Write", `{"file_path":"n.txt","content":"1\n2\n3\n"}`, "n.txt", 3, 0, false},
		{"claude multiedit", "MultiEdit", `{"file_path":"m.go","edits":[{"old_string":"a","new_string":"b"},{"old_string":"c","new_string":"d\ne"}]}`, "m.go", 3, 2, true},
		{"notebook", "NotebookEdit", `{"notebook_path":"nb.ipynb","new_source":"print(1)"}`, "nb.ipynb", 1, 0, false},
		{"codex apply_patch function", "apply_patch", `{"input":"*** Begin Patch\n*** Update File: src/x.rs\n@@\n-old\n+new\n+more\n*** End Patch"}`, "src/x.rs", 2, 1, true},
		{"codex apply_patch custom", "apply_patch", "*** Begin Patch\n*** Add File: a.md\n+hi\n*** Add File: b.md\n+yo\n*** End Patch", "a.md, b.md", 2, 0, true},
		{"codex shell heredoc", "shell", `{"command":["apply_patch","*** Begin Patch\n*** Delete File: gone.txt\n*** End Patch"]}`, "gone.txt", 0, 0, true},
		{"opencode edit", "edit", `{"filePath":"o.ts","oldString":"a","newString":"b"}`, "o.ts", 1, 1, false},
		{"opencode write", "write", `{"filePath":"w.ts","content":"x"}`, "w.ts", 1, 0, false},
		{"opencode patch", "patch", `{"patchText":"--- a/p.go\n+++ b/p.go\n@@\n-a\n+b"}`, "p.go", 1, 1, true},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			d := DiffFromToolCall(tc.tool, tc.input)
			if d == nil {
				t.Fatal("no diff")
			}
			if d.File != tc.file || d.Added != tc.added || d.Removed != tc.removed || (d.Patch != "") != tc.patch {
				t.Errorf("diff = %+v", d)
			}
		})
	}
	for _, tool := range []string{"Read", "Bash", "grep"} {
		if d := DiffFromToolCall(tool, `{"file_path":"x"}`); d != nil {
			t.Errorf("%s produced a diff", tool)
		}
	}
}
