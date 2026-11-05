package api

import (
	"strings"

	"github.com/router-for-me/CLIProxyAPI/v8/internal/config"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/traces"
	coreauth "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/auth"
)

// newTracesService creates the trace recorder. It stays idle until the
// traces block enables it.
func newTracesService(authManager *coreauth.Manager) *traces.Service {
	return traces.New(traces.Options{Accounts: tracesAccountDirectory(authManager)})
}

// applyTracesConfig hot-reloads the traces block.
func (s *Server) applyTracesConfig(cfg *config.Config) {
	if s == nil || s.traces == nil || cfg == nil {
		return
	}
	traces.SetProxyURL(cfg.ProxyURL)
	s.traces.Apply(cfg)
}

// tracesAccountDirectory lists credentials for the limits view. API keys of
// key-based credentials are never exposed.
func tracesAccountDirectory(authManager *coreauth.Manager) traces.AccountDirectory {
	if authManager == nil {
		return nil
	}
	return func() []traces.AccountInfo {
		auths := authManager.List()
		out := make([]traces.AccountInfo, 0, len(auths))
		for _, a := range auths {
			if a == nil || a.ID == "" {
				continue
			}
			info := traces.AccountInfo{ID: a.ID, Label: strings.TrimSpace(a.Label), Provider: a.Provider, Tags: coreauth.CredentialTags(a)}
			if kind, email := a.AccountInfo(); kind == "oauth" {
				info.Email = email
			}
			for _, key := range []string{"plan_type", "subscription_type", "plan"} {
				if v, ok := a.Metadata[key].(string); ok && strings.TrimSpace(v) != "" {
					info.Plan = strings.TrimSpace(v)
					break
				}
			}
			out = append(out, info)
		}
		return out
	}
}
