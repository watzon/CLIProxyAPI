package store

import (
	"context"
	"database/sql"
	"errors"
	"time"
)

type sqliteTx struct {
	tx *sql.Tx
	s  *SQLite
}

var bg = context.Background()

func (t *sqliteTx) GetSession(id string) (*Session, error) {
	s, err := scanSession(t.tx.QueryRowContext(bg, `SELECT `+sessionCols+` FROM sessions WHERE id = ?`, id))
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	return s, err
}

func (t *sqliteTx) UpsertSession(s *Session) error {
	_, err := t.tx.ExecContext(bg, `INSERT INTO sessions (`+sessionCols+`) VALUES (?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?)
ON CONFLICT(id) DO UPDATE SET root_id=excluded.root_id, parent_id=excluded.parent_id, title=excluded.title, project_id=excluded.project_id,
project_name=excluded.project_name, project_key=excluded.project_key, project_source=excluded.project_source, project_rule=excluded.project_rule,
header_project=excluded.header_project, path_project=excluded.path_project, folder=excluded.folder, git_remote=excluded.git_remote, client_project_hash=excluded.client_project_hash,
client=excluded.client, client_version=excluded.client_version, key_label=excluded.key_label, tags=excluded.tags, machine=excluded.machine,
client_ip=excluded.client_ip, device_id=excluded.device_id, model=excluded.model, synthetic=excluded.synthetic, started_at=excluded.started_at, last_at=excluded.last_at`,
		s.ID, s.RootID, s.ParentID, s.Title, s.ProjectID, s.ProjectName, s.ProjectKey, s.ProjectSource, s.ProjectRule, s.HeaderProject, s.PathProject, s.Folder, s.GitRemote, s.ClientProjectHash,
		s.Client, s.ClientVersion, s.KeyLabel, encodeList(s.Tags), s.Machine, s.ClientIP, s.DeviceID, s.Model, boolInt(s.Synthetic), ms(s.StartedAt), ms(s.LastAt))
	return err
}

func (t *sqliteTx) FindSyntheticSession(k SyntheticKey, since time.Time) (string, error) {
	var id string
	err := t.tx.QueryRowContext(bg, `SELECT id FROM sessions WHERE synthetic = 1 AND key_label = ? AND client = ? AND folder = ? AND header_project = ? AND path_project = ?
AND (folder != '' OR git_remote = ?) AND last_at >= ? ORDER BY last_at DESC LIMIT 1`,
		k.KeyLabel, k.Client, k.Folder, k.HeaderProject, k.PathProject, k.GitRemote, ms(since)).Scan(&id)
	if errors.Is(err, sql.ErrNoRows) {
		return "", nil
	}
	return id, err
}

func (t *sqliteTx) MaxSeq(sessionID string) (int, error) {
	var seq sql.NullInt64
	if err := t.tx.QueryRowContext(bg, `SELECT MAX(seq) FROM turns WHERE session_id = ?`, sessionID).Scan(&seq); err != nil {
		return 0, err
	}
	return int(seq.Int64), nil
}

func (t *sqliteTx) InsertTurn(tr *Turn) error {
	_, err := t.tx.ExecContext(bg, `INSERT OR REPLACE INTO turns (id, session_id, root_id, seq, request_id, started_at, ended_at, latency_ms, ttft_ms, gap_next_ms, path, format, stream, model, served_model, provider, account, status, http_status, error, reply_error, attempts, stop_reason, tok_input, tok_output, tok_reasoning, tok_cache_read, tok_cw5, tok_cw1, tok_total, upstream_cost_usd, tool_names, capture_level, content_available, content_dropped)
VALUES (?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?)`,
		tr.ID, tr.SessionID, tr.RootID, tr.Seq, tr.RequestID, ms(tr.StartedAt), ms(tr.EndedAt), tr.LatencyMs, nullInt(tr.TTFTMs), nullInt(tr.GapNextMs), tr.Path, tr.Format, boolInt(tr.Stream),
		tr.Model, tr.ServedModel, tr.Provider, tr.Account, tr.Status, tr.HTTPStatus, tr.Error, tr.ReplyError, tr.Attempts, tr.StopReason,
		tr.Tokens.Input, tr.Tokens.Output, tr.Tokens.Reasoning, tr.Tokens.CacheRead, tr.Tokens.CacheWrite5m, tr.Tokens.CacheWrite1h, tr.Tokens.Total,
		nullFloat(tr.UpstreamCostUSD), encodeList(tr.ToolNames), tr.CaptureLevel, boolInt(tr.ContentAvailable), boolInt(tr.ContentDropped))
	return err
}

func (t *sqliteTx) GetTurn(id string) (*Turn, error) { return getTurn(bg, t.tx, id) }

func (t *sqliteTx) TurnIDByRequestID(requestID string) (string, error) {
	if requestID == "" {
		return "", nil
	}
	var id string
	err := t.tx.QueryRowContext(bg, `SELECT id FROM turns WHERE request_id = ? ORDER BY started_at DESC LIMIT 1`, requestID).Scan(&id)
	if errors.Is(err, sql.ErrNoRows) {
		return "", nil
	}
	return id, err
}

func (t *sqliteTx) PrevTurnInSession(sessionID string, before time.Time, excludeID string) (*Turn, error) {
	tr, err := scanTurn(t.tx.QueryRowContext(bg, `SELECT `+turnCols+` FROM turns t WHERE t.session_id = ? AND t.started_at <= ? AND t.id <> ? ORDER BY t.started_at DESC, t.seq DESC LIMIT 1`, sessionID, ms(before), excludeID))
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	return tr, err
}

func (t *sqliteTx) NeighborTurns(rootID string, at time.Time, excludeID string) (*Turn, *Turn, error) {
	prev, err := scanTurn(t.tx.QueryRowContext(bg, `SELECT `+turnCols+` FROM turns t WHERE t.root_id = ? AND t.started_at <= ? AND t.id <> ? ORDER BY t.started_at DESC LIMIT 1`, rootID, ms(at), excludeID))
	if errors.Is(err, sql.ErrNoRows) {
		prev, err = nil, nil
	}
	if err != nil {
		return nil, nil, err
	}
	next, err := scanTurn(t.tx.QueryRowContext(bg, `SELECT `+turnCols+` FROM turns t WHERE t.root_id = ? AND t.started_at > ? AND t.id <> ? ORDER BY t.started_at ASC LIMIT 1`, rootID, ms(at), excludeID))
	if errors.Is(err, sql.ErrNoRows) {
		next, err = nil, nil
	}
	return prev, next, err
}

func (t *sqliteTx) SetGapNext(turnID string, gapMs *int64) error {
	_, err := t.tx.ExecContext(bg, `UPDATE turns SET gap_next_ms = ? WHERE id = ?`, nullInt(gapMs), turnID)
	return err
}

func (t *sqliteTx) UpdateTurnAggregates(tr *Turn) error {
	_, err := t.tx.ExecContext(bg, `UPDATE turns SET served_model=?, provider=?, account=?, status=?, attempts=?, tok_input=?, tok_output=?, tok_reasoning=?, tok_cache_read=?, tok_cw5=?, tok_cw1=?, tok_total=?, upstream_cost_usd=? WHERE id = ?`,
		tr.ServedModel, tr.Provider, tr.Account, tr.Status, tr.Attempts, tr.Tokens.Input, tr.Tokens.Output, tr.Tokens.Reasoning, tr.Tokens.CacheRead, tr.Tokens.CacheWrite5m, tr.Tokens.CacheWrite1h, tr.Tokens.Total,
		nullFloat(tr.UpstreamCostUSD), tr.ID)
	return err
}

// PutBlob stores a content blob once. It returns the compressed size added (0 if it existed).
func (t *sqliteTx) PutBlob(hash, kind string, raw []byte) (int64, error) {
	var exists int
	err := t.tx.QueryRowContext(bg, `SELECT 1 FROM blobs WHERE hash = ?`, hash).Scan(&exists)
	if err == nil {
		return 0, nil
	}
	if !errors.Is(err, sql.ErrNoRows) {
		return 0, err
	}
	data := t.s.enc.EncodeAll(raw, nil)
	if _, err = t.tx.ExecContext(bg, `INSERT OR IGNORE INTO blobs (hash, kind, size, zsize, data, created_at) VALUES (?,?,?,?,?,?)`,
		hash, kind, len(raw), len(data), data, ms(time.Now())); err != nil {
		return 0, err
	}
	return int64(len(data)), nil
}

func (t *sqliteTx) InsertTurnBlobs(turnID string, refs []BlobRef) error {
	if _, err := t.tx.ExecContext(bg, `DELETE FROM turn_blobs WHERE turn_id = ?`, turnID); err != nil {
		return err
	}
	for _, r := range refs {
		if _, err := t.tx.ExecContext(bg, `INSERT INTO turn_blobs (turn_id, ord, hash, role, is_new) VALUES (?,?,?,?,?)`, turnID, r.Ord, r.Hash, r.Role, boolInt(r.IsNew)); err != nil {
			return err
		}
	}
	return nil
}

func (t *sqliteTx) TurnBlobRefs(turnID string) ([]BlobRef, error) { return blobRefs(bg, t.tx, turnID) }

func (t *sqliteTx) InsertUsage(u *UsageRow) error {
	_, err := t.tx.ExecContext(bg, `INSERT INTO usage_rows (turn_id, trace_id, at, auth_id, provider, model, response_model, failed, status_code, latency_ms, tok_input, tok_output, tok_reasoning, tok_cache_read, tok_cw5, tok_cw1, tok_total, upstream_cost_usd) VALUES (?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?)`,
		u.TurnID, u.TraceID, ms(u.At), u.AuthID, u.Provider, u.Model, u.ResponseModel, boolInt(u.Failed), u.StatusCode, u.LatencyMs,
		u.Tokens.Input, u.Tokens.Output, u.Tokens.Reasoning, u.Tokens.CacheRead, u.Tokens.CacheWrite5m, u.Tokens.CacheWrite1h, u.Tokens.Total, nullFloat(u.UpstreamCostUSD))
	return err
}

func (t *sqliteTx) UsageForTurn(turnID string) ([]UsageRow, error) {
	return usageRows(bg, t.tx, turnID)
}

func (t *sqliteTx) InsertAttempt(a *Attempt) error {
	_, err := t.tx.ExecContext(bg, `INSERT INTO attempts (turn_id, trace_id, at, auth_id, provider, model, success, http_status, error) VALUES (?,?,?,?,?,?,?,?,?)`,
		a.TurnID, a.TraceID, ms(a.At), a.AuthID, a.Provider, a.Model, boolInt(a.Success), a.HTTPStatus, a.Error)
	return err
}

func (t *sqliteTx) AttemptsForTurn(turnID string) ([]Attempt, error) {
	return attemptRows(bg, t.tx, `turn_id = ?`, turnID)
}

func (t *sqliteTx) UpsertToolCall(c *ToolCall) error {
	_, err := t.tx.ExecContext(bg, `INSERT INTO tool_calls (root_id, session_id, turn_id, call_id, name, file, added, removed, has_diff, failed) VALUES (?,?,?,?,?,?,?,?,?,?)
ON CONFLICT(root_id, call_id) DO NOTHING`,
		c.RootID, c.SessionID, c.TurnID, c.CallID, c.Name, c.File, c.Added, c.Removed, boolInt(c.HasDiff), boolInt(c.Failed))
	return err
}

func (t *sqliteTx) MarkToolCallFailed(rootID, callID string) error {
	_, err := t.tx.ExecContext(bg, `UPDATE tool_calls SET failed = 1 WHERE root_id = ? AND call_id = ?`, rootID, callID)
	return err
}

func (t *sqliteTx) FolderRemote(folder string) (string, error) {
	var remote string
	err := t.tx.QueryRowContext(bg, `SELECT git_remote FROM folders WHERE folder = ?`, folder).Scan(&remote)
	if errors.Is(err, sql.ErrNoRows) {
		return "", nil
	}
	return remote, err
}

func (t *sqliteTx) SetFolderRemote(folder, remote string) error {
	_, err := t.tx.ExecContext(bg, `INSERT INTO folders (folder, git_remote) VALUES (?, ?) ON CONFLICT(folder) DO UPDATE SET git_remote = excluded.git_remote`, folder, remote)
	return err
}

func (t *sqliteTx) LastLimitSample(accountID, window string) (*LimitSample, error) {
	row := t.tx.QueryRowContext(bg, `SELECT account_id, at, win_name, used_pct, status, resets_at, overage_status, provider, plan FROM limit_samples WHERE account_id = ? AND win_name = ? ORDER BY at DESC, id DESC LIMIT 1`, accountID, window)
	sample, err := scanLimitSample(row)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	return sample, err
}

func (t *sqliteTx) InsertLimitSample(s *LimitSample) error {
	var resets any
	if s.ResetsAt != nil {
		resets = ms(*s.ResetsAt)
	}
	_, err := t.tx.ExecContext(bg, `INSERT INTO limit_samples (account_id, at, win_name, used_pct, status, resets_at, overage_status, provider, plan) VALUES (?,?,?,?,?,?,?,?,?)`,
		s.AccountID, ms(s.At), s.Window, nullFloat(s.UsedPct), s.Status, resets, s.OverageStatus, s.Provider, s.Plan)
	return err
}

func (t *sqliteTx) SetAccountPlan(accountID, plan string) error {
	_, err := t.tx.ExecContext(bg, `INSERT INTO accounts (account_id, plan) VALUES (?, ?) ON CONFLICT(account_id) DO UPDATE SET plan = excluded.plan`, accountID, plan)
	return err
}

func scanLimitSample(row scanner) (*LimitSample, error) {
	var s LimitSample
	var at int64
	var used sql.NullFloat64
	var resets sql.NullInt64
	if err := row.Scan(&s.AccountID, &at, &s.Window, &used, &s.Status, &resets, &s.OverageStatus, &s.Provider, &s.Plan); err != nil {
		return nil, err
	}
	s.At = fromMs(at)
	s.UsedPct = ptrFloat(used)
	if resets.Valid {
		r := fromMs(resets.Int64)
		s.ResetsAt = &r
	}
	return &s, nil
}
