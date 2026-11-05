package api

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/config"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/traces"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/traces/capture"
	"github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/usage"
)

// fakeEditor applies traces config writes to an in-memory config.
type fakeEditor struct {
	cfg    *config.Config
	writes []string
}

func (f *fakeEditor) SetTracesConfigValue(_ context.Context, path []string, value any) (*config.Config, error) {
	f.writes = append(f.writes, strings.Join(path, "."))
	next := f.cfg.CloneForRuntime()
	switch strings.Join(path, ".") {
	case "projects.rules":
		next.Traces.Projects.Rules = value.([]config.TracesProjectRule)
	case "prices.overrides":
		next.Traces.Prices.Overrides = value.([]config.TracesPriceOverride)
	default:
		return nil, fmt.Errorf("unexpected path %v", path)
	}
	f.cfg = next
	return next.CloneForRuntime(), nil
}

func ptr(v float64) *float64 { return &v }

func setup(t *testing.T, enabled bool) (*gin.Engine, *traces.Service, *fakeEditor) {
	t.Helper()
	gin.SetMode(gin.TestMode)
	empty := ""
	cfg := &config.Config{}
	cfg.AuthDir = t.TempDir()
	cfg.Traces = config.TracesConfig{Enabled: enabled, Capture: "full", Prices: config.TracesPricesConfig{
		Source:    &empty,
		Overrides: []config.TracesPriceOverride{{Model: "model-a", Input: ptr(1), Output: ptr(2)}},
	}}
	svc := traces.New(traces.Options{Accounts: func() []traces.AccountInfo {
		return []traces.AccountInfo{{ID: "acct-1", Label: "Personal", Email: "me@example.com", Provider: "claude", Tags: []string{"home"}}}
	}})
	svc.Apply(cfg)
	t.Cleanup(svc.Close)
	editor := &fakeEditor{cfg: cfg}
	h := New(svc, editor)
	engine := gin.New()
	h.Register(engine.Group("/v8/management"))
	return engine, svc, editor
}

// record sends one Claude turn through the real capture middleware.
func record(t *testing.T, svc *traces.Service, session, request, reply string, status int) {
	t.Helper()
	engine := gin.New()
	engine.Use(capture.Middleware())
	engine.POST("/v1/messages", func(c *gin.Context) {
		c.Set("userApiKey", "client-key")
		ctx := capture.ContextWithGinTurn(context.Background(), c)
		capture.ObserveRequest(ctx, "claude", "model-a", false, []byte(request))
		capture.RecordAttempt(ctx, capture.AttemptEvent{AuthID: "acct-1", Provider: "claude", Success: status < 400, HTTPStatus: status})
		capture.RecordUsage(ctx, usage.Record{AuthID: "acct-1", Provider: "claude", Model: "model-a", Detail: usage.Detail{
			TokenBreakdown: usage.NewIndependentTokenBreakdown(100, 0, 0, 10, 0, 0),
			InputTokens:    100, OutputTokens: 10,
		}})
		c.Data(status, "application/json", []byte(reply))
	})
	req := httptest.NewRequest(http.MethodPost, "/v1/messages", strings.NewReader(request))
	req.Header.Set("User-Agent", "claude-cli/2.1.0")
	req.Header.Set("X-Claude-Code-Session-Id", session)
	engine.ServeHTTP(httptest.NewRecorder(), req)
	svc.Flush()
}

func call(t *testing.T, engine *gin.Engine, method, path, body string) (int, []byte) {
	t.Helper()
	req := httptest.NewRequest(method, "/v8/management"+path, strings.NewReader(body))
	rec := httptest.NewRecorder()
	engine.ServeHTTP(rec, req)
	return rec.Code, rec.Body.Bytes()
}

func getObject(t *testing.T, engine *gin.Engine, path string) map[string]any {
	t.Helper()
	code, body := call(t, engine, http.MethodGet, path, "")
	if code != http.StatusOK {
		t.Fatalf("GET %s = %d: %s", path, code, body)
	}
	var out map[string]any
	if err := json.Unmarshal(body, &out); err != nil {
		t.Fatalf("GET %s: %v", path, err)
	}
	return out
}

func requireKeys(t *testing.T, where string, obj any, keys ...string) {
	t.Helper()
	m, ok := obj.(map[string]any)
	if !ok {
		t.Fatalf("%s is %T, want an object", where, obj)
	}
	for _, k := range keys {
		if _, present := m[k]; !present {
			t.Errorf("%s is missing %q (keys: %v)", where, k, m)
		}
	}
}

func first(t *testing.T, where string, v any) any {
	t.Helper()
	list, ok := v.([]any)
	if !ok || len(list) == 0 {
		t.Fatalf("%s is empty or not a list: %v", where, v)
	}
	return list[0]
}

var tokenKeys = []string{"input", "output", "reasoning", "cache_read", "cache_write_5m", "cache_write_1h", "total"}
var totKeys = []string{"cost_usd", "tokens", "active_ms", "model_ms", "sessions", "subagent_sessions", "turns", "failed_turns", "recovered_turns", "unpriced_turns"}
var sessionKeys = []string{"id", "title", "project", "folder", "client", "client_version", "model", "key_label", "tags", "machine", "turns", "tokens", "cost_usd", "active_ms", "model_ms", "errors", "subagents", "parent_id", "started_at", "last_at"}
var turnKeys = []string{"id", "seq", "session_id", "started_at", "ended_at", "latency_ms", "ttft_ms", "model", "served_model", "provider", "account", "status", "http_status", "error", "attempts", "tokens", "cost_usd", "tool_names"}
var blockKeys = []string{"kind", "text", "tool_name", "tool_call_id", "is_error", "truncated", "full_size", "diff"}

const sessionA = "6e1f4c5a-0000-4000-8000-00000000000a"

func seed(t *testing.T, svc *traces.Service) {
	t.Helper()
	sys := `"system":"Primary working directory: /Users/dev/code/shop"`
	user := `{"role":"user","content":"Add checkout ` + strings.Repeat("x", 30000) + `"}`
	record(t, svc, sessionA, `{`+sys+`,"messages":[`+user+`]}`,
		`{"type":"message","stop_reason":"tool_use","content":[{"type":"tool_use","id":"tu","name":"Write","input":{"file_path":"cart.go","content":"package cart\n"}}]}`, http.StatusOK)
	record(t, svc, sessionA, `{`+sys+`,"messages":[`+user+`,{"role":"assistant","content":[{"type":"tool_use","id":"tu","name":"Write","input":{"file_path":"cart.go","content":"package cart\n"}}]},{"role":"user","content":[{"type":"tool_result","tool_use_id":"tu","content":"ok"}]}]}`,
		`{"type":"error","error":{"type":"overloaded_error","message":"Overloaded"}}`, 529)
}

func TestEndpointShapes(t *testing.T) {
	engine, svc, editor := setup(t, true)
	seed(t, svc)

	health := getObject(t, engine, "/traces/health")
	requireKeys(t, "health", health, "enabled", "capture", "queue_depth", "queue_size", "dropped_events", "dropped_content", "db_bytes", "content_bytes", "last_write_at", "oldest_turn_at", "prices_updated_at", "prices_source")
	if health["enabled"] != true {
		t.Errorf("health.enabled = %v", health["enabled"])
	}

	stats := getObject(t, engine, "/traces/stats?group=project&bucket=day")
	requireKeys(t, "stats", stats, "from", "to", "totals", "previous", "groups", "buckets")
	requireKeys(t, "stats.totals", stats["totals"], totKeys...)
	requireKeys(t, "stats.previous", stats["previous"], totKeys...)
	requireKeys(t, "stats.totals.tokens", stats["totals"].(map[string]any)["tokens"], tokenKeys...)
	requireKeys(t, "stats.groups[0]", first(t, "groups", stats["groups"]), "key", "label", "cost_usd", "tokens", "turns", "sessions", "active_ms")
	bucket := first(t, "buckets", stats["buckets"])
	requireKeys(t, "stats.buckets[0]", bucket, "start", "groups")
	for _, group := range []string{"model", "client", "key", "tag", "account", "machine"} {
		getObject(t, engine, "/traces/stats?group="+group+"&bucket=week")
	}

	projects := getObject(t, engine, "/traces/projects")
	requireKeys(t, "projects", projects, "projects", "rules", "unassigned_sessions")
	project := first(t, "projects", projects["projects"])
	requireKeys(t, "projects[0]", project, "id", "name", "key", "sessions", "turns", "active_ms", "model_ms", "tokens", "cost_usd", "top_model", "last_active_at", "daily")
	requireKeys(t, "projects[0].daily[0]", first(t, "daily", project.(map[string]any)["daily"]), "day", "cost_usd", "active_ms")
	projectID := project.(map[string]any)["id"].(string)

	sessions := getObject(t, engine, "/traces/sessions?sort=cost&order=asc&limit=5")
	requireKeys(t, "sessions", sessions, "total", "summary", "sessions")
	requireKeys(t, "sessions.summary", sessions["summary"], "turns", "active_ms", "cost_usd")
	s := first(t, "sessions", sessions["sessions"]).(map[string]any)
	requireKeys(t, "sessions[0]", s, sessionKeys...)
	sessionID := s["id"].(string)

	detail := getObject(t, engine, "/traces/sessions/"+sessionID)
	requireKeys(t, "session detail", detail, "session", "turns", "lanes", "breaks")
	requireKeys(t, "session detail.session", detail["session"], append(sessionKeys, "device_id", "files_changed", "lines_added", "lines_removed")...)
	turn := first(t, "turns", detail["turns"]).(map[string]any)
	requireKeys(t, "turns[0]", turn, turnKeys...)
	requireKeys(t, "lanes[0]", first(t, "lanes", detail["lanes"]), "session_id", "label", "parent_turn_id", "turns")
	if files := detail["session"].(map[string]any)["files_changed"]; files != float64(1) {
		t.Errorf("files_changed = %v", files)
	}

	turnDetail := getObject(t, engine, "/traces/turns/"+turn["id"].(string))
	requireKeys(t, "turn detail", turnDetail, "turn", "attempts", "cost_breakdown", "facts", "content_available", "blocks")
	requireKeys(t, "attempts[0]", first(t, "attempts", turnDetail["attempts"]), "n", "account", "http_status", "status", "error", "latency_ms")
	requireKeys(t, "cost_breakdown[0]", first(t, "cost_breakdown", turnDetail["cost_breakdown"]), "type", "tokens", "cost_usd")
	requireKeys(t, "facts", turnDetail["facts"], "request_id", "folder", "client", "machine", "stop_reason", "parent_turn_id")
	var truncatedIndex = -1
	for i, b := range turnDetail["blocks"].([]any) {
		requireKeys(t, "block", b, blockKeys...)
		bm := b.(map[string]any)
		if bm["truncated"] == true && bm["kind"] == "user" {
			truncatedIndex = i
			if len(bm["text"].(string)) != traces.MaxBlockText {
				t.Errorf("truncated text length = %d", len(bm["text"].(string)))
			}
		}
		if bm["kind"] == "tool_call" {
			requireKeys(t, "diff", bm["diff"], "file", "old", "new")
		}
	}
	if truncatedIndex < 0 {
		t.Fatalf("the 30 KB prompt was not truncated")
	}
	full := getObject(t, engine, fmt.Sprintf("/traces/turns/%s/blocks/%d", turn["id"], truncatedIndex))
	if full["truncated"] != false || full["full_size"].(float64) != float64(len(full["text"].(string))) {
		t.Errorf("full block = truncated %v size %v len %d", full["truncated"], full["full_size"], len(full["text"].(string)))
	}

	errs := getObject(t, engine, "/traces/errors?limit=10")
	requireKeys(t, "errors", errs, "total", "groups", "items")
	requireKeys(t, "errors.groups[0]", first(t, "groups", errs["groups"]), "type", "account", "count", "last_at")
	requireKeys(t, "errors.items[0]", first(t, "items", errs["items"]), "turn_id", "session_id", "title", "project", "at", "account", "http_status", "error", "recovered")

	limits := getObject(t, engine, "/traces/limits")
	account := first(t, "accounts", limits["accounts"])
	requireKeys(t, "accounts[0]", account, "account_id", "label", "email", "provider", "plan", "tags", "windows", "overage_status", "history", "usage", "plan_price_usd")
	requireKeys(t, "accounts[0].usage", account.(map[string]any)["usage"], "turns", "tokens", "cost_usd")
	code, body := call(t, engine, http.MethodPut, "/traces/limits/acct-1", `{"plan_price_usd": 100}`)
	if code != http.StatusOK || !strings.Contains(string(body), `"plan_price_usd":100`) {
		t.Errorf("PUT limits = %d %s", code, body)
	}
	if code, _ = call(t, engine, http.MethodPut, "/traces/limits/acct-1", `{"plan_price_usd": "x"}`); code != http.StatusBadRequest {
		t.Errorf("invalid plan price = %d", code)
	}

	prices := getObject(t, engine, "/traces/prices")
	requireKeys(t, "prices", prices, "source", "updated_at", "rows")
	requireKeys(t, "prices.rows[0]", first(t, "rows", prices["rows"]), "model", "input", "output", "cache_read", "cache_write_5m", "cache_write_1h", "origin")
	code, body = call(t, engine, http.MethodPut, "/traces/prices", `{"overrides":[{"model":"model-a","input":5,"output":6,"cache_read":0.5,"cache-write-5m":7}]}`)
	if code != http.StatusOK || !strings.Contains(string(body), `"input":5`) || editor.cfg.Traces.Prices.Overrides[0].CacheWrite5m == nil {
		t.Errorf("PUT prices = %d %s", code, body)
	}

	code, body = call(t, engine, http.MethodPut, "/traces/project-rules", `{"rules":[{"match":"~/code/{name}","project":"acme-{name}"}]}`)
	if code != http.StatusOK || !strings.Contains(string(body), "acme-{name}") {
		t.Fatalf("PUT rules = %d %s", code, body)
	}
	rules := getObject(t, engine, "/traces/project-rules")
	requireKeys(t, "rules[0]", first(t, "rules", rules["rules"]), "match", "project")
	if after := getObject(t, engine, "/traces/projects"); first(t, "projects", after["projects"]).(map[string]any)["name"] != "acme-shop" {
		t.Errorf("rules change was not applied to stored sessions: %v", after["projects"])
	}
	projectID = traces.ProjectID("acme-shop")
	code, body = call(t, engine, http.MethodPut, "/traces/projects/"+projectID, `{"name":"Shop"}`)
	if code != http.StatusOK || !strings.Contains(string(body), `"name":"Shop"`) {
		t.Fatalf("PUT project = %d %s", code, body)
	}
	if got := editor.cfg.Traces.Projects.Rules; len(got) != 2 || got[0].Match != "~/code/shop" || got[0].Project != "Shop" {
		t.Errorf("rename rules = %+v", got)
	}

	for _, group := range []string{"project", "session"} {
		code, body = call(t, engine, http.MethodGet, "/traces/export.csv?group="+group+"&period=month", "")
		want := "period_start,project,sessions,turns,active_hours,model_hours,input_tokens,output_tokens,cache_read_tokens,cache_write_tokens,cost_usd"
		if group == "session" {
			want = "session_id,started_at,project,client,model,key,turns,active_hours,tokens,cost_usd,title"
		}
		lines := strings.Split(strings.TrimSpace(string(body)), "\n")
		if code != http.StatusOK || lines[0] != want || len(lines) != 2 {
			t.Errorf("export %s = %d %q", group, code, body)
		}
	}

	if code, _ = call(t, engine, http.MethodDelete, "/traces?session="+sessionID, ""); code != http.StatusBadRequest {
		t.Errorf("delete without confirm = %d", code)
	}
	if code, _ = call(t, engine, http.MethodDelete, "/traces?confirm=true", ""); code != http.StatusBadRequest {
		t.Errorf("delete without filter = %d", code)
	}
	if code, _ = call(t, engine, http.MethodDelete, "/traces?confirm=true&from=2026-01-01T00:00:00Z", ""); code != http.StatusBadRequest {
		t.Errorf("delete with half a range = %d", code)
	}
	from := time.Now().Add(-time.Hour).UTC().Format(time.RFC3339)
	to := time.Now().Add(time.Hour).UTC().Format(time.RFC3339)
	code, body = call(t, engine, http.MethodDelete, "/traces?confirm=true&from="+from+"&to="+to, "")
	if code != http.StatusOK || string(body) != `{"deleted_sessions":1,"deleted_turns":2}` {
		t.Errorf("delete = %d %s", code, body)
	}
}

func TestBadRequestsAndNotFound(t *testing.T) {
	engine, _, _ := setup(t, true)
	for path, want := range map[string]int{
		"/traces/stats?group=nope":                                         http.StatusBadRequest,
		"/traces/stats?bucket=year":                                        http.StatusBadRequest,
		"/traces/sessions?from=yesterday":                                  http.StatusBadRequest,
		"/traces/sessions?status=maybe":                                    http.StatusBadRequest,
		"/traces/sessions?limit=-1":                                        http.StatusBadRequest,
		"/traces/errors?from=2026-02-01T00:00:00Z&to=2026-01-01T00:00:00Z": http.StatusBadRequest,
		"/traces/sessions/unknown":                                         http.StatusNotFound,
		"/traces/turns/unknown":                                            http.StatusNotFound,
		"/traces/turns/unknown/blocks/0":                                   http.StatusNotFound,
		"/traces/turns/unknown/blocks/x":                                   http.StatusBadRequest,
		"/traces/sessions?q=checkout&client=claude":                        http.StatusOK,
	} {
		if code, body := call(t, engine, http.MethodGet, path, ""); code != want {
			t.Errorf("GET %s = %d, want %d: %s", path, code, want, body)
		}
	}
	if code, _ := call(t, engine, http.MethodPut, "/traces/project-rules", `{"rules":[{"match":""}]}`); code != http.StatusBadRequest {
		t.Errorf("invalid rules = %d", code)
	}
	if code, _ := call(t, engine, http.MethodPut, "/traces/prices", `{"overrides":[{"input":1}]}`); code != http.StatusBadRequest {
		t.Errorf("override without model = %d", code)
	}
	if code, _ := call(t, engine, http.MethodPut, "/traces/projects/nope", `{"name":"x"}`); code != http.StatusNotFound {
		t.Errorf("unknown project = %d", code)
	}
}

func TestFilterFieldsTakeSeveralValues(t *testing.T) {
	engine, svc, _ := setup(t, true)
	seed(t, svc)
	for path, want := range map[string]float64{
		"/traces/sessions?project=shop":                 1,
		"/traces/sessions?project=nope":                 0,
		"/traces/sessions?project=nope&project=shop":    1,
		"/traces/sessions?project=shop&project=shop":    1,
		"/traces/sessions?project=shop&machine=nowhere": 0,
		"/traces/sessions?project=+shop+&project=":      1,
	} {
		if got := getObject(t, engine, path)["total"]; got != want {
			t.Errorf("GET %s total = %v, want %v", path, got, want)
		}
	}
	tooMany := "/traces/sessions?tag=a"
	for i := 0; i < 50; i++ {
		tooMany += fmt.Sprintf("&tag=t%d", i)
	}
	if code, body := call(t, engine, http.MethodGet, tooMany, ""); code != http.StatusBadRequest || !strings.Contains(string(body), "too_many_values") {
		t.Errorf("51 tag values = %d %s, want 400 too_many_values", code, body)
	}
}

func TestDisabledEndpointsAnswerEmpty(t *testing.T) {
	engine, _, _ := setup(t, false)
	if health := getObject(t, engine, "/traces/health"); health["enabled"] != false {
		t.Errorf("health.enabled = %v", health["enabled"])
	}
	for _, path := range []string{"/traces/stats", "/traces/projects", "/traces/sessions", "/traces/errors", "/traces/limits", "/traces/prices", "/traces/project-rules"} {
		obj := getObject(t, engine, path)
		for key, value := range obj {
			if value == nil && key != "updated_at" && key != "cost_usd" {
				t.Errorf("%s.%s is null; lists must be empty, not null", path, key)
			}
		}
	}
	if sessions := getObject(t, engine, "/traces/sessions"); sessions["total"] != float64(0) || len(sessions["sessions"].([]any)) != 0 {
		t.Errorf("sessions = %v", sessions)
	}
	if code, _ := call(t, engine, http.MethodPut, "/traces/limits/a", `{"plan_price_usd":1}`); code != http.StatusConflict {
		t.Errorf("PUT limits while disabled = %d", code)
	}
	if code, _ := call(t, engine, http.MethodGet, "/traces/export.csv", ""); code != http.StatusOK {
		t.Errorf("export while disabled = %d", code)
	}
}

func TestPanelContractPricesAndSessionDelete(t *testing.T) {
	engine, svc, editor := setup(t, true)
	seed(t, svc)

	// The panel sends config key names and leaves out empty price fields.
	code, body := call(t, engine, http.MethodPut, "/traces/prices", `{"overrides":[{"model":"model-a","input":2,"output":4,"cache-read":0.2},{"model":"half-*","input":1}]}`)
	if code != http.StatusOK {
		t.Fatalf("PUT prices = %d %s", code, body)
	}
	saved := editor.cfg.Traces.Prices.Overrides
	if len(saved) != 2 || *saved[0].CacheRead != 0.2 || saved[0].CacheWrite5m != nil || saved[1].Output != nil {
		t.Fatalf("saved overrides = %+v", saved)
	}
	var prices struct {
		Rows []map[string]any `json:"rows"`
	}
	if err := json.Unmarshal(body, &prices); err != nil {
		t.Fatal(err)
	}
	rows := map[string]map[string]any{}
	for _, r := range prices.Rows {
		rows[r["model"].(string)] = r
	}
	// Override rows are listed as written in config, so the panel can save them
	// back unchanged. Derived prices for left-out fields are tested in package price.
	if a := rows["model-a"]; a["origin"] != "override" || a["cache_read"] != 0.2 || a["cache_write_5m"] != nil || a["cache_write_1h"] != nil {
		t.Errorf("model-a row = %v (left-out fields must stay null, not 0 or derived)", a)
	}
	if half := rows["half-*"]; half["output"] != nil {
		t.Errorf("a missing output price must stay null: %v", half)
	}
	if _, ok := svc.Prices().Lookup("half-model"); ok {
		t.Errorf("an override without an output price must not price a model as 0")
	}

	sessions := getObject(t, engine, "/traces/sessions")
	id := first(t, "sessions", sessions["sessions"]).(map[string]any)["id"].(string)
	code, body = call(t, engine, http.MethodDelete, "/traces?confirm=true&session="+id, "")
	if code != http.StatusOK || string(body) != `{"deleted_sessions":1,"deleted_turns":2}` {
		t.Fatalf("DELETE session = %d %s", code, body)
	}
	if code, _ = call(t, engine, http.MethodGet, "/traces/sessions/"+id, ""); code != http.StatusNotFound {
		t.Errorf("deleted session still answers: %d", code)
	}
}
