package capture

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/gin-gonic/gin"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/httpwire"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/interfaces"
	"github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/usage"
)

type fakeSink struct {
	mu       sync.Mutex
	limit    int
	turns    []*TurnEvent
	usages   []*UsageEvent
	attempts []*AttemptEvent
}

func (f *fakeSink) SubmitTurn(ev *TurnEvent) {
	f.mu.Lock()
	f.turns = append(f.turns, ev)
	f.mu.Unlock()
}
func (f *fakeSink) SubmitUsage(ev *UsageEvent) {
	f.mu.Lock()
	f.usages = append(f.usages, ev)
	f.mu.Unlock()
}
func (f *fakeSink) SubmitAttempt(ev *AttemptEvent) {
	f.mu.Lock()
	f.attempts = append(f.attempts, ev)
	f.mu.Unlock()
}
func (f *fakeSink) ReplyLimit() int { return f.limit }

func withSink(t *testing.T, s Sink) {
	t.Helper()
	SetSink(s)
	t.Cleanup(func() { SetSink(nil) })
}

func newEngine(handler gin.HandlerFunc) *gin.Engine {
	gin.SetMode(gin.TestMode)
	engine := gin.New()
	engine.Use(Middleware())
	engine.POST("/v1/messages", handler)
	engine.POST("/v1/messages/count_tokens", handler)
	return engine
}

func TestDisabledTapsDoNothing(t *testing.T) {
	SetSink(nil)
	var sawHeader string
	var wrapped bool
	var ctxChanged bool
	engine := newEngine(func(c *gin.Context) {
		sawHeader = c.GetHeader(HeaderProject)
		_, wrapped = c.Writer.(*replyWriter)
		base := context.Background()
		ctxChanged = ContextWithGinTurn(base, c) != base
		c.String(http.StatusOK, "ok")
	})
	req := httptest.NewRequest(http.MethodPost, "/v1/messages", strings.NewReader("{}"))
	req.Header.Set(HeaderProject, "secret-project")
	engine.ServeHTTP(httptest.NewRecorder(), req)
	if sawHeader != "" {
		t.Errorf("X-CPA-Project reached the handler (it could be forwarded upstream)")
	}
	if wrapped || ctxChanged {
		t.Errorf("disabled middleware wrapped the writer (%v) or changed the context (%v)", wrapped, ctxChanged)
	}

	turn := newTurn("r", "/p", false, nil)
	ctx := WithTurn(context.Background(), turn)
	ObserveRequest(ctx, "claude", "m", true, []byte("body"))
	ObserveSession(ctx)
	if turn.observed || turn.body != nil {
		t.Errorf("ObserveRequest copied the body while disabled")
	}
	if got, tr := BeginWebsocketTurn(ctx, &gin.Context{Request: req}); tr != nil || got != ctx {
		t.Errorf("BeginWebsocketTurn started a turn while disabled")
	}
	var nilTurn *Turn
	nilTurn.FinishWebsocketTurn([]byte("[]"), nil, nil)
	RecordUsage(ctx, usage.Record{TraceID: "x"})
	RecordAttempt(ctx, AttemptEvent{})
}

func TestMiddlewareCapturesTurn(t *testing.T) {
	sink := &fakeSink{limit: 10}
	withSink(t, sink)
	engine := newEngine(func(c *gin.Context) {
		c.Set("userApiKey", "client-key")
		ctx := ContextWithGinTurn(context.Background(), c)
		ObserveRequest(ctx, "claude", "model-a", true, []byte(`{"messages":[]}`))
		ObserveRequest(ctx, "claude", "model-b", true, []byte(`ignored`))
		c.Header("Content-Type", "text/event-stream")
		c.Status(http.StatusOK)
		_, _ = c.Writer.WriteString(": keep-alive\n\n")
		_, _ = c.Writer.Write([]byte("data: 1\n\n"))
		_, _ = c.Writer.Write([]byte("data: 2\n\n"))
	})
	req := httptest.NewRequest(http.MethodPost, "/v1/messages", strings.NewReader("{}"))
	req.Header.Set("Authorization", "Bearer client-key")
	req.Header.Set("User-Agent", "claude-cli/1.2.3")
	req.Header.Set(HeaderCwd, "/tmp/x")
	req.Header.Set("Cookie", "a=b")
	rec := httptest.NewRecorder()
	engine.ServeHTTP(rec, req)
	if !strings.HasSuffix(rec.Body.String(), "data: 2\n\n") {
		t.Fatalf("client reply changed: %q", rec.Body.String())
	}
	if len(sink.turns) != 1 {
		t.Fatalf("turns = %d, want 1", len(sink.turns))
	}
	ev := sink.turns[0]
	if ev.Model != "model-a" || string(ev.Request) != `{"messages":[]}` || !ev.Stream || ev.Format != "claude" {
		t.Errorf("first observation must win: %+v", ev)
	}
	if string(ev.Reply) != ": keep-ali" || !ev.ReplyTruncated {
		t.Errorf("reply = %q truncated=%v, want the first 10 bytes", ev.Reply, ev.ReplyTruncated)
	}
	if ev.HTTPStatus != http.StatusOK || ev.APIKey != "client-key" || ev.ReplyContentType != "text/event-stream" {
		t.Errorf("status=%d key=%q type=%q", ev.HTTPStatus, ev.APIKey, ev.ReplyContentType)
	}
	if ev.FirstByteAt.IsZero() || ev.FirstByteAt.Before(ev.StartedAt) {
		t.Errorf("first byte time not set after keep-alive: %v", ev.FirstByteAt)
	}
	if ev.Headers.Get("Authorization") != "" || ev.Headers.Get("Cookie") != "" {
		t.Errorf("credentials stored in headers: %v", ev.Headers)
	}
	if ev.Headers.Get("User-Agent") != "claude-cli/1.2.3" || ev.Headers.Get(HeaderCwd) != "/tmp/x" {
		t.Errorf("allowlisted headers missing: %v", ev.Headers)
	}
}

func TestMiddlewareSkipsNonModelRequests(t *testing.T) {
	sink := &fakeSink{limit: 100}
	withSink(t, sink)
	engine := newEngine(func(c *gin.Context) { c.String(http.StatusOK, "ok") })
	for _, path := range []string{"/v1/messages", "/v1/messages/count_tokens"} {
		engine.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest(http.MethodPost, path, strings.NewReader("{}")))
	}
	if len(sink.turns) != 0 {
		t.Errorf("requests without a model execution became turns: %d", len(sink.turns))
	}
	for path, want := range map[string]bool{
		"/v1/chat/completions": true, "/v1/responses": true, "/backend-api/codex/responses": true,
		"/v1beta/models/gemini-2.5-pro:streamGenerateContent": true, "/v1/messages/count_tokens": false,
		"/v1/models": false, "/v8/management/traces/sessions": false,
	} {
		if IsModelPath(path) != want {
			t.Errorf("IsModelPath(%q) = %v", path, !want)
		}
	}
}

func TestMiddlewareRecordsPanicAsServerError(t *testing.T) {
	sink := &fakeSink{limit: 100}
	withSink(t, sink)
	gin.SetMode(gin.TestMode)
	engine := gin.New()
	engine.Use(gin.CustomRecovery(func(c *gin.Context, _ any) { c.AbortWithStatus(http.StatusInternalServerError) }))
	engine.Use(Middleware())
	engine.POST("/v1/messages", func(c *gin.Context) {
		ObserveRequest(ContextWithGinTurn(context.Background(), c), "claude", "m", false, []byte("{}"))
		panic("boom")
	})
	rec := httptest.NewRecorder()
	engine.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/v1/messages", strings.NewReader("{}")))
	if rec.Code != http.StatusInternalServerError {
		t.Fatalf("panic was swallowed: %d", rec.Code)
	}
	if len(sink.turns) != 1 || sink.turns[0].HTTPStatus != http.StatusInternalServerError {
		t.Errorf("panic turn = %+v", sink.turns)
	}
}

func TestWebsocketTurnAndJoins(t *testing.T) {
	sink := &fakeSink{limit: 1000}
	withSink(t, sink)
	req := httptest.NewRequest(http.MethodGet, "/v1/responses", nil)
	req.Header.Set("User-Agent", "codex_cli_rs/1")
	c, _ := gin.CreateTestContext(httptest.NewRecorder())
	c.Request = req
	c.Set(ginClientHeadersKey, http.Header{HeaderProject: {"p"}})
	ctx, turn := BeginWebsocketTurn(context.Background(), c)
	if turn == nil || TurnFromContext(ctx) != turn {
		t.Fatal("websocket turn not attached to the context")
	}
	ObserveRequest(ctx, "openai-response", "gpt", true, []byte(`{"input":"hi"}`))
	RecordUsage(ctx, usage.Record{Model: "gpt"})
	RecordAttempt(ctx, AttemptEvent{AuthID: "a", Success: true})
	turn.FinishWebsocketTurn([]byte(`[{"type":"message"}]`), &interfaces.ErrorMessage{StatusCode: 429}, nil)
	turn.FinishWebsocketTurn([]byte(`[]`), nil, nil)
	if len(sink.turns) != 1 {
		t.Fatalf("turns = %d, want exactly 1", len(sink.turns))
	}
	ev := sink.turns[0]
	if !ev.Websocket || ev.HTTPStatus != 429 || ev.ReplyKind != ReplyKindResponsesOutput || ev.Headers.Get(HeaderProject) != "p" {
		t.Errorf("websocket turn = %+v", ev)
	}
	if len(sink.usages) != 1 || sink.usages[0].TurnID != turn.ID() || len(sink.attempts) != 1 || sink.attempts[0].TurnID != turn.ID() {
		t.Errorf("joins: usages=%+v attempts=%+v", sink.usages, sink.attempts)
	}
	// Records without a turn or trace ID cannot be joined and are skipped.
	RecordUsage(context.Background(), usage.Record{})
	if len(sink.usages) != 1 {
		t.Errorf("unjoinable usage record was submitted")
	}
}

type failingFlushWriter struct {
	gin.ResponseWriter
}

func (failingFlushWriter) FlushError() error { return http.ErrHandlerTimeout }

func TestReplyWriterKeepsFlushErrors(t *testing.T) {
	c, _ := gin.CreateTestContext(httptest.NewRecorder())
	w := &replyWriter{ResponseWriter: failingFlushWriter{ResponseWriter: c.Writer}, limit: 10}
	if err := httpwire.FlushResponse(w); err != http.ErrHandlerTimeout {
		t.Fatalf("FlushResponse through the trace writer = %v, want the inner flush error", err)
	}
}
