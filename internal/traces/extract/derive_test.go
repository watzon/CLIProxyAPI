package extract

import (
	"net/http"
	"testing"
)

func hdr(kv ...string) http.Header {
	h := http.Header{}
	for i := 0; i+1 < len(kv); i += 2 {
		h.Set(kv[i], kv[i+1])
	}
	return h
}

func TestDeriveFolderPerClient(t *testing.T) {
	tests := []struct {
		name        string
		format      string
		headers     http.Header
		body        string
		wantClient  string
		wantVersion string
		wantFolder  string
		wantRemote  string
		wantHash    string
		wantDevice  string
	}{
		{
			name:        "claude code system prompt",
			format:      "claude",
			headers:     hdr("User-Agent", "claude-cli/2.1.318 (external, cli)"),
			body:        `{"system":[{"type":"text","text":"You are Claude Code.\nPrimary working directory: /Users/alice/code/app\nPlatform: darwin"}],"metadata":{"user_id":"{\"device_id\":\"dev-9\",\"session_id\":\"s\"}"},"messages":[{"role":"user","content":"hi"}]}`,
			wantClient:  ClientClaudeCode,
			wantVersion: "2.1.318",
			wantFolder:  "~/code/app",
			wantDevice:  "dev-9",
		},
		{
			name:       "claude code live cwd",
			format:     "claude",
			headers:    hdr("User-Agent", "claude-cli/2.0.1"),
			body:       `{"system":"plain","safeguards":[{"classifier_context":{"live_cwd":"/home/bob/src/x/"}}],"messages":[{"role":"user","content":"hi"}]}`,
			wantClient: ClientClaudeCode, wantVersion: "2.0.1", wantFolder: "~/src/x",
		},
		{
			name:       "omp first user message",
			format:     "openai",
			headers:    hdr("User-Agent", "omp/0.9.2"),
			body:       `{"messages":[{"role":"system","content":"sys"},{"role":"user","content":"context: current working directory: 'C:\\Users\\Carol\\work\\proj'\nhello"}]}`,
			wantClient: ClientOMP, wantVersion: "0.9.2", wantFolder: "~/work/proj",
		},
		{
			name:   "codex turn metadata header",
			format: "openai-response",
			headers: hdr("User-Agent", "codex_cli_rs/0.120.0 (Mac OS)", "Originator", "codex_cli_rs",
				"X-Codex-Turn-Metadata", `{"workspaces":{"/private/tmp/repo":{"associated_remote_urls":{"origin":"https://github.com/Acme/Repo.git"}}}}`),
			body:       `{"input":"hi"}`,
			wantClient: ClientCodex, wantVersion: "0.120.0", wantFolder: "/tmp/repo", wantRemote: "github.com/Acme/Repo",
		},
		{
			name:       "codex cwd tag",
			format:     "openai-response",
			headers:    hdr("User-Agent", "something", "Originator", "codex_exec"),
			body:       `{"input":[{"type":"message","role":"user","content":[{"type":"input_text","text":"<environment_context>\n  <cwd>D:\\src\\tool</cwd>\n</environment_context>"}]}]}`,
			wantClient: ClientCodex, wantFolder: "d:/src/tool",
		},
		{
			name:       "opencode system prompt and project hash",
			format:     "openai",
			headers:    hdr("User-Agent", "opencode/0.15.3", "X-Opencode-Project", "abc123"),
			body:       `{"messages":[{"role":"system","content":"You are opencode.\nWorking directory: /home/dan/p/q/"},{"role":"user","content":"hi"}]}`,
			wantClient: ClientOpenCode, wantVersion: "0.15.3", wantFolder: "~/p/q", wantHash: "abc123",
		},
		{
			name:       "generic client gets no folder",
			format:     "openai",
			headers:    hdr("User-Agent", "python-httpx/0.27.0"),
			body:       `{"messages":[{"role":"system","content":"Working directory: /home/eve/x"},{"role":"user","content":"hi"}]}`,
			wantClient: "python-httpx", wantVersion: "0.27.0",
		},
		{
			name:       "X-CPA-Cwd and X-CPA-Client override",
			format:     "claude",
			headers:    hdr("User-Agent", "claude-cli/1.0.0", "X-CPA-Cwd", "/Users/frank/override", "X-CPA-Client", "t3code"),
			body:       `{"system":"Primary working directory: /Users/frank/other","messages":[]}`,
			wantClient: "t3code", wantVersion: "1.0.0", wantFolder: "~/override",
		},
		{
			name:       "no user agent",
			format:     "openai",
			headers:    hdr(),
			body:       `{}`,
			wantClient: ClientUnknown,
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			d := Derive(tc.headers, ParseRequest(tc.format, []byte(tc.body)))
			if d.Client != tc.wantClient || d.ClientVersion != tc.wantVersion {
				t.Errorf("client = %q %q, want %q %q", d.Client, d.ClientVersion, tc.wantClient, tc.wantVersion)
			}
			if d.Folder != tc.wantFolder || d.GitRemote != tc.wantRemote {
				t.Errorf("folder = %q remote = %q, want %q %q", d.Folder, d.GitRemote, tc.wantFolder, tc.wantRemote)
			}
			if d.ClientProjectHash != tc.wantHash || d.DeviceID != tc.wantDevice {
				t.Errorf("hash = %q device = %q, want %q %q", d.ClientProjectHash, d.DeviceID, tc.wantHash, tc.wantDevice)
			}
		})
	}
}

func TestNormalizePath(t *testing.T) {
	cases := map[string]string{
		"/Users/alice/code/app/":      "~/code/app",
		"/home/bob":                   "~",
		`C:\Users\Carol\work\proj\`:   "~/work/proj",
		`c:\users\carol\x`:            "c:/users/carol/x",
		"c:/Users/Dave/y":             "~/y",
		`D:\data\set`:                 "d:/data/set",
		"/private/tmp/build":          "/tmp/build",
		"/private/var/folders/ab":     "/var/folders/ab",
		"/srv/app//":                  "/srv/app",
		"/":                           "/",
		"  '/Users/q/quoted path'  ":  "~/quoted path",
		"":                            "",
		"relative/dir/":               "relative/dir",
		"/Users/alice/code/app/sub/x": "~/code/app/sub/x",
	}
	for in, want := range cases {
		if got := NormalizePath(in); got != want {
			t.Errorf("NormalizePath(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestNormalizeGitRemote(t *testing.T) {
	want := "github.com/acme/demo"
	for _, in := range []string{
		"git@github.com:acme/demo.git",
		"https://github.com/acme/demo.git",
		"https://user:token@GitHub.com/acme/demo",
		"ssh://git@github.com:22/acme/demo.git",
		"https://github.com/acme/demo/",
	} {
		if got := NormalizeGitRemote(in); got != want {
			t.Errorf("NormalizeGitRemote(%q) = %q, want %q", in, got, want)
		}
	}
	if got := ProjectNameFromRemote(want); got != "demo" {
		t.Errorf("ProjectNameFromRemote = %q", got)
	}
	if got := ProjectNameFromFolder("~/code/app"); got != "app" {
		t.Errorf("ProjectNameFromFolder = %q", got)
	}
}

func TestTitleStripsWrappers(t *testing.T) {
	msgs := []Message{
		{Role: "user", Blocks: []Block{{Kind: KindUser, Text: "<environment_context><cwd>/x</cwd></environment_context>"}}},
		{Role: "user", Blocks: []Block{{Kind: KindUser, Text: "<system-reminder>\nnoise\n</system-reminder>\n\n  Refactor   the parser\nsecond line"}}},
	}
	if got := Title(msgs); got != "Refactor the parser" {
		t.Errorf("Title = %q", got)
	}
	long := make([]rune, 200)
	for i := range long {
		long[i] = 'é'
	}
	got := Title([]Message{{Role: "user", Blocks: []Block{{Kind: KindUser, Text: string(long)}}}})
	if n := len([]rune(got)); n != MaxTitleLength {
		t.Errorf("title length = %d, want %d", n, MaxTitleLength)
	}
	cmd := Title([]Message{{Role: "user", Blocks: []Block{{Kind: KindUser, Text: "<command-message>review</command-message>\n<command-name>/review</command-name>\n<command-args>pr 12</command-args>"}}}})
	if cmd != "/review pr 12" {
		t.Errorf("command title = %q", cmd)
	}
}

func TestRedact(t *testing.T) {
	in := "anthropic sk-" + "ant-api03-abcdefghijklmnopqrstuvwxyz0123 openai sk-" + "proj-ABCDEFGHIJKLMNOPQRSTUVWX " +
		"gh gh" + "p_0123456789abcdefghijABCDEFGHIJ012345 pat github" + "_pat_11ABCDEFG0123456789_abcdefghijklmnopqrstuvwxyz " +
		"aws AKIA" + "ABCDEFGHIJKLMNOP auth: Bearer abcdefghijklmnopqrstuvwxyz.0123== " +
		"-----BEGIN RSA PRIVATE " + "KEY-----\nMIIEow\n-----END RSA PRIVATE " + "KEY----- short sk-abc"
	want := "anthropic [redacted:anthropic-key] openai [redacted:api-key] " +
		"gh [redacted:github-token] pat [redacted:github-pat] " +
		"aws [redacted:aws-key] auth: [redacted:bearer] " +
		"[redacted:private-key] short sk-abc"
	if got := Redact(in); got != want {
		t.Errorf("Redact =\n%q\nwant\n%q", got, want)
	}
	m := RedactMessage(Message{Role: "user", Blocks: []Block{{Kind: KindUser, Text: "key AKIA" + "ABCDEFGHIJKLMNOP"}}})
	if m.Blocks[0].Text != "key [redacted:aws-key]" {
		t.Errorf("RedactMessage = %q", m.Blocks[0].Text)
	}
}
