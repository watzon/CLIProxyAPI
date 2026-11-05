package traces

import (
	"time"

	"github.com/router-for-me/CLIProxyAPI/v8/internal/traces/store"
)

// Response shapes of the management API. Field names are the contract with
// the management panel; see the traces build spec.

// Health is GET /traces/health.
type Health struct {
	Enabled         bool       `json:"enabled"`
	Capture         string     `json:"capture"`
	QueueDepth      int        `json:"queue_depth"`
	QueueSize       int        `json:"queue_size"`
	DroppedEvents   int64      `json:"dropped_events"`
	DroppedContent  int64      `json:"dropped_content"`
	DBBytes         int64      `json:"db_bytes"`
	ContentBytes    int64      `json:"content_bytes"`
	LastWriteAt     *time.Time `json:"last_write_at"`
	OldestTurnAt    *time.Time `json:"oldest_turn_at"`
	PricesUpdatedAt *time.Time `json:"prices_updated_at"`
	PricesSource    string     `json:"prices_source"`
}

// Totals is the TOT object.
type Totals struct {
	CostUSD          *float64     `json:"cost_usd"`
	Tokens           store.Tokens `json:"tokens"`
	ActiveMs         int64        `json:"active_ms"`
	ModelMs          int64        `json:"model_ms"`
	Sessions         int          `json:"sessions"`
	SubagentSessions int          `json:"subagent_sessions"`
	Turns            int          `json:"turns"`
	FailedTurns      int          `json:"failed_turns"`
	RecoveredTurns   int          `json:"recovered_turns"`
	UnpricedTurns    int          `json:"unpriced_turns"`
}

// StatsGroup is one entry of stats.groups.
type StatsGroup struct {
	Key      string       `json:"key"`
	Label    string       `json:"label"`
	CostUSD  *float64     `json:"cost_usd"`
	Tokens   store.Tokens `json:"tokens"`
	Turns    int          `json:"turns"`
	Sessions int          `json:"sessions"`
	ActiveMs int64        `json:"active_ms"`
}

// BucketValue is one group value inside a bucket.
type BucketValue struct {
	CostUSD  *float64 `json:"cost_usd"`
	Tokens   int64    `json:"tokens"`
	ActiveMs int64    `json:"active_ms"`
}

// Bucket is one time bucket of stats.buckets.
type Bucket struct {
	Start  time.Time              `json:"start"`
	Groups map[string]BucketValue `json:"groups"`
}

// StatsResponse is GET /traces/stats.
type StatsResponse struct {
	From     time.Time    `json:"from"`
	To       time.Time    `json:"to"`
	Totals   Totals       `json:"totals"`
	Previous Totals       `json:"previous"`
	Groups   []StatsGroup `json:"groups"`
	Buckets  []Bucket     `json:"buckets"`
}

// DailyValue is one day of a project sparkline.
type DailyValue struct {
	Day      string   `json:"day"`
	CostUSD  *float64 `json:"cost_usd"`
	ActiveMs int64    `json:"active_ms"`
}

// Project is one entry of GET /traces/projects.
type Project struct {
	ID           string       `json:"id"`
	Name         string       `json:"name"`
	Key          string       `json:"key"`
	Sessions     int          `json:"sessions"`
	Turns        int          `json:"turns"`
	ActiveMs     int64        `json:"active_ms"`
	ModelMs      int64        `json:"model_ms"`
	Tokens       store.Tokens `json:"tokens"`
	CostUSD      *float64     `json:"cost_usd"`
	TopModel     string       `json:"top_model"`
	LastActiveAt time.Time    `json:"last_active_at"`
	Daily        []DailyValue `json:"daily"`
}

// RuleHits is a project rule with the number of sessions it matched.
type RuleHits struct {
	Match   string `json:"match"`
	Project string `json:"project"`
	Hits    int    `json:"hits"`
}

// ProjectsResponse is GET /traces/projects.
type ProjectsResponse struct {
	Projects           []Project  `json:"projects"`
	Rules              []RuleHits `json:"rules"`
	UnassignedSessions int        `json:"unassigned_sessions"`
}

// SessionView is the S object.
type SessionView struct {
	ID      string `json:"id"`
	Title   string `json:"title"`
	Project string `json:"project"`
	// ProjectSource says how the project was chosen: header, path, rule, remote,
	// folder, inherited or none.
	ProjectSource string       `json:"project_source"`
	Folder        string       `json:"folder"`
	Client        string       `json:"client"`
	ClientVersion string       `json:"client_version"`
	Model         string       `json:"model"`
	KeyLabel      string       `json:"key_label"`
	Tags          []string     `json:"tags"`
	Machine       string       `json:"machine"`
	Turns         int          `json:"turns"`
	Tokens        store.Tokens `json:"tokens"`
	CostUSD       *float64     `json:"cost_usd"`
	ActiveMs      int64        `json:"active_ms"`
	ModelMs       int64        `json:"model_ms"`
	Errors        int          `json:"errors"`
	Subagents     int          `json:"subagents"`
	ParentID      *string      `json:"parent_id"`
	StartedAt     time.Time    `json:"started_at"`
	LastAt        time.Time    `json:"last_at"`
}

// SessionsSummary sums the matched sessions.
type SessionsSummary struct {
	Turns    int      `json:"turns"`
	ActiveMs int64    `json:"active_ms"`
	CostUSD  *float64 `json:"cost_usd"`
}

// SessionsResponse is GET /traces/sessions.
type SessionsResponse struct {
	Total    int             `json:"total"`
	Summary  SessionsSummary `json:"summary"`
	Sessions []SessionView   `json:"sessions"`
}

// SessionDetailView is S plus detail fields.
type SessionDetailView struct {
	SessionView
	DeviceID     *string `json:"device_id"`
	FilesChanged int     `json:"files_changed"`
	LinesAdded   int     `json:"lines_added"`
	LinesRemoved int     `json:"lines_removed"`
}

// TurnView is the TURN object.
type TurnView struct {
	ID              string       `json:"id"`
	Seq             int          `json:"seq"`
	SessionID       string       `json:"session_id"`
	StartedAt       time.Time    `json:"started_at"`
	EndedAt         time.Time    `json:"ended_at"`
	LatencyMs       int64        `json:"latency_ms"`
	TTFTMs          *int64       `json:"ttft_ms"`
	Model           string       `json:"model"`
	ServedModel     string       `json:"served_model"`
	Provider        string       `json:"provider"`
	Account         string       `json:"account"`
	Status          string       `json:"status"`
	HTTPStatus      int          `json:"http_status"`
	Error           *string      `json:"error"`
	Attempts        int          `json:"attempts"`
	Tokens          store.Tokens `json:"tokens"`
	CostUSD         *float64     `json:"cost_usd"`
	ToolNames       []string     `json:"tool_names"`
	UpstreamCostUSD *float64     `json:"upstream_cost_usd,omitempty"`
}

// Lane is one session row of the trace timeline.
type Lane struct {
	SessionID    string  `json:"session_id"`
	Label        string  `json:"label"`
	ParentTurnID *string `json:"parent_turn_id"`
	Turns        int     `json:"turns"`
}

// Break is a pause longer than idle-gap.
type Break struct {
	From time.Time `json:"from"`
	To   time.Time `json:"to"`
}

// SessionDetailResponse is GET /traces/sessions/:id.
type SessionDetailResponse struct {
	Session SessionDetailView `json:"session"`
	Turns   []TurnView        `json:"turns"`
	Lanes   []Lane            `json:"lanes"`
	Breaks  []Break           `json:"breaks"`
}

// AttemptView is one entry of turn attempts.
type AttemptView struct {
	N          int     `json:"n"`
	Account    string  `json:"account"`
	HTTPStatus int     `json:"http_status"`
	Status     string  `json:"status"`
	Error      *string `json:"error"`
	LatencyMs  int64   `json:"latency_ms"`
}

// CostPart is one entry of cost_breakdown.
type CostPart struct {
	Type    string   `json:"type"`
	Tokens  int64    `json:"tokens"`
	CostUSD *float64 `json:"cost_usd"`
}

// TurnFacts is the facts object.
type TurnFacts struct {
	RequestID    string  `json:"request_id"`
	Folder       string  `json:"folder"`
	Client       string  `json:"client"`
	Machine      string  `json:"machine"`
	StopReason   *string `json:"stop_reason"`
	ParentTurnID *string `json:"parent_turn_id"`
}

// DiffView is the diff of a tool call block: {file, old, new} or {file, patch}.
type DiffView struct {
	File  string  `json:"file"`
	Old   *string `json:"old,omitempty"`
	New   *string `json:"new,omitempty"`
	Patch *string `json:"patch,omitempty"`
}

// BlockView is the BLOCK object.
type BlockView struct {
	Kind       string    `json:"kind"`
	Text       string    `json:"text"`
	ToolName   *string   `json:"tool_name"`
	ToolCallID *string   `json:"tool_call_id"`
	IsError    bool      `json:"is_error"`
	Truncated  bool      `json:"truncated"`
	FullSize   int       `json:"full_size"`
	Diff       *DiffView `json:"diff"`
}

// TurnDetailResponse is GET /traces/turns/:id.
type TurnDetailResponse struct {
	Turn             TurnView      `json:"turn"`
	Attempts         []AttemptView `json:"attempts"`
	CostBreakdown    []CostPart    `json:"cost_breakdown"`
	Facts            TurnFacts     `json:"facts"`
	ContentAvailable bool          `json:"content_available"`
	Blocks           []BlockView   `json:"blocks"`
}

// ErrorGroup is one entry of errors.groups.
type ErrorGroup struct {
	Type    string    `json:"type"`
	Account string    `json:"account"`
	Count   int       `json:"count"`
	LastAt  time.Time `json:"last_at"`
}

// ErrorItem is one entry of errors.items.
type ErrorItem struct {
	TurnID     string    `json:"turn_id"`
	SessionID  string    `json:"session_id"`
	Title      string    `json:"title"`
	Project    string    `json:"project"`
	At         time.Time `json:"at"`
	Account    string    `json:"account"`
	HTTPStatus int       `json:"http_status"`
	Error      string    `json:"error"`
	Recovered  bool      `json:"recovered"`
}

// ErrorsResponse is GET /traces/errors.
type ErrorsResponse struct {
	Total  int          `json:"total"`
	Groups []ErrorGroup `json:"groups"`
	Items  []ErrorItem  `json:"items"`
}

// LimitWindowView is one window of an account.
type LimitWindowView struct {
	Name     string     `json:"name"`
	UsedPct  *float64   `json:"used_pct"`
	Status   string     `json:"status"`
	ResetsAt *time.Time `json:"resets_at"`
}

// LimitHistoryPoint is one history sample.
type LimitHistoryPoint struct {
	At      time.Time `json:"at"`
	Window  string    `json:"window"`
	UsedPct *float64  `json:"used_pct"`
}

// AccountUsage is the usage of an account in the filter range.
type AccountUsage struct {
	Turns   int      `json:"turns"`
	Tokens  int64    `json:"tokens"`
	CostUSD *float64 `json:"cost_usd"`
}

// AccountLimits is one entry of GET /traces/limits.
type AccountLimits struct {
	AccountID     string              `json:"account_id"`
	Label         string              `json:"label"`
	Email         string              `json:"email"`
	Provider      string              `json:"provider"`
	Plan          *string             `json:"plan"`
	Tags          []string            `json:"tags"`
	Windows       []LimitWindowView   `json:"windows"`
	OverageStatus *string             `json:"overage_status"`
	History       []LimitHistoryPoint `json:"history"`
	Usage         AccountUsage        `json:"usage"`
	PlanPriceUSD  *float64            `json:"plan_price_usd"`
}

// LimitsResponse is GET /traces/limits.
type LimitsResponse struct {
	Accounts []AccountLimits `json:"accounts"`
}

// PriceRowView is one entry of GET /traces/prices.
type PriceRowView struct {
	Model        string   `json:"model"`
	Input        *float64 `json:"input"`
	Output       *float64 `json:"output"`
	CacheRead    *float64 `json:"cache_read"`
	CacheWrite5m *float64 `json:"cache_write_5m"`
	CacheWrite1h *float64 `json:"cache_write_1h"`
	Origin       string   `json:"origin"`
}

// PricesResponse is GET /traces/prices.
type PricesResponse struct {
	Source    string         `json:"source"`
	UpdatedAt *time.Time     `json:"updated_at"`
	Rows      []PriceRowView `json:"rows"`
}

// DeleteResponse is DELETE /traces.
type DeleteResponse struct {
	DeletedSessions int64 `json:"deleted_sessions"`
	DeletedTurns    int64 `json:"deleted_turns"`
}
