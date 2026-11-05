package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"os"
	"strings"
	"time"
)

func prefixedSessionCols(alias string) string {
	cols := strings.Split(sessionCols, ",")
	for i, c := range cols {
		cols[i] = alias + "." + strings.TrimSpace(c)
	}
	return strings.Join(cols, ", ")
}

// QueryTurns returns matching turns joined with their session, ordered by
// root session and start time.
func (s *SQLite) QueryTurns(ctx context.Context, f TurnFilter) ([]Turn, error) {
	var where []string
	var args []any
	if !f.From.IsZero() {
		where = append(where, "t.started_at >= ?")
		args = append(args, ms(f.From))
	}
	if !f.To.IsZero() {
		where = append(where, "t.started_at < ?")
		args = append(args, ms(f.To))
	}
	if f.Project != "" {
		where = append(where, "(s.project_id = ? OR s.project_name = ?)")
		args = append(args, f.Project, f.Project)
	}
	if f.Folder != "" {
		prefix := strings.TrimRight(f.Folder, "/")
		where = append(where, "(s.folder = ? OR instr(s.folder, ?) = 1)")
		args = append(args, prefix, prefix+"/")
	}
	if f.Client != "" {
		where = append(where, "s.client = ?")
		args = append(args, f.Client)
	}
	if f.Model != "" {
		where = append(where, "(t.model = ? OR t.served_model = ?)")
		args = append(args, f.Model, f.Model)
	}
	if f.Key != "" {
		where = append(where, "s.key_label = ?")
		args = append(args, f.Key)
	}
	if f.Tag == UntaggedFilter {
		where = append(where, "s.tags = '[]'")
	} else if f.Tag != "" {
		// Tags are stored as a JSON list; match the quoted tag.
		quoted := encodeList([]string{f.Tag})
		where = append(where, "instr(s.tags, ?) > 0")
		args = append(args, quoted[1:len(quoted)-1])
	}
	if f.Account != "" {
		where = append(where, "t.account = ?")
		args = append(args, f.Account)
	}
	if f.Machine != "" {
		where = append(where, "s.machine = ?")
		args = append(args, f.Machine)
	}
	switch f.Status {
	case "ok":
		where = append(where, "t.status <> 'error'")
	case "error":
		where = append(where, "t.status = 'error'")
	}
	if f.Query != "" {
		where = append(where, "(instr(lower(s.title), lower(?)) > 0 OR instr(lower(COALESCE(r.title, '')), lower(?)) > 0)")
		args = append(args, f.Query, f.Query)
	}
	if len(f.SessionIDs) > 0 {
		where = append(where, "t.session_id IN ("+placeholders(len(f.SessionIDs))+")")
		args = append(args, stringArgs(f.SessionIDs)...)
	}
	if len(f.RootIDs) > 0 {
		where = append(where, "t.root_id IN ("+placeholders(len(f.RootIDs))+")")
		args = append(args, stringArgs(f.RootIDs)...)
	}
	query := `SELECT ` + turnCols + `, ` + prefixedSessionCols("s") + `, COALESCE(r.title, '')
FROM turns t JOIN sessions s ON s.id = t.session_id LEFT JOIN sessions r ON r.id = t.root_id`
	if len(where) > 0 {
		query += " WHERE " + strings.Join(where, " AND ")
	}
	query += " ORDER BY t.root_id, t.started_at, t.seq"
	rows, err := s.db.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, fmt.Errorf("query turns: %w", err)
	}
	defer closeRows(rows)
	var out []Turn
	for rows.Next() {
		var t Turn
		var aux turnAux
		var sess Session
		var tags string
		var synthetic int
		var started, last int64
		dest := turnDest(&t, &aux)
		dest = append(dest, &sess.ID, &sess.RootID, &sess.ParentID, &sess.Title, &sess.ProjectID, &sess.ProjectName, &sess.ProjectKey, &sess.ProjectSource, &sess.ProjectRule, &sess.HeaderProject, &sess.Folder, &sess.GitRemote, &sess.ClientProjectHash, &sess.Client, &sess.ClientVersion, &sess.KeyLabel, &tags, &sess.Machine, &sess.ClientIP, &sess.DeviceID, &sess.Model, &synthetic, &started, &last, &t.RootTitle)
		if err = rows.Scan(dest...); err != nil {
			return nil, fmt.Errorf("scan turn: %w", err)
		}
		aux.apply(&t)
		sess.Tags = decodeList(tags)
		sess.Synthetic = synthetic != 0
		sess.StartedAt = fromMs(started)
		sess.LastAt = fromMs(last)
		t.Session = &sess
		out = append(out, t)
	}
	return out, rows.Err()
}

func (s *SQLite) sessionsWhere(ctx context.Context, where string, args ...any) ([]Session, error) {
	query := `SELECT ` + sessionCols + ` FROM sessions`
	if where != "" {
		query += " WHERE " + where
	}
	query += " ORDER BY started_at"
	rows, err := s.db.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, fmt.Errorf("query sessions: %w", err)
	}
	defer closeRows(rows)
	var out []Session
	for rows.Next() {
		sess, errScan := scanSession(rows)
		if errScan != nil {
			return nil, errScan
		}
		out = append(out, *sess)
	}
	return out, rows.Err()
}

// Sessions returns the sessions with the given IDs.
func (s *SQLite) Sessions(ctx context.Context, ids []string) ([]Session, error) {
	if len(ids) == 0 {
		return nil, nil
	}
	return s.sessionsWhere(ctx, "id IN ("+placeholders(len(ids))+")", stringArgs(ids)...)
}

// SessionsByRoot returns every session of the given session trees.
func (s *SQLite) SessionsByRoot(ctx context.Context, rootIDs []string) ([]Session, error) {
	if len(rootIDs) == 0 {
		return nil, nil
	}
	return s.sessionsWhere(ctx, "root_id IN ("+placeholders(len(rootIDs))+")", stringArgs(rootIDs)...)
}

// AllSessions returns every session.
func (s *SQLite) AllSessions(ctx context.Context) ([]Session, error) {
	return s.sessionsWhere(ctx, "")
}

// UpdateSessionProjects stores re-resolved project fields.
func (s *SQLite) UpdateSessionProjects(ctx context.Context, sessions []Session) error {
	return s.Write(ctx, func(tx Tx) error {
		st := tx.(*sqliteTx)
		for _, sess := range sessions {
			if _, err := st.tx.ExecContext(ctx, `UPDATE sessions SET project_id=?, project_name=?, project_key=?, project_source=?, project_rule=?, git_remote=? WHERE id = ?`,
				sess.ProjectID, sess.ProjectName, sess.ProjectKey, sess.ProjectSource, sess.ProjectRule, sess.GitRemote, sess.ID); err != nil {
				return err
			}
		}
		return nil
	})
}

// Turn returns one turn, or nil.
func (s *SQLite) Turn(ctx context.Context, id string) (*Turn, error) { return getTurn(ctx, s.db, id) }

// UsageForTurn returns the usage rows of a turn.
func (s *SQLite) UsageForTurn(ctx context.Context, turnID string) ([]UsageRow, error) {
	return usageRows(ctx, s.db, turnID)
}

// AttemptsForTurns returns the attempts of the given turns.
func (s *SQLite) AttemptsForTurns(ctx context.Context, turnIDs []string) ([]Attempt, error) {
	if len(turnIDs) == 0 {
		return nil, nil
	}
	return attemptRows(ctx, s.db, "turn_id IN ("+placeholders(len(turnIDs))+")", stringArgs(turnIDs)...)
}

// TurnBlobRefs returns the ordered content refs of a turn.
func (s *SQLite) TurnBlobRefs(ctx context.Context, turnID string) ([]BlobRef, error) {
	return blobRefs(ctx, s.db, turnID)
}

// Blob returns the decompressed blob, or nil when it does not exist.
func (s *SQLite) Blob(ctx context.Context, hash string) ([]byte, error) {
	var data []byte
	err := s.db.QueryRowContext(ctx, `SELECT data FROM blobs WHERE hash = ?`, hash).Scan(&data)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return s.dec.DecodeAll(data, nil)
}

func (s *SQLite) toolCallsWhere(ctx context.Context, where string, args ...any) ([]ToolCall, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT root_id, session_id, turn_id, call_id, name, file, added, removed, has_diff, failed FROM tool_calls WHERE `+where, args...)
	if err != nil {
		return nil, err
	}
	defer closeRows(rows)
	var out []ToolCall
	for rows.Next() {
		var c ToolCall
		var hasDiff, failed int
		if err = rows.Scan(&c.RootID, &c.SessionID, &c.TurnID, &c.CallID, &c.Name, &c.File, &c.Added, &c.Removed, &hasDiff, &failed); err != nil {
			return nil, err
		}
		c.HasDiff = hasDiff != 0
		c.Failed = failed != 0
		out = append(out, c)
	}
	return out, rows.Err()
}

// ToolCalls returns the tool calls of the given sessions.
func (s *SQLite) ToolCalls(ctx context.Context, sessionIDs []string) ([]ToolCall, error) {
	if len(sessionIDs) == 0 {
		return nil, nil
	}
	return s.toolCallsWhere(ctx, "session_id IN ("+placeholders(len(sessionIDs))+")", stringArgs(sessionIDs)...)
}

// ToolCallsByID returns tool calls of one session tree by call ID.
func (s *SQLite) ToolCallsByID(ctx context.Context, rootID string, callIDs []string) ([]ToolCall, error) {
	if len(callIDs) == 0 {
		return nil, nil
	}
	args := append([]any{rootID}, stringArgs(callIDs)...)
	return s.toolCallsWhere(ctx, "root_id = ? AND call_id IN ("+placeholders(len(callIDs))+")", args...)
}

// LimitSamples returns samples taken at or after since.
func (s *SQLite) LimitSamples(ctx context.Context, since time.Time) ([]LimitSample, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT account_id, at, win_name, used_pct, status, resets_at, overage_status, provider, plan FROM limit_samples WHERE at >= ? ORDER BY at, id`, ms(since))
	if err != nil {
		return nil, err
	}
	defer closeRows(rows)
	var out []LimitSample
	for rows.Next() {
		sample, errScan := scanLimitSample(rows)
		if errScan != nil {
			return nil, errScan
		}
		out = append(out, *sample)
	}
	return out, rows.Err()
}

// Accounts returns the stored account rows.
func (s *SQLite) Accounts(ctx context.Context) ([]Account, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT account_id, plan_price_usd, plan FROM accounts ORDER BY account_id`)
	if err != nil {
		return nil, err
	}
	defer closeRows(rows)
	var out []Account
	for rows.Next() {
		var a Account
		var price sql.NullFloat64
		if err = rows.Scan(&a.AccountID, &price, &a.Plan); err != nil {
			return nil, err
		}
		a.PlanPriceUSD = ptrFloat(price)
		out = append(out, a)
	}
	return out, rows.Err()
}

// SetPlanPrice stores the monthly plan price of an account (nil clears it).
func (s *SQLite) SetPlanPrice(ctx context.Context, accountID string, price *float64) error {
	_, err := s.db.ExecContext(ctx, `INSERT INTO accounts (account_id, plan_price_usd) VALUES (?, ?) ON CONFLICT(account_id) DO UPDATE SET plan_price_usd = excluded.plan_price_usd`, accountID, nullFloat(price))
	return err
}

// SeenModels returns every model name recorded in traces.
func (s *SQLite) SeenModels(ctx context.Context) ([]string, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT model FROM turns WHERE model <> '' UNION SELECT served_model FROM turns WHERE served_model <> '' UNION SELECT model FROM usage_rows WHERE model <> '' ORDER BY 1`)
	if err != nil {
		return nil, err
	}
	defer closeRows(rows)
	var out []string
	for rows.Next() {
		var m string
		if err = rows.Scan(&m); err != nil {
			return nil, err
		}
		out = append(out, m)
	}
	return out, rows.Err()
}

// GetKV returns a cached value and its update time.
func (s *SQLite) GetKV(ctx context.Context, key string) ([]byte, time.Time, error) {
	var value []byte
	var updated int64
	err := s.db.QueryRowContext(ctx, `SELECT value, updated_at FROM kv WHERE key = ?`, key).Scan(&value, &updated)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, time.Time{}, nil
	}
	return value, fromMs(updated), err
}

// PutKV stores a cached value.
func (s *SQLite) PutKV(ctx context.Context, key string, value []byte) error {
	_, err := s.db.ExecContext(ctx, `INSERT INTO kv (key, value, updated_at) VALUES (?, ?, ?) ON CONFLICT(key) DO UPDATE SET value = excluded.value, updated_at = excluded.updated_at`, key, value, ms(time.Now()))
	return err
}

// Delete removes the selected turns and the sessions left without turns.
func (s *SQLite) Delete(ctx context.Context, f DeleteFilter) (int64, int64, error) {
	var where []string
	var args []any
	if f.SessionID != "" {
		where = append(where, "(s.id = ? OR s.root_id = ?)")
		args = append(args, f.SessionID, f.SessionID)
	}
	if f.Project != "" {
		where = append(where, "(s.project_id = ? OR s.project_name = ?)")
		args = append(args, f.Project, f.Project)
	}
	if f.Key != "" {
		where = append(where, "s.key_label = ?")
		args = append(args, f.Key)
	}
	if !f.From.IsZero() && !f.To.IsZero() {
		where = append(where, "t.started_at >= ? AND t.started_at < ?")
		args = append(args, ms(f.From), ms(f.To))
	}
	if len(where) == 0 {
		return 0, 0, fmt.Errorf("delete needs a filter")
	}
	var deletedSessions, deletedTurns int64
	err := s.Write(ctx, func(tx Tx) error {
		st := tx.(*sqliteTx).tx
		if _, err := st.ExecContext(ctx, `CREATE TEMP TABLE IF NOT EXISTS doomed (id TEXT PRIMARY KEY)`); err != nil {
			return err
		}
		if _, err := st.ExecContext(ctx, `DELETE FROM doomed`); err != nil {
			return err
		}
		if _, err := st.ExecContext(ctx, `INSERT OR IGNORE INTO doomed (id) SELECT t.id FROM turns t JOIN sessions s ON s.id = t.session_id WHERE `+strings.Join(where, " AND "), args...); err != nil {
			return err
		}
		for _, table := range []string{"usage_rows", "attempts", "turn_blobs", "tool_calls"} {
			if _, err := st.ExecContext(ctx, `DELETE FROM `+table+` WHERE turn_id IN (SELECT id FROM doomed)`); err != nil {
				return err
			}
		}
		res, err := st.ExecContext(ctx, `DELETE FROM turns WHERE id IN (SELECT id FROM doomed)`)
		if err != nil {
			return err
		}
		deletedTurns, _ = res.RowsAffected()
		res, err = st.ExecContext(ctx, `DELETE FROM sessions WHERE id NOT IN (SELECT DISTINCT session_id FROM turns)`)
		if err != nil {
			return err
		}
		deletedSessions, _ = res.RowsAffected()
		_, err = st.ExecContext(ctx, `DELETE FROM doomed`)
		return err
	})
	return deletedSessions, deletedTurns, err
}

// Stats returns storage numbers.
func (s *SQLite) Stats(ctx context.Context) (Stats, error) {
	var st Stats
	for _, suffix := range []string{"", "-wal", "-shm"} {
		if info, err := os.Stat(s.path + suffix); err == nil {
			st.DBBytes += info.Size()
		}
	}
	var content sql.NullInt64
	if err := s.db.QueryRowContext(ctx, `SELECT SUM(zsize) FROM blobs`).Scan(&content); err != nil {
		return st, err
	}
	st.ContentBytes = content.Int64
	var oldest sql.NullInt64
	if err := s.db.QueryRowContext(ctx, `SELECT MIN(started_at) FROM turns`).Scan(&oldest); err != nil {
		return st, err
	}
	if oldest.Valid {
		t := fromMs(oldest.Int64)
		st.OldestTurnAt = &t
	}
	return st, nil
}

// DropContentBefore removes content refs of turns started before t.
func (s *SQLite) DropContentBefore(ctx context.Context, t time.Time) (int64, error) {
	var n int64
	err := s.Write(ctx, func(tx Tx) error {
		st := tx.(*sqliteTx).tx
		if _, err := st.ExecContext(ctx, `DELETE FROM turn_blobs WHERE turn_id IN (SELECT id FROM turns WHERE started_at < ? AND content_available = 1)`, ms(t)); err != nil {
			return err
		}
		res, err := st.ExecContext(ctx, `UPDATE turns SET content_available = 0, content_dropped = 1 WHERE started_at < ? AND content_available = 1`, ms(t))
		if err != nil {
			return err
		}
		n, _ = res.RowsAffected()
		return nil
	})
	return n, err
}

// DropOldestContent removes content refs of the n oldest turns with content.
func (s *SQLite) DropOldestContent(ctx context.Context, n int) (int64, error) {
	var dropped int64
	err := s.Write(ctx, func(tx Tx) error {
		st := tx.(*sqliteTx).tx
		const pick = `SELECT id FROM turns WHERE content_available = 1 ORDER BY started_at LIMIT ?`
		if _, err := st.ExecContext(ctx, `DELETE FROM turn_blobs WHERE turn_id IN (`+pick+`)`, n); err != nil {
			return err
		}
		res, err := st.ExecContext(ctx, `UPDATE turns SET content_available = 0, content_dropped = 1 WHERE id IN (`+pick+`)`, n)
		if err != nil {
			return err
		}
		dropped, _ = res.RowsAffected()
		return nil
	})
	return dropped, err
}

// CollectGarbage deletes unreferenced blobs and orphan usage and attempt rows
// older than orphanBefore, then returns free pages to the file system.
func (s *SQLite) CollectGarbage(ctx context.Context, orphanBefore time.Time) error {
	err := s.Write(ctx, func(tx Tx) error {
		st := tx.(*sqliteTx).tx
		if _, err := st.ExecContext(ctx, `DELETE FROM blobs WHERE hash NOT IN (SELECT hash FROM turn_blobs)`); err != nil {
			return err
		}
		for _, table := range []string{"usage_rows", "attempts"} {
			if _, err := st.ExecContext(ctx, `DELETE FROM `+table+` WHERE at < ? AND turn_id NOT IN (SELECT id FROM turns)`, ms(orphanBefore)); err != nil {
				return err
			}
		}
		return nil
	})
	if err != nil {
		return err
	}
	if _, err = s.db.ExecContext(ctx, `PRAGMA incremental_vacuum`); err != nil {
		return fmt.Errorf("incremental vacuum: %w", err)
	}
	if _, err = s.db.ExecContext(ctx, `PRAGMA wal_checkpoint(TRUNCATE)`); err != nil {
		return fmt.Errorf("wal checkpoint: %w", err)
	}
	return nil
}
