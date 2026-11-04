package auth

import (
	"context"
	"strings"
	"sync/atomic"

	internalconfig "github.com/router-for-me/CLIProxyAPI/v8/internal/config"
	coreusage "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/usage"
)

// CredentialTagsMetadataKey is the credential JSON field that lists credential tags.
const CredentialTagsMetadataKey = "tags"

// clientKeyTags holds the normalized client API key -> tags table from the
// latest runtime configuration. It is process-wide, like the global model
// registry, because the scheduler evaluates eligibility without a Manager.
var clientKeyTags atomic.Pointer[map[string][]string]

// setClientKeyTags replaces the client API key -> tags table from cfg.
func setClientKeyTags(cfg *internalconfig.Config) {
	if cfg == nil || len(cfg.APIKeyTags) == 0 {
		clientKeyTags.Store(nil)
		return
	}
	table := make(map[string][]string, len(cfg.APIKeyTags))
	for key, tags := range cfg.APIKeyTags {
		key = strings.TrimSpace(key)
		normalized := normalizeTags(tags)
		if key == "" || len(normalized) == 0 {
			continue
		}
		table[key] = normalized
	}
	clientKeyTags.Store(&table)
}

// clientTagsFromContext returns the tags configured for the client API key that
// authenticated the request in ctx. Requests without a key or with an untagged
// key return nil.
func clientTagsFromContext(ctx context.Context) []string {
	table := clientKeyTags.Load()
	if table == nil || ctx == nil {
		return nil
	}
	key := coreusage.APIKeyFromContext(ctx)
	if key == "" {
		return nil
	}
	return (*table)[key]
}

// CredentialTags returns the normalized tags stored in the credential metadata.
func CredentialTags(auth *Auth) []string {
	if auth == nil || len(auth.Metadata) == 0 {
		return nil
	}
	return ParseCredentialTags(auth.Metadata[CredentialTagsMetadataKey])
}

// ParseCredentialTags normalizes a raw "tags" value from credential JSON.
// It accepts a JSON string list or a comma-separated string; other values yield nil.
func ParseCredentialTags(value any) []string {
	switch raw := value.(type) {
	case []string:
		return normalizeTags(raw)
	case []any:
		values := make([]string, 0, len(raw))
		for _, item := range raw {
			if s, ok := item.(string); ok {
				values = append(values, s)
			}
		}
		return normalizeTags(values)
	case string:
		return normalizeTags(strings.Split(raw, ","))
	default:
		return nil
	}
}

// credentialTagsAllow reports whether a request whose client key carries
// clientTags may use a credential that carries credTags. Untagged keys use only
// untagged credentials; tagged keys and tagged credentials must share a tag.
// This keeps tagged pools isolated even when a request path loses its key.
func credentialTagsAllow(clientTags, credTags []string) bool {
	if len(clientTags) == 0 && len(credTags) == 0 {
		return true
	}
	for _, want := range clientTags {
		for _, have := range credTags {
			if want == have {
				return true
			}
		}
	}
	return false
}

func normalizeTags(tags []string) []string {
	if len(tags) == 0 {
		return nil
	}
	out := make([]string, 0, len(tags))
	seen := make(map[string]struct{}, len(tags))
	for _, tag := range tags {
		tag = strings.ToLower(strings.TrimSpace(tag))
		if tag == "" {
			continue
		}
		if _, dup := seen[tag]; dup {
			continue
		}
		seen[tag] = struct{}{}
		out = append(out, tag)
	}
	if len(out) == 0 {
		return nil
	}
	return out
}
