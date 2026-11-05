package traces

import (
	"context"

	"github.com/router-for-me/CLIProxyAPI/v8/internal/traces/store"
	log "github.com/sirupsen/logrus"
)

// ReresolveProjects re-applies the current project rules to every stored
// session and saves the sessions whose project changed.
func (s *Service) ReresolveProjects(ctx context.Context) error {
	s.mu.Lock()
	st := s.st
	s.mu.Unlock()
	if st == nil {
		return nil
	}
	return reresolveProjects(ctx, st, s.Settings().Rules)
}

func reresolveProjects(ctx context.Context, st store.Store, rules []ProjectRule) error {
	sessions, err := st.AllSessions(ctx)
	if err != nil {
		return err
	}
	byID := make(map[string]*store.Session, len(sessions))
	for i := range sessions {
		byID[sessions[i].ID] = &sessions[i]
	}
	var changed []store.Session
	// Sessions are ordered by start, so parents resolve before their subagents.
	for i := range sessions {
		sess := &sessions[i]
		before := *sess
		if sess.ProjectSource == SourceRule && sess.Folder == "" {
			sess.ProjectName = ""
		}
		applyProject(rules, sess, byID[sess.ParentID])
		if sess.ProjectID != before.ProjectID || sess.ProjectName != before.ProjectName || sess.ProjectKey != before.ProjectKey || sess.ProjectSource != before.ProjectSource || sess.ProjectRule != before.ProjectRule {
			changed = append(changed, *sess)
		}
	}
	if len(changed) == 0 {
		return nil
	}
	if err = st.UpdateSessionProjects(ctx, changed); err != nil {
		return err
	}
	log.Infof("traces: re-resolved the project of %d sessions", len(changed))
	return nil
}
