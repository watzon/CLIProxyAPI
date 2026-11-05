package traces

import (
	"context"
	"encoding/json"
	"strconv"
	"strings"

	"github.com/router-for-me/CLIProxyAPI/v8/internal/traces/extract"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/traces/store"
	log "github.com/sirupsen/logrus"
)

const (
	// rederiveKey is the kv key that records the last re-derive version that ran.
	rederiveKey = "projects.rederive"
	// rederiveVersion changes when the folder, title or project logic changes in a
	// way that stored sessions need to see. The job runs once per version.
	rederiveVersion = 2
	// rederiveMaxTurns bounds the turns of one session that the job reads.
	rederiveMaxTurns = 5
	// rederiveChunk bounds the sessions per turn query.
	rederiveChunk = 500
)

// ReresolveProjects re-derives missing folders and compaction titles from the
// stored content, then re-applies the current project rules to every stored
// session. It saves the sessions that changed.
func (s *Service) ReresolveProjects(ctx context.Context) error {
	s.mu.Lock()
	st := s.st
	s.mu.Unlock()
	if st == nil {
		return nil
	}
	return reresolveProjects(ctx, st, s.Settings())
}

// rederiveDone reports whether the current re-derive version already ran.
func rederiveDone(ctx context.Context, st store.Store) bool {
	raw, _, err := st.GetKV(ctx, rederiveKey)
	if err != nil {
		return false
	}
	version, err := strconv.Atoi(strings.TrimSpace(string(raw)))
	return err == nil && version >= rederiveVersion
}

// runReresolve is the maintenance-loop job. It records the version after a
// complete run, so a start-up run happens once per version.
func (s *Service) runReresolve(ctx context.Context, st store.Store) {
	// Use st directly: taking s.mu here could deadlock with Apply stopping this loop.
	if err := reresolveProjects(ctx, st, s.Settings()); err != nil {
		log.WithError(err).Warn("traces: project re-resolution failed")
		return
	}
	if err := st.PutKV(ctx, rederiveKey, []byte(strconv.Itoa(rederiveVersion))); err != nil {
		log.WithError(err).Warn("traces: could not record the re-derive version")
	}
}

func reresolveProjects(ctx context.Context, st store.Store, settings Settings) error {
	sessions, err := st.AllSessions(ctx)
	if err != nil {
		return err
	}
	rederived, err := rederiveSessions(ctx, st, settings, sessions)
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
		parent := byID[sess.ParentID]
		if sess.Folder == "" {
			sess.Folder = rederived[sess.ID].Folder
		}
		if sess.Folder == "" && parent != nil {
			sess.Folder = parent.Folder
		}
		if remote := rederived[sess.ID].GitRemote; remote != "" && sess.GitRemote == "" {
			sess.GitRemote = remote
		}
		if title := rederived[sess.ID].Title; title != "" {
			sess.Title = title
		}
		applyProject(settings, sess, parent)
		if sess.ProjectID != before.ProjectID || sess.ProjectName != before.ProjectName || sess.ProjectKey != before.ProjectKey ||
			sess.ProjectSource != before.ProjectSource || sess.ProjectRule != before.ProjectRule ||
			sess.Folder != before.Folder || sess.GitRemote != before.GitRemote || sess.Title != before.Title {
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

// rederived holds what the job found again in the stored content of a session.
type rederived struct {
	Folder    string
	GitRemote string
	Title     string
}

// rederiveSessions reads the stored request content of sessions that have no
// folder or a title that extract.NeedsTitle rejects, and derives folder and title again
// with the current extractors. Sessions without stored content are skipped.
func rederiveSessions(ctx context.Context, st store.Store, settings Settings, sessions []store.Session) (map[string]rederived, error) {
	needs := map[string]*store.Session{}
	var ids []string
	for i := range sessions {
		sess := &sessions[i]
		needFolder := sess.Folder == "" && extract.HasFolderExtractor(sess.Client)
		needTitle := extract.NeedsTitle(sess.Title)
		if needFolder || needTitle {
			needs[sess.ID] = sess
			ids = append(ids, sess.ID)
		}
	}
	out := map[string]rederived{}
	if len(ids) == 0 {
		return out, nil
	}
	// Turns arrive ordered by start. Keep the turns that have stored content.
	bySession := map[string][]store.Turn{}
	for start := 0; start < len(ids); start += rederiveChunk {
		turns, errQuery := st.QueryTurns(ctx, store.TurnFilter{SessionIDs: ids[start:min(len(ids), start+rederiveChunk)]})
		if errQuery != nil {
			return nil, errQuery
		}
		for _, t := range turns {
			if t.ContentAvailable {
				bySession[t.SessionID] = append(bySession[t.SessionID], t)
			}
		}
	}
	for id, list := range bySession {
		if errCtx := ctx.Err(); errCtx != nil {
			return nil, errCtx
		}
		sess := needs[id]
		var found rederived
		if sess.Folder == "" && extract.HasFolderExtractor(sess.Client) {
			for _, t := range list[:min(len(list), rederiveMaxTurns)] {
				req, errReq := storedRequest(ctx, st, t.ID)
				if errReq != nil {
					return nil, errReq
				}
				if folder, remote := extract.DeriveFolder(sess.Client, nil, req); folder != "" {
					found.Folder, found.GitRemote = folder, remote
					break
				}
			}
		}
		if extract.NeedsTitle(sess.Title) {
			// The newest turn carries the longest history, so it holds the first real prompt.
			for i := len(list) - 1; i >= 0 && i >= len(list)-rederiveMaxTurns; i-- {
				req, errReq := storedRequest(ctx, st, list[i].ID)
				if errReq != nil {
					return nil, errReq
				}
				if title := extract.Title(req.Messages); title != "" {
					if settings.RedactSecrets {
						title = extract.Redact(title)
					}
					found.Title = title
					break
				}
			}
		}
		if found != (rederived{}) {
			out[id] = found
		}
	}
	return out, nil
}

// storedRequest rebuilds the normalized request of a turn from its stored
// content units. Reply units and the tool list are left out.
func storedRequest(ctx context.Context, st store.Store, turnID string) (*extract.Request, error) {
	refs, err := st.TurnBlobRefs(ctx, turnID)
	if err != nil {
		return nil, err
	}
	req := &extract.Request{}
	for _, ref := range refs {
		if ref.Role != store.RoleSystem && ref.Role != store.RoleMessage {
			continue
		}
		raw, errBlob := st.Blob(ctx, ref.Hash)
		if errBlob != nil {
			return nil, errBlob
		}
		var msg extract.Message
		if len(raw) == 0 || json.Unmarshal(raw, &msg) != nil {
			continue
		}
		if ref.Role == store.RoleSystem {
			req.System = &msg
			continue
		}
		req.Messages = append(req.Messages, msg)
	}
	return req, nil
}
