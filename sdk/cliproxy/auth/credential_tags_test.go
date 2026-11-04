package auth

import (
	"context"
	"net/http"
	"testing"

	internalconfig "github.com/router-for-me/CLIProxyAPI/v8/internal/config"
	cliproxyexecutor "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/executor"
	coreusage "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/usage"
)

func TestCredentialTagsAllow(t *testing.T) {
	tests := []struct {
		name   string
		client []string
		cred   []string
		want   bool
	}{
		{name: "untagged key, untagged credential", want: true},
		{name: "untagged key, tagged credential", cred: []string{"work"}, want: false},
		{name: "tagged key, untagged credential", client: []string{"work"}, want: false},
		{name: "matching tag", client: []string{"work"}, cred: []string{"work"}, want: true},
		{name: "different tag", client: []string{"personal"}, cred: []string{"work"}, want: false},
		{name: "one shared tag", client: []string{"personal", "clients"}, cred: []string{"clients"}, want: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := credentialTagsAllow(tt.client, tt.cred); got != tt.want {
				t.Fatalf("credentialTagsAllow(%v, %v) = %v, want %v", tt.client, tt.cred, got, tt.want)
			}
		})
	}
}

func TestCredentialTagsFromMetadata(t *testing.T) {
	tests := []struct {
		name string
		raw  any
		want []string
	}{
		{name: "missing", raw: nil, want: nil},
		{name: "json list", raw: []any{" Work ", "personal", "work", 3}, want: []string{"work", "personal"}},
		{name: "string list", raw: []string{"personal"}, want: []string{"personal"}},
		{name: "comma string", raw: "work, clients ,", want: []string{"work", "clients"}},
		{name: "empty list", raw: []any{}, want: nil},
		{name: "wrong type", raw: 42, want: nil},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			auth := &Auth{Metadata: map[string]any{}}
			if tt.raw != nil {
				auth.Metadata[CredentialTagsMetadataKey] = tt.raw
			}
			got := CredentialTags(auth)
			if len(got) != len(tt.want) {
				t.Fatalf("CredentialTags() = %v, want %v", got, tt.want)
			}
			for i := range got {
				if got[i] != tt.want[i] {
					t.Fatalf("CredentialTags() = %v, want %v", got, tt.want)
				}
			}
		})
	}
}

// newTaggedTestManager registers two work, two personal and one untagged Claude
// credential and configures client keys for each pool. It mutates the
// process-wide key table, so callers must not run in parallel.
func newTaggedTestManager(t *testing.T, selector Selector) (*Manager, string) {
	t.Helper()
	model := "claude-tags-test-model"
	ids := []string{"work-a", "work-b", "personal-a", "personal-b", "untagged"}
	registerSchedulerModels(t, "claude", model, ids...)

	manager := NewManager(nil, selector, nil)
	manager.executors["claude"] = schedulerTestExecutor{}
	manager.SetConfig(&internalconfig.Config{SDKConfig: internalconfig.SDKConfig{
		APIKeys: []string{"key-work", "key-personal", "key-plain"},
		APIKeyTags: map[string][]string{
			"key-work":       {"work"},
			" key-personal ": {"Personal"},
		},
	}})
	t.Cleanup(func() { setClientKeyTags(nil) })

	tags := map[string]any{
		"work-a":     []any{"work"},
		"work-b":     []any{"work"},
		"personal-a": []any{"personal"},
		"personal-b": "personal",
	}
	for _, id := range ids {
		auth := &Auth{ID: id, Provider: "claude", Metadata: map[string]any{}}
		if value, ok := tags[id]; ok {
			auth.Metadata[CredentialTagsMetadataKey] = value
		}
		if _, errRegister := manager.Register(context.Background(), auth); errRegister != nil {
			t.Fatalf("Register(%s) error = %v", id, errRegister)
		}
	}
	return manager, model
}

func pickTagged(t *testing.T, manager *Manager, model, clientKey string, opts cliproxyexecutor.Options, tried map[string]struct{}) (*Auth, error) {
	t.Helper()
	ctx := context.Background()
	if clientKey != "" {
		ctx = coreusage.WithAPIKey(ctx, clientKey)
	}
	if tried == nil {
		tried = map[string]struct{}{}
	}
	auth, _, _, errPick := manager.pickNextMixed(ctx, []string{"claude"}, model, opts, tried)
	return auth, errPick
}

func TestManagerPickRespectsCredentialTags(t *testing.T) {
	manager, model := newTaggedTestManager(t, &RoundRobinSelector{})

	allowed := map[string]map[string]bool{
		"key-work":     {"work-a": true, "work-b": true},
		"key-personal": {"personal-a": true, "personal-b": true},
		"key-plain":    {"untagged": true},
		"key-unknown":  {"untagged": true},
		"":             {"untagged": true},
	}
	for clientKey, want := range allowed {
		seen := map[string]bool{}
		for i := 0; i < 10; i++ {
			got, errPick := pickTagged(t, manager, model, clientKey, cliproxyexecutor.Options{}, nil)
			if errPick != nil {
				t.Fatalf("key %q: pickNextMixed() error = %v", clientKey, errPick)
			}
			if !want[got.ID] {
				t.Fatalf("key %q: picked %q, want one of %v", clientKey, got.ID, want)
			}
			seen[got.ID] = true
		}
		if len(seen) != len(want) {
			t.Fatalf("key %q: round-robin used %v, want all of %v", clientKey, seen, want)
		}
	}
}

func TestManagerPickFailsClosedWhenTaggedPoolIsExhausted(t *testing.T) {
	manager, model := newTaggedTestManager(t, &RoundRobinSelector{})

	tried := map[string]struct{}{"personal-a": {}, "personal-b": {}}
	got, errPick := pickTagged(t, manager, model, "key-personal", cliproxyexecutor.Options{}, tried)
	if errPick == nil {
		t.Fatalf("pickNextMixed() = %q, want an error once every personal credential was tried", got.ID)
	}
}

func TestManagerPickTagsOverrideSessionAffinity(t *testing.T) {
	selector := NewSessionAffinitySelector(&RoundRobinSelector{})
	t.Cleanup(selector.Stop)
	manager, model := newTaggedTestManager(t, selector)

	sessionOpts := func() cliproxyexecutor.Options {
		return cliproxyexecutor.Options{Headers: http.Header{"X-Claude-Code-Session-Id": {"shared-session"}}}
	}
	first, errPick := pickTagged(t, manager, model, "key-work", sessionOpts(), nil)
	if errPick != nil {
		t.Fatalf("work pick error = %v", errPick)
	}
	if first.ID != "work-a" && first.ID != "work-b" {
		t.Fatalf("work pick = %q, want a work credential", first.ID)
	}
	for i := 0; i < 5; i++ {
		got, errPick := pickTagged(t, manager, model, "key-personal", sessionOpts(), nil)
		if errPick != nil {
			t.Fatalf("personal pick error = %v", errPick)
		}
		if got.ID != "personal-a" && got.ID != "personal-b" {
			t.Fatalf("personal pick reused session binding %q, want a personal credential", got.ID)
		}
	}
}

func TestManagerPickHonorsTagsForPinnedCredential(t *testing.T) {
	manager, model := newTaggedTestManager(t, &RoundRobinSelector{})

	opts := cliproxyexecutor.Options{Metadata: map[string]any{cliproxyexecutor.PinnedAuthMetadataKey: "work-a"}}
	got, errPick := pickTagged(t, manager, model, "key-personal", opts, nil)
	if errPick == nil {
		t.Fatalf("pinned pick = %q, want an error: a personal key must not reach a pinned work credential", got.ID)
	}
	got, errPick = pickTagged(t, manager, model, "key-work", opts, nil)
	if errPick != nil || got.ID != "work-a" {
		t.Fatalf("pinned pick with work key = %v, %v; want work-a", got, errPick)
	}
}
