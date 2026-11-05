package capture

import (
	"context"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v8/internal/logging"
	"github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/usage"
)

// RecordUsage joins a usage record to the turn in ctx and submits it.
// Records without a turn keep their trace ID so the writer can join them later.
func RecordUsage(ctx context.Context, record usage.Record) {
	sink := current()
	if sink == nil {
		return
	}
	ev := &UsageEvent{
		TurnID:  TurnFromContext(ctx).ID(),
		TraceID: record.TraceID,
		At:      time.Now(),
		Record:  record,
	}
	if ev.TurnID == "" && ev.TraceID == "" {
		return
	}
	sink.SubmitUsage(ev)
}

// RecordAttempt joins one upstream try to the turn in ctx and submits it.
func RecordAttempt(ctx context.Context, ev AttemptEvent) {
	sink := current()
	if sink == nil {
		return
	}
	ev.TurnID = TurnFromContext(ctx).ID()
	if ev.TraceID == "" {
		ev.TraceID = logging.GetRequestID(ctx)
	}
	if ev.TurnID == "" && ev.TraceID == "" {
		return
	}
	if ev.At.IsZero() {
		ev.At = time.Now()
	}
	sink.SubmitAttempt(&ev)
}
