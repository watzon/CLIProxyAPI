package api

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
)

func tracesHealth(t *testing.T, server *Server) (map[string]any, http.Header) {
	t.Helper()
	req := httptest.NewRequest(http.MethodGet, "/v8/management/traces/health", nil)
	req.Header.Set("Authorization", "Bearer test-management-key")
	req.Header.Set("Origin", "http://127.0.0.1:5173")
	rr := httptest.NewRecorder()
	server.engine.ServeHTTP(rr, req)
	if rr.Code != http.StatusOK {
		t.Fatalf("GET traces/health = %d: %s", rr.Code, rr.Body.String())
	}
	var body map[string]any
	if err := json.Unmarshal(rr.Body.Bytes(), &body); err != nil {
		t.Fatal(err)
	}
	return body, rr.Header()
}

func TestTracesRoutesHeaderAndHotReload(t *testing.T) {
	t.Setenv("MANAGEMENT_PASSWORD", "test-management-key")
	server := newTestServer(t)
	t.Cleanup(func() { server.traces.Close() })

	body, headers := tracesHealth(t, server)
	if headers.Get("X-CPA-SUPPORT-TRACES") != "1" {
		t.Fatalf("X-CPA-SUPPORT-TRACES = %q", headers.Get("X-CPA-SUPPORT-TRACES"))
	}
	if !strings.Contains(strings.ToLower(headers.Get("Access-Control-Expose-Headers")), "x-cpa-support-traces") {
		t.Fatalf("CORS does not expose X-CPA-SUPPORT-TRACES: %q", headers.Get("Access-Control-Expose-Headers"))
	}
	if body["enabled"] != false {
		t.Fatalf("traces enabled by default: %v", body)
	}

	cfg := server.cfg.CloneForRuntime()
	cfg.Traces.Enabled = true
	cfg.Traces.Dir = filepath.Join(t.TempDir(), "traces")
	empty := ""
	cfg.Traces.Prices.Source = &empty
	server.UpdateClients(cfg)
	if body, _ = tracesHealth(t, server); body["enabled"] != true {
		t.Fatalf("hot reload did not enable traces: %v", body)
	}
	cfg = cfg.CloneForRuntime()
	cfg.Traces.Capture = "full"
	server.UpdateClients(cfg)
	if body, _ = tracesHealth(t, server); body["capture"] != "full" {
		t.Fatalf("hot reload did not change capture: %v", body)
	}
	cfg = cfg.CloneForRuntime()
	cfg.Traces.Enabled = false
	server.UpdateClients(cfg)
	if body, _ = tracesHealth(t, server); body["enabled"] != false {
		t.Fatalf("hot reload did not disable traces: %v", body)
	}
}
