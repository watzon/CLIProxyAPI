package traces_test

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/api"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/config"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/registry"
	runtimeexecutor "github.com/router-for-me/CLIProxyAPI/v8/internal/runtime/executor"
	_ "github.com/router-for-me/CLIProxyAPI/v8/internal/translator"
	sdkaccess "github.com/router-for-me/CLIProxyAPI/v8/sdk/access"
	cliproxyauth "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/auth"
	sdkconfig "github.com/router-for-me/CLIProxyAPI/v8/sdk/config"
)

const (
	e2eClientKey  = "e2e-client-key"
	e2eMgmtSecret = "e2e-management-secret"
	claudeModel   = "claude-sonnet-4-5"
	codexModel    = "gpt-5.4"
)

func sse(events ...string) string {
	var sb strings.Builder
	for _, e := range events {
		var probe struct {
			Type string `json:"type"`
		}
		_ = json.Unmarshal([]byte(e), &probe)
		fmt.Fprintf(&sb, "event: %s\ndata: %s\n\n", probe.Type, e)
	}
	return sb.String()
}

// fakeUpstream answers the Claude Messages and OpenAI Responses calls the
// executors make, with tool calls and plan-limit headers.
func fakeUpstream(t *testing.T) *httptest.Server {
	t.Helper()
	srv, _ := recordingUpstream(t)
	return srv
}

// recordingUpstream is fakeUpstream plus a view of the headers of every request it got.
func recordingUpstream(t *testing.T) (*httptest.Server, func() []http.Header) {
	t.Helper()
	var mu sync.Mutex
	claudeCalls := 0
	var seen []http.Header
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		_ = body
		mu.Lock()
		seen = append(seen, r.Header.Clone())
		mu.Unlock()
		w.Header().Set("Content-Type", "text/event-stream")
		switch {
		case strings.Contains(r.URL.Path, "/messages"):
			mu.Lock()
			claudeCalls++
			n := claudeCalls
			mu.Unlock()
			w.Header().Set("Anthropic-Ratelimit-Unified-5h-Status", "allowed")
			w.Header().Set("Anthropic-Ratelimit-Unified-5h-Utilization", "0.42")
			w.Header().Set("Anthropic-Ratelimit-Unified-5h-Reset", fmt.Sprint(time.Now().Add(3*time.Hour).Unix()))
			start := `{"type":"message_start","message":{"id":"msg_` + fmt.Sprint(n) + `","type":"message","role":"assistant","model":"` + claudeModel + `","content":[],"usage":{"input_tokens":100,"cache_read_input_tokens":2000,"cache_creation_input_tokens":300,"output_tokens":1}}}`
			if n == 1 {
				_, _ = io.WriteString(w, sse(start,
					`{"type":"content_block_start","index":0,"content_block":{"type":"text","text":""}}`,
					`{"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":"I will edit the file."}}`,
					`{"type":"content_block_stop","index":0}`,
					`{"type":"content_block_start","index":1,"content_block":{"type":"tool_use","id":"toolu_1","name":"Edit","input":{}}}`,
					`{"type":"content_block_delta","index":1,"delta":{"type":"input_json_delta","partial_json":"{\"file_path\":\"/Users/dev/code/demo/main.go\",\"old_string\":\"a\\nb\",\"new_string\":\"a\\nc\\nd\"}"}}`,
					`{"type":"content_block_stop","index":1}`,
					`{"type":"message_delta","delta":{"stop_reason":"tool_use"},"usage":{"output_tokens":50}}`,
					`{"type":"message_stop"}`,
				))
				return
			}
			_, _ = io.WriteString(w, sse(start,
				`{"type":"content_block_start","index":0,"content_block":{"type":"text","text":""}}`,
				`{"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":"Done."}}`,
				`{"type":"content_block_stop","index":0}`,
				`{"type":"message_delta","delta":{"stop_reason":"end_turn"},"usage":{"output_tokens":50}}`,
				`{"type":"message_stop"}`,
			))
		case strings.Contains(r.URL.Path, "/responses"):
			w.Header().Set("X-Codex-Primary-Used-Percent", "12")
			w.Header().Set("X-Codex-Primary-Window-Minutes", "300")
			w.Header().Set("X-Codex-Primary-Reset-After-Seconds", "3600")
			w.Header().Set("X-Codex-Plan-Type", "plus")
			item := `{"type":"function_call","id":"fc_1","status":"completed","call_id":"call_1","name":"apply_patch","arguments":"{\"input\":\"*** Begin Patch\\n*** Add File: README.md\\n+# Demo\\n+hello\\n*** End Patch\"}"}`
			_, _ = io.WriteString(w, sse(
				`{"type":"response.created","response":{"id":"resp_1","object":"response","status":"in_progress","model":"`+codexModel+`","output":[]}}`,
				`{"type":"response.output_item.done","output_index":0,"item":`+item+`}`,
				`{"type":"response.completed","response":{"id":"resp_1","object":"response","status":"completed","model":"`+codexModel+`","output":[`+item+`],"usage":{"input_tokens":1000,"input_tokens_details":{"cached_tokens":600},"output_tokens":80,"output_tokens_details":{"reasoning_tokens":20},"total_tokens":1080}}}`,
			))
		default:
			http.NotFound(w, r)
		}
	}))
	return srv, func() []http.Header {
		mu.Lock()
		defer mu.Unlock()
		return append([]http.Header(nil), seen...)
	}
}

func newE2EServer(t *testing.T, upstream string) *api.Server {
	t.Helper()
	gin.SetMode(gin.TestMode)
	t.Setenv("MANAGEMENT_PASSWORD", e2eMgmtSecret)
	tmp := t.TempDir()
	authDir := filepath.Join(tmp, "auth")
	if err := os.MkdirAll(authDir, 0o700); err != nil {
		t.Fatal(err)
	}
	empty := ""
	f64 := func(v float64) *float64 { return &v }
	cfg := &config.Config{SDKConfig: sdkconfig.SDKConfig{APIKeys: []string{e2eClientKey}}}
	cfg.AuthDir = authDir
	cfg.Traces = config.TracesConfig{
		Enabled: true,
		Dir:     filepath.Join(tmp, "traces"),
		Capture: "full",
		Prices: config.TracesPricesConfig{Source: &empty, Overrides: []config.TracesPriceOverride{
			{Model: claudeModel, Input: f64(3), Output: f64(15), CacheRead: f64(0.3), CacheWrite5m: f64(3.75), CacheWrite1h: f64(6)},
			{Model: "gpt-5*", Input: f64(1.25), Output: f64(10), CacheRead: f64(0.125)},
		}},
	}
	configPath := filepath.Join(tmp, "config.yaml")
	if err := os.WriteFile(configPath, []byte("config-version: 8\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	manager := cliproxyauth.NewManager(nil, &cliproxyauth.RoundRobinSelector{}, nil)
	manager.SetRetryConfig(0, 0, 0)
	manager.RegisterExecutor(runtimeexecutor.NewClaudeExecutor(cfg))
	manager.RegisterExecutor(runtimeexecutor.NewCodexExecutor(cfg))
	reg := registry.GetGlobalRegistry()
	for _, a := range []struct{ id, provider, model string }{{"e2e-claude", "claude", claudeModel}, {"e2e-codex", "codex", codexModel}} {
		reg.RegisterClient(a.id, a.provider, []*registry.ModelInfo{{ID: a.model}})
		id := a.id
		t.Cleanup(func() { reg.UnregisterClient(id) })
		if _, err := manager.Register(context.Background(), &cliproxyauth.Auth{
			ID: a.id, Provider: a.provider, Status: cliproxyauth.StatusActive, Label: a.id,
			Attributes: map[string]string{"api_key": "upstream-" + a.id, "base_url": upstream},
		}); err != nil {
			t.Fatal(err)
		}
	}
	server := api.NewServer(cfg, manager, sdkaccess.NewManager(), configPath)
	t.Cleanup(func() { _ = server.Stop(context.Background()) })
	return server
}

func doJSON(t *testing.T, h http.Handler, method, path string, body string, headers map[string]string) (int, []byte) {
	t.Helper()
	var reader io.Reader
	if body != "" {
		reader = strings.NewReader(body)
	}
	req := httptest.NewRequest(method, path, reader)
	req.Header.Set("Content-Type", "application/json")
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	return rec.Code, rec.Body.Bytes()
}

func mgmtGet(t *testing.T, h http.Handler, path string, out any) {
	t.Helper()
	code, body := doJSON(t, h, http.MethodGet, "/v8/management"+path, "", map[string]string{"Authorization": "Bearer " + e2eMgmtSecret})
	if code != http.StatusOK {
		t.Fatalf("GET %s = %d: %s", path, code, body)
	}
	if err := json.Unmarshal(body, out); err != nil {
		t.Fatalf("decode %s: %v\n%s", path, err, body)
	}
	if os.Getenv("TRACES_E2E_DUMP") != "" {
		t.Logf("GET %s\n%s", path, body)
	}
}

type e2eSession struct {
	ID            string `json:"id"`
	Title         string `json:"title"`
	Project       string `json:"project"`
	ProjectSource string `json:"project_source"`
	Folder        string `json:"folder"`
	Client        string `json:"client"`
	Turns         int    `json:"turns"`
	FilesChanged  int    `json:"files_changed"`
	LinesAdded    int    `json:"lines_added"`
	LinesRemoved  int    `json:"lines_removed"`
	DeviceID      *string
	CostUSD       *float64 `json:"cost_usd"`
}

type e2eTurn struct {
	ID       string            `json:"id"`
	Seq      int               `json:"seq"`
	Status   string            `json:"status"`
	Account  string            `json:"account"`
	Attempts int               `json:"attempts"`
	Tokens   map[string]int64  `json:"tokens"`
	CostUSD  *float64          `json:"cost_usd"`
	Tools    []string          `json:"tool_names"`
	Extra    map[string]string `json:"-"`
}

type e2eBlock struct {
	Kind     string          `json:"kind"`
	Text     string          `json:"text"`
	ToolName *string         `json:"tool_name"`
	IsError  bool            `json:"is_error"`
	Diff     json.RawMessage `json:"diff"`
}

type e2eTurnDetail struct {
	Turn             e2eTurn    `json:"turn"`
	ContentAvailable bool       `json:"content_available"`
	Blocks           []e2eBlock `json:"blocks"`
	Facts            struct {
		Folder     string  `json:"folder"`
		Client     string  `json:"client"`
		StopReason *string `json:"stop_reason"`
	} `json:"facts"`
	CostBreakdown []struct {
		Type    string   `json:"type"`
		Tokens  int64    `json:"tokens"`
		CostUSD *float64 `json:"cost_usd"`
	} `json:"cost_breakdown"`
}

// waitTurn polls the turn until usage has been joined (writer flushes every second).
func waitTurn(t *testing.T, h http.Handler, id string) e2eTurnDetail {
	t.Helper()
	deadline := time.Now().Add(15 * time.Second)
	for {
		var d e2eTurnDetail
		mgmtGet(t, h, "/traces/turns/"+id, &d)
		if d.Turn.Tokens["total"] > 0 && d.Turn.Account != "" {
			return d
		}
		if time.Now().After(deadline) {
			t.Fatalf("turn %s never received usage: %+v", id, d.Turn)
		}
		time.Sleep(100 * time.Millisecond)
	}
}

func findSession(t *testing.T, h http.Handler, title string) e2eSession {
	t.Helper()
	deadline := time.Now().Add(15 * time.Second)
	for {
		var list struct {
			Sessions []e2eSession `json:"sessions"`
		}
		mgmtGet(t, h, "/traces/sessions", &list)
		for _, s := range list.Sessions {
			if s.Title == title {
				return s
			}
		}
		if time.Now().After(deadline) {
			t.Fatalf("session %q not recorded: %+v", title, list.Sessions)
		}
		time.Sleep(100 * time.Millisecond)
	}
}

func TestTracesEndToEnd(t *testing.T) {
	upstream := fakeUpstream(t)
	defer upstream.Close()
	server := newE2EServer(t, upstream.URL)
	h := server.Handler()

	claudeHeaders := map[string]string{
		"Authorization":            "Bearer " + e2eClientKey,
		"User-Agent":               "claude-cli/2.1.318 (external, cli)",
		"X-Claude-Code-Session-Id": "5d3a1c2e-1111-4222-8333-944455556666",
	}
	system := `[{"type":"text","text":"You are a coding agent.\n\nPrimary working directory: /Users/dev/code/demo\nIs a git repository: true"}]`
	meta := `{"user_id":"{\"device_id\":\"dev-123\",\"account_uuid\":\"\",\"session_id\":\"5d3a1c2e-1111-4222-8333-944455556666\"}"}`
	user := `{"role":"user","content":[{"type":"text","text":"<system-reminder>context</system-reminder>Fix the bug in main.go\nthanks"}]}`
	first := `{"model":"` + claudeModel + `","stream":true,"max_tokens":512,"system":` + system + `,"metadata":` + meta + `,"messages":[` + user + `]}`
	code, body := doJSON(t, h, http.MethodPost, "/v1/messages", first, claudeHeaders)
	if code != http.StatusOK || !strings.Contains(string(body), "toolu_1") {
		t.Fatalf("first Claude request = %d: %s", code, body)
	}
	assistant := `{"role":"assistant","content":[{"type":"text","text":"I will edit the file."},{"type":"tool_use","id":"toolu_1","name":"Edit","input":{"file_path":"/Users/dev/code/demo/main.go","old_string":"a\nb","new_string":"a\nc\nd"}}]}`
	result := `{"role":"user","content":[{"type":"tool_result","tool_use_id":"toolu_1","content":"The file was updated."}]}`
	second := `{"model":"` + claudeModel + `","stream":true,"max_tokens":512,"system":` + system + `,"metadata":` + meta + `,"messages":[` + user + `,` + assistant + `,` + result + `]}`
	if code, body = doJSON(t, h, http.MethodPost, "/v1/messages", second, claudeHeaders); code != http.StatusOK {
		t.Fatalf("second Claude request = %d: %s", code, body)
	}

	codexHeaders := map[string]string{
		"Authorization":         "Bearer " + e2eClientKey,
		"User-Agent":            "codex_cli_rs/0.120.0 (Mac OS 15.0; arm64)",
		"Originator":            "codex_cli_rs",
		"Session_id":            "0199a1b2-c3d4-7e5f-8a9b-0c1d2e3f4a5b",
		"X-Codex-Turn-Metadata": `{"workspaces":{"/Users/dev/code/demo":{"associated_remote_urls":{"origin":"git@github.com:acme/demo.git"}}}}`,
	}
	responsesReq := `{"model":"` + codexModel + `","stream":true,"instructions":"You are Codex.","input":[{"type":"message","role":"user","content":[{"type":"input_text","text":"<environment_context><cwd>/Users/dev/code/demo</cwd></environment_context>"}]},{"type":"message","role":"user","content":[{"type":"input_text","text":"Add a README"}]}]}`
	if code, body = doJSON(t, h, http.MethodPost, "/v1/responses", responsesReq, codexHeaders); code != http.StatusOK || !strings.Contains(string(body), "apply_patch") {
		t.Fatalf("Responses request = %d: %s", code, body)
	}

	// Claude session: two turns, diff stats, title without the wrapper.
	sess := findSession(t, h, "Fix the bug in main.go")
	var detail struct {
		Session struct {
			e2eSession
			DeviceID *string `json:"device_id"`
		} `json:"session"`
		Turns []e2eTurn `json:"turns"`
		Lanes []struct {
			SessionID string `json:"session_id"`
			Turns     int    `json:"turns"`
		} `json:"lanes"`
	}
	mgmtGet(t, h, "/traces/sessions/"+sess.ID, &detail)
	if len(detail.Turns) != 2 || len(detail.Lanes) != 1 || detail.Lanes[0].Turns != 2 {
		t.Fatalf("session detail turns=%d lanes=%+v", len(detail.Turns), detail.Lanes)
	}
	s := detail.Session
	if s.Project != "demo" || s.Folder != "~/code/demo" || s.Client != "claude-code" {
		t.Errorf("session project=%q folder=%q client=%q", s.Project, s.Folder, s.Client)
	}
	if s.FilesChanged != 1 || s.LinesAdded != 2 || s.LinesRemoved != 1 {
		t.Errorf("diff stats files=%d +%d -%d, want 1 +2 -1", s.FilesChanged, s.LinesAdded, s.LinesRemoved)
	}
	if s.DeviceID == nil || *s.DeviceID != "dev-123" {
		t.Errorf("device_id = %v", s.DeviceID)
	}

	turn1 := waitTurn(t, h, detail.Turns[0].ID)
	wantTokens := map[string]int64{"input": 100, "cache_read": 2000, "cache_write_5m": 300, "cache_write_1h": 0, "output": 50, "total": 2450}
	for k, v := range wantTokens {
		if turn1.Turn.Tokens[k] != v {
			t.Errorf("turn 1 tokens[%s] = %d, want %d (all %v)", k, turn1.Turn.Tokens[k], v, turn1.Turn.Tokens)
		}
	}
	if turn1.Turn.CostUSD == nil || fmt.Sprintf("%.6f", *turn1.Turn.CostUSD) != "0.002775" {
		t.Errorf("turn 1 cost = %v, want 0.002775", turn1.Turn.CostUSD)
	}
	if turn1.Turn.Account != "e2e-claude" || turn1.Turn.Status != "ok" || turn1.Turn.Attempts != 1 {
		t.Errorf("turn 1 account=%q status=%q attempts=%d", turn1.Turn.Account, turn1.Turn.Status, turn1.Turn.Attempts)
	}
	if !turn1.ContentAvailable || turn1.Facts.StopReason == nil || *turn1.Facts.StopReason != "tool_use" {
		t.Errorf("turn 1 content=%v stop=%v", turn1.ContentAvailable, turn1.Facts.StopReason)
	}
	var kinds []string
	var editDiff json.RawMessage
	for _, b := range turn1.Blocks {
		kinds = append(kinds, b.Kind)
		if b.Kind == "tool_call" {
			editDiff = b.Diff
		}
	}
	if strings.Join(kinds, ",") != "system,user,assistant,tool_call" {
		t.Errorf("turn 1 block kinds = %v", kinds)
	}
	if !strings.Contains(string(editDiff), `"file":"/Users/dev/code/demo/main.go"`) || !strings.Contains(string(editDiff), `"new":"a\nc\nd"`) {
		t.Errorf("turn 1 diff = %s", editDiff)
	}

	// The resent conversation stores each message once: turn 2 shows only the
	// tool result and the new reply.
	turn2 := waitTurn(t, h, detail.Turns[1].ID)
	kinds = nil
	for _, b := range turn2.Blocks {
		kinds = append(kinds, b.Kind)
	}
	if strings.Join(kinds, ",") != "tool_result,assistant" {
		t.Errorf("turn 2 block kinds = %v (%+v)", kinds, turn2.Blocks)
	}
	if len(turn2.Blocks) > 0 && (turn2.Blocks[0].ToolName == nil || *turn2.Blocks[0].ToolName != "Edit") {
		t.Errorf("tool result is not linked to its call: %+v", turn2.Blocks[0])
	}

	// Responses session: git remote project, apply_patch diff, OpenAI token normalization.
	codexSess := findSession(t, h, "Add a README")
	var codexDetail struct {
		Turns []e2eTurn `json:"turns"`
	}
	mgmtGet(t, h, "/traces/sessions/"+codexSess.ID, &codexDetail)
	if codexSess.Client != "codex" || codexSess.Project != "demo" || len(codexDetail.Turns) != 1 {
		t.Fatalf("codex session %+v turns=%d", codexSess, len(codexDetail.Turns))
	}
	cturn := waitTurn(t, h, codexDetail.Turns[0].ID)
	wantCodex := map[string]int64{"input": 400, "cache_read": 600, "output": 80, "reasoning": 20, "total": 1080}
	for k, v := range wantCodex {
		if cturn.Turn.Tokens[k] != v {
			t.Errorf("codex tokens[%s] = %d, want %d (all %v)", k, cturn.Turn.Tokens[k], v, cturn.Turn.Tokens)
		}
	}
	if cturn.Turn.CostUSD == nil || fmt.Sprintf("%.6f", *cturn.Turn.CostUSD) != "0.001375" {
		t.Errorf("codex cost = %v, want 0.001375", cturn.Turn.CostUSD)
	}
	var patch json.RawMessage
	for _, b := range cturn.Blocks {
		if b.Kind == "tool_call" {
			patch = b.Diff
		}
	}
	if !strings.Contains(string(patch), `"file":"README.md"`) || !strings.Contains(string(patch), `"patch":"*** Begin Patch`) {
		t.Errorf("apply_patch diff = %s", patch)
	}

	// Stats, limits and health see the same data.
	var stats struct {
		Totals struct {
			Turns    int      `json:"turns"`
			Sessions int      `json:"sessions"`
			CostUSD  *float64 `json:"cost_usd"`
		} `json:"totals"`
		Groups []struct {
			Key   string `json:"key"`
			Label string `json:"label"`
		} `json:"groups"`
	}
	mgmtGet(t, h, "/traces/stats?group=model&bucket=hour", &stats)
	if stats.Totals.Turns != 3 || stats.Totals.Sessions != 2 || len(stats.Groups) != 2 {
		t.Errorf("stats totals=%+v groups=%+v", stats.Totals, stats.Groups)
	}
	var limits struct {
		Accounts []struct {
			AccountID string `json:"account_id"`
			Plan      *string
			Windows   []struct {
				Name    string   `json:"name"`
				UsedPct *float64 `json:"used_pct"`
			} `json:"windows"`
		} `json:"accounts"`
	}
	mgmtGet(t, h, "/traces/limits", &limits)
	found := map[string]float64{}
	for _, a := range limits.Accounts {
		for _, w := range a.Windows {
			if w.UsedPct != nil {
				found[a.AccountID+"/"+w.Name] = *w.UsedPct
			}
		}
	}
	if found["e2e-claude/5h"] != 42 || found["e2e-codex/primary"] != 12 {
		t.Errorf("limit windows = %v", found)
	}
	var health struct {
		Enabled bool   `json:"enabled"`
		Capture string `json:"capture"`
	}
	mgmtGet(t, h, "/traces/health", &health)
	if !health.Enabled || health.Capture != "full" {
		t.Errorf("health = %+v", health)
	}
}

func claudeCodeBody(prompt string) string {
	return `{"model":"` + claudeModel + `","stream":true,"max_tokens":64,"system":"plain","messages":[{"role":"user","content":"` + prompt + `"}]}`
}

// TestTracesClientHintsOverHTTP covers the optional client hints: the
// /p/<project> base URL, X-CPA-Git-Remote, and the removal of every X-CPA-*
// header before the upstream call.
func TestTracesClientHintsOverHTTP(t *testing.T) {
	upstream, upstreamHeaders := recordingUpstream(t)
	defer upstream.Close()
	server := newE2EServer(t, upstream.URL)
	h := server.Handler()
	headers := func(extra map[string]string) map[string]string {
		out := map[string]string{
			"Authorization": "Bearer " + e2eClientKey,
			"User-Agent":    "claude-cli/2.1.318 (external, cli)",
		}
		for k, v := range extra {
			out[k] = v
		}
		return out
	}

	// /p/<project>/v1/messages routes like /v1/messages and names the project.
	code, body := doJSON(t, h, http.MethodPost, "/p/path-app/v1/messages", claudeCodeBody("prompt via path"), headers(nil))
	if code != http.StatusOK || !strings.Contains(string(body), "toolu_1") {
		t.Fatalf("/p/path-app/v1/messages = %d: %s", code, body)
	}
	s := findSession(t, h, "prompt via path")
	if s.Project != "path-app" || s.ProjectSource != "path" {
		t.Errorf("path session = %+v", s)
	}

	// X-CPA-Git-Remote names the project through the remote, and an unknown
	// X-CPA-* header is dropped too.
	extra := map[string]string{"X-CPA-Git-Remote": "git@github.com:Acme/Remote-App.git", "X-CPA-Anything": "secret", "X-CPA-Cwd": "/Users/dev/code/some-folder"}
	if code, body = doJSON(t, h, http.MethodPost, "/v1/messages", claudeCodeBody("prompt via remote"), headers(extra)); code != http.StatusOK {
		t.Fatalf("remote request = %d: %s", code, body)
	}
	s = findSession(t, h, "prompt via remote")
	if s.Project != "Remote-App" || s.ProjectSource != "remote" || s.Folder != "~/code/some-folder" {
		t.Errorf("remote session = %+v", s)
	}

	// X-CPA-Project wins over the path.
	if code, body = doJSON(t, h, http.MethodPost, "/p/path-app/v1/messages", claudeCodeBody("prompt via header"), headers(map[string]string{"X-CPA-Project": "from-header"})); code != http.StatusOK {
		t.Fatalf("header request = %d: %s", code, body)
	}
	if s = findSession(t, h, "prompt via header"); s.Project != "from-header" || s.ProjectSource != "header" {
		t.Errorf("header session = %+v", s)
	}

	for _, got := range upstreamHeaders() {
		for name := range got {
			if strings.HasPrefix(strings.ToUpper(name), "X-CPA-") {
				t.Errorf("header %s reached the upstream", name)
			}
		}
	}

	// A client without a way to send headers still works on the old routes.
	if code, body = doJSON(t, h, http.MethodPost, "/p//v1/messages", claudeCodeBody("x"), headers(nil)); code != http.StatusBadRequest {
		t.Errorf("empty project name = %d: %s", code, body)
	}
	// Management stays where it is and never takes the prefix.
	var health struct {
		Enabled bool `json:"enabled"`
	}
	mgmtGet(t, h, "/traces/health", &health)
	if !health.Enabled {
		t.Errorf("management health = %+v", health)
	}
	if code, _ = doJSON(t, h, http.MethodGet, "/p/path-app/v8/management/traces/health", "", map[string]string{"Authorization": "Bearer " + e2eMgmtSecret}); code != http.StatusNotFound {
		t.Errorf("prefixed management = %d, want 404", code)
	}
	if code, _ = doJSON(t, h, http.MethodGet, "/p/path-app/healthz", "", nil); code != http.StatusOK {
		t.Errorf("prefixed healthz = %d, want 200", code)
	}
}
