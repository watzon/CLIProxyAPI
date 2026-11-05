package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"
)

const tracesBlock = `traces:
  enabled: true
  dir: "/var/lib/cpa-traces"
  capture: full
  capture-by-key:
    client-a: metadata
  idle-gap: 15m
  queue-size: 500
  max-stream-capture: 4MB
  retention:
    content-max-age: 30d
    content-max-size: 1GB
  redact-secrets: false
  prices:
    source: ""
    refresh: 12h
    overrides:
      - model: "local-*"
        input: 0
        output: 0.5
        cache-write-1h: 1
  projects:
    rules:
      - match: "~/code/{name}"
        project: "{name}"
  client-labels:
    "10.0.0.5": desk
`

func TestTracesBlockLoadsFromV8AndLegacyLayouts(t *testing.T) {
	for _, tc := range []struct{ name, raw string }{
		{"v8", "config-version: 8\naccess:\n  api-keys: [k]\n" + tracesBlock},
		{"legacy", "api-keys: [k]\n" + tracesBlock},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cfg, err := ParseConfigBytes([]byte(tc.raw))
			if err != nil {
				t.Fatalf("ParseConfigBytes() error = %v", err)
			}
			tr := cfg.Traces
			if !tr.Enabled || tr.Dir != "/var/lib/cpa-traces" || tr.Capture != "full" || tr.CaptureByKey["client-a"] != "metadata" {
				t.Errorf("traces = %+v", tr)
			}
			if tr.Retention.ContentMaxAge != "30d" || tr.RedactSecrets == nil || *tr.RedactSecrets || tr.Prices.Source == nil || *tr.Prices.Source != "" {
				t.Errorf("retention/redact/source = %+v %v %v", tr.Retention, tr.RedactSecrets, tr.Prices.Source)
			}
			if len(tr.Prices.Overrides) != 1 || *tr.Prices.Overrides[0].Output != 0.5 || tr.Prices.Overrides[0].CacheRead != nil {
				t.Errorf("overrides = %+v", tr.Prices.Overrides)
			}
			if len(tr.Projects.Rules) != 1 || tr.Projects.Rules[0].Project != "{name}" || tr.ClientLabels["10.0.0.5"] != "desk" {
				t.Errorf("rules/labels = %+v %+v", tr.Projects.Rules, tr.ClientLabels)
			}
		})
	}
	if err := ValidateV8Config([]byte("config-version: 8\n" + tracesBlock)); err != nil {
		t.Fatalf("ValidateV8Config() rejected traces: %v", err)
	}
	var legacy yaml.Node
	if err := yaml.Unmarshal([]byte("api-keys: [k]\n"+tracesBlock), &legacy); err != nil {
		t.Fatal(err)
	}
	if IsV8ConfigLayout(legacy.Content[0]) {
		t.Fatal("a traces block must not mark a legacy file as v8")
	}
}

func TestTracesBlockSurvivesSaveAndMigration(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.yaml")
	raw := "api-keys: [k]\n# trace settings\n" + tracesBlock
	if err := os.WriteFile(path, []byte(raw), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg, err := LoadConfig(path)
	if err != nil {
		t.Fatalf("LoadConfig() error = %v", err)
	}
	cfg.Traces.IdleGap = "20m"
	if err = SaveConfigPreserveComments(path, cfg, true); err != nil {
		t.Fatalf("SaveConfigPreserveComments() error = %v", err)
	}
	saved, _ := os.ReadFile(path)
	if !strings.Contains(string(saved), "# trace settings") || !strings.Contains(string(saved), "idle-gap: 20m") || strings.Contains(string(saved), "#traces") {
		t.Fatalf("traces block lost on v8 migration:\n%s", saved)
	}
	reloaded, err := LoadConfig(path)
	if err != nil {
		t.Fatalf("LoadConfig() after save error = %v", err)
	}
	if reloaded.Traces.IdleGap != "20m" || len(reloaded.Traces.Projects.Rules) != 1 || !reloaded.Traces.Enabled {
		t.Fatalf("reloaded traces = %+v", reloaded.Traces)
	}
	clone := reloaded.CloneForRuntime()
	clone.Traces.Projects.Rules[0].Project = "changed"
	if reloaded.Traces.Projects.Rules[0].Project == "changed" {
		t.Fatal("CloneForRuntime shares the rules slice")
	}
}

func TestEmptyTracesBlockIsNotWritten(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.yaml")
	if err := os.WriteFile(path, []byte("config-version: 8\naccess:\n  api-keys: [k]\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg, err := LoadConfig(path)
	if err != nil {
		t.Fatal(err)
	}
	if err = SaveConfigPreserveComments(path, cfg, true); err != nil {
		t.Fatal(err)
	}
	if saved, _ := os.ReadFile(path); strings.Contains(string(saved), "traces") {
		t.Fatalf("an unconfigured traces block was written:\n%s", saved)
	}
}
