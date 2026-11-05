package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/klauspost/compress/zstd"
	log "github.com/sirupsen/logrus"
	_ "modernc.org/sqlite" // pure-Go SQLite driver, works without cgo
)

// DBFileName is the SQLite file inside the traces folder.
const DBFileName = "traces.db"

const schema = `
CREATE TABLE IF NOT EXISTS kv (
	key TEXT PRIMARY KEY,
	value BLOB,
	updated_at INTEGER NOT NULL
);
CREATE TABLE IF NOT EXISTS sessions (
	id TEXT PRIMARY KEY,
	root_id TEXT NOT NULL,
	parent_id TEXT NOT NULL DEFAULT '',
	title TEXT NOT NULL DEFAULT '',
	project_id TEXT NOT NULL DEFAULT '',
	project_name TEXT NOT NULL DEFAULT '',
	project_key TEXT NOT NULL DEFAULT '',
	project_source TEXT NOT NULL DEFAULT '',
	project_rule TEXT NOT NULL DEFAULT '',
	header_project TEXT NOT NULL DEFAULT '',
	folder TEXT NOT NULL DEFAULT '',
	git_remote TEXT NOT NULL DEFAULT '',
	client_project_hash TEXT NOT NULL DEFAULT '',
	client TEXT NOT NULL DEFAULT '',
	client_version TEXT NOT NULL DEFAULT '',
	key_label TEXT NOT NULL DEFAULT '',
	tags TEXT NOT NULL DEFAULT '[]',
	machine TEXT NOT NULL DEFAULT '',
	client_ip TEXT NOT NULL DEFAULT '',
	device_id TEXT NOT NULL DEFAULT '',
	model TEXT NOT NULL DEFAULT '',
	synthetic INTEGER NOT NULL DEFAULT 0,
	started_at INTEGER NOT NULL,
	last_at INTEGER NOT NULL
);
CREATE INDEX IF NOT EXISTS sessions_root ON sessions(root_id);
CREATE INDEX IF NOT EXISTS sessions_last ON sessions(last_at);
CREATE INDEX IF NOT EXISTS sessions_synthetic ON sessions(synthetic, key_label, client, folder, last_at);
CREATE TABLE IF NOT EXISTS turns (
	id TEXT PRIMARY KEY,
	session_id TEXT NOT NULL,
	root_id TEXT NOT NULL,
	seq INTEGER NOT NULL,
	request_id TEXT NOT NULL DEFAULT '',
	started_at INTEGER NOT NULL,
	ended_at INTEGER NOT NULL,
	latency_ms INTEGER NOT NULL,
	ttft_ms INTEGER,
	gap_next_ms INTEGER,
	path TEXT NOT NULL DEFAULT '',
	format TEXT NOT NULL DEFAULT '',
	stream INTEGER NOT NULL DEFAULT 0,
	model TEXT NOT NULL DEFAULT '',
	served_model TEXT NOT NULL DEFAULT '',
	provider TEXT NOT NULL DEFAULT '',
	account TEXT NOT NULL DEFAULT '',
	status TEXT NOT NULL DEFAULT 'ok',
	http_status INTEGER NOT NULL DEFAULT 0,
	error TEXT NOT NULL DEFAULT '',
	reply_error TEXT NOT NULL DEFAULT '',
	attempts INTEGER NOT NULL DEFAULT 0,
	stop_reason TEXT NOT NULL DEFAULT '',
	tok_input INTEGER NOT NULL DEFAULT 0,
	tok_output INTEGER NOT NULL DEFAULT 0,
	tok_reasoning INTEGER NOT NULL DEFAULT 0,
	tok_cache_read INTEGER NOT NULL DEFAULT 0,
	tok_cw5 INTEGER NOT NULL DEFAULT 0,
	tok_cw1 INTEGER NOT NULL DEFAULT 0,
	tok_total INTEGER NOT NULL DEFAULT 0,
	upstream_cost_usd REAL,
	tool_names TEXT NOT NULL DEFAULT '[]',
	capture_level TEXT NOT NULL DEFAULT '',
	content_available INTEGER NOT NULL DEFAULT 0,
	content_dropped INTEGER NOT NULL DEFAULT 0
);
CREATE INDEX IF NOT EXISTS turns_started ON turns(started_at);
CREATE INDEX IF NOT EXISTS turns_session ON turns(session_id, started_at);
CREATE INDEX IF NOT EXISTS turns_root ON turns(root_id, started_at);
CREATE INDEX IF NOT EXISTS turns_request ON turns(request_id);
CREATE TABLE IF NOT EXISTS usage_rows (
	id INTEGER PRIMARY KEY AUTOINCREMENT,
	turn_id TEXT NOT NULL,
	trace_id TEXT NOT NULL DEFAULT '',
	at INTEGER NOT NULL,
	auth_id TEXT NOT NULL DEFAULT '',
	provider TEXT NOT NULL DEFAULT '',
	model TEXT NOT NULL DEFAULT '',
	response_model TEXT NOT NULL DEFAULT '',
	failed INTEGER NOT NULL DEFAULT 0,
	status_code INTEGER NOT NULL DEFAULT 0,
	latency_ms INTEGER NOT NULL DEFAULT 0,
	tok_input INTEGER NOT NULL DEFAULT 0,
	tok_output INTEGER NOT NULL DEFAULT 0,
	tok_reasoning INTEGER NOT NULL DEFAULT 0,
	tok_cache_read INTEGER NOT NULL DEFAULT 0,
	tok_cw5 INTEGER NOT NULL DEFAULT 0,
	tok_cw1 INTEGER NOT NULL DEFAULT 0,
	tok_total INTEGER NOT NULL DEFAULT 0,
	upstream_cost_usd REAL
);
CREATE INDEX IF NOT EXISTS usage_turn ON usage_rows(turn_id);
CREATE TABLE IF NOT EXISTS attempts (
	id INTEGER PRIMARY KEY AUTOINCREMENT,
	turn_id TEXT NOT NULL,
	trace_id TEXT NOT NULL DEFAULT '',
	at INTEGER NOT NULL,
	auth_id TEXT NOT NULL DEFAULT '',
	provider TEXT NOT NULL DEFAULT '',
	model TEXT NOT NULL DEFAULT '',
	success INTEGER NOT NULL DEFAULT 0,
	http_status INTEGER NOT NULL DEFAULT 0,
	error TEXT NOT NULL DEFAULT ''
);
CREATE INDEX IF NOT EXISTS attempts_turn ON attempts(turn_id);
CREATE TABLE IF NOT EXISTS blobs (
	hash TEXT PRIMARY KEY,
	kind TEXT NOT NULL,
	size INTEGER NOT NULL,
	zsize INTEGER NOT NULL,
	data BLOB NOT NULL,
	created_at INTEGER NOT NULL
);
CREATE TABLE IF NOT EXISTS turn_blobs (
	turn_id TEXT NOT NULL,
	ord INTEGER NOT NULL,
	hash TEXT NOT NULL,
	role TEXT NOT NULL,
	is_new INTEGER NOT NULL DEFAULT 0,
	PRIMARY KEY (turn_id, ord)
);
CREATE INDEX IF NOT EXISTS turn_blobs_hash ON turn_blobs(hash);
CREATE TABLE IF NOT EXISTS tool_calls (
	root_id TEXT NOT NULL,
	session_id TEXT NOT NULL,
	turn_id TEXT NOT NULL,
	call_id TEXT NOT NULL,
	name TEXT NOT NULL DEFAULT '',
	file TEXT NOT NULL DEFAULT '',
	added INTEGER NOT NULL DEFAULT 0,
	removed INTEGER NOT NULL DEFAULT 0,
	has_diff INTEGER NOT NULL DEFAULT 0,
	failed INTEGER NOT NULL DEFAULT 0,
	PRIMARY KEY (root_id, call_id)
);
CREATE INDEX IF NOT EXISTS tool_calls_session ON tool_calls(session_id);
CREATE TABLE IF NOT EXISTS folders (
	folder TEXT PRIMARY KEY,
	git_remote TEXT NOT NULL
);
CREATE TABLE IF NOT EXISTS limit_samples (
	id INTEGER PRIMARY KEY AUTOINCREMENT,
	account_id TEXT NOT NULL,
	at INTEGER NOT NULL,
	win_name TEXT NOT NULL,
	used_pct REAL,
	status TEXT NOT NULL DEFAULT '',
	resets_at INTEGER,
	overage_status TEXT NOT NULL DEFAULT '',
	provider TEXT NOT NULL DEFAULT '',
	plan TEXT NOT NULL DEFAULT ''
);
CREATE INDEX IF NOT EXISTS limit_samples_account ON limit_samples(account_id, win_name, at);
CREATE TABLE IF NOT EXISTS accounts (
	account_id TEXT PRIMARY KEY,
	plan_price_usd REAL,
	plan TEXT NOT NULL DEFAULT ''
);
`

// SQLite is the SQLite Store.
type SQLite struct {
	db   *sql.DB
	path string
	enc  *zstd.Encoder
	dec  *zstd.Decoder
}

// OpenSQLite opens (or creates) the trace database in dir. The folder is
// created owner-only (0700); on Windows the mode bits are ignored and the
// folder keeps the user-profile ACL.
func OpenSQLite(dir string) (*SQLite, error) {
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, fmt.Errorf("create traces folder: %w", err)
	}
	if err := os.Chmod(dir, 0o700); err != nil {
		log.WithError(err).Debug("traces: could not set folder permissions")
	}
	path := filepath.Join(dir, DBFileName)
	// SQLite URI filenames treat %, ? and # specially, so escape them.
	uriPath := strings.NewReplacer("%", "%25", "?", "%3f", "#", "%23").Replace(filepath.ToSlash(path))
	dsn := "file:" + uriPath + "?_pragma=busy_timeout(10000)&_pragma=journal_mode(WAL)&_pragma=synchronous(NORMAL)"
	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		return nil, fmt.Errorf("open trace database: %w", err)
	}
	db.SetMaxOpenConns(4)
	ctx := context.Background()
	// auto_vacuum only takes effect before the first table exists.
	if _, err = db.ExecContext(ctx, "PRAGMA auto_vacuum = INCREMENTAL"); err != nil {
		_ = db.Close()
		return nil, fmt.Errorf("configure trace database: %w", err)
	}
	if _, err = db.ExecContext(ctx, schema); err != nil {
		_ = db.Close()
		return nil, fmt.Errorf("create trace schema: %w", err)
	}
	if err = os.Chmod(path, 0o600); err != nil {
		log.WithError(err).Debug("traces: could not set database file permissions")
	}
	enc, err := zstd.NewWriter(nil, zstd.WithEncoderLevel(zstd.SpeedDefault))
	if err != nil {
		_ = db.Close()
		return nil, fmt.Errorf("create zstd encoder: %w", err)
	}
	dec, err := zstd.NewReader(nil)
	if err != nil {
		_ = db.Close()
		return nil, fmt.Errorf("create zstd decoder: %w", err)
	}
	return &SQLite{db: db, path: path, enc: enc, dec: dec}, nil
}

// Close closes the database.
func (s *SQLite) Close() error {
	if s == nil || s.db == nil {
		return nil
	}
	s.dec.Close()
	if err := s.enc.Close(); err != nil {
		log.WithError(err).Debug("traces: close zstd encoder")
	}
	return s.db.Close()
}

// Write runs fn in one transaction.
func (s *SQLite) Write(ctx context.Context, fn func(Tx) error) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("begin trace transaction: %w", err)
	}
	if err = fn(&sqliteTx{tx: tx, s: s}); err != nil {
		if errRollback := tx.Rollback(); errRollback != nil {
			log.WithError(errRollback).Debug("traces: rollback failed")
		}
		return err
	}
	if err = tx.Commit(); err != nil {
		return fmt.Errorf("commit trace transaction: %w", err)
	}
	return nil
}

func ms(t time.Time) int64 {
	if t.IsZero() {
		return 0
	}
	return t.UnixMilli()
}

func fromMs(v int64) time.Time {
	if v == 0 {
		return time.Time{}
	}
	return time.UnixMilli(v).UTC()
}

func boolInt(b bool) int {
	if b {
		return 1
	}
	return 0
}

func encodeList(list []string) string {
	if len(list) == 0 {
		return "[]"
	}
	data, err := json.Marshal(list)
	if err != nil {
		return "[]"
	}
	return string(data)
}

func decodeList(raw string) []string {
	var out []string
	if raw == "" {
		return []string{}
	}
	if err := json.Unmarshal([]byte(raw), &out); err != nil || out == nil {
		return []string{}
	}
	return out
}

func nullInt(p *int64) any {
	if p == nil {
		return nil
	}
	return *p
}

func nullFloat(p *float64) any {
	if p == nil {
		return nil
	}
	return *p
}

func ptrInt(v sql.NullInt64) *int64 {
	if !v.Valid {
		return nil
	}
	x := v.Int64
	return &x
}

func ptrFloat(v sql.NullFloat64) *float64 {
	if !v.Valid {
		return nil
	}
	x := v.Float64
	return &x
}

type scanner interface {
	Scan(dest ...any) error
}

type queryer interface {
	QueryContext(ctx context.Context, query string, args ...any) (*sql.Rows, error)
	QueryRowContext(ctx context.Context, query string, args ...any) *sql.Row
	ExecContext(ctx context.Context, query string, args ...any) (sql.Result, error)
}

const sessionCols = `id, root_id, parent_id, title, project_id, project_name, project_key, project_source, project_rule, header_project, folder, git_remote, client_project_hash, client, client_version, key_label, tags, machine, client_ip, device_id, model, synthetic, started_at, last_at`

func scanSession(row scanner) (*Session, error) {
	var s Session
	var tags string
	var synthetic int
	var started, last int64
	if err := row.Scan(&s.ID, &s.RootID, &s.ParentID, &s.Title, &s.ProjectID, &s.ProjectName, &s.ProjectKey, &s.ProjectSource, &s.ProjectRule, &s.HeaderProject, &s.Folder, &s.GitRemote, &s.ClientProjectHash, &s.Client, &s.ClientVersion, &s.KeyLabel, &tags, &s.Machine, &s.ClientIP, &s.DeviceID, &s.Model, &synthetic, &started, &last); err != nil {
		return nil, err
	}
	s.Tags = decodeList(tags)
	s.Synthetic = synthetic != 0
	s.StartedAt = fromMs(started)
	s.LastAt = fromMs(last)
	return &s, nil
}

const turnCols = `t.id, t.session_id, t.root_id, t.seq, t.request_id, t.started_at, t.ended_at, t.latency_ms, t.ttft_ms, t.gap_next_ms, t.path, t.format, t.stream, t.model, t.served_model, t.provider, t.account, t.status, t.http_status, t.error, t.reply_error, t.attempts, t.stop_reason, t.tok_input, t.tok_output, t.tok_reasoning, t.tok_cache_read, t.tok_cw5, t.tok_cw1, t.tok_total, t.upstream_cost_usd, t.tool_names, t.capture_level, t.content_available, t.content_dropped`

func turnDest(t *Turn, aux *turnAux) []any {
	return []any{&t.ID, &t.SessionID, &t.RootID, &t.Seq, &t.RequestID, &aux.started, &aux.ended, &t.LatencyMs, &aux.ttft, &aux.gap, &t.Path, &t.Format, &aux.stream, &t.Model, &t.ServedModel, &t.Provider, &t.Account, &t.Status, &t.HTTPStatus, &t.Error, &t.ReplyError, &t.Attempts, &t.StopReason, &t.Tokens.Input, &t.Tokens.Output, &t.Tokens.Reasoning, &t.Tokens.CacheRead, &t.Tokens.CacheWrite5m, &t.Tokens.CacheWrite1h, &t.Tokens.Total, &aux.upstream, &aux.tools, &t.CaptureLevel, &aux.content, &aux.dropped}
}

type turnAux struct {
	started, ended          int64
	ttft, gap               sql.NullInt64
	stream                  int
	upstream                sql.NullFloat64
	tools                   string
	content, dropped        int
	sessionStarted, sessEnd int64
}

func (aux *turnAux) apply(t *Turn) {
	t.StartedAt = fromMs(aux.started)
	t.EndedAt = fromMs(aux.ended)
	t.TTFTMs = ptrInt(aux.ttft)
	t.GapNextMs = ptrInt(aux.gap)
	t.Stream = aux.stream != 0
	t.UpstreamCostUSD = ptrFloat(aux.upstream)
	t.ToolNames = decodeList(aux.tools)
	t.ContentAvailable = aux.content != 0
	t.ContentDropped = aux.dropped != 0
}

func scanTurn(row scanner) (*Turn, error) {
	var t Turn
	var aux turnAux
	if err := row.Scan(turnDest(&t, &aux)...); err != nil {
		return nil, err
	}
	aux.apply(&t)
	return &t, nil
}

func getTurn(ctx context.Context, q queryer, id string) (*Turn, error) {
	t, err := scanTurn(q.QueryRowContext(ctx, `SELECT `+turnCols+` FROM turns t WHERE t.id = ?`, id))
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	return t, err
}

func usageRows(ctx context.Context, q queryer, turnID string) ([]UsageRow, error) {
	rows, err := q.QueryContext(ctx, `SELECT id, turn_id, trace_id, at, auth_id, provider, model, response_model, failed, status_code, latency_ms, tok_input, tok_output, tok_reasoning, tok_cache_read, tok_cw5, tok_cw1, tok_total, upstream_cost_usd FROM usage_rows WHERE turn_id = ? ORDER BY at, id`, turnID)
	if err != nil {
		return nil, err
	}
	defer closeRows(rows)
	var out []UsageRow
	for rows.Next() {
		var u UsageRow
		var at int64
		var failed int
		var upstream sql.NullFloat64
		if err = rows.Scan(&u.ID, &u.TurnID, &u.TraceID, &at, &u.AuthID, &u.Provider, &u.Model, &u.ResponseModel, &failed, &u.StatusCode, &u.LatencyMs, &u.Tokens.Input, &u.Tokens.Output, &u.Tokens.Reasoning, &u.Tokens.CacheRead, &u.Tokens.CacheWrite5m, &u.Tokens.CacheWrite1h, &u.Tokens.Total, &upstream); err != nil {
			return nil, err
		}
		u.At = fromMs(at)
		u.Failed = failed != 0
		u.UpstreamCostUSD = ptrFloat(upstream)
		out = append(out, u)
	}
	return out, rows.Err()
}

func attemptRows(ctx context.Context, q queryer, where string, args ...any) ([]Attempt, error) {
	rows, err := q.QueryContext(ctx, `SELECT id, turn_id, trace_id, at, auth_id, provider, model, success, http_status, error FROM attempts WHERE `+where+` ORDER BY at, id`, args...)
	if err != nil {
		return nil, err
	}
	defer closeRows(rows)
	var out []Attempt
	for rows.Next() {
		var a Attempt
		var at int64
		var success int
		if err = rows.Scan(&a.ID, &a.TurnID, &a.TraceID, &at, &a.AuthID, &a.Provider, &a.Model, &success, &a.HTTPStatus, &a.Error); err != nil {
			return nil, err
		}
		a.At = fromMs(at)
		a.Success = success != 0
		out = append(out, a)
	}
	return out, rows.Err()
}

func blobRefs(ctx context.Context, q queryer, turnID string) ([]BlobRef, error) {
	rows, err := q.QueryContext(ctx, `SELECT ord, hash, role, is_new FROM turn_blobs WHERE turn_id = ? ORDER BY ord`, turnID)
	if err != nil {
		return nil, err
	}
	defer closeRows(rows)
	var out []BlobRef
	for rows.Next() {
		var r BlobRef
		var isNew int
		if err = rows.Scan(&r.Ord, &r.Hash, &r.Role, &isNew); err != nil {
			return nil, err
		}
		r.IsNew = isNew != 0
		out = append(out, r)
	}
	return out, rows.Err()
}

func closeRows(rows *sql.Rows) {
	if err := rows.Close(); err != nil {
		log.WithError(err).Debug("traces: close rows")
	}
}

func placeholders(n int) string {
	if n <= 0 {
		return ""
	}
	return strings.TrimSuffix(strings.Repeat("?,", n), ",")
}

func stringArgs(values []string) []any {
	out := make([]any, len(values))
	for i, v := range values {
		out[i] = v
	}
	return out
}
