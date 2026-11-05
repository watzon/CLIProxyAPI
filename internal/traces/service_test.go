package traces

import (
	"context"
	"database/sql"
	"fmt"
	"net/http"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v8/internal/config"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/traces/capture"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/traces/store"
	"github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/usage"
)

var t0 = time.Date(2026, 10, 1, 12, 0, 0, 0, time.UTC)

func f64(v float64) *float64 { return &v }

func testConfig(t *testing.T) *config.Config {
	t.Helper()
	empty := ""
	cfg := &config.Config{}
	cfg.AuthDir = t.TempDir()
	cfg.Traces = config.TracesConfig{
		Enabled: true, Capture: CaptureFull, IdleGap: "10m",
		Prices: config.TracesPricesConfig{Source: &empty, Overrides: []config.TracesPriceOverride{
			{Model: "priced-model", Input: f64(2), Output: f64(10), CacheRead: f64(0.2), CacheWrite5m: f64(2.5), CacheWrite1h: f64(4)},
		}},
	}
	return cfg
}

func newTestService(t *testing.T, cfg *config.Config) *Service {
	t.Helper()
	svc := New(Options{Now: func() time.Time { return t0.Add(48 * time.Hour) }})
	svc.Apply(cfg)
	t.Cleanup(svc.Close)
	if !svc.Enabled() {
		t.Fatal("service did not start")
	}
	return svc
}

type turnSpec struct {
	id, session, parent, model, key string
	start                           time.Time
	latency                         time.Duration
	status                          int
	request, reply                  string
	headers                         http.Header
}

func (s turnSpec) event() *capture.TurnEvent {
	if s.model == "" {
		s.model = "priced-model"
	}
	if s.status == 0 {
		s.status = http.StatusOK
	}
	if s.headers == nil {
		s.headers = http.Header{"User-Agent": {"claude-cli/2.0.0"}}
	}
	return &capture.TurnEvent{
		ID: s.id, RequestID: "req-" + s.id, Path: "/v1/messages", Format: "claude", Model: s.model, Stream: false,
		StartedAt: s.start, EndedAt: s.start.Add(s.latency), HTTPStatus: s.status, Headers: s.headers, ClientIP: "10.0.0.5",
		APIKey: s.key, SessionID: s.session, ParentSessionID: s.parent, Request: []byte(s.request), Reply: []byte(s.reply),
		ReplyKind: capture.ReplyKindHTTP, ReplyContentType: "application/json",
	}
}

func usageEvent(turnID, model string, in, out int64) *capture.UsageEvent {
	return &capture.UsageEvent{TurnID: turnID, At: t0, Record: usage.Record{
		AuthID: "acct-1", Provider: "claude", Model: model,
		Detail: usage.Detail{InputTokens: in, OutputTokens: out, TokenBreakdown: usage.NewIndependentTokenBreakdown(in, 0, 0, out, 0, 0)},
	}}
}

const claudeReq = `{"model":"priced-model","system":"Primary working directory: /Users/zed/code/alpha","messages":[{"role":"user","content":"Build the alpha feature"}]}`
const claudeReply = `{"type":"message","model":"priced-model","stop_reason":"end_turn","content":[{"type":"text","text":"ok"}]}`

func TestSettingsParsing(t *testing.T) {
	for in, want := range map[string]time.Duration{"365d": 365 * 24 * time.Hour, "1d12h": 36 * time.Hour, "10m": 10 * time.Minute, "0.5d": 12 * time.Hour} {
		if got, err := ParseDuration(in); err != nil || got != want {
			t.Errorf("ParseDuration(%q) = %v %v, want %v", in, got, err, want)
		}
	}
	for _, bad := range []string{"", "xd", "5x"} {
		if _, err := ParseDuration(bad); err == nil {
			t.Errorf("ParseDuration(%q) accepted", bad)
		}
	}
	for in, want := range map[string]int64{"8MB": 8 << 20, "20GB": 20 << 30, "512KiB": 512 << 10, "100": 100, "1.5k": 1536} {
		if got, err := ParseSize(in); err != nil || got != want {
			t.Errorf("ParseSize(%q) = %d %v, want %d", in, got, err, want)
		}
	}
	cfg := &config.Config{}
	cfg.AuthDir = "/srv/auths"
	cfg.APIKeyTags = map[string][]string{"k1": {"work"}}
	cfg.Traces = config.TracesConfig{
		Capture: "bogus", IdleGap: "nope", MaxStreamCapture: "1KB", CaptureByKey: map[string]string{"k1": "FULL"},
		ClientLabels: map[string]string{"10.0.0.0/8": "lan", "10.0.0.5": "desk", "10.1.0.0/16": "lab", "junk": "x"},
	}
	s := SettingsFromConfig(cfg)
	if s.Capture != CaptureMetadata || s.IdleGap != DefaultIdleGap || s.MaxStreamCapture != 1024 || !s.RedactSecrets {
		t.Errorf("defaults not applied: %+v", s)
	}
	if s.Dir != filepath.Join("/srv/auths", "traces") || s.PriceSource != DefaultPriceSource {
		t.Errorf("dir=%q source=%q", s.Dir, s.PriceSource)
	}
	if s.CaptureLevel("k1") != CaptureFull || s.CaptureLevel("other") != CaptureMetadata {
		t.Errorf("capture-by-key not applied")
	}
	for ip, want := range map[string]string{"10.0.0.5": "desk", "10.1.2.3": "lab", "10.9.9.9": "lan", "192.168.1.1": "192.168.1.1", "::1": "::1"} {
		if got := s.Machine(ip); got != want {
			t.Errorf("Machine(%s) = %q, want %q", ip, got, want)
		}
	}
	if l := KeyLabel("client-key"); !strings.HasPrefix(l, "key-") || len(l) != 12 || KeyLabel("") != "" {
		t.Errorf("KeyLabel = %q", l)
	}
	if s.KeyTags["k1"][0] != "work" {
		t.Errorf("key tags not loaded")
	}
}

func TestProjectRulesAndResolution(t *testing.T) {
	rules := CompileRules([]config.TracesProjectRule{
		{Match: "~/code/{name}", Project: "{name}"},
		{Match: "/Users/zed/work/big-repo", Project: "Big"},
		{Match: "~/mono/{name}/src", Project: "mono-{name}"},
		{Match: "", Project: "ignored"},
	})
	if len(rules) != 3 {
		t.Fatalf("rules = %d", len(rules))
	}
	cases := map[string]string{
		"~/code/app":            "app",
		"~/code/app/sub/dir":    "app",
		"~/code":                "",
		"~/work/big-repo":       "Big",
		"~/work/big-repo/x":     "Big",
		"~/work/big-repo-other": "",
		"~/mono/svc/src/main":   "mono-svc",
		"~/mono/svc/docs":       "",
	}
	for folder, want := range cases {
		got := ""
		for _, r := range rules {
			if p, ok := r.Apply(folder); ok {
				got = p
				break
			}
		}
		if got != want {
			t.Errorf("rules on %q = %q, want %q", folder, got, want)
		}
	}
	parent := &store.Session{ProjectName: "parent-proj", ProjectKey: "pk"}
	type c struct {
		sess       store.Session
		parent     *store.Session
		name, from string
	}
	settings := SettingsFromConfig(nil)
	settings.Rules = rules
	for _, tc := range []c{
		{store.Session{HeaderProject: "hdr", Folder: "~/code/app"}, nil, "hdr", SourceHeader},
		{store.Session{HeaderProject: "hdr", PathProject: "pth"}, nil, "hdr", SourceHeader},
		{store.Session{PathProject: "pth", Folder: "~/code/app", GitRemote: "github.com/a/b"}, nil, "pth", SourcePath},
		{store.Session{Folder: "~/code/app", GitRemote: "github.com/a/b"}, nil, "app", SourceRule},
		{store.Session{Folder: "~/other/x", GitRemote: "github.com/a/b"}, nil, "b", SourceRemote},
		{store.Session{GitRemote: "github.com/a/b"}, nil, "b", SourceRemote},
		{store.Session{Folder: "~/other/x"}, nil, "x", SourceFolder},
		{store.Session{}, parent, "parent-proj", SourceInherited},
		{store.Session{ProjectName: "earlier", ProjectSource: SourceInherited}, nil, "earlier", SourceInherited},
		{store.Session{ProjectName: "earlier", ProjectSource: legacySourceInherit}, nil, "earlier", SourceInherited},
		// A name from any other source is derived again, so a stale name goes away.
		{store.Session{ProjectName: "stale", ProjectSource: SourceFolder}, nil, UnassignedName, SourceNone},
		{store.Session{}, nil, UnassignedName, SourceNone},
	} {
		name, _, source, _ := resolveProject(settings, &tc.sess, tc.parent)
		if name != tc.name || source != tc.from {
			t.Errorf("resolveProject(%+v) = %s/%s, want %s/%s", tc.sess, name, source, tc.name, tc.from)
		}
	}
}

func TestQueueDropsContentBeforeEvents(t *testing.T) {
	q := newQueue(10, 1024)
	for i := 0; i < 12; i++ {
		q.SubmitTurn(&capture.TurnEvent{ID: string(rune('a' + i)), Request: []byte("body"), Reply: []byte("reply")})
	}
	if q.depth() != 10 || q.droppedEvents.Load() != 2 || q.droppedContent.Load() != 5 {
		t.Fatalf("depth=%d droppedEvents=%d droppedContent=%d", q.depth(), q.droppedEvents.Load(), q.droppedContent.Load())
	}
	items := q.take(100)
	for i, item := range items {
		ev := item.(*capture.TurnEvent)
		if ev.ID == "" {
			t.Errorf("event %d lost its metadata", i)
		}
		if hasContent := len(ev.Request) > 0; hasContent != (i < 5) || ev.ContentDropped == (i < 5) {
			t.Errorf("event %d content=%v dropped=%v", i, hasContent, ev.ContentDropped)
		}
	}
	q.SubmitUsage(&capture.UsageEvent{TurnID: "x"})
	if q.depth() != 1 || q.contentBytes != 0 {
		t.Errorf("queue did not reset after take: depth=%d bytes=%d", q.depth(), q.contentBytes)
	}
}

func TestActiveTimeBreaksAndCost(t *testing.T) {
	svc := newTestService(t, testConfig(t))
	ctx := context.Background()
	specs := []turnSpec{
		{id: "t1", session: "s1", start: t0, latency: 30 * time.Second},
		{id: "t2", session: "s1", start: t0.Add(2 * time.Minute), latency: 20 * time.Second},
		// 30 minute break (> idle-gap 10m).
		{id: "t3", session: "s1", start: t0.Add(32 * time.Minute), latency: 40 * time.Second},
		// Subagent turn inside the tree, between t3 and t4.
		{id: "c1", session: "s1-child", parent: "s1", start: t0.Add(33 * time.Minute), latency: 10 * time.Second},
		{id: "t4", session: "s1", start: t0.Add(36 * time.Minute), latency: 50 * time.Second, model: "mystery-model"},
	}
	for _, s := range specs {
		s.request, s.reply = claudeReq, claudeReply
		svc.queue.SubmitTurn(s.event())
		svc.queue.SubmitUsage(usageEvent(s.id, s.model, 1000, 100))
	}
	svc.Flush()
	detail, err := svc.SessionDetail(ctx, "s1")
	if err != nil || detail == nil {
		t.Fatalf("detail: %v", err)
	}
	// Gaps between tree turn starts: 2m (counts), 30m (break), 1m (counts), 3m (counts); last turn latency 50s.
	wantActive := (2*time.Minute + time.Minute + 3*time.Minute + 50*time.Second).Milliseconds()
	if detail.Session.ActiveMs != wantActive {
		t.Errorf("active_ms = %d, want %d", detail.Session.ActiveMs, wantActive)
	}
	if detail.Session.ModelMs != (150 * time.Second).Milliseconds() {
		t.Errorf("model_ms = %d", detail.Session.ModelMs)
	}
	if len(detail.Breaks) != 1 || !detail.Breaks[0].From.Equal(t0.Add(2*time.Minute+20*time.Second)) || !detail.Breaks[0].To.Equal(t0.Add(32*time.Minute)) {
		t.Errorf("breaks = %+v", detail.Breaks)
	}
	if len(detail.Lanes) != 2 || detail.Lanes[1].SessionID != "s1-child" || detail.Lanes[1].ParentTurnID == nil || *detail.Lanes[1].ParentTurnID != "t3" {
		t.Errorf("lanes = %+v", detail.Lanes)
	}
	if detail.Session.Turns != 5 || detail.Session.Subagents != 1 || detail.Session.Project != "alpha" {
		t.Errorf("session = %+v", detail.Session.SessionView)
	}
	if detail.Session.CostUSD == nil {
		t.Fatal("partially priced session must keep the known cost")
	}
	// Four priced turns: 1000 input * $2/M + 100 output * $10/M each.
	if got := *detail.Session.CostUSD; got < 0.01199 || got > 0.01201 {
		t.Errorf("cost = %v, want 0.012", got)
	}
	var unknown *TurnView
	for i := range detail.Turns {
		if detail.Turns[i].ID == "t4" {
			unknown = &detail.Turns[i]
		}
	}
	if unknown == nil || unknown.CostUSD != nil || unknown.Tokens.Total != 1100 {
		t.Errorf("unknown model turn must have null cost: %+v", unknown)
	}
	stats, err := svc.Stats(ctx, store.TurnFilter{From: t0.Add(-time.Hour), To: t0.Add(time.Hour)}, "model", "day")
	if err != nil {
		t.Fatal(err)
	}
	if stats.Totals.UnpricedTurns != 1 || stats.Totals.Turns != 5 || stats.Totals.Sessions != 1 || stats.Totals.SubagentSessions != 1 || stats.Totals.ActiveMs != wantActive {
		t.Errorf("totals = %+v", stats.Totals)
	}
	for _, g := range stats.Groups {
		if g.Key == "mystery-model" && g.CostUSD != nil {
			t.Errorf("unpriced group must have null cost")
		}
	}
	list, err := svc.Sessions(ctx, store.TurnFilter{From: t0.Add(-time.Hour), To: t0.Add(time.Hour)}, SessionQuery{Limit: 10})
	if err != nil || list.Total != 1 || list.Sessions[0].Turns != 5 {
		t.Errorf("top-level list = %+v %v", list, err)
	}
	list, _ = svc.Sessions(ctx, store.TurnFilter{From: t0.Add(-time.Hour), To: t0.Add(time.Hour)}, SessionQuery{Limit: 10, IncludeSubagents: true})
	if list.Total != 2 {
		t.Errorf("include_subagents total = %d", list.Total)
	}
}

func openDB(t *testing.T, svc *Service) *sql.DB {
	t.Helper()
	db, err := sql.Open("sqlite", "file:"+filepath.ToSlash(filepath.Join(svc.Settings().Dir, store.DBFileName)))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	return db
}

func count(t *testing.T, db *sql.DB, query string) int {
	t.Helper()
	var n int
	if err := db.QueryRow(query).Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n
}

func TestDedupAndRedaction(t *testing.T) {
	svc := newTestService(t, testConfig(t))
	ctx := context.Background()
	secret := "sk-" + "ant-api03-ABCDEFGHIJKLMNOPQRSTUVWXYZ" // split so secret scanners skip the fake key
	system := `"system":"You are a helper."`
	user := `{"role":"user","content":"Deploy with key ` + secret + `"}`
	assistant := `{"role":"assistant","content":[{"type":"text","text":"Running."},{"type":"tool_use","id":"tu1","name":"Bash","input":{"command":"deploy"}}]}`
	result := `{"role":"user","content":[{"type":"tool_result","tool_use_id":"tu1","content":"Exit code: 2\nfailed","is_error":false}]}`
	first := turnSpec{id: "d1", session: "dedup", start: t0, latency: time.Second,
		request: `{` + system + `,"messages":[` + user + `]}`,
		reply:   `{"type":"message","content":[{"type":"text","text":"Running."},{"type":"tool_use","id":"tu1","name":"Bash","input":{"command":"deploy"}}]}`}
	second := turnSpec{id: "d2", session: "dedup", start: t0.Add(time.Minute), latency: time.Second,
		request: `{` + system + `,"messages":[` + user + `,` + assistant + `,` + result + `]}`, reply: claudeReply}
	svc.queue.SubmitTurn(first.event())
	svc.queue.SubmitTurn(second.event())
	svc.Flush()
	db := openDB(t, svc)
	// Units: system, user, assistant (reply of d1 and history of d2), tool result, final reply.
	if n := count(t, db, `SELECT COUNT(*) FROM blobs`); n != 5 {
		t.Errorf("blobs = %d, want 5 (each message stored once)", n)
	}
	if n := count(t, db, `SELECT COUNT(*) FROM turn_blobs WHERE turn_id = 'd2'`); n != 5 {
		t.Errorf("turn d2 refs = %d, want 5", n)
	}
	detail, err := svc.TurnDetail(ctx, "d2")
	if err != nil || detail == nil {
		t.Fatal(err)
	}
	var got []string
	for _, b := range detail.Blocks {
		got = append(got, b.Kind)
	}
	if strings.Join(got, ",") != "tool_result,assistant" {
		t.Errorf("turn d2 blocks = %v", got)
	}
	if !detail.Blocks[0].IsError || detail.Blocks[0].ToolName == nil || *detail.Blocks[0].ToolName != "Bash" {
		t.Errorf("tool result = %+v", detail.Blocks[0])
	}
	first1, _ := svc.TurnDetail(ctx, "d1")
	for _, b := range first1.Blocks {
		if strings.Contains(b.Text, secret) {
			t.Errorf("secret stored: %q", b.Text)
		}
		if b.Kind == "tool_call" && !b.IsError {
			t.Errorf("tool call with a failed result must be marked failed")
		}
	}
	sess, _ := svc.SessionDetail(ctx, "dedup")
	if strings.Contains(sess.Session.Title, secret) || !strings.Contains(sess.Session.Title, "[redacted:anthropic-key]") {
		t.Errorf("title not redacted: %q", sess.Session.Title)
	}
	block, err := svc.TurnBlock(ctx, "d1", 0)
	if err != nil || block == nil || block.Kind != "system" {
		t.Errorf("block 0 = %+v %v", block, err)
	}
	if b, _ := svc.TurnBlock(ctx, "d1", 99); b != nil {
		t.Errorf("out-of-range block returned")
	}
}

func TestMetadataCaptureStoresNoContent(t *testing.T) {
	cfg := testConfig(t)
	cfg.Traces.Capture = CaptureMetadata
	cfg.Traces.CaptureByKey = map[string]string{"vip": CaptureFull}
	cfg.APIKeyTags = map[string][]string{"vip": {"gold"}}
	svc := newTestService(t, cfg)
	svc.queue.SubmitTurn(turnSpec{id: "m1", session: "meta", start: t0, latency: time.Second, request: claudeReq, reply: claudeReply}.event())
	svc.queue.SubmitTurn(turnSpec{id: "m2", session: "meta2", start: t0, latency: time.Second, request: claudeReq, reply: claudeReply, key: "vip"}.event())
	svc.Flush()
	d1, _ := svc.TurnDetail(context.Background(), "m1")
	d2, _ := svc.TurnDetail(context.Background(), "m2")
	if d1.ContentAvailable || len(d1.Blocks) != 0 || !d2.ContentAvailable {
		t.Errorf("metadata turn content=%v blocks=%d; per-key full content=%v", d1.ContentAvailable, len(d1.Blocks), d2.ContentAvailable)
	}
	sess, _ := svc.SessionDetail(context.Background(), "meta")
	if sess.Session.Title != "Build the alpha feature" || sess.Session.Folder != "~/code/alpha" {
		t.Errorf("metadata mode must still derive title and folder: %+v", sess.Session.SessionView)
	}
	window := store.TurnFilter{From: t0.Add(-time.Hour), To: t0.Add(time.Hour)}
	tagged := window
	tagged.Tag = []string{"gold"}
	list, err := svc.Sessions(context.Background(), tagged, SessionQuery{Limit: 10})
	if err != nil || list.Total != 1 || list.Sessions[0].ID != "meta2" || list.Sessions[0].Tags[0] != "gold" || list.Sessions[0].KeyLabel != KeyLabel("vip") {
		t.Errorf("tag filter = %+v %v", list, err)
	}
	untagged := window
	untagged.Tag = []string{GroupUntagged}
	if list, _ = svc.Sessions(context.Background(), untagged, SessionQuery{Limit: 10}); list.Total != 1 || list.Sessions[0].ID != "meta" {
		t.Errorf("untagged filter = %+v", list.Sessions)
	}
	byKey := window
	byKey.Key = []string{KeyLabel("vip")}
	if list, _ = svc.Sessions(context.Background(), byKey, SessionQuery{Limit: 10}); list.Total != 1 {
		t.Errorf("key filter total = %d", list.Total)
	}
	byFolder := window
	byFolder.Folder = []string{"~/code"}
	if list, _ = svc.Sessions(context.Background(), byFolder, SessionQuery{Limit: 10}); list.Total != 2 {
		t.Errorf("folder prefix filter total = %d", list.Total)
	}
	byFolder.Folder = []string{"~/cod"}
	if list, _ = svc.Sessions(context.Background(), byFolder, SessionQuery{Limit: 10}); list.Total != 0 {
		t.Errorf("folder prefix must match whole segments, total = %d", list.Total)
	}
	eitherTag := window
	eitherTag.Tag = []string{"gold", GroupUntagged}
	if list, _ = svc.Sessions(context.Background(), eitherTag, SessionQuery{Limit: 10}); list.Total != 2 {
		t.Errorf("two tag values must match either one, total = %d", list.Total)
	}
	tagAndFolder := eitherTag
	tagAndFolder.Tag = []string{"gold"}
	tagAndFolder.Folder = []string{"~/elsewhere", "~/code"}
	if list, _ = svc.Sessions(context.Background(), tagAndFolder, SessionQuery{Limit: 10}); list.Total != 1 || list.Sessions[0].ID != "meta2" {
		t.Errorf("fields must all match, values of one field any: %+v", list.Sessions)
	}
}

func TestRetention(t *testing.T) {
	cfg := testConfig(t)
	svc := newTestService(t, cfg)
	ctx := context.Background()
	for i, start := range []time.Time{t0.Add(-400 * 24 * time.Hour), t0.Add(-10 * 24 * time.Hour), t0} {
		id := []string{"old", "mid", "new"}[i]
		req := `{"messages":[{"role":"user","content":"unique prompt ` + id + `"}]}`
		svc.queue.SubmitTurn(turnSpec{id: id, session: "ret-" + id, start: start, latency: time.Second, request: req, reply: claudeReply}.event())
	}
	svc.Flush()
	st := svc.Store()
	settings := svc.Settings()
	if err := RunRetention(ctx, st, settings, t0); err != nil {
		t.Fatal(err)
	}
	db := openDB(t, svc)
	if n := count(t, db, `SELECT COUNT(*) FROM turns WHERE content_available = 1`); n != 2 {
		t.Errorf("turns with content after age retention = %d, want 2", n)
	}
	if n := count(t, db, `SELECT COUNT(*) FROM turns`); n != 3 {
		t.Errorf("metadata rows must stay: %d", n)
	}
	if n := count(t, db, `SELECT COUNT(*) FROM blobs WHERE hash NOT IN (SELECT hash FROM turn_blobs)`); n != 0 {
		t.Errorf("unreferenced blobs left: %d", n)
	}
	settings.ContentMaxSize = 1
	if err := RunRetention(ctx, st, settings, t0); err != nil {
		t.Fatal(err)
	}
	if n := count(t, db, `SELECT COUNT(*) FROM turns WHERE content_available = 1`); n != 0 {
		t.Errorf("size retention left %d turns with content", n)
	}
	if stats, _ := st.Stats(ctx); stats.ContentBytes != 0 {
		t.Errorf("content bytes = %d", stats.ContentBytes)
	}
	old, _ := svc.TurnDetail(ctx, "old")
	if old == nil || old.ContentAvailable || len(old.Blocks) != 0 {
		t.Errorf("old turn after retention = %+v", old)
	}
}

func TestNormalizeTokens(t *testing.T) {
	anthropic := usage.Record{Provider: "claude", Detail: usage.Detail{InputTokens: 100, OutputTokens: 50, ReasoningTokens: 10, CacheReadTokens: 2000, CacheCreationTokens: 300, CacheCreation1hTokens: usage.Int64Ptr(100)}}
	if got := NormalizeTokens(anthropic); got != (store.Tokens{Input: 100, Output: 50, Reasoning: 10, CacheRead: 2000, CacheWrite5m: 200, CacheWrite1h: 100, Total: 2450}) {
		t.Errorf("anthropic fallback = %+v", got)
	}
	openai := usage.Record{Provider: "openai", Detail: usage.Detail{InputTokens: 1000, OutputTokens: 80, ReasoningTokens: 20, CachedTokens: 600}}
	if got := NormalizeTokens(openai); got != (store.Tokens{Input: 400, Output: 80, Reasoning: 20, CacheRead: 600, Total: 1080}) {
		t.Errorf("openai fallback = %+v", got)
	}
	canonical := usage.Record{Provider: "anything", Detail: usage.Detail{TokenBreakdown: usage.NewSubsetTokenBreakdown(1000, 600, 0, 80, 20, 1080)}}
	if got := NormalizeTokens(canonical); got != (store.Tokens{Input: 400, Output: 80, Reasoning: 20, CacheRead: 600, Total: 1080}) {
		t.Errorf("breakdown = %+v", got)
	}
}

func TestParseLimitHeaders(t *testing.T) {
	now := time.Unix(1_800_000_000, 0).UTC()
	h := http.Header{}
	h.Set("Anthropic-Ratelimit-Unified-5h-Status", "allowed")
	h.Set("Anthropic-Ratelimit-Unified-5h-Utilization", "0.25")
	h.Set("Anthropic-Ratelimit-Unified-5h-Reset", "1800003600")
	h.Set("Anthropic-Ratelimit-Unified-7d_oi-Status", "allowed_warning")
	h.Set("Anthropic-Ratelimit-Unified-Overage-Status", "rejected")
	h.Set("Anthropic-Ratelimit-Unified-Status", "allowed")
	h.Set("X-Codex-Secondary-Used-Percent", "100")
	h.Set("X-Codex-Secondary-Reset-At", "1800007200")
	h.Set("X-Codex-Additional-Gpt5Pro-Primary-Used-Percent", "3")
	h.Set("X-Codex-Plan-Type", "pro")
	snap := ParseLimitHeaders(h, now)
	byName := map[string]LimitWindow{}
	for _, w := range snap.Windows {
		byName[w.Name] = w
	}
	if w := byName["5h"]; w.UsedPct == nil || *w.UsedPct != 25 || w.Status != "allowed" || w.ResetsAt == nil || w.ResetsAt.Unix() != 1800003600 {
		t.Errorf("5h = %+v", w)
	}
	if w := byName["7d_oi"]; w.UsedPct != nil || w.Status != "allowed_warning" {
		t.Errorf("7d_oi = %+v", w)
	}
	if w := byName["secondary"]; w.Status != "rejected" || w.ResetsAt == nil {
		t.Errorf("secondary = %+v", w)
	}
	if _, ok := byName["gpt5pro-primary"]; !ok {
		t.Errorf("additional codex window missing: %v", byName)
	}
	if snap.OverageStatus != "rejected" || snap.Plan != "pro" || len(snap.Windows) != 4 {
		t.Errorf("snapshot = %+v", snap)
	}
}

func TestLimitSamplesOnlyOnChangeAndLateJoin(t *testing.T) {
	svc := newTestService(t, testConfig(t))
	ctx := context.Background()
	h := http.Header{}
	h.Set("Anthropic-Ratelimit-Unified-5h-Utilization", "0.10")
	rec := func(util string) *capture.UsageEvent {
		hh := h.Clone()
		hh.Set("Anthropic-Ratelimit-Unified-5h-Utilization", util)
		ev := usageEvent("", "priced-model", 10, 1)
		ev.TraceID = "req-late"
		ev.Record.ResponseHeaders = hh
		return ev
	}
	// Usage arrives before its turn and is joined by the inbound request ID.
	svc.queue.SubmitUsage(rec("0.10"))
	svc.queue.SubmitUsage(rec("0.10"))
	svc.queue.SubmitUsage(rec("0.30"))
	svc.Flush()
	svc.queue.SubmitTurn(turnSpec{id: "late", session: "late-s", start: t0, latency: time.Second, request: claudeReq, reply: claudeReply}.event())
	svc.Flush()
	svc.Flush()
	detail, _ := svc.TurnDetail(ctx, "late")
	if detail == nil || detail.Turn.Tokens.Total != 33 || detail.Turn.Account != "acct-1" {
		t.Fatalf("late usage not joined: %+v", detail)
	}
	limits, err := svc.Limits(ctx, store.TurnFilter{From: t0.Add(-time.Hour), To: t0.Add(time.Hour)})
	if err != nil || len(limits.Accounts) != 1 {
		t.Fatalf("limits = %+v %v", limits, err)
	}
	acct := limits.Accounts[0]
	if len(acct.Windows) != 1 || *acct.Windows[0].UsedPct != 30 {
		t.Errorf("windows = %+v", acct.Windows)
	}
	if acct.Usage.Turns != 1 {
		t.Errorf("usage = %+v", acct.Usage)
	}
	if err = svc.SetPlanPrice(ctx, "acct-1", f64(200)); err != nil {
		t.Fatal(err)
	}
	limits, _ = svc.Limits(ctx, store.TurnFilter{From: t0.Add(-time.Hour), To: t0.Add(time.Hour)})
	if p := limits.Accounts[0].PlanPriceUSD; p == nil || *p != 200 {
		t.Errorf("plan price = %v", p)
	}
}

func TestRetriedAndFailedTurns(t *testing.T) {
	svc := newTestService(t, testConfig(t))
	ctx := context.Background()
	svc.queue.SubmitTurn(turnSpec{id: "r1", session: "err", start: t0, latency: 2 * time.Second, request: claudeReq, reply: claudeReply}.event())
	svc.queue.SubmitAttempt(&capture.AttemptEvent{TurnID: "r1", At: t0.Add(time.Second), AuthID: "acct-a", HTTPStatus: 429, Error: "rate limited"})
	svc.queue.SubmitAttempt(&capture.AttemptEvent{TurnID: "r1", At: t0.Add(2 * time.Second), AuthID: "acct-b", Success: true, HTTPStatus: 200})
	svc.queue.SubmitTurn(turnSpec{id: "f1", session: "err", start: t0.Add(time.Minute), latency: time.Second, status: 529, request: claudeReq, reply: `{"type":"error","error":{"type":"overloaded_error","message":"Overloaded"}}`}.event())
	svc.Flush()
	r1, _ := svc.TurnDetail(ctx, "r1")
	if r1.Turn.Status != StatusRetried || r1.Turn.Attempts != 2 || r1.Turn.Account != "acct-b" || len(r1.Attempts) != 2 || r1.Attempts[0].Status != "error" || r1.Attempts[0].LatencyMs != 1000 {
		t.Errorf("retried turn = %+v attempts=%+v", r1.Turn, r1.Attempts)
	}
	f1, _ := svc.TurnDetail(ctx, "f1")
	if f1.Turn.Status != StatusError || f1.Turn.Error == nil || *f1.Turn.Error != "Overloaded" {
		t.Errorf("failed turn = %+v", f1.Turn)
	}
	errs, err := svc.Errors(ctx, store.TurnFilter{From: t0.Add(-time.Hour), To: t0.Add(time.Hour)}, 10, 0)
	if err != nil || errs.Total != 2 {
		t.Fatalf("errors = %+v %v", errs, err)
	}
	if errs.Items[0].TurnID != "f1" || errs.Items[0].Recovered || errs.Items[1].TurnID != "r1" || !errs.Items[1].Recovered {
		t.Errorf("items = %+v", errs.Items)
	}
	types := map[string]bool{}
	for _, g := range errs.Groups {
		types[g.Type] = true
	}
	if !types["rate_limit"] || !types["overloaded"] {
		t.Errorf("groups = %+v", errs.Groups)
	}
	stats, _ := svc.Stats(ctx, store.TurnFilter{From: t0.Add(-time.Hour), To: t0.Add(time.Hour)}, "project", "hour")
	if stats.Totals.FailedTurns != 1 || stats.Totals.RecoveredTurns != 1 {
		t.Errorf("totals = %+v", stats.Totals)
	}
}

func TestDisabledServiceIsInert(t *testing.T) {
	svc := New(Options{})
	cfg := testConfig(t)
	cfg.Traces.Enabled = false
	svc.Apply(cfg)
	ctx := context.Background()
	if svc.Enabled() || capture.Enabled() || svc.Store() != nil {
		t.Fatal("disabled service started")
	}
	if h := svc.Health(ctx); h.Enabled {
		t.Errorf("health enabled = true")
	}
	stats, err := svc.Stats(ctx, store.TurnFilter{From: t0, To: t0.Add(time.Hour)}, "project", "day")
	if err != nil || stats.Totals.Turns != 0 || stats.Groups == nil {
		t.Errorf("stats = %+v %v", stats, err)
	}
	if d, err := svc.SessionDetail(ctx, "x"); d != nil || err != nil {
		t.Errorf("detail = %+v %v", d, err)
	}
	// Enable, then disable again: recording stops and the sink is removed.
	cfg.Traces.Enabled = true
	svc.Apply(cfg)
	if !svc.Enabled() || !capture.Enabled() {
		t.Fatal("service did not start")
	}
	cfg.Traces.Enabled = false
	svc.Apply(cfg)
	if svc.Enabled() || capture.Enabled() {
		t.Fatal("service did not stop")
	}
}

func TestReresolveProjectsAfterRuleChange(t *testing.T) {
	cfg := testConfig(t)
	svc := newTestService(t, cfg)
	ctx := context.Background()
	svc.queue.SubmitTurn(turnSpec{id: "p1", session: "proj", start: t0, latency: time.Second, request: claudeReq, reply: claudeReply}.event())
	svc.Flush()
	cfg.Traces.Projects.Rules = []config.TracesProjectRule{{Match: "~/code/{name}", Project: "team-{name}"}}
	svc.Apply(cfg)
	if err := svc.ReresolveProjects(ctx); err != nil {
		t.Fatal(err)
	}
	projects, err := svc.Projects(ctx, store.TurnFilter{From: t0.Add(-time.Hour), To: t0.Add(time.Hour)})
	if err != nil || len(projects.Projects) != 1 || projects.Projects[0].Name != "team-alpha" || projects.Projects[0].ID != ProjectID("team-alpha") {
		t.Fatalf("projects = %+v %v", projects, err)
	}
	if len(projects.Rules) != 1 || projects.Rules[0].Hits != 1 || len(projects.Projects[0].Daily) != 1 {
		t.Errorf("rules = %+v daily = %d", projects.Rules, len(projects.Projects[0].Daily))
	}
	name, folders, _ := svc.ProjectFolders(ctx, ProjectID("team-alpha"))
	if name != "team-alpha" || len(folders) != 1 || folders[0] != "~/code/alpha" {
		t.Errorf("folders = %s %v", name, folders)
	}
	res, err := svc.Delete(ctx, store.DeleteFilter{Project: "team-alpha"})
	if err != nil || res.DeletedSessions != 1 || res.DeletedTurns != 1 {
		t.Errorf("delete = %+v %v", res, err)
	}
}

func TestPriceCatalogFetchAndOfflineCache(t *testing.T) {
	cfg := testConfig(t)
	source := "https://catalog.example.invalid/api.json"
	cfg.Traces.Prices.Source = &source
	cfg.Traces.Prices.Overrides = nil
	catalog := []byte(`{"acme":{"models":{"acme-1":{"id":"acme-1","cost":{"input":4,"output":8}}}}}`)
	var fetched []string
	svc := New(Options{Now: time.Now, Fetcher: func(_ context.Context, url string) ([]byte, error) {
		fetched = append(fetched, url)
		return catalog, nil
	}})
	svc.Apply(cfg)
	deadline := time.Now().Add(5 * time.Second)
	for {
		if _, updated := svc.Prices().Info(); !updated.IsZero() {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("catalog was never fetched")
		}
		time.Sleep(10 * time.Millisecond)
	}
	if row, ok := svc.Prices().Lookup("acme-1"); !ok || row.Input != 4 {
		t.Errorf("lookup after fetch = %+v %v", row, ok)
	}
	svc.Close()
	if len(fetched) != 1 || fetched[0] != source {
		t.Errorf("fetches = %v", fetched)
	}

	// An offline restart keeps the prices cached in the trace database.
	offline := New(Options{Now: time.Now, Fetcher: func(context.Context, string) ([]byte, error) {
		return nil, context.DeadlineExceeded
	}})
	offline.Apply(cfg)
	defer offline.Close()
	if row, ok := offline.Prices().Lookup("acme-1"); !ok || row.Output != 8 {
		t.Errorf("cached lookup = %+v %v", row, ok)
	}
	view, err := offline.PricesView(context.Background())
	if err != nil || view.Source != source || view.UpdatedAt == nil {
		t.Errorf("prices view = %+v %v", view, err)
	}
}

func TestProjectIDMatchesStatsKeyAndFilter(t *testing.T) {
	svc := newTestService(t, testConfig(t))
	ctx := context.Background()
	noFolder := `{"messages":[{"role":"user","content":"No folder here"}]}`
	svc.queue.SubmitTurn(turnSpec{id: "u1", session: "loose", start: t0, latency: time.Second, request: noFolder, reply: claudeReply}.event())
	svc.queue.SubmitTurn(turnSpec{id: "a1", session: "alpha", start: t0, latency: time.Second, request: claudeReq, reply: claudeReply}.event())
	svc.Flush()
	window := store.TurnFilter{From: t0.Add(-time.Hour), To: t0.Add(time.Hour)}
	projects, err := svc.Projects(ctx, window)
	if err != nil {
		t.Fatal(err)
	}
	ids := map[string]string{}
	for _, p := range projects.Projects {
		ids[p.Name] = p.ID
	}
	if ids[UnassignedName] != "unassigned" || ids["alpha"] != ProjectID("alpha") || projects.UnassignedSessions != 1 {
		t.Fatalf("project ids = %v unassigned_sessions = %d", ids, projects.UnassignedSessions)
	}
	stats, _ := svc.Stats(ctx, window, "project", "day")
	keys := map[string]bool{}
	for _, g := range stats.Groups {
		keys[g.Key] = true
	}
	if !keys["unassigned"] || !keys[ids["alpha"]] {
		t.Errorf("stats keys %v do not match project ids %v", keys, ids)
	}
	for id, want := range map[string]string{"unassigned": "loose", ids["alpha"]: "alpha"} {
		f := window
		f.Project = []string{id}
		list, errList := svc.Sessions(ctx, f, SessionQuery{Limit: 10})
		if errList != nil || list.Total != 1 || list.Sessions[0].ID != want {
			t.Errorf("project=%s sessions = %+v %v", id, list.Sessions, errList)
		}
	}
}

func TestDefaultConfigYAMLMatchesRuntimeDefaults(t *testing.T) {
	cfg, err := config.ParseConfigBytes([]byte("traces:\n" + indent(DefaultConfigYAML)))
	if err != nil {
		t.Fatal(err)
	}
	cfg.AuthDir = "/auth"
	fromYAML := SettingsFromConfig(cfg)
	empty := &config.Config{}
	empty.AuthDir = "/auth"
	defaults := SettingsFromConfig(empty)
	fromYAML.CaptureByKey, defaults.CaptureByKey = nil, nil
	fromYAML.KeyTags, defaults.KeyTags = nil, nil
	if fmt.Sprintf("%+v", fromYAML) != fmt.Sprintf("%+v", defaults) {
		t.Errorf("DefaultConfigYAML differs from the runtime defaults:\n%+v\n%+v", fromYAML, defaults)
	}
}

func indent(s string) string {
	lines := strings.Split(strings.TrimRight(s, "\n"), "\n")
	for i := range lines {
		lines[i] = "  " + lines[i]
	}
	return strings.Join(lines, "\n") + "\n"
}

// A model priced by a glob override must not show up as an extra override row:
// the panel saves every override row back to config.
func TestPricesViewListsOnlyConfigOverrides(t *testing.T) {
	cfg := testConfig(t)
	cfg.Traces.Prices.Overrides = []config.TracesPriceOverride{{Model: "priced-*", Input: f64(2), Output: f64(10)}}
	svc := newTestService(t, cfg)
	svc.queue.SubmitTurn(turnSpec{id: "g1", session: "glob", start: t0, latency: time.Second, request: claudeReq, reply: claudeReply}.event())
	svc.Flush()
	view, err := svc.PricesView(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(view.Rows) != 1 || view.Rows[0].Model != "priced-*" || view.Rows[0].Origin != "override" || view.Rows[0].CacheRead != nil {
		t.Fatalf("rows = %+v, want only the config override as written", view.Rows)
	}
}
