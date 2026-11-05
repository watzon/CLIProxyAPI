package traces

import (
	"crypto/sha256"
	"encoding/hex"
	"net/http"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/traces/capture"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/traces/extract"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/traces/store"
	coresession "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/session"
	"github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/usage"
)

// syntheticSessionGap ends a synthesized session after this much inactivity.
const syntheticSessionGap = 30 * time.Minute

// Turn statuses.
const (
	StatusOK      = "ok"
	StatusRetried = "retried"
	StatusError   = "error"
)

func (s *Service) ingestTurn(tx store.Tx, settings Settings, ev *capture.TurnEvent) error {
	headers := ev.Headers
	if headers == nil {
		headers = http.Header{}
	}
	req := extract.ParseRequest(ev.Format, ev.Request)
	reply := extract.ParseReply(ev.Format, ev.ReplyKind, ev.ReplyContentType, ev.Reply)
	derived := extract.Derive(headers, req)
	keyLabel := KeyLabel(ev.APIKey)
	level := settings.CaptureLevel(ev.APIKey)
	if ev.SessionID == "" {
		// Routes that skip the session tap (plugin executors) use the same derivation.
		if info, ok := coresession.ExtractSessionInfo(headers, ev.Request, nil); ok {
			ev.SessionID, ev.ParentSessionID = info.SessionID, info.ParentSessionID
		}
	}

	sess, parent, err := s.resolveSession(tx, settings, ev, derived, keyLabel)
	if err != nil {
		return err
	}
	if extract.NeedsTitle(sess.Title) {
		title := extract.Title(req.Messages)
		if title == "" {
			title = sess.Title
		}
		if settings.RedactSecrets {
			title = extract.Redact(title)
		}
		sess.Title = title
	}
	if derived.Folder != "" {
		sess.Folder = derived.Folder
	}
	if derived.GitRemote != "" {
		if sess.Folder != "" {
			if errRemote := tx.SetFolderRemote(sess.Folder, derived.GitRemote); errRemote != nil {
				return errRemote
			}
		}
		sess.GitRemote = derived.GitRemote
	} else if sess.Folder != "" {
		remote, errRemote := tx.FolderRemote(sess.Folder)
		if errRemote != nil {
			return errRemote
		}
		if remote != "" {
			sess.GitRemote = remote
		}
	}
	applyProject(settings, sess, parent)
	if err = tx.UpsertSession(sess); err != nil {
		return err
	}

	seq, err := tx.MaxSeq(sess.ID)
	if err != nil {
		return err
	}
	turn := &store.Turn{
		ID:           ev.ID,
		SessionID:    sess.ID,
		RootID:       sess.RootID,
		Seq:          seq + 1,
		RequestID:    ev.RequestID,
		StartedAt:    ev.StartedAt.UTC(),
		EndedAt:      ev.EndedAt.UTC(),
		LatencyMs:    ev.EndedAt.Sub(ev.StartedAt).Milliseconds(),
		Path:         ev.Path,
		Format:       ev.Format,
		Stream:       ev.Stream,
		Model:        ev.Model,
		ServedModel:  reply.Model,
		HTTPStatus:   ev.HTTPStatus,
		StopReason:   reply.StopReason,
		ToolNames:    reply.ToolNames(),
		CaptureLevel: level,
		ReplyError:   reply.ErrorText,
	}
	if turn.Model == "" {
		turn.Model = req.Model
	}
	if existing, errGet := tx.GetTurn(ev.ID); errGet == nil && existing != nil {
		turn.Seq = existing.Seq
	}
	if !ev.FirstByteAt.IsZero() && ev.Stream {
		ttft := ev.FirstByteAt.Sub(ev.StartedAt).Milliseconds()
		turn.TTFTMs = &ttft
	}
	switch {
	case ev.ErrorText != "":
		turn.Error = ev.ErrorText
	case ev.HTTPStatus >= 400:
		turn.Error = extract.ErrorText(ev.Reply)
		if turn.Error == "" {
			turn.Error = http.StatusText(ev.HTTPStatus)
		}
	case reply.ErrorText != "":
		turn.Error = reply.ErrorText
	}
	if settings.RedactSecrets {
		turn.Error = extract.Redact(turn.Error)
	}
	turn.ContentDropped = ev.ContentDropped
	if level == CaptureFull && !ev.ContentDropped && (len(ev.Request) > 0 || len(ev.Reply) > 0) {
		refs, errContent := s.storeContent(tx, settings, sess.ID, turn, req, reply)
		if errContent != nil {
			return errContent
		}
		if err = tx.InsertTurnBlobs(turn.ID, refs); err != nil {
			return err
		}
		turn.ContentAvailable = len(refs) > 0
	}
	if err = tx.InsertTurn(turn); err != nil {
		return err
	}
	if err = linkGaps(tx, turn); err != nil {
		return err
	}
	if err = recordToolCalls(tx, turn, req, reply); err != nil {
		return err
	}
	return recomputeTurn(tx, turn.ID)
}

// resolveSession finds or creates the session of a turn and refreshes its
// session-level fields. The parent session is returned when known.
func (s *Service) resolveSession(tx store.Tx, settings Settings, ev *capture.TurnEvent, derived extract.Derived, keyLabel string) (*store.Session, *store.Session, error) {
	id := strings.TrimSpace(ev.SessionID)
	parentID := strings.TrimSpace(ev.ParentSessionID)
	synthetic := false
	if id == "" {
		synthetic = true
		found, err := tx.FindSyntheticSession(store.SyntheticKey{
			KeyLabel: keyLabel, Client: derived.Client, Folder: derived.Folder,
			HeaderProject: derived.HeaderProject, PathProject: derived.PathProject, GitRemote: derived.GitRemote,
		}, ev.StartedAt.Add(-syntheticSessionGap))
		if err != nil {
			return nil, nil, err
		}
		id = found
		if id == "" {
			id = "syn-" + uuid.NewString()
		}
	}
	sess, err := tx.GetSession(id)
	if err != nil {
		return nil, nil, err
	}
	var parent *store.Session
	if parentID != "" && parentID != id {
		if parent, err = tx.GetSession(parentID); err != nil {
			return nil, nil, err
		}
	}
	if sess == nil {
		sess = &store.Session{ID: id, ParentID: parentID, RootID: id, Synthetic: synthetic, StartedAt: ev.StartedAt.UTC(), LastAt: ev.EndedAt.UTC()}
		if parentID != "" && parentID != id {
			sess.RootID = parentID
			if parent != nil && parent.RootID != "" {
				sess.RootID = parent.RootID
			}
		}
	} else if parent == nil && sess.ParentID != "" {
		if parent, err = tx.GetSession(sess.ParentID); err != nil {
			return nil, nil, err
		}
	}
	if ev.StartedAt.Before(sess.StartedAt) || sess.StartedAt.IsZero() {
		sess.StartedAt = ev.StartedAt.UTC()
	}
	if ev.EndedAt.After(sess.LastAt) {
		sess.LastAt = ev.EndedAt.UTC()
	}
	if derived.Client != "" && (sess.Client == "" || sess.Client == extract.ClientUnknown) {
		sess.Client = derived.Client
	}
	if derived.ClientVersion != "" {
		sess.ClientVersion = derived.ClientVersion
	}
	if keyLabel != "" {
		sess.KeyLabel = keyLabel
		sess.Tags = append([]string(nil), settings.KeyTags[strings.TrimSpace(ev.APIKey)]...)
	}
	if ev.ClientIP != "" {
		sess.ClientIP = ev.ClientIP
		sess.Machine = settings.Machine(ev.ClientIP)
	}
	if derived.DeviceID != "" {
		sess.DeviceID = derived.DeviceID
	}
	if derived.ClientProjectHash != "" {
		sess.ClientProjectHash = derived.ClientProjectHash
	}
	if derived.HeaderProject != "" {
		sess.HeaderProject = derived.HeaderProject
	}
	if derived.PathProject != "" {
		sess.PathProject = derived.PathProject
	}
	if ev.Model != "" {
		sess.Model = ev.Model
	}
	if sess.Folder == "" && derived.Folder == "" && parent != nil {
		sess.Folder = parent.Folder
	}
	return sess, parent, nil
}

// applyProject resolves the project of a session and stores it with its source.
func applyProject(settings Settings, sess *store.Session, parent *store.Session) {
	name, key, source, rule := resolveProject(settings, sess, parent)
	sess.ProjectName = name
	sess.ProjectKey = key
	sess.ProjectSource = source
	sess.ProjectRule = rule
	sess.ProjectID = ProjectID(name)
}

// Project sources, as stored per session and shown as "project_source".
const (
	SourceHeader    = "header"
	SourcePath      = "path"
	SourceRule      = "rule"
	SourceRemote    = "remote"
	SourceFolder    = "folder"
	SourceInherited = "inherited"
	SourceNone      = "none"
	UnassignedName  = "unassigned"
)

// legacySourceInherit is the old spelling of SourceInherited in stored rows.
const legacySourceInherit = "inherit"

// resolveProject picks the project of a session. The first match wins:
//
//  1. X-CPA-Project header (header)
//  2. /p/<project> in the base URL (path)
//  3. a config rule that matches the folder (rule)
//  4. the git remote (remote)
//  5. the folder name, unless the folder is inside a worktree container (folder)
//  6. the parent session, or an earlier turn of this session (inherited)
//  7. else "unassigned" (none)
func resolveProject(settings Settings, sess *store.Session, parent *store.Session) (name, key, source, rule string) {
	if sess.HeaderProject != "" {
		return sess.HeaderProject, sess.HeaderProject, SourceHeader, ""
	}
	if sess.PathProject != "" {
		return sess.PathProject, sess.PathProject, SourcePath, ""
	}
	if sess.Folder != "" {
		for _, r := range settings.Rules {
			if project, ok := r.Apply(sess.Folder); ok {
				return project, project, SourceRule, r.Match
			}
		}
	}
	if sess.GitRemote != "" {
		return extract.ProjectNameFromRemote(sess.GitRemote), sess.GitRemote, SourceRemote, ""
	}
	if sess.Folder != "" && !settings.IsWorktreeFolder(sess.Folder) {
		return extract.ProjectNameFromFolder(sess.Folder), sess.Folder, SourceFolder, ""
	}
	if parent != nil && parent.ProjectName != "" && parent.ProjectName != UnassignedName {
		return parent.ProjectName, parent.ProjectKey, SourceInherited, ""
	}
	// Only an inherited project stays without a signal of its own. Every other
	// source is derived again from the stored signals, so a stale name goes away.
	if sess.ProjectName != "" && sess.ProjectName != UnassignedName && (sess.ProjectSource == SourceInherited || sess.ProjectSource == legacySourceInherit) {
		return sess.ProjectName, sess.ProjectKey, SourceInherited, ""
	}
	return UnassignedName, UnassignedName, SourceNone, ""
}

// storeContent writes the content units of a turn as blobs and returns the
// ordered refs. A unit is new when the previous turn of the session did not
// carry it; the reply is always shown.
func (s *Service) storeContent(tx store.Tx, settings Settings, sessionID string, turn *store.Turn, req *extract.Request, reply *extract.Reply) ([]store.BlobRef, error) {
	seen := map[string]bool{}
	prev, err := tx.PrevTurnInSession(sessionID, turn.StartedAt, turn.ID)
	if err != nil {
		return nil, err
	}
	if prev != nil {
		refs, errRefs := tx.TurnBlobRefs(prev.ID)
		if errRefs != nil {
			return nil, errRefs
		}
		for _, r := range refs {
			seen[r.Hash] = true
		}
	}
	var refs []store.BlobRef
	add := func(msg extract.Message, role string, alwaysNew bool) error {
		if settings.RedactSecrets {
			msg = extract.RedactMessage(msg)
		}
		raw := msg.Canonical()
		hash := msg.Hash()
		if _, errPut := tx.PutBlob(hash, role, raw); errPut != nil {
			return errPut
		}
		refs = append(refs, store.BlobRef{Ord: len(refs), Hash: hash, Role: role, IsNew: alwaysNew || !seen[hash]})
		return nil
	}
	if req.System != nil {
		if err = add(*req.System, store.RoleSystem, false); err != nil {
			return nil, err
		}
	}
	if req.Tools != "" {
		tools := extract.Message{Role: "tools", Blocks: []extract.Block{{Kind: extract.KindSystem, Text: req.Tools}}}
		if err = add(tools, store.RoleTools, false); err != nil {
			return nil, err
		}
	}
	for _, msg := range req.Messages {
		if err = add(msg, store.RoleMessage, false); err != nil {
			return nil, err
		}
	}
	for _, msg := range reply.Messages {
		if err = add(msg, store.RoleReply, true); err != nil {
			return nil, err
		}
	}
	return refs, nil
}

// linkGaps updates the gap to the next turn start in the session tree for the
// new turn and its predecessor. Active time is computed from these gaps.
func linkGaps(tx store.Tx, turn *store.Turn) error {
	prev, next, err := tx.NeighborTurns(turn.RootID, turn.StartedAt, turn.ID)
	if err != nil {
		return err
	}
	if prev != nil {
		gap := turn.StartedAt.Sub(prev.StartedAt).Milliseconds()
		if err = tx.SetGapNext(prev.ID, &gap); err != nil {
			return err
		}
	}
	var gap *int64
	if next != nil {
		g := next.StartedAt.Sub(turn.StartedAt).Milliseconds()
		gap = &g
	}
	return tx.SetGapNext(turn.ID, gap)
}

// recordToolCalls stores the tool calls of the reply and the request history,
// and marks calls whose results report failure.
func recordToolCalls(tx store.Tx, turn *store.Turn, req *extract.Request, reply *extract.Reply) error {
	record := func(b extract.Block) error {
		callID := b.ToolCallID
		if callID == "" {
			sum := sha256.Sum256([]byte(b.ToolName + "\x00" + b.Text))
			callID = "anon:" + hex.EncodeToString(sum[:])[:16]
		}
		call := &store.ToolCall{RootID: turn.RootID, SessionID: turn.SessionID, TurnID: turn.ID, CallID: callID, Name: b.ToolName}
		if diff := extract.DiffFromToolCall(b.ToolName, b.Text); diff != nil {
			call.File = diff.File
			call.Added = diff.Added
			call.Removed = diff.Removed
			call.HasDiff = true
		}
		return tx.UpsertToolCall(call)
	}
	for _, msg := range reply.Messages {
		for _, b := range msg.Blocks {
			if b.Kind == extract.KindToolCall {
				if err := record(b); err != nil {
					return err
				}
			}
		}
	}
	// History before the last assistant message was handled with earlier turns;
	// only the tail can carry calls or results not seen yet.
	tail := req.Messages
	for i := len(req.Messages) - 1; i >= 0; i-- {
		if req.Messages[i].Role == "assistant" || req.Messages[i].Role == "model" {
			tail = req.Messages[i:]
			break
		}
	}
	for _, msg := range tail {
		for _, b := range msg.Blocks {
			switch b.Kind {
			case extract.KindToolCall:
				if err := record(b); err != nil {
					return err
				}
			case extract.KindToolResult:
				if b.ToolCallID != "" && extract.ResultFailed(b.Text, b.IsError) {
					if err := tx.MarkToolCallFailed(turn.RootID, b.ToolCallID); err != nil {
						return err
					}
				}
			}
		}
	}
	return nil
}

func (s *Service) resolveTurnID(tx store.Tx, turnID, traceID string) (string, error) {
	if turnID != "" {
		return turnID, nil
	}
	return tx.TurnIDByRequestID(traceID)
}

func (s *Service) ingestUsage(tx store.Tx, ev *capture.UsageEvent) (bool, error) {
	rec := ev.Record
	if err := sampleLimits(tx, rec.AuthID, rec.Provider, rec.ResponseHeaders, ev.At); err != nil {
		return false, err
	}
	turnID, err := s.resolveTurnID(tx, ev.TurnID, ev.TraceID)
	if err != nil {
		return false, err
	}
	if turnID == "" {
		return true, nil
	}
	row := &store.UsageRow{
		TurnID:        turnID,
		TraceID:       ev.TraceID,
		At:            ev.At.UTC(),
		AuthID:        rec.AuthID,
		Provider:      rec.Provider,
		Model:         rec.Model,
		ResponseModel: rec.ResponseModel,
		Failed:        rec.Failed,
		StatusCode:    rec.Fail.StatusCode,
		LatencyMs:     rec.Latency.Milliseconds(),
		Tokens:        NormalizeTokens(rec),
	}
	if rec.Detail.CostDetails != nil && rec.Detail.CostDetails.Cost != nil {
		c := *rec.Detail.CostDetails.Cost
		row.UpstreamCostUSD = &c
	}
	if err = tx.InsertUsage(row); err != nil {
		return false, err
	}
	// The ID may be a fallback join; a turn that is not stored yet is recomputed on insert.
	return false, recomputeTurn(tx, turnID)
}

func (s *Service) ingestAttempt(tx store.Tx, ev *capture.AttemptEvent) (bool, error) {
	turnID, err := s.resolveTurnID(tx, ev.TurnID, ev.TraceID)
	if err != nil {
		return false, err
	}
	if turnID == "" {
		return true, nil
	}
	a := &store.Attempt{
		TurnID: turnID, TraceID: ev.TraceID, At: ev.At.UTC(), AuthID: ev.AuthID, Provider: ev.Provider,
		Model: ev.Model, Success: ev.Success, HTTPStatus: ev.HTTPStatus, Error: extract.Redact(ev.Error),
	}
	if err = tx.InsertAttempt(a); err != nil {
		return false, err
	}
	return false, recomputeTurn(tx, turnID)
}

// recomputeTurn refreshes token totals, account, provider, attempt count and
// status of a turn from its usage and attempt rows.
func recomputeTurn(tx store.Tx, turnID string) error {
	turn, err := tx.GetTurn(turnID)
	if err != nil || turn == nil {
		return err
	}
	rows, err := tx.UsageForTurn(turnID)
	if err != nil {
		return err
	}
	attempts, err := tx.AttemptsForTurn(turnID)
	if err != nil {
		return err
	}
	var tokens store.Tokens
	var upstream *float64
	failed := false
	// Account and provider come from the last successful try, then the last try.
	account, provider := "", ""
	for _, a := range attempts {
		if !a.Success {
			failed = true
			continue
		}
		account, provider = a.AuthID, a.Provider
	}
	for _, r := range rows {
		tokens.Add(r.Tokens)
		if r.UpstreamCostUSD != nil {
			v := *r.UpstreamCostUSD
			if upstream != nil {
				v += *upstream
			}
			upstream = &v
		}
		if r.Failed {
			failed = true
			continue
		}
		if r.AuthID != "" {
			account = r.AuthID
		}
		if r.Provider != "" {
			provider = r.Provider
		}
		if r.ResponseModel != "" {
			turn.ServedModel = r.ResponseModel
		} else if r.Model != "" {
			turn.ServedModel = r.Model
		}
	}
	if account == "" && len(attempts) > 0 {
		last := attempts[len(attempts)-1]
		account, provider = last.AuthID, last.Provider
	}
	if account == "" && len(rows) > 0 {
		last := rows[len(rows)-1]
		account, provider = last.AuthID, last.Provider
	}
	turn.Account, turn.Provider = account, provider
	turn.Tokens = tokens
	turn.UpstreamCostUSD = upstream
	turn.Attempts = len(attempts)
	if turn.Attempts == 0 {
		turn.Attempts = len(rows)
	}
	if turn.Attempts == 0 {
		turn.Attempts = 1
	}
	switch {
	case turn.HTTPStatus >= 400 || turn.HTTPStatus == 0 || turn.ReplyError != "":
		turn.Status = StatusError
	case failed:
		turn.Status = StatusRetried
	default:
		turn.Status = StatusOK
	}
	return tx.UpdateTurnAggregates(turn)
}

// NormalizeTokens maps a usage record to the trace token object. Uncached
// input excludes cache reads and writes; output includes reasoning.
func NormalizeTokens(rec usage.Record) store.Tokens {
	d := rec.Detail
	var t store.Tokens
	b := d.TokenBreakdown
	var cacheWrite int64
	if b.Valid() {
		t.Input = b.Input.UncachedTokens
		t.CacheRead = b.Input.CacheReadTokens
		cacheWrite = b.Input.CacheWriteTokens
		t.Output = b.Output.TotalTokens
		t.Reasoning = b.Output.ReasoningTokens
	} else {
		cacheRead := d.CacheReadTokens
		if cacheRead == 0 && d.CacheCreationTokens == 0 {
			cacheRead = d.CachedTokens
		}
		cacheWrite = d.CacheCreationTokens
		if anthropicStyle(rec) {
			t.Input = d.InputTokens
		} else {
			t.Input = d.InputTokens - cacheRead - cacheWrite
			if t.Input < 0 {
				t.Input = 0
			}
		}
		t.CacheRead = cacheRead
		t.Output = d.OutputTokens
		t.Reasoning = d.ReasoningTokens
	}
	var w1h int64
	if d.CacheCreation1hTokens != nil {
		w1h += *d.CacheCreation1hTokens
	}
	if d.CacheCreation12hTokens != nil {
		w1h += *d.CacheCreation12hTokens
	}
	if d.CacheDetails != nil && w1h == 0 && d.CacheDetails.Ephemeral1hInputTokens != nil {
		w1h = *d.CacheDetails.Ephemeral1hInputTokens
	}
	if w1h > cacheWrite {
		w1h = cacheWrite
	}
	t.CacheWrite1h = w1h
	t.CacheWrite5m = cacheWrite - w1h
	t.Total = t.Input + t.CacheRead + t.CacheWrite5m + t.CacheWrite1h + t.Output
	return t
}

func anthropicStyle(rec usage.Record) bool {
	for _, v := range []string{rec.Provider, rec.ExecutorType} {
		lower := strings.ToLower(v)
		if strings.Contains(lower, "claude") || strings.Contains(lower, "anthropic") {
			return true
		}
	}
	return false
}
