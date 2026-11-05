package extract

import "testing"

const (
	sdkSystem = `"system":[{"type":"text","text":"x-anthropic-billing-header: cc_version=2.1.318; cc_entrypoint=sdk-ts;\nYou are Claude Code."}]`
	sdkUA     = "claude-cli/2.1.318 (external, sdk-ts)"
)

func TestClaudeCodeFolderFromMessages(t *testing.T) {
	sessionStart := `{"type":"text","text":"SessionStart hook additional context: <env>\n - Primary working directory: /Users/x/.t3/scratch/2026-11-01-some-slug-4d82b739\n - Platform: darwin\n</env>"}`
	envUpdate := `{"type":"text","text":"# Environment update\n - Primary working directory: /Users/x/code/app/packages/db (was /Users/x/code/app)"}`
	envUpdateModules := `{"type":"text","text":"# Environment update\n - Primary working directory: /Users/x/code/app/node_modules/pkg (was /Users/x/code/app/packages/db)"}`
	tests := []struct {
		name       string
		body       string
		wantFolder string
	}{
		{
			name:       "system prompt shape still wins",
			body:       `{"system":"Primary working directory: /Users/x/code/sys","messages":[{"role":"user","content":[` + sessionStart + `]}]}`,
			wantFolder: "~/code/sys",
		},
		{
			name:       "session start hook context in a user message",
			body:       `{` + sdkSystem + `,"messages":[{"role":"user","content":[` + sessionStart + `,{"type":"text","text":"fix the bug"}]}]}`,
			wantFolder: "~/.t3/scratch/2026-11-01-some-slug-4d82b739",
		},
		{
			name:       "environment update names the older folder",
			body:       `{` + sdkSystem + `,"messages":[{"role":"user","content":[` + envUpdate + `,{"type":"text","text":"go on"}]}]}`,
			wantFolder: "~/code/app",
		},
		{
			name:       "earliest occurrence wins over later updates",
			body:       `{` + sdkSystem + `,"messages":[{"role":"user","content":[` + envUpdate + `]},{"role":"assistant","content":"ok"},{"role":"user","content":[` + envUpdateModules + `]}]}`,
			wantFolder: "~/code/app",
		},
		{
			name:       "session start line comes before later updates",
			body:       `{` + sdkSystem + `,"messages":[{"role":"user","content":[` + sessionStart + `]},{"role":"user","content":[` + envUpdate + `]}]}`,
			wantFolder: "~/.t3/scratch/2026-11-01-some-slug-4d82b739",
		},
		{
			name:       "plain user string content",
			body:       `{` + sdkSystem + `,"messages":[{"role":"user","content":"# Environment update\n - Primary working directory: /Users/x/code/plain"}]}`,
			wantFolder: "~/code/plain",
		},
		{
			name:       "tool results are not searched",
			body:       `{` + sdkSystem + `,"messages":[{"role":"user","content":[{"type":"tool_result","tool_use_id":"t","content":"Primary working directory: /Users/x/code/leak"}]}]}`,
			wantFolder: "",
		},
		{
			name:       "no folder anywhere",
			body:       `{` + sdkSystem + `,"messages":[{"role":"user","content":"hi"}]}`,
			wantFolder: "",
		},
		{
			name:       "was tail is dropped from the system prompt shape too",
			body:       `{"system":"Primary working directory: /Users/x/code/new (was /Users/x/code/old)","messages":[{"role":"user","content":"hi"}]}`,
			wantFolder: "~/code/old",
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			req := ParseRequest("claude", []byte(tc.body))
			d := Derive(hdr("User-Agent", sdkUA), req)
			if d.Client != ClientClaudeCode || d.Folder != tc.wantFolder {
				t.Fatalf("client=%q folder=%q, want %q", d.Client, d.Folder, tc.wantFolder)
			}
		})
	}
}

func TestDeriveGitRemoteAndPathProjectHeaders(t *testing.T) {
	req := ParseRequest("claude", []byte(`{"system":"plain","messages":[{"role":"user","content":"hi"}]}`))
	d := Derive(hdr("User-Agent", "claude-cli/2.0.0", HeaderGitRemote, "git@GitHub.com:Acme/Repo.git", HeaderPathProject, " my-app "), req)
	if d.GitRemote != "github.com/Acme/Repo" || d.PathProject != "my-app" {
		t.Errorf("remote=%q path project=%q", d.GitRemote, d.PathProject)
	}
	https := Derive(hdr(HeaderGitRemote, "https://github.com/Acme/Repo"), nil)
	if https.GitRemote != d.GitRemote {
		t.Errorf("ssh and https remotes differ: %q vs %q", d.GitRemote, https.GitRemote)
	}
	// The header beats the remote from the client's own metadata.
	codex := Derive(hdr("User-Agent", "codex_cli_rs/0.120.0", "Originator", "codex_cli_rs", HeaderGitRemote, "ssh://git@github.com/Acme/Other.git",
		"X-Codex-Turn-Metadata", `{"workspaces":{"/work/repo":{"associated_remote_urls":{"origin":"https://github.com/Acme/Repo.git"}}}}`), ParseRequest("openai-response", []byte(`{"input":"hi"}`)))
	if codex.GitRemote != "github.com/Acme/Other" || codex.Folder != "/work/repo" {
		t.Errorf("codex remote=%q folder=%q", codex.GitRemote, codex.Folder)
	}
}

func TestTitleSkipsCompactionAndInjectedContext(t *testing.T) {
	user := func(text string) Message { return Message{Role: "user", Blocks: []Block{{Kind: KindUser, Text: text}}} }
	msgs := []Message{
		user("This session is being continued from a previous conversation that ran out of context.\nSummary:\n..."),
		user("SessionStart hook additional context: <env>\n - Primary working directory: /Users/x/code/app\n</env>"),
		user("# Environment update\n - Primary working directory: /Users/x/code/app/sub (was /Users/x/code/app)"),
		user("<system-reminder>only context</system-reminder>"),
		user("Now add the retry logic\nplease"),
	}
	if got := Title(msgs); got != "Now add the retry logic" {
		t.Errorf("Title = %q", got)
	}
	if got := Title(msgs[:4]); got != "" {
		t.Errorf("Title of injected text only = %q, want empty", got)
	}
	if !IsCompactionText("  This session is being continued from a previous conversation that ran out of context.") || IsCompactionText("Fix it") {
		t.Errorf("IsCompactionText misjudged")
	}
}
