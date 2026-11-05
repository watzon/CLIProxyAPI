package capture

import (
	"context"
	"fmt"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/httpwire"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/logging"
)

const (
	ginTurnKey          = "cpa.traces.turn"
	ginClientHeadersKey = "cpa.traces.client-headers"
)

// Middleware creates a turn for each model request, keeps a copy of the reply
// as written to the client, and submits the turn when the handler returns.
// It always removes the X-CPA-* trace headers so they never reach an upstream.
func Middleware() gin.HandlerFunc {
	return func(c *gin.Context) {
		req := c.Request
		if req == nil {
			c.Next()
			return
		}
		clientHeaders := takeClientHeaders(req.Header)
		sink := current()
		if sink == nil {
			c.Next()
			return
		}
		if len(clientHeaders) > 0 {
			c.Set(ginClientHeadersKey, clientHeaders)
		}
		if req.Method != http.MethodPost || req.URL == nil || !IsModelPath(req.URL.Path) || isUpgrade(req) {
			c.Next()
			return
		}
		headers := AllowedHeaders(req.Header)
		mergeHeaders(headers, clientHeaders)
		turn := newTurn(logging.GetGinRequestID(c), req.URL.Path, false, headers)
		c.Set(ginTurnKey, turn)
		writer := &replyWriter{ResponseWriter: c.Writer, limit: sink.ReplyLimit()}
		c.Writer = writer
		defer func() {
			recovered := recover()
			c.Writer = writer.ResponseWriter
			finishHTTPTurn(c, turn, writer, sink, recovered != nil)
			if recovered != nil {
				panic(recovered)
			}
		}()
		c.Next()
	}
}

// ContextWithGinTurn copies the turn stored on c into ctx. The execution
// context outlives the gin.Context, so the turn must travel in ctx.
func ContextWithGinTurn(ctx context.Context, c *gin.Context) context.Context {
	if active.Load() == nil || c == nil || ctx == nil {
		return ctx
	}
	value, ok := c.Get(ginTurnKey)
	if !ok {
		return ctx
	}
	if t, okTurn := value.(*Turn); okTurn && t != nil {
		return WithTurn(ctx, t)
	}
	return ctx
}

func finishHTTPTurn(c *gin.Context, turn *Turn, writer *replyWriter, sink Sink, panicked bool) {
	ev := turn.event()
	if ev == nil {
		return
	}
	ev.EndedAt = time.Now()
	writer.mu.Lock()
	ev.Reply = writer.buf
	ev.ReplyTruncated = writer.truncated
	ev.FirstByteAt = writer.firstAt
	writer.buf = nil
	writer.mu.Unlock()
	ev.ReplyKind = ReplyKindHTTP
	ev.ReplyContentType = writer.Header().Get("Content-Type")
	ev.HTTPStatus = writer.Status()
	if panicked {
		ev.HTTPStatus = http.StatusInternalServerError
		ev.ErrorText = "handler panic"
	}
	ev.APIKey = ginAPIKey(c)
	ev.ClientIP = strings.TrimSpace(c.ClientIP())
	sink.SubmitTurn(ev)
}

func ginAPIKey(c *gin.Context) string {
	if c == nil {
		return ""
	}
	value, ok := c.Get("userApiKey")
	if !ok || value == nil {
		return ""
	}
	switch v := value.(type) {
	case string:
		return strings.TrimSpace(v)
	case fmt.Stringer:
		return strings.TrimSpace(v.String())
	default:
		return strings.TrimSpace(fmt.Sprint(v))
	}
}

func takeClientHeaders(h http.Header) http.Header {
	if h == nil {
		return nil
	}
	var out http.Header
	for _, name := range []string{HeaderProject, HeaderCwd, HeaderClient} {
		if values := h.Values(name); len(values) > 0 {
			if out == nil {
				out = make(http.Header, 3)
			}
			out[http.CanonicalHeaderKey(name)] = append([]string(nil), values...)
			h.Del(name)
		}
	}
	return out
}

func mergeHeaders(dst, src http.Header) {
	for k, v := range src {
		dst[http.CanonicalHeaderKey(k)] = v
	}
}

func isUpgrade(req *http.Request) bool {
	return strings.EqualFold(strings.TrimSpace(req.Header.Get("Upgrade")), "websocket")
}

// replyWriter keeps a bounded copy of everything written to the client.
type replyWriter struct {
	gin.ResponseWriter
	limit int

	mu        sync.Mutex
	buf       []byte
	truncated bool
	firstAt   time.Time
}

func (w *replyWriter) Write(p []byte) (int, error) {
	w.capture(p)
	return w.ResponseWriter.Write(p)
}

func (w *replyWriter) WriteString(s string) (int, error) {
	w.capture([]byte(s))
	return w.ResponseWriter.WriteString(s)
}

// Unwrap lets http.ResponseController reach the underlying writer.
func (w *replyWriter) Unwrap() http.ResponseWriter { return w.ResponseWriter }

// FlushError keeps transport errors observable through this wrapper, like
// the other response writer wrappers do.
func (w *replyWriter) FlushError() error { return httpwire.FlushResponse(w.ResponseWriter) }

func (w *replyWriter) capture(p []byte) {
	if len(p) == 0 {
		return
	}
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.firstAt.IsZero() && carriesPayload(p) {
		w.firstAt = time.Now()
	}
	room := w.limit - len(w.buf)
	if room <= 0 {
		w.truncated = true
		return
	}
	if len(p) > room {
		w.buf = append(w.buf, p[:room]...)
		w.truncated = true
		return
	}
	w.buf = append(w.buf, p...)
}

// carriesPayload reports whether a write holds more than keep-alive padding
// (blank lines or SSE comments).
func carriesPayload(p []byte) bool {
	for _, line := range strings.Split(string(p), "\n") {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, ":") {
			continue
		}
		return true
	}
	return false
}
