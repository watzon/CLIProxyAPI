package management

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/gin-gonic/gin"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/config"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/traces"
)

func TestSetTracesConfigValue(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.yaml")
	raw := "config-version: 8\naccess:\n  api-keys: [k]\n# Trace settings stay documented.\ntraces:\n  enabled: true\n  projects:\n    # Folder rules.\n    rules:\n      - match: \"~/a\"\n        project: a\n"
	if err := os.WriteFile(path, []byte(raw), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg, err := config.LoadConfig(path)
	if err != nil {
		t.Fatal(err)
	}
	h := NewHandler(cfg, path, nil)
	ctx := context.Background()

	saved, err := h.SetTracesConfigValue(ctx, []string{"projects", "rules"}, []config.TracesProjectRule{})
	if err != nil {
		t.Fatalf("clear rules: %v", err)
	}
	if len(saved.Traces.Projects.Rules) != 0 || !saved.Traces.Enabled {
		t.Fatalf("saved traces = %+v", saved.Traces)
	}
	data, _ := os.ReadFile(path)
	text := string(data)
	if strings.Contains(text, "~/a") || !strings.Contains(text, "# Trace settings stay documented.") || !strings.Contains(text, "# Folder rules.") {
		t.Fatalf("clearing the rules did not persist or lost comments:\n%s", text)
	}

	price := 1.5
	saved, err = h.SetTracesConfigValue(ctx, []string{"prices", "overrides"}, []config.TracesPriceOverride{{Model: "m-*", Input: &price}})
	if err != nil {
		t.Fatalf("set overrides: %v", err)
	}
	reloaded, err := config.LoadConfig(path)
	if err != nil {
		t.Fatal(err)
	}
	if len(reloaded.Traces.Prices.Overrides) != 1 || *reloaded.Traces.Prices.Overrides[0].Input != 1.5 || len(saved.Traces.Prices.Overrides) != 1 {
		t.Fatalf("overrides = %+v", reloaded.Traces.Prices.Overrides)
	}
	if h.cfg.Traces.Prices.Overrides[0].Model != "m-*" {
		t.Fatalf("handler config was not updated")
	}

	// A file without a traces block gets one.
	bare := filepath.Join(t.TempDir(), "config.yaml")
	if err = os.WriteFile(bare, []byte("config-version: 8\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg2, _ := config.LoadConfig(bare)
	h2 := NewHandler(cfg2, bare, nil)
	if _, err = h2.SetTracesConfigValue(ctx, []string{"projects", "rules"}, []config.TracesProjectRule{{Match: "~/b", Project: "b"}}); err != nil {
		t.Fatalf("create block: %v", err)
	}
	if again, _ := config.LoadConfig(bare); len(again.Traces.Projects.Rules) != 1 {
		t.Fatalf("rules not written to a file without a traces block")
	}
}

func TestConfigV8TracesReadShowsDefaultsAndKeyLabels(t *testing.T) {
	gin.SetMode(gin.TestMode)
	newRouter := func(raw string) (*gin.Engine, string) {
		path := filepath.Join(t.TempDir(), "config.yaml")
		if err := os.WriteFile(path, []byte(raw), 0o600); err != nil {
			t.Fatal(err)
		}
		cfg, err := config.LoadConfig(path)
		if err != nil {
			t.Fatal(err)
		}
		h := &Handler{cfg: cfg, configFilePath: path}
		router := gin.New()
		router.GET("/v8/management/config", h.ConfigV8)
		router.PATCH("/v8/management/config", h.ConfigV8)
		for _, method := range []string{http.MethodGet, http.MethodPut, http.MethodPatch} {
			router.Handle(method, "/v8/management/config/*path", h.ConfigV8)
		}
		return router, path
	}
	get := func(router *gin.Engine, url string) (int, string) {
		rec := httptest.NewRecorder()
		router.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, url, nil))
		return rec.Code, rec.Body.String()
	}

	// No traces block: the read still answers with every default.
	router, _ := newRouter("config-version: 8\n")
	code, body := get(router, "/v8/management/config/traces")
	if code != http.StatusOK {
		t.Fatalf("GET config/traces = %d %s", code, body)
	}
	var block map[string]any
	if err := json.Unmarshal([]byte(body), &block); err != nil {
		t.Fatal(err)
	}
	retention, _ := block["retention"].(map[string]any)
	if block["enabled"] != false || block["capture"] != "metadata" || block["idle-gap"] != "10m" || block["redact-secrets"] != true ||
		retention["content-max-age"] != "365d" || retention["content-max-size"] != "20GB" {
		t.Fatalf("default traces block = %s", body)
	}

	// Configured block: set values win, unset keys get defaults, raw keys never leave.
	const rawKey = "sk-client-secret-value"
	router, path := newRouter("config-version: 8\naccess:\n  api-keys: [" + rawKey + "]\ntraces:\n  enabled: true\n  capture: full\n  capture-by-key:\n    " + rawKey + ": metadata\n")
	label := traces.KeyLabel(rawKey)
	for _, url := range []string{"/v8/management/config/traces", "/v8/management/config/traces/capture-by-key"} {
		code, body = get(router, url)
		if code != http.StatusOK || strings.Contains(body, rawKey) || !strings.Contains(body, `"`+label+`":"metadata"`) {
			t.Fatalf("GET %s = %d %s", url, code, body)
		}
	}
	code, body = get(router, "/v8/management/config/traces")
	if !strings.Contains(body, `"capture":"full"`) || !strings.Contains(body, `"idle-gap":"10m"`) {
		t.Fatalf("configured block = %s", body)
	}
	if _, root := get(router, "/v8/management/config"); strings.Contains(strings.SplitN(root, `"traces"`, 2)[1], rawKey) {
		t.Fatalf("full config read exposes the raw capture key: %s", root)
	}

	// Writing the label back keeps the raw key in the file.
	rec := httptest.NewRecorder()
	router.ServeHTTP(rec, httptest.NewRequest(http.MethodPatch, "/v8/management/config/traces", strings.NewReader(`{"capture-by-key":{"`+label+`":"full"}}`)))
	if rec.Code != http.StatusOK {
		t.Fatalf("PATCH traces = %d %s", rec.Code, rec.Body.String())
	}
	saved, err := config.LoadConfig(path)
	if err != nil {
		t.Fatal(err)
	}
	if got := saved.Traces.CaptureByKey; len(got) != 1 || got[rawKey] != "full" {
		t.Fatalf("capture-by-key after write = %v", got)
	}
}
