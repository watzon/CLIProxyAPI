// Package extract turns client requests and replies of every supported wire
// format into one normalized conversation model, and derives trace metadata
// (client, folder, title, diffs) from it.
package extract

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"sort"
	"strconv"
	"strings"

	"github.com/MoeForge/moego/ojson"
)

// Block kinds.
const (
	KindUser       = "user"
	KindAssistant  = "assistant"
	KindThinking   = "thinking"
	KindToolCall   = "tool_call"
	KindToolResult = "tool_result"
	KindSystem     = "system"
)

// Block is one normalized piece of conversation content. For tool calls, Text
// holds the call input (canonical JSON, or raw text for free-form tools).
type Block struct {
	Kind       string `json:"kind"`
	Text       string `json:"text,omitempty"`
	ToolName   string `json:"tool_name,omitempty"`
	ToolCallID string `json:"tool_call_id,omitempty"`
	IsError    bool   `json:"is_error,omitempty"`
}

// Message is one content unit. Each unit is hashed and stored once.
type Message struct {
	Role   string  `json:"role"`
	Blocks []Block `json:"blocks"`
}

// Canonical returns the deterministic JSON encoding used for hashing.
func (m Message) Canonical() []byte {
	data, err := json.Marshal(m)
	if err != nil {
		return nil
	}
	return data
}

// Hash returns the SHA-256 of the canonical encoding, hex encoded.
func (m Message) Hash() string {
	sum := sha256.Sum256(m.Canonical())
	return hex.EncodeToString(sum[:])
}

// Empty reports whether the unit carries no content.
func (m Message) Empty() bool {
	for _, b := range m.Blocks {
		if b.Text != "" || b.ToolName != "" || b.ToolCallID != "" {
			return false
		}
	}
	return true
}

// canonicalJSON renders a JSON value with sorted object keys and no spaces.
func canonicalJSON(r ojson.Result) string {
	var sb strings.Builder
	writeCanonical(&sb, r)
	return sb.String()
}

func writeCanonical(sb *strings.Builder, r ojson.Result) {
	switch {
	case r.IsObject():
		type kv struct {
			key   string
			value ojson.Result
		}
		var items []kv
		r.ForEach(func(key, value ojson.Result) bool {
			items = append(items, kv{key: key.String(), value: value})
			return true
		})
		sort.SliceStable(items, func(i, j int) bool { return items[i].key < items[j].key })
		sb.WriteByte('{')
		for i, item := range items {
			if i > 0 {
				sb.WriteByte(',')
			}
			sb.Write(ojson.AppendJSONString(nil, item.key))
			sb.WriteByte(':')
			writeCanonical(sb, item.value)
		}
		sb.WriteByte('}')
	case r.IsArray():
		sb.WriteByte('[')
		for i, item := range r.Array() {
			if i > 0 {
				sb.WriteByte(',')
			}
			writeCanonical(sb, item)
		}
		sb.WriteByte(']')
	case r.Type == ojson.String:
		sb.Write(ojson.AppendJSONString(nil, r.String()))
	case r.Type == ojson.Number:
		sb.WriteString(strings.TrimSpace(r.Raw))
	case r.Type == ojson.True:
		sb.WriteString("true")
	case r.Type == ojson.False:
		sb.WriteString("false")
	default:
		sb.WriteString("null")
	}
}

// canonicalArguments normalizes a tool argument string: valid JSON is
// re-encoded canonically, anything else is kept as text.
func canonicalArguments(raw string) string {
	trimmed := strings.TrimSpace(raw)
	if trimmed == "" {
		return ""
	}
	if ojson.Valid(trimmed) {
		return canonicalJSON(ojson.Parse(trimmed))
	}
	return raw
}

// textOf flattens a content value (string, block list, or object with text)
// into plain text.
func textOf(r ojson.Result) string {
	switch {
	case !r.Exists():
		return ""
	case r.Type == ojson.String:
		return r.String()
	case r.IsArray():
		var parts []string
		for _, item := range r.Array() {
			if t := textOf(item); t != "" {
				parts = append(parts, t)
			}
		}
		return strings.Join(parts, "\n")
	case r.IsObject():
		for _, key := range []string{"text", "content", "output", "thinking"} {
			if v := r.Get(key); v.Exists() {
				return textOf(v)
			}
		}
		if t := r.Get("type").String(); strings.Contains(t, "image") {
			return "[image]"
		}
		return ""
	default:
		return r.String()
	}
}

func itoa(n int) string { return strconv.Itoa(n) }
