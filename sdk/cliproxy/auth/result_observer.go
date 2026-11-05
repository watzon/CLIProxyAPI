package auth

import (
	"context"
	"sync/atomic"
)

// ResultObserver receives every execution result next to Hook.OnResult, one
// call per credential try. It runs on the request path and must not block.
type ResultObserver func(ctx context.Context, result Result)

var resultObserver atomic.Pointer[ResultObserver]

// SetResultObserver installs a process-wide result observer. A nil fn removes it.
func SetResultObserver(fn ResultObserver) {
	if fn == nil {
		resultObserver.Store(nil)
		return
	}
	resultObserver.Store(&fn)
}

// emitResult calls the manager hook and the process-wide observer.
func (m *Manager) emitResult(ctx context.Context, result Result) {
	m.hook.OnResult(ctx, result)
	if fn := resultObserver.Load(); fn != nil {
		(*fn)(ctx, result)
	}
}
