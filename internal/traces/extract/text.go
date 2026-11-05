package extract

import (
	"regexp"
	"strings"
	"unicode/utf8"
)

// MaxTitleLength is the longest session title, in characters.
const MaxTitleLength = 120

var (
	// Client-injected wrappers that are not part of what the user typed.
	reWrapperBlocks = regexp.MustCompile(`(?s)<(system-reminder|environment_context|user_instructions|INSTRUCTIONS|permissions instructions|local-command-stdout|local-command-stderr|command-message|user-prompt-submit-hook)>.*?</(?:system-reminder|environment_context|user_instructions|INSTRUCTIONS|permissions instructions|local-command-stdout|local-command-stderr|command-message|user-prompt-submit-hook)>`)
	reCommandName   = regexp.MustCompile(`(?s)<command-name>(.*?)</command-name>`)
	reCommandArgs   = regexp.MustCompile(`(?s)<command-args>(.*?)</command-args>`)
)

// compactionPrefix starts the first prompt of a session that the client
// continued after it ran out of context. It is a summary, not a user prompt.
const compactionPrefix = "This session is being continued from a previous conversation"

// injectedPrefixes start user text that clients inject. It is context, not a
// prompt the user typed.
var injectedPrefixes = []string{
	"SessionStart hook additional context",
	"# Environment update",
}

// IsCompactionText reports whether text is the summary prompt that a client
// sends after it compacted a conversation.
func IsCompactionText(text string) bool {
	return strings.HasPrefix(strings.TrimSpace(text), compactionPrefix)
}

// Title returns the first line of the first user prompt, without client
// wrappers, cut to MaxTitleLength characters. Compaction summaries and
// injected context never become the title.
func Title(messages []Message) string {
	for _, msg := range messages {
		for _, b := range msg.Blocks {
			if b.Kind != KindUser {
				continue
			}
			if title := titleFromText(b.Text); title != "" {
				return title
			}
		}
	}
	return ""
}

func titleFromText(text string) string {
	cleaned := reWrapperBlocks.ReplaceAllString(text, "")
	if m := reCommandName.FindStringSubmatch(cleaned); m != nil {
		// A slash command: show the command and its arguments.
		command := strings.TrimSpace(m[1])
		if args := reCommandArgs.FindStringSubmatch(cleaned); args != nil {
			command = strings.TrimSpace(command + " " + strings.TrimSpace(args[1]))
		}
		cleaned = command
	}
	trimmed := strings.TrimSpace(cleaned)
	if trimmed == "" || trimmed == "[image]" || strings.HasPrefix(trimmed, "# AGENTS.md instructions") || IsCompactionText(trimmed) {
		return ""
	}
	for _, prefix := range injectedPrefixes {
		if strings.HasPrefix(trimmed, prefix) {
			return ""
		}
	}
	for _, line := range strings.Split(trimmed, "\n") {
		line = strings.Join(strings.Fields(line), " ")
		if line == "" {
			continue
		}
		return truncateRunes(line, MaxTitleLength)
	}
	return ""
}

func truncateRunes(s string, n int) string {
	if utf8.RuneCountInString(s) <= n {
		return s
	}
	runes := []rune(s)
	return string(runes[:n])
}

type redaction struct {
	kind string
	re   *regexp.Regexp
}

// redactions run in order, so the specific Anthropic pattern wins over the
// generic "sk-" pattern.
var redactions = []redaction{
	{"private-key", regexp.MustCompile(`-----BEGIN [A-Z ]*PRIVATE KEY-----[\s\S]*?-----END [A-Z ]*PRIVATE KEY-----`)},
	{"anthropic-key", regexp.MustCompile(`sk-ant-[A-Za-z0-9_-]{20,}`)},
	{"api-key", regexp.MustCompile(`sk-[A-Za-z0-9_-]{20,}`)},
	{"github-token", regexp.MustCompile(`ghp_[A-Za-z0-9]{36}`)},
	{"github-pat", regexp.MustCompile(`github_pat_[A-Za-z0-9_]{40,}`)},
	{"aws-key", regexp.MustCompile(`AKIA[0-9A-Z]{16}`)},
	{"bearer", regexp.MustCompile(`(?i)bearer [A-Za-z0-9._~+/-]{20,}=*`)},
}

// Redact replaces known secret patterns with "[redacted:<kind>]".
func Redact(s string) string {
	if s == "" {
		return s
	}
	for _, r := range redactions {
		if r.re.MatchString(s) {
			s = r.re.ReplaceAllString(s, "[redacted:"+r.kind+"]")
		}
	}
	return s
}

// RedactMessage redacts every text field of m.
func RedactMessage(m Message) Message {
	out := Message{Role: m.Role, Blocks: make([]Block, len(m.Blocks))}
	for i, b := range m.Blocks {
		b.Text = Redact(b.Text)
		out.Blocks[i] = b
	}
	return out
}
