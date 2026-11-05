package traces

import (
	"sync"
	"sync/atomic"

	"github.com/router-for-me/CLIProxyAPI/v8/internal/traces/capture"
)

// maxQueuedContentBytes bounds request and reply bodies waiting in memory.
const maxQueuedContentBytes = 256 << 20

// queue is the bounded in-memory event queue. Producers never block: when the
// queue is half full, or its content budget is spent, new events lose their
// content (metadata is kept); when it is full, new events are dropped.
type queue struct {
	mu           sync.Mutex
	items        []any
	limit        int
	contentBytes int64
	notify       chan struct{}

	replyLimit     atomic.Int64
	droppedEvents  atomic.Int64
	droppedContent atomic.Int64
}

func newQueue(limit, replyLimit int) *queue {
	q := &queue{limit: limit, notify: make(chan struct{}, 1)}
	q.replyLimit.Store(int64(replyLimit))
	return q
}

func (q *queue) setLimits(limit, replyLimit int) {
	q.mu.Lock()
	q.limit = limit
	q.mu.Unlock()
	q.replyLimit.Store(int64(replyLimit))
}

// ReplyLimit implements capture.Sink.
func (q *queue) ReplyLimit() int { return int(q.replyLimit.Load()) }

// SubmitTurn implements capture.Sink.
func (q *queue) SubmitTurn(ev *capture.TurnEvent) {
	if ev == nil {
		return
	}
	q.mu.Lock()
	depth := len(q.items)
	if depth >= q.limit {
		q.mu.Unlock()
		q.droppedEvents.Add(1)
		return
	}
	size := int64(ev.ContentBytes())
	if size > 0 && (depth >= q.limit/2 || q.contentBytes+size > maxQueuedContentBytes) {
		ev.DropContent()
		size = 0
		q.droppedContent.Add(1)
	}
	q.contentBytes += size
	q.items = append(q.items, ev)
	q.mu.Unlock()
	q.wake()
}

// SubmitUsage implements capture.Sink.
func (q *queue) SubmitUsage(ev *capture.UsageEvent) { q.pushMeta(ev) }

// SubmitAttempt implements capture.Sink.
func (q *queue) SubmitAttempt(ev *capture.AttemptEvent) { q.pushMeta(ev) }

func (q *queue) pushMeta(ev any) {
	q.mu.Lock()
	if len(q.items) >= q.limit {
		q.mu.Unlock()
		q.droppedEvents.Add(1)
		return
	}
	q.items = append(q.items, ev)
	q.mu.Unlock()
	q.wake()
}

func (q *queue) wake() {
	select {
	case q.notify <- struct{}{}:
	default:
	}
}

// take removes up to n events from the head of the queue.
func (q *queue) take(n int) []any {
	q.mu.Lock()
	defer q.mu.Unlock()
	if len(q.items) == 0 {
		return nil
	}
	if n > len(q.items) {
		n = len(q.items)
	}
	out := make([]any, n)
	copy(out, q.items[:n])
	for i := range n {
		if ev, ok := out[i].(*capture.TurnEvent); ok {
			q.contentBytes -= int64(ev.ContentBytes())
		}
		q.items[i] = nil
	}
	q.items = q.items[n:]
	if len(q.items) == 0 {
		q.items = nil
		q.contentBytes = 0
	}
	return out
}

func (q *queue) depth() int {
	q.mu.Lock()
	defer q.mu.Unlock()
	return len(q.items)
}
