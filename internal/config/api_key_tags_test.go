package config

import (
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

func TestAPIKeyTagsLoadFromV8AndLegacyLayouts(t *testing.T) {
	want := map[string][]string{"key-work": {"work"}, "key-personal": {"personal", "clients"}}
	for _, tc := range []struct {
		name, raw string
	}{
		{"v8", "config-version: 8\naccess:\n  api-keys: [key-work, key-personal]\n  key-tags:\n    key-work: [work]\n    key-personal: [personal, clients]\n"},
		{"legacy", "api-keys: [key-work, key-personal]\napi-key-tags:\n  key-work: [work]\n  key-personal: [personal, clients]\n"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cfg, err := ParseConfigBytes([]byte(tc.raw))
			if err != nil {
				t.Fatalf("ParseConfigBytes() error = %v", err)
			}
			if !reflect.DeepEqual(cfg.APIKeyTags, want) {
				t.Fatalf("APIKeyTags = %v, want %v", cfg.APIKeyTags, want)
			}
		})
	}
	if err := ValidateV8Config([]byte("config-version: 8\naccess:\n  key-tags:\n    key-work: [work]\n")); err != nil {
		t.Fatalf("ValidateV8Config() rejected access.key-tags: %v", err)
	}
}

func TestAPIKeyTagsSurviveSaveAndReload(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.yaml")
	raw := "config-version: 8\naccess:\n  api-keys: [key-work, key-personal]\n  key-tags:\n    key-work: [work]\n    key-personal: [personal]\n"
	if err := os.WriteFile(path, []byte(raw), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg, err := LoadConfig(path)
	if err != nil {
		t.Fatalf("LoadConfig() error = %v", err)
	}

	cfg.APIKeyTags["key-personal"] = []string{"personal", "clients"}
	delete(cfg.APIKeyTags, "key-work")
	if err = SaveConfigPreserveComments(path, cfg); err != nil {
		t.Fatalf("SaveConfigPreserveComments() error = %v", err)
	}
	saved, _ := os.ReadFile(path)
	if strings.Contains(string(saved), "api-key-tags") {
		t.Fatalf("saved v8 config uses the legacy field name:\n%s", saved)
	}
	reloaded, err := LoadConfig(path)
	if err != nil {
		t.Fatalf("LoadConfig() after save error = %v", err)
	}
	want := map[string][]string{"key-personal": {"personal", "clients"}}
	if !reflect.DeepEqual(reloaded.APIKeyTags, want) {
		t.Fatalf("reloaded APIKeyTags = %v, want %v\nsaved:\n%s", reloaded.APIKeyTags, want, saved)
	}

	// Removing the last tagged key must also persist; a stale entry would
	// silently bring the restriction back on the next reload.
	reloaded.APIKeyTags = nil
	if err = SaveConfigPreserveComments(path, reloaded); err != nil {
		t.Fatalf("SaveConfigPreserveComments() error = %v", err)
	}
	saved, _ = os.ReadFile(path)
	final, err := LoadConfig(path)
	if err != nil {
		t.Fatalf("LoadConfig() after clearing error = %v", err)
	}
	if len(final.APIKeyTags) != 0 || strings.Contains(string(saved), "key-tags") {
		t.Fatalf("cleared APIKeyTags came back: %v\nsaved:\n%s", final.APIKeyTags, saved)
	}
}
