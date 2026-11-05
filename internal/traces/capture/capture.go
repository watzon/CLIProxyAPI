// Package capture holds the request-path taps of the traces feature.
//
// The taps copy what they need and hand it to a Sink without blocking. When no
// Sink is active (traces disabled) every tap returns at once and copies nothing.
package capture

import (
	"bytes"
	"context"
	"net/http"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/google/uuid"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/logging"
	"github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/usage"
)

// Client headers that steer trace derivation. They are read by the proxy and
// never forwarded upstream.
const (
	HeaderProject = "X-CPA-Project"
	HeaderCwd     = "X-CPA-Cwd"
	HeaderClient  = "X-CPA-Client"
)

// maxRequestCapture bounds the request body copied per turn. A larger body is
// not copied (its turn keeps metadata only).
const maxRequestCapture = 64 << 20

// Reply kinds tell the writer how to parse TurnEvent.Reply.
const (
	// ReplyKindHTTP is the reply as written to the client (JSON or SSE).
	ReplyKindHTTP = "http"
	// ReplyKindResponsesOutput is the JSON output array of a completed
	// OpenAI Responses turn (WebSocket transport).
	ReplyKindResponsesOutput = "responses-output"
)

// Sink receives captured events. Implementations must never block the caller.
type Sink interface {
	SubmitTurn(*TurnEvent)
	SubmitUsage(*UsageEvent)
	SubmitAttempt(*AttemptEvent)
	// ReplyLimit is the most reply bytes to keep per turn.
	ReplyLimit() int
}

type sinkBox struct{ sink Sink }

var active atomic.Pointer[sinkBox]

// SetSink activates s. A nil s turns every tap into a no-op.
func SetSink(s Sink) {
	if s == nil {
		active.Store(nil)
		return
	}
	active.Store(&sinkBox{sink: s})
}

// ReleaseSink deactivates s if it is still the active sink. A newer sink set
// by another owner stays active.
func ReleaseSink(s Sink) {
	if box := active.Load(); box != nil && box.sink == s {
		active.CompareAndSwap(box, nil)
	}
}

// Enabled reports whether a sink is active.
func Enabled() bool { return active.Load() != nil }

func current() Sink {
	if box := active.Load(); box != nil {
		return box.sink
	}
	return nil
}

// TurnEvent is one finished client request to a model endpoint.
type TurnEvent struct {
	ID               string
	RequestID        string
	Path             string
	Websocket        bool
	Format           string
	Model            string
	Stream           bool
	StartedAt        time.Time
	EndedAt          time.Time
	FirstByteAt      time.Time
	HTTPStatus       int
	ErrorText        string
	Headers          http.Header
	ClientIP         string
	APIKey           string
	SessionID        string
	ParentSessionID  string
	Request          []byte
	Reply            []byte
	ReplyKind        string
	ReplyContentType string
	ReplyTruncated   bool
	ContentDropped   bool
}

// ContentBytes is the size of the request and reply bodies held by the event.
func (e *TurnEvent) ContentBytes() int {
	if e == nil {
		return 0
	}
	return len(e.Request) + len(e.Reply)
}

// DropContent releases the bodies and keeps the metadata.
func (e *TurnEvent) DropContent() {
	if e == nil {
		return
	}
	e.Request = nil
	e.Reply = nil
	e.ContentDropped = true
}

// UsageEvent is one usage record joined to its turn.
type UsageEvent struct {
	TurnID  string
	TraceID string
	At      time.Time
	Record  usage.Record
}

// AttemptEvent is one upstream try (one credential) for a turn.
type AttemptEvent struct {
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

// Turn is the in-flight state of one traced client request.
type Turn struct {
	id        string
	requestID string
	startedAt time.Time
	path      string
	websocket bool
	headers   http.Header
	clientIP  string
	apiKey    string

	mu              sync.Mutex
	observed        bool
	finished        bool
	format          string
	model           string
	stream          bool
	sessionID       string
	parentSessionID string
	body            []byte
	bodyDropped     bool
}

// ID returns the turn ID used to join usage and attempt events.
func (t *Turn) ID() string {
	if t == nil {
		return ""
	}
	return t.id
}

func newTurn(requestID, path string, websocket bool, headers http.Header) *Turn {
	id := ""
	if v7, err := uuid.NewV7(); err == nil {
		id = v7.String()
	} else {
		id = uuid.NewString()
	}
	return &Turn{
		id:        id,
		requestID: requestID,
		startedAt: time.Now(),
		path:      path,
		websocket: websocket,
		headers:   headers,
	}
}

type turnContextKey struct{}

// WithTurn attaches t to ctx.
func WithTurn(ctx context.Context, t *Turn) context.Context {
	if ctx == nil || t == nil {
		return ctx
	}
	return context.WithValue(ctx, turnContextKey{}, t)
}

// TurnFromContext returns the turn attached to ctx, or nil.
func TurnFromContext(ctx context.Context) *Turn {
	if ctx == nil {
		return nil
	}
	t, _ := ctx.Value(turnContextKey{}).(*Turn)
	return t
}

// ObserveRequest records the client request body of the turn in ctx. Only the
// first call per turn counts, so nested or retried executions keep the body the
// client sent. Call it after the session hierarchy is in ctx.
func ObserveRequest(ctx context.Context, format, model string, stream bool, body []byte) {
	if active.Load() == nil {
		return
	}
	t := TurnFromContext(ctx)
	if t == nil {
		return
	}
	meta := logging.GetClientRequestMetadata(ctx)
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.observed {
		return
	}
	t.observed = true
	t.format = format
	t.model = model
	t.stream = stream
	t.sessionID = meta.SessionID
	t.parentSessionID = meta.ParentSessionID
	if len(body) > maxRequestCapture {
		t.bodyDropped = true
		return
	}
	t.body = bytes.Clone(body)
}

// ObserveSession refreshes the session IDs of the turn in ctx after the
// handler has derived the session hierarchy from headers and payload.
func ObserveSession(ctx context.Context) {
	if active.Load() == nil {
		return
	}
	t := TurnFromContext(ctx)
	if t == nil {
		return
	}
	meta := logging.GetClientRequestMetadata(ctx)
	if meta.SessionID == "" {
		return
	}
	t.mu.Lock()
	t.sessionID = meta.SessionID
	t.parentSessionID = meta.ParentSessionID
	t.mu.Unlock()
}

// event builds the TurnEvent from the turn state. It returns nil when no model
// request was observed or the turn was already finished.
func (t *Turn) event() *TurnEvent {
	t.mu.Lock()
	defer t.mu.Unlock()
	if !t.observed || t.finished {
		return nil
	}
	t.finished = true
	ev := &TurnEvent{
		ID:              t.id,
		RequestID:       t.requestID,
		Path:            t.path,
		Websocket:       t.websocket,
		Format:          t.format,
		Model:           t.model,
		Stream:          t.stream,
		StartedAt:       t.startedAt,
		Headers:         t.headers,
		ClientIP:        t.clientIP,
		APIKey:          t.apiKey,
		SessionID:       t.sessionID,
		ParentSessionID: t.parentSessionID,
		Request:         t.body,
		ContentDropped:  t.bodyDropped,
	}
	t.body = nil
	return ev
}

// storedHeaderNames is the header allowlist. Credentials, cookies and the
// client key are never stored.
var storedHeaderNames = []string{
	"User-Agent",
	"Originator",
	"X-App",
	"X-Codex-Turn-Metadata",
	"X-Opencode-Project",
	HeaderProject,
	HeaderCwd,
	HeaderClient,
	"X-Claude-Code-Session-Id",
	"X-Claude-Code-Agent-Id",
	"X-Claude-Code-Parent-Agent-Id",
	"Session-Id",
	"Session_id",
	"X-Codex-Parent-Thread-Id",
	"X-Openai-Subagent",
	"X-Http-Session-Id",
	"X-Session-Id",
	"X-Session-Affinity",
	"X-Parent-Session-Id",
	"X-Parent-Session-Affinity",
	"X-Parent-Id",
	"X-Slot-Session-Id",
	"X-Parent-Slot-Session-Id",
	"X-Task-Id",
	"X-Parent-Task-Id",
	"X-Conversation-Id",
}

// AllowedHeaders copies only the allowlisted headers from src.
func AllowedHeaders(src http.Header) http.Header {
	out := make(http.Header)
	for _, name := range storedHeaderNames {
		values := src.Values(name)
		if len(values) == 0 {
			// Header names with "_" are not canonicalized by net/http.
			values = src[name]
		}
		if len(values) > 0 {
			out[http.CanonicalHeaderKey(name)] = append([]string(nil), values...)
		}
	}
	return out
}

// IsModelPath reports whether path is a model endpoint whose requests become turns.
func IsModelPath(path string) bool {
	path = strings.TrimRight(path, "/")
	if strings.Contains(path, ":generateContent") || strings.Contains(path, ":streamGenerateContent") {
		return true
	}
	for _, suffix := range []string{"/completions", "/messages", "/responses", "/responses/compact", "/interactions"} {
		if strings.HasSuffix(path, suffix) {
			return true
		}
	}
	return false
}
