package traces

import (
	"context"
	"net/http"

	"github.com/router-for-me/CLIProxyAPI/v8/internal/traces/capture"
	coreauth "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/auth"
	"github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/usage"
)

// usagePlugin is the usage sink registered as "traces". It does nothing while
// traces are disabled.
type usagePlugin struct{}

// HandleUsage implements usage.Plugin.
func (usagePlugin) HandleUsage(ctx context.Context, record usage.Record) {
	capture.RecordUsage(ctx, record)
}

// registerGlobalTaps installs the usage sink and the attempt observer once per process.
func registerGlobalTaps() {
	usage.RegisterNamedPlugin("traces", usagePlugin{})
	coreauth.SetResultObserver(func(ctx context.Context, r coreauth.Result) {
		if !capture.Enabled() {
			return
		}
		ev := capture.AttemptEvent{AuthID: r.AuthID, Provider: r.Provider, Model: r.Model, Success: r.Success}
		if r.Error != nil {
			ev.HTTPStatus = r.Error.HTTPStatus
			ev.Error = r.Error.Message
		} else if r.Success {
			ev.HTTPStatus = http.StatusOK
		}
		capture.RecordAttempt(ctx, ev)
	})
}
