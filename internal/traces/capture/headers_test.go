package capture

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/gin-gonic/gin"
)

func TestMiddlewareRemovesEveryXCPAHeader(t *testing.T) {
	for _, enabled := range []bool{false, true} {
		name := "disabled"
		if enabled {
			name = "enabled"
		}
		t.Run(name, func(t *testing.T) {
			sink := &fakeSink{limit: 100}
			if enabled {
				withSink(t, sink)
			} else {
				SetSink(nil)
			}
			var seen http.Header
			engine := newEngine(func(c *gin.Context) {
				seen = c.Request.Header.Clone()
				ObserveRequest(ContextWithGinTurn(context.Background(), c), "claude", "m", false, []byte("{}"))
				c.String(http.StatusOK, "ok")
			})
			req := httptest.NewRequest(http.MethodPost, "/v1/messages", strings.NewReader("{}"))
			req.Header.Set("User-Agent", "claude-cli/1.2.3")
			req.Header.Set("X-CPA-Anything", "x")
			req.Header["x-cpa-lower"] = []string{"x"}
			req.Header.Set(HeaderProject, "p")
			req.Header.Set(HeaderCwd, "/tmp/x")
			req.Header.Set(HeaderClient, "t3code")
			req.Header.Set(HeaderGitRemote, "git@github.com:acme/app.git")
			req.Header.Set(HeaderPathProject, "spoofed")
			req.Header.Set("X-CPA-Claude-Workload", "cron")
			engine.ServeHTTP(httptest.NewRecorder(), req)
			for name := range seen {
				if strings.HasPrefix(strings.ToUpper(name), "X-CPA-") && http.CanonicalHeaderKey(name) != "X-Cpa-Claude-Workload" {
					t.Errorf("header %q reached the handler", name)
				}
			}
			if seen.Get("User-Agent") == "" {
				t.Errorf("unrelated headers must stay")
			}
			// The Claude executor reads this one from the request.
			if seen.Get("X-CPA-Claude-Workload") != "cron" {
				t.Errorf("X-CPA-Claude-Workload was removed")
			}
			if !enabled {
				return
			}
			if len(sink.turns) != 1 {
				t.Fatalf("turns = %d", len(sink.turns))
			}
			got := sink.turns[0].Headers
			if got.Get(HeaderProject) != "p" || got.Get(HeaderCwd) != "/tmp/x" || got.Get(HeaderClient) != "t3code" || got.Get(HeaderGitRemote) != "git@github.com:acme/app.git" {
				t.Errorf("known hints not kept for the turn: %v", got)
			}
			if got.Get("X-CPA-Anything") != "" || got.Get(HeaderPathProject) != "" {
				t.Errorf("unknown or spoofed hints kept: %v", got)
			}
		})
	}
}

func TestWebsocketHandshakeReadsAndRemovesXCPAHeaders(t *testing.T) {
	sink := &fakeSink{limit: 100}
	withSink(t, sink)
	var seen http.Header
	gin.SetMode(gin.TestMode)
	engine := gin.New()
	engine.Use(Middleware())
	engine.GET("/v1/responses", func(c *gin.Context) {
		seen = c.Request.Header.Clone()
		ctx, turn := BeginWebsocketTurn(context.Background(), c)
		ObserveRequest(ctx, "openai-response", "m", true, []byte(`{"input":"hi"}`))
		turn.FinishWebsocketTurn([]byte(`[]`), nil, nil)
		c.Status(http.StatusSwitchingProtocols)
	})
	handler := StripPathProject(engine)
	req := httptest.NewRequest(http.MethodGet, "/p/ws-app/v1/responses", nil)
	req.Header.Set("Upgrade", "websocket")
	req.Header.Set("Connection", "Upgrade")
	req.Header.Set(HeaderGitRemote, "https://github.com/acme/app")
	req.Header.Set("X-CPA-Anything", "x")
	handler.ServeHTTP(httptest.NewRecorder(), req)
	for name := range seen {
		if strings.HasPrefix(strings.ToUpper(name), "X-CPA-") {
			t.Errorf("header %q reached the websocket handler", name)
		}
	}
	if len(sink.turns) != 1 {
		t.Fatalf("turns = %d", len(sink.turns))
	}
	got := sink.turns[0].Headers
	if got.Get(HeaderGitRemote) != "https://github.com/acme/app" || got.Get(HeaderPathProject) != "ws-app" || got.Get("X-CPA-Anything") != "" {
		t.Errorf("websocket turn headers = %v", got)
	}
}

func TestStripPathProject(t *testing.T) {
	type seen struct{ path, raw, project string }
	var got seen
	var routed bool
	engine := gin.New()
	engine.Use(func(c *gin.Context) {
		got = seen{path: c.Request.URL.Path, raw: c.Request.URL.RawPath, project: PathProjectFrom(c.Request.Context())}
		routed = true
	})
	engine.POST("/v1/messages", func(c *gin.Context) { c.String(http.StatusOK, "msg") })
	engine.GET("/v1/models/*model", func(c *gin.Context) { c.String(http.StatusOK, "model:"+c.Param("model")) })
	engine.GET("/v8/management/ping", func(c *gin.Context) { c.String(http.StatusOK, "mgmt") })
	handler := StripPathProject(engine)

	tests := []struct {
		name, method, url string
		wantCode          int
		wantBody          string
		wantProject       string
		wantPath          string
	}{
		{"plain route is untouched", "POST", "/v1/messages", 200, "msg", "", "/v1/messages"},
		{"prefix then v1", "POST", "/p/my-app/v1/messages", 200, "msg", "my-app", "/v1/messages"},
		{"escaped name", "POST", "/p/my%20app%2Fx/v1/messages", 200, "msg", "my app/x", "/v1/messages"},
		{"wildcard params stay right", "GET", "/p/my-app/v1/models/claude%2Fx", 200, "model:/claude/x", "my-app", "/v1/models/claude/x"},
		{"query is kept", "POST", "/p/my-app/v1/messages?beta=true", 200, "msg", "my-app", "/v1/messages"},
		{"empty name", "POST", "/p//v1/messages", 400, "", "", ""},
		{"blank name", "POST", "/p/%20/v1/messages", 400, "", "", ""},
		{"management is never prefixed", "GET", "/p/my-app/v8/management/ping", 404, "", "", ""},
		{"old management is never prefixed", "GET", "/p/my-app/v0/management/x", 404, "", "", ""},
		{"panel is never prefixed", "GET", "/p/my-app/management.html", 404, "", "", ""},
		{"management itself still works", "GET", "/v8/management/ping", 200, "mgmt", "", "/v8/management/ping"},
		{"p alone is not a prefix", "POST", "/p", 404, "", "", ""},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got, routed = seen{}, false
			rec := httptest.NewRecorder()
			handler.ServeHTTP(rec, httptest.NewRequest(tc.method, tc.url, strings.NewReader("{}")))
			if rec.Code != tc.wantCode || (tc.wantBody != "" && rec.Body.String() != tc.wantBody) {
				t.Fatalf("%s %s = %d %q, want %d %q", tc.method, tc.url, rec.Code, rec.Body.String(), tc.wantCode, tc.wantBody)
			}
			if tc.wantCode == 200 && (got.project != tc.wantProject || got.path != tc.wantPath) {
				t.Errorf("project=%q path=%q, want %q %q", got.project, got.path, tc.wantProject, tc.wantPath)
			}
			if tc.wantCode == 400 && routed {
				t.Errorf("a rejected request reached the router")
			}
		})
	}
}

func TestPathProjectReachesTheTurn(t *testing.T) {
	sink := &fakeSink{limit: 100}
	withSink(t, sink)
	handler := StripPathProject(newEngine(func(c *gin.Context) {
		ObserveRequest(ContextWithGinTurn(context.Background(), c), "claude", "m", false, []byte("{}"))
		c.String(http.StatusOK, "ok")
	}))
	handler.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest(http.MethodPost, "/p/my-app/v1/messages", strings.NewReader("{}")))
	if len(sink.turns) != 1 || sink.turns[0].Headers.Get(HeaderPathProject) != "my-app" || sink.turns[0].Path != "/v1/messages" {
		t.Fatalf("turn = %+v", sink.turns)
	}
	// Traces off: the prefix is still removed and nothing is recorded.
	SetSink(nil)
	var path string
	off := StripPathProject(newEngine(func(c *gin.Context) { path = c.Request.URL.Path; c.String(http.StatusOK, "ok") }))
	rec := httptest.NewRecorder()
	off.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/p/my-app/v1/messages", strings.NewReader("{}")))
	if rec.Code != http.StatusOK || path != "/v1/messages" {
		t.Errorf("traces off: code=%d path=%q", rec.Code, path)
	}
}
