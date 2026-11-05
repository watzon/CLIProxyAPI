package traces

import (
	"context"
	"fmt"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v8/internal/config"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/traces/extract"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/traces/store"
)

func TestWorktreeFolderMatching(t *testing.T) {
	settings := SettingsFromConfig(nil)
	for folder, want := range map[string]bool{
		"~/.t3/scratch/2026-11-01-slug-4d82b739":   true,
		"~/.t3/scratch/2026-11-01-slug/sub/dir":    true,
		"~/.t3/scratch":                            false,
		"~/.claude/worktrees/feature-x":            true,
		"~/.codex/worktrees/abcd/repo":             true,
		"~/code/app/.worktrees/feature":            true,
		"~/code/app/.claude/worktrees/feature/src": true,
		"~/code/app/.worktrees":                    false,
		"~/code/app":                               false,
		"~/code/app/packages/db":                   false,
		"~/code/my.worktrees/feature":              false,
		"/srv/.worktrees/feature":                  true,
		"/.worktrees/feature":                      false,
		"c:/work/app/.claude/worktrees/feature":    true,
		"":                                         false,
		"~/other/.t3/scratch/x":                    false,
	} {
		if got := settings.IsWorktreeFolder(folder); got != want {
			t.Errorf("IsWorktreeFolder(%q) = %v, want %v", folder, got, want)
		}
	}
	// An explicit empty list turns the check off; a custom list replaces the defaults.
	none := []string{}
	off := SettingsFromConfig(&config.Config{Traces: config.TracesConfig{Projects: config.TracesProjectsConfig{WorktreeDirs: &none}}})
	if off.IsWorktreeFolder("~/.t3/scratch/x") {
		t.Errorf("empty worktree-dirs still matched")
	}
	custom := []string{"~/wt/*", "*/trees/*"}
	on := SettingsFromConfig(&config.Config{Traces: config.TracesConfig{Projects: config.TracesProjectsConfig{WorktreeDirs: &custom}}})
	if !on.IsWorktreeFolder("~/wt/a") || !on.IsWorktreeFolder("~/code/x/trees/b") || on.IsWorktreeFolder("~/.t3/scratch/x") {
		t.Errorf("custom worktree-dirs not applied")
	}
}

// sdkRequest is a Claude Code SDK request: no folder in the system prompt, the
// environment in user text.
func sdkRequest(folderLine, prompt string) string {
	env := `{"type":"text","text":"SessionStart hook additional context: <env>\n - ` + folderLine + `\n</env>"}`
	return `{"model":"priced-model","system":[{"type":"text","text":"x-anthropic-billing-header: cc_version=2.1.318; cc_entrypoint=sdk-ts;"}],"messages":[{"role":"user","content":[` + env + `,{"type":"text","text":` + fmt.Sprintf("%q", prompt) + `}]}]}`
}

func headersWith(kv ...string) http.Header {
	h := http.Header{"User-Agent": {"claude-cli/2.1.318"}}
	for i := 0; i+1 < len(kv); i += 2 {
		h.Set(kv[i], kv[i+1])
	}
	return h
}

func sessionViews(t *testing.T, svc *Service) map[string]SessionView {
	t.Helper()
	list, err := svc.Sessions(context.Background(), store.TurnFilter{From: t0.Add(-time.Hour), To: t0.Add(2 * time.Hour)}, SessionQuery{Limit: 50, IncludeSubagents: true})
	if err != nil {
		t.Fatal(err)
	}
	out := map[string]SessionView{}
	for _, s := range list.Sessions {
		out[s.ID] = s
	}
	return out
}

func TestProjectSourceOfEveryKind(t *testing.T) {
	cfg := testConfig(t)
	cfg.Traces.Projects.Rules = []config.TracesProjectRule{{Match: "~/ruled/{name}", Project: "R-{name}"}}
	svc := newTestService(t, cfg)
	at := t0
	submit := func(id, session, parent string, headers http.Header, request string) {
		at = at.Add(time.Minute)
		svc.queue.SubmitTurn(turnSpec{id: id, session: session, parent: parent, start: at, latency: time.Second, headers: headers, request: request, reply: claudeReply}.event())
	}
	plain := `{"model":"priced-model","system":"plain","messages":[{"role":"user","content":"hello"}]}`
	submit("t-header", "s-header", "", headersWith("X-CPA-Project", "from-header", "X-CPA-Path-Project", "from-path"), claudeReq)
	submit("t-path", "s-path", "", headersWith("X-CPA-Path-Project", "from-path"), claudeReq)
	submit("t-rule", "s-rule", "", headersWith(), strings.Replace(claudeReq, "/Users/zed/code/alpha", "/Users/zed/ruled/beta", 1))
	submit("t-remote", "s-remote", "", headersWith("X-CPA-Git-Remote", "git@github.com:Acme/Gamma.git"), claudeReq)
	submit("t-folder", "s-folder", "", headersWith(), strings.Replace(claudeReq, "/Users/zed/code/alpha", "/Users/zed/code/delta", 1))
	submit("t-parent", "s-parent", "", headersWith("X-CPA-Project", "parent-proj"), plain)
	submit("t-kid", "s-kid", "s-parent", headersWith(), plain)
	submit("t-none", "s-none", "", headersWith(), plain)
	svc.Flush()
	views := sessionViews(t, svc)
	for id, want := range map[string][2]string{
		"s-header": {"from-header", "header"},
		"s-path":   {"from-path", "path"},
		"s-rule":   {"R-beta", "rule"},
		"s-remote": {"Gamma", "remote"},
		"s-folder": {"delta", "folder"},
		"s-kid":    {"parent-proj", "inherited"},
		"s-none":   {UnassignedName, "none"},
	} {
		got, ok := views[id]
		if !ok || got.Project != want[0] || got.ProjectSource != want[1] {
			t.Errorf("%s: project=%q source=%q, want %q %q (found=%v)", id, got.Project, got.ProjectSource, want[0], want[1], ok)
		}
	}
	detail, err := svc.SessionDetail(context.Background(), "s-remote")
	if err != nil || detail.Session.ProjectSource != "remote" {
		t.Errorf("session detail source = %+v %v", detail, err)
	}
}

func TestClaudeSDKFolderFromMessagesAndWorktrees(t *testing.T) {
	cfg := testConfig(t)
	svc := newTestService(t, cfg)
	at := t0
	submit := func(id, session string, headers http.Header, request string) {
		at = at.Add(time.Minute)
		svc.queue.SubmitTurn(turnSpec{id: id, session: session, start: at, latency: time.Second, headers: headers, request: request, reply: claudeReply}.event())
	}
	submit("t1", "scratch", headersWith(), sdkRequest("Primary working directory: /Users/x/.t3/scratch/2026-11-01-some-slug-4d82b739", "Build it"))
	submit("t2", "real", headersWith(), sdkRequest("Primary working directory: /Users/x/code/app", "Build it"))
	submit("t3", "scratch-remote", headersWith("X-CPA-Git-Remote", "https://github.com/acme/widgets"), sdkRequest("Primary working directory: /Users/x/.claude/worktrees/feat", "Build it"))
	svc.Flush()
	views := sessionViews(t, svc)
	if v := views["scratch"]; v.Folder != "~/.t3/scratch/2026-11-01-some-slug-4d82b739" || v.Project != UnassignedName || v.ProjectSource != "none" {
		t.Errorf("scratch session = %+v", v)
	}
	if v := views["real"]; v.Folder != "~/code/app" || v.Project != "app" || v.ProjectSource != "folder" {
		t.Errorf("real session = %+v", v)
	}
	if v := views["scratch-remote"]; v.Project != "widgets" || v.ProjectSource != "remote" || v.Folder != "~/.claude/worktrees/feat" {
		t.Errorf("worktree session with a remote = %+v", v)
	}
	// A config rule that matches the worktree folder still wins.
	cfg.Traces.Projects.Rules = []config.TracesProjectRule{{Match: "~/.t3/scratch/{name}", Project: "scratch"}}
	svc.Apply(cfg)
	if err := svc.ReresolveProjects(context.Background()); err != nil {
		t.Fatal(err)
	}
	if v := sessionViews(t, svc)["scratch"]; v.Project != "scratch" || v.ProjectSource != "rule" {
		t.Errorf("rule on a worktree folder = %+v", v)
	}
}

func TestWorktreeDirsChangeReresolvesInTheBackground(t *testing.T) {
	cfg := testConfig(t)
	none := []string{}
	cfg.Traces.Projects.WorktreeDirs = &none
	svc := newTestService(t, cfg)
	svc.queue.SubmitTurn(turnSpec{id: "t1", session: "wt", start: t0, latency: time.Second, headers: headersWith(), request: sdkRequest("Primary working directory: /Users/x/.t3/scratch/slug-1", "Build it"), reply: claudeReply}.event())
	svc.Flush()
	if v := sessionViews(t, svc)["wt"]; v.Project != "slug-1" || v.ProjectSource != "folder" {
		t.Fatalf("with the check off the folder names the project: %+v", v)
	}
	cfg.Traces.Projects.WorktreeDirs = nil
	svc.Apply(cfg)
	deadline := time.Now().Add(10 * time.Second)
	for {
		v := sessionViews(t, svc)["wt"]
		if v.Project == UnassignedName && v.ProjectSource == "none" {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("worktree-dirs change did not re-resolve: %+v", v)
		}
		time.Sleep(50 * time.Millisecond)
	}
}

func TestTitleSkipsCompactionOnIngest(t *testing.T) {
	svc := newTestService(t, testConfig(t))
	compact := func(prompt string) string {
		return `{"model":"priced-model","system":"plain","messages":[{"role":"user","content":"This session is being continued from a previous conversation that ran out of context.\nSummary..."},{"role":"assistant","content":"ok"},{"role":"user","content":[{"type":"text","text":"# Environment update\n - Primary working directory: /Users/x/code/app"},{"type":"text","text":` + fmt.Sprintf("%q", prompt) + `}]}]}`
	}
	svc.queue.SubmitTurn(turnSpec{id: "t1", session: "c", start: t0, latency: time.Second, headers: headersWith(), request: `{"model":"priced-model","system":"plain","messages":[{"role":"user","content":"This session is being continued from a previous conversation that ran out of context."}]}`, reply: claudeReply}.event())
	svc.Flush()
	if v := sessionViews(t, svc)["c"]; v.Title != "" {
		t.Errorf("title from a compaction summary: %q", v.Title)
	}
	svc.queue.SubmitTurn(turnSpec{id: "t2", session: "c", start: t0.Add(time.Minute), latency: time.Second, headers: headersWith(), request: compact("Now add retries\nplease"), reply: claudeReply}.event())
	svc.Flush()
	if v := sessionViews(t, svc)["c"]; v.Title != "Now add retries" {
		t.Errorf("title = %q", v.Title)
	}
}

// degrade writes the state an older version left: no folder, a compaction
// summary as title, and a project from the wrong signals.
func degrade(t *testing.T, st store.Store, id string) {
	t.Helper()
	sessions, err := st.Sessions(context.Background(), []string{id})
	if err != nil || len(sessions) != 1 {
		t.Fatalf("session %s: %v %v", id, sessions, err)
	}
	sess := sessions[0]
	sess.Folder, sess.Title = "", "This session is being continued from a previous conversation that ran out of context."
	sess.ProjectName, sess.ProjectKey, sess.ProjectID, sess.ProjectSource = UnassignedName, UnassignedName, ProjectID(UnassignedName), ""
	if err = st.UpdateSessionProjects(context.Background(), []store.Session{sess}); err != nil {
		t.Fatal(err)
	}
}

func TestStartupRederiveFixesStoredSessions(t *testing.T) {
	cfg := testConfig(t)
	svc := newTestService(t, cfg)
	hist := `{"role":"user","content":[{"type":"text","text":"This session is being continued from a previous conversation that ran out of context."}]},{"role":"assistant","content":"ok"},{"role":"user","content":[{"type":"text","text":"# Environment update\n - Primary working directory: /Users/x/code/app/packages/db (was /Users/x/code/app)"},{"type":"text","text":"Now fix the flaky test"}]}`
	req := `{"model":"priced-model","system":[{"type":"text","text":"x-anthropic-billing-header: cc_entrypoint=sdk-ts;"}],"messages":[` + hist + `]}`
	svc.queue.SubmitTurn(turnSpec{id: "t1", session: "old", start: t0, latency: time.Second, headers: headersWith(), request: req, reply: claudeReply}.event())
	// A session without stored content cannot be fixed and stays as it is.
	svc.queue.SubmitTurn(turnSpec{id: "t2", session: "bare", start: t0.Add(time.Minute), latency: time.Second, headers: headersWith("X-CPA-Project", "keep"), request: `{"model":"priced-model","system":"plain","messages":[{"role":"user","content":"hi"}]}`, reply: claudeReply}.event())
	svc.Flush()
	st := svc.Store()
	degrade(t, st, "old")
	if err := st.PutKV(context.Background(), rederiveKey, []byte("0")); err != nil {
		t.Fatal(err)
	}
	if rederiveDone(context.Background(), st) {
		t.Fatal("a version 0 marker counts as done")
	}
	svc.Close()

	// Start again on the same database. The job runs in the background.
	svc = newTestService(t, cfg)
	deadline := time.Now().Add(10 * time.Second)
	for {
		v := sessionViews(t, svc)["old"]
		if v.Folder == "~/code/app" && v.Title == "Now fix the flaky test" && v.Project == "app" && v.ProjectSource == "folder" {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("stored session was not fixed: %+v", v)
		}
		time.Sleep(50 * time.Millisecond)
	}
	if v := sessionViews(t, svc)["bare"]; v.Project != "keep" || v.ProjectSource != "header" {
		t.Errorf("unrelated session changed: %+v", v)
	}
	// The marker is recorded after the run, so the next start skips the job.
	deadline = time.Now().Add(10 * time.Second)
	for !rederiveDone(context.Background(), svc.Store()) {
		if time.Now().After(deadline) {
			t.Fatal("re-derive version was not recorded")
		}
		time.Sleep(50 * time.Millisecond)
	}
}

func TestRederiveSkipsSessionsWithoutContent(t *testing.T) {
	cfg := testConfig(t)
	cfg.Traces.Capture = CaptureMetadata
	svc := newTestService(t, cfg)
	svc.queue.SubmitTurn(turnSpec{id: "t1", session: "meta", start: t0, latency: time.Second, headers: headersWith(), request: sdkRequest("Primary working directory: /Users/x/code/app", "Build it"), reply: claudeReply}.event())
	svc.Flush()
	degrade(t, svc.Store(), "meta")
	if err := svc.ReresolveProjects(context.Background()); err != nil {
		t.Fatal(err)
	}
	if v := sessionViews(t, svc)["meta"]; v.Folder != "" || v.Project != UnassignedName {
		t.Errorf("a session without content was changed: %+v", v)
	}
}

func TestSettingsDefaultsMatchExtractRegistry(t *testing.T) {
	// A default pattern must survive normalization: no pattern may compile to nothing.
	if got := len(CompileWorktreeDirs(DefaultWorktreeDirs)); got != len(DefaultWorktreeDirs) {
		t.Fatalf("compiled %d of %d default worktree patterns", got, len(DefaultWorktreeDirs))
	}
	if extract.NormalizePath("/Users/me/.t3/scratch/x") != "~/.t3/scratch/x" {
		t.Fatal("home normalization changed")
	}
}
