package capture

import (
	"bytes"
	"context"
	"net/http"
	"strings"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/interfaces"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/logging"
)

// BeginWebsocketTurn starts a turn for one message on a Responses WebSocket.
// It returns ctx unchanged and a nil turn when traces are disabled.
func BeginWebsocketTurn(ctx context.Context, c *gin.Context) (context.Context, *Turn) {
	if active.Load() == nil || c == nil || c.Request == nil {
		return ctx, nil
	}
	headers := AllowedHeaders(c.Request.Header)
	if value, ok := c.Get(ginClientHeadersKey); ok {
		if h, okHeader := value.(http.Header); okHeader {
			mergeHeaders(headers, h)
		}
	}
	path := ""
	if c.Request.URL != nil {
		path = c.Request.URL.Path
	}
	turn := newTurn(logging.GetGinRequestID(c), path, true, headers)
	turn.apiKey = ginAPIKey(c)
	turn.clientIP = strings.TrimSpace(c.ClientIP())
	return WithTurn(ctx, turn), turn
}

// FinishWebsocket submits the turn. output is the JSON output array of the
// completed response; status is 200 unless the turn failed.
func (t *Turn) FinishWebsocket(output []byte, status int, errText string) {
	if t == nil {
		return
	}
	sink := current()
	if sink == nil {
		return
	}
	ev := t.event()
	if ev == nil {
		return
	}
	ev.EndedAt = time.Now()
	ev.HTTPStatus = status
	ev.ErrorText = errText
	ev.ReplyKind = ReplyKindResponsesOutput
	if len(output) > sink.ReplyLimit() {
		// A cut JSON array cannot be parsed, so keep none of it.
		ev.ReplyTruncated = true
	} else {
		ev.Reply = bytes.Clone(output)
	}
	sink.SubmitTurn(ev)
}

// clientClosedRequest is the status recorded when the client went away mid-turn.
const clientClosedRequest = 499

// FinishWebsocketTurn submits the turn from the result of one forwarded
// WebSocket turn: the completed output array, the turn error, and a transport
// error (client gone, write failure).
func (t *Turn) FinishWebsocketTurn(output []byte, msg *interfaces.ErrorMessage, transportErr error) {
	if t == nil {
		return
	}
	status, text := http.StatusOK, ""
	switch {
	case msg != nil:
		status = msg.StatusCode
		if status == 0 {
			status = http.StatusBadGateway
		}
		if msg.Error != nil {
			text = msg.Error.Error()
		}
	case transportErr != nil:
		status = clientClosedRequest
		text = transportErr.Error()
	}
	t.FinishWebsocket(output, status, text)
}
