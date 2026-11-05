// Package store persists traces. Store is the storage contract; SQLite
// (pure Go, no cgo) is the only implementation for now, and the interface keeps
// room for a Postgres implementation later.
package store

import (
	"context"
	"time"
)

// Tokens is the normalized token object of a turn or usage row.
type Tokens struct {
	Input        int64 `json:"input"`
	Output       int64 `json:"output"`
	Reasoning    int64 `json:"reasoning"`
	CacheRead    int64 `json:"cache_read"`
	CacheWrite5m int64 `json:"cache_write_5m"`
	CacheWrite1h int64 `json:"cache_write_1h"`
	Total        int64 `json:"total"`
}

// Add accumulates o into t.
func (t *Tokens) Add(o Tokens) {
	t.Input += o.Input
	t.Output += o.Output
	t.Reasoning += o.Reasoning
	t.CacheRead += o.CacheRead
	t.CacheWrite5m += o.CacheWrite5m
	t.CacheWrite1h += o.CacheWrite1h
	t.Total += o.Total
}

// Session is one client conversation.
type Session struct {
	ID                string
	RootID            string
	ParentID          string
	Title             string
	ProjectID         string
	ProjectName       string
	ProjectKey        string
	ProjectSource     string
	ProjectRule       string
	HeaderProject     string
	PathProject       string
	Folder            string
	GitRemote         string
	ClientProjectHash string
	Client            string
	ClientVersion     string
	KeyLabel          string
	Tags              []string
	Machine           string
	ClientIP          string
	DeviceID          string
	Model             string
	Synthetic         bool
	StartedAt         time.Time
	LastAt            time.Time
}

// Turn is one client request to a model endpoint.
type Turn struct {
	ID               string
	SessionID        string
	RootID           string
	Seq              int
	RequestID        string
	StartedAt        time.Time
	EndedAt          time.Time
	LatencyMs        int64
	TTFTMs           *int64
	GapNextMs        *int64
	Path             string
	Format           string
	Stream           bool
	Model            string
	ServedModel      string
	Provider         string
	Account          string
	Status           string
	HTTPStatus       int
	Error            string
	ReplyError       string
	Attempts         int
	StopReason       string
	Tokens           Tokens
	UpstreamCostUSD  *float64
	ToolNames        []string
	CaptureLevel     string
	ContentAvailable bool
	ContentDropped   bool

	// Session is filled by queries that join the session row.
	Session *Session
	// RootTitle is the title of the root session, filled by joined queries.
	RootTitle string
}

// UsageRow is one usage record (one upstream attempt with token counts).
type UsageRow struct {
	ID              int64
	TurnID          string
	TraceID         string
	At              time.Time
	AuthID          string
	Provider        string
	Model           string
	ResponseModel   string
	Failed          bool
	StatusCode      int
	LatencyMs       int64
	Tokens          Tokens
	UpstreamCostUSD *float64
}

// Attempt is one upstream try for a turn.
type Attempt struct {
	ID         int64
	TurnID     string
	TraceID    string
	At         time.Time
	AuthID     string
	Provider   string
	Model      string
	Success    bool
	HTTPStatus int
	Error      string
}

// BlobRef places a content blob in a turn.
type BlobRef struct {
	Ord   int
	Hash  string
	Role  string
	IsNew bool
}

// Blob roles inside a turn.
const (
	RoleSystem  = "system"
	RoleTools   = "tools"
	RoleMessage = "message"
	RoleReply   = "reply"
)

// ToolCall is one tool call of a session, with its file change summary.
type ToolCall struct {
	RootID    string
	SessionID string
	TurnID    string
	CallID    string
	Name      string
	File      string
	Added     int
	Removed   int
	HasDiff   bool
	Failed    bool
}

// LimitSample is one plan-limit observation for an account window.
type LimitSample struct {
	AccountID     string
	At            time.Time
	Window        string
	UsedPct       *float64
	Status        string
	ResetsAt      *time.Time
	OverageStatus string
	Provider      string
	Plan          string
}

// Account holds user-entered account data.
type Account struct {
	AccountID    string
	PlanPriceUSD *float64
	Plan         string
}

// UntaggedFilter as TurnFilter.Tag selects sessions without tags. It is the
// stats group key of untagged sessions.
const UntaggedFilter = "__untagged__"

// TurnFilter selects turns. Empty fields do not filter.
type TurnFilter struct {
	From       time.Time
	To         time.Time
	Project    string
	Folder     string
	Client     string
	Model      string
	Key        string
	Tag        string
	Account    string
	Machine    string
	Status     string
	Query      string
	SessionIDs []string
	RootIDs    []string
}

// DeleteFilter selects what DELETE /traces removes.
type DeleteFilter struct {
	SessionID string
	Project   string
	Key       string
	From      time.Time
	To        time.Time
}

// Stats are storage numbers for the health endpoint.
type Stats struct {
	DBBytes      int64
	ContentBytes int64
	OldestTurnAt *time.Time
}

// SyntheticKey identifies the synthetic session of a request that carries no
// session ID. Different project hints never share a session.
type SyntheticKey struct {
	KeyLabel, Client, Folder string
	HeaderProject            string
	PathProject              string
	// GitRemote only counts when Folder is empty.
	GitRemote string
}

// Tx is the write side, used by the single writer goroutine.
type Tx interface {
	GetSession(id string) (*Session, error)
	UpsertSession(s *Session) error
	// FindSyntheticSession finds the latest synthetic session of the same client
	// key, client, folder and explicit project hints that was active since.
	FindSyntheticSession(key SyntheticKey, since time.Time) (string, error)
	MaxSeq(sessionID string) (int, error)
	InsertTurn(t *Turn) error
	GetTurn(id string) (*Turn, error)
	TurnIDByRequestID(requestID string) (string, error)
	PrevTurnInSession(sessionID string, before time.Time, excludeID string) (*Turn, error)
	NeighborTurns(rootID string, at time.Time, excludeID string) (prev, next *Turn, err error)
	SetGapNext(turnID string, gapMs *int64) error
	UpdateTurnAggregates(t *Turn) error
	PutBlob(hash, kind string, raw []byte) (int64, error)
	InsertTurnBlobs(turnID string, refs []BlobRef) error
	TurnBlobRefs(turnID string) ([]BlobRef, error)
	InsertUsage(u *UsageRow) error
	UsageForTurn(turnID string) ([]UsageRow, error)
	InsertAttempt(a *Attempt) error
	AttemptsForTurn(turnID string) ([]Attempt, error)
	UpsertToolCall(c *ToolCall) error
	MarkToolCallFailed(rootID, callID string) error
	FolderRemote(folder string) (string, error)
	SetFolderRemote(folder, remote string) error
	LastLimitSample(accountID, window string) (*LimitSample, error)
	InsertLimitSample(s *LimitSample) error
	SetAccountPlan(accountID, plan string) error
}

// Store is the storage contract.
type Store interface {
	// Write runs fn in one transaction.
	Write(ctx context.Context, fn func(Tx) error) error

	QueryTurns(ctx context.Context, f TurnFilter) ([]Turn, error)
	Sessions(ctx context.Context, ids []string) ([]Session, error)
	SessionsByRoot(ctx context.Context, rootIDs []string) ([]Session, error)
	AllSessions(ctx context.Context) ([]Session, error)
	UpdateSessionProjects(ctx context.Context, sessions []Session) error
	Turn(ctx context.Context, id string) (*Turn, error)
	UsageForTurn(ctx context.Context, turnID string) ([]UsageRow, error)
	AttemptsForTurns(ctx context.Context, turnIDs []string) ([]Attempt, error)
	TurnBlobRefs(ctx context.Context, turnID string) ([]BlobRef, error)
	Blob(ctx context.Context, hash string) ([]byte, error)
	ToolCalls(ctx context.Context, sessionIDs []string) ([]ToolCall, error)
	ToolCallsByID(ctx context.Context, rootID string, callIDs []string) ([]ToolCall, error)
	LimitSamples(ctx context.Context, since time.Time) ([]LimitSample, error)
	Accounts(ctx context.Context) ([]Account, error)
	SetPlanPrice(ctx context.Context, accountID string, price *float64) error
	SeenModels(ctx context.Context) ([]string, error)
	GetKV(ctx context.Context, key string) ([]byte, time.Time, error)
	PutKV(ctx context.Context, key string, value []byte) error
	Delete(ctx context.Context, f DeleteFilter) (sessions, turns int64, err error)
	Stats(ctx context.Context) (Stats, error)

	// DropContentBefore removes content refs of turns started before t.
	DropContentBefore(ctx context.Context, t time.Time) (int64, error)
	// DropOldestContent removes content refs of the n oldest turns that still have content.
	DropOldestContent(ctx context.Context, n int) (int64, error)
	// CollectGarbage deletes unreferenced blobs and orphan rows, then vacuums.
	CollectGarbage(ctx context.Context, orphanBefore time.Time) error

	Close() error
}
