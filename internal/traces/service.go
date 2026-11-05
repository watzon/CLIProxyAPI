// Package traces records every model request the proxy serves (sessions,
// turns, attempts, tokens, cost and optional content) into a local database
// and answers the management trace queries.
package traces

import (
	"context"
	"net/http"
	"reflect"
	"sync"
	"sync/atomic"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v8/internal/config"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/httpfetch"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/traces/capture"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/traces/price"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/traces/store"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/util"
	sdkconfig "github.com/router-for-me/CLIProxyAPI/v8/sdk/config"
	log "github.com/sirupsen/logrus"
)

const (
	batchSize       = 200
	batchInterval   = time.Second
	pendingMaxAge   = 5 * time.Minute
	pendingMaxTries = 30
	pendingMaxCount = 5000
	retentionPeriod = 24 * time.Hour
	priceCacheKey   = "prices.catalog"
	priceFetchLimit = 64 << 20
)

// Fetcher downloads the price catalog. Tests inject one that does not use the network.
type Fetcher func(ctx context.Context, url string) ([]byte, error)

// AccountInfo describes one upstream credential for the limits view.
type AccountInfo struct {
	ID       string
	Label    string
	Email    string
	Provider string
	Plan     string
	Tags     []string
}

// AccountDirectory lists the credentials known to the proxy.
type AccountDirectory func() []AccountInfo

// Options configure a Service.
type Options struct {
	Fetcher  Fetcher
	Accounts AccountDirectory
	Now      func() time.Time
}

// Service owns the trace database, the event queue and the writer.
type Service struct {
	opts   Options
	prices *price.Table

	mu       sync.Mutex
	settings atomic.Pointer[Settings]
	st       store.Store
	dir      string
	queue    *queue
	cancel   context.CancelFunc
	wg       sync.WaitGroup
	flushReq chan chan struct{}
	pending  []pendingEvent
	running  atomic.Bool

	lastWrite    atomic.Int64
	reresolveReq chan struct{}
	priceKick    chan struct{}
}

type pendingEvent struct {
	ev  any
	at  time.Time
	try int
}

var registerOnce sync.Once

// New creates a stopped Service. Call Apply to start it.
func New(opts Options) *Service {
	if opts.Now == nil {
		opts.Now = time.Now
	}
	s := &Service{opts: opts, prices: price.NewTable()}
	empty := SettingsFromConfig(nil)
	s.settings.Store(&empty)
	registerOnce.Do(registerGlobalTaps)
	return s
}

// Settings returns the current parsed settings.
func (s *Service) Settings() Settings { return *s.settings.Load() }

// Store returns the open store, or nil when traces are disabled.
func (s *Service) Store() store.Store {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.st
}

// Prices returns the price table.
func (s *Service) Prices() *price.Table { return s.prices }

// Enabled reports whether recording runs.
func (s *Service) Enabled() bool { return s.running.Load() }

// Apply (re)configures the service from cfg. It is safe to call on every
// config reload. The database folder is only read on start.
func (s *Service) Apply(cfg *config.Config) {
	next := SettingsFromConfig(cfg)
	s.mu.Lock()
	defer s.mu.Unlock()
	prev := s.settings.Load()
	s.settings.Store(&next)
	s.prices.SetOverrides(next.Overrides)

	if !next.Enabled {
		if s.st != nil {
			s.stopLocked()
			log.Info("traces: recording stopped")
		}
		return
	}
	if s.st == nil {
		if err := s.startLocked(next); err != nil {
			log.WithError(err).Error("traces: failed to start; requests are served without recording")
		}
		return
	}
	if next.Dir != s.dir {
		log.Warnf("traces: dir changed to %s; restart the proxy to use it (still writing to %s)", next.Dir, s.dir)
	}
	s.queue.setLimits(next.QueueSize, next.MaxStreamCapture)
	if !reflect.DeepEqual(prev.Rules, next.Rules) || !reflect.DeepEqual(prev.Worktrees, next.Worktrees) {
		s.requestReresolve()
	}
	if prev.PriceSource != next.PriceSource || prev.PriceRefresh != next.PriceRefresh {
		select {
		case s.priceKick <- struct{}{}:
		default:
		}
	}
}

func (s *Service) startLocked(settings Settings) error {
	st, err := store.OpenSQLite(settings.Dir)
	if err != nil {
		return err
	}
	ctx, cancel := context.WithCancel(context.Background())
	s.st = st
	s.dir = settings.Dir
	s.queue = newQueue(settings.QueueSize, settings.MaxStreamCapture)
	s.cancel = cancel
	s.flushReq = make(chan chan struct{})
	s.reresolveReq = make(chan struct{}, 1)
	s.priceKick = make(chan struct{}, 1)
	s.pending = nil
	s.loadCachedPrices(ctx)
	s.wg.Add(3)
	go s.writerLoop(ctx, st, s.queue, s.flushReq)
	go s.maintenanceLoop(ctx, st, s.reresolveReq)
	go s.priceLoop(ctx, st, s.priceKick)
	capture.SetSink(s.queue)
	s.running.Store(true)
	log.Infof("traces: recording to %s", settings.Dir)
	return nil
}

func (s *Service) stopLocked() {
	capture.ReleaseSink(s.queue)
	s.running.Store(false)
	if s.cancel != nil {
		s.cancel()
	}
	s.wg.Wait()
	if s.st != nil {
		if err := s.st.Close(); err != nil {
			log.WithError(err).Warn("traces: close database")
		}
	}
	s.st = nil
	s.queue = nil
	s.cancel = nil
	s.dir = ""
}

// Close flushes queued events and closes the database.
func (s *Service) Close() {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.st != nil {
		s.stopLocked()
	}
}

// Flush writes every queued event now. Tests and shutdown use it.
func (s *Service) Flush() {
	s.mu.Lock()
	req := s.flushReq
	running := s.st != nil
	s.mu.Unlock()
	if !running || req == nil {
		return
	}
	done := make(chan struct{})
	select {
	case req <- done:
		<-done
	case <-time.After(30 * time.Second):
		log.Warn("traces: flush timed out")
	}
}

// QueueStats returns depth, size and drop counters.
func (s *Service) QueueStats() (depth, size int, droppedEvents, droppedContent int64) {
	s.mu.Lock()
	q := s.queue
	s.mu.Unlock()
	settings := s.Settings()
	if q == nil {
		return 0, settings.QueueSize, 0, 0
	}
	return q.depth(), settings.QueueSize, q.droppedEvents.Load(), q.droppedContent.Load()
}

// LastWriteAt returns the time of the last committed batch, or nil.
func (s *Service) LastWriteAt() *time.Time {
	v := s.lastWrite.Load()
	if v == 0 {
		return nil
	}
	t := time.UnixMilli(v).UTC()
	return &t
}

func (s *Service) requestReresolve() {
	if s.reresolveReq == nil {
		return
	}
	select {
	case s.reresolveReq <- struct{}{}:
	default:
	}
}

// RequestReresolve schedules a background re-resolution of session projects.
func (s *Service) RequestReresolve() {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.requestReresolve()
}

func (s *Service) writerLoop(ctx context.Context, st store.Store, q *queue, flushReq chan chan struct{}) {
	defer s.wg.Done()
	ticker := time.NewTicker(batchInterval)
	defer ticker.Stop()
	drain := func() {
		for {
			batch := q.take(batchSize)
			if len(batch) == 0 {
				break
			}
			s.writeBatch(st, batch)
		}
		s.retryPending(st)
	}
	for {
		select {
		case <-ctx.Done():
			drain()
			return
		case done := <-flushReq:
			drain()
			close(done)
		case <-q.notify:
			if q.depth() >= batchSize {
				drain()
			}
		case <-ticker.C:
			drain()
		}
	}
}

func (s *Service) writeBatch(st store.Store, batch []any) {
	settings := s.Settings()
	var deferred []pendingEvent
	err := st.Write(context.Background(), func(tx store.Tx) error {
		for _, item := range batch {
			if retry, errEvent := s.applyEvent(tx, settings, item); errEvent != nil {
				log.WithError(errEvent).Warn("traces: skipped an event")
			} else if retry {
				deferred = append(deferred, pendingEvent{ev: item, at: s.opts.Now()})
			}
		}
		return nil
	})
	if err != nil {
		log.WithError(err).Warn("traces: batch write failed")
		return
	}
	s.lastWrite.Store(s.opts.Now().UnixMilli())
	s.pending = append(s.pending, deferred...)
	if len(s.pending) > pendingMaxCount {
		s.pending = s.pending[len(s.pending)-pendingMaxCount:]
	}
}

// retryPending joins usage and attempt events that arrived before their turn.
func (s *Service) retryPending(st store.Store) {
	if len(s.pending) == 0 {
		return
	}
	now := s.opts.Now()
	items := s.pending
	s.pending = nil
	settings := s.Settings()
	var keep []pendingEvent
	err := st.Write(context.Background(), func(tx store.Tx) error {
		for _, p := range items {
			p.try++
			// Records of routes that never become turns (images, token counts) end here.
			if now.Sub(p.at) > pendingMaxAge || p.try > pendingMaxTries {
				continue
			}
			retry, errEvent := s.applyEvent(tx, settings, p.ev)
			if errEvent != nil {
				log.WithError(errEvent).Debug("traces: dropped a pending event")
				continue
			}
			if retry {
				keep = append(keep, p)
			}
		}
		return nil
	})
	if err != nil {
		log.WithError(err).Debug("traces: pending retry failed")
		keep = items
	}
	s.pending = append(keep, s.pending...)
}

func (s *Service) applyEvent(tx store.Tx, settings Settings, item any) (bool, error) {
	switch ev := item.(type) {
	case *capture.TurnEvent:
		return false, s.ingestTurn(tx, settings, ev)
	case *capture.UsageEvent:
		return s.ingestUsage(tx, ev)
	case *capture.AttemptEvent:
		return s.ingestAttempt(tx, ev)
	}
	return false, nil
}

func (s *Service) maintenanceLoop(ctx context.Context, st store.Store, reresolve chan struct{}) {
	defer s.wg.Done()
	first := time.NewTimer(time.Minute)
	defer first.Stop()
	ticker := time.NewTicker(retentionPeriod)
	defer ticker.Stop()
	// Stored sessions from older versions get their folder, title and project
	// derived again once. The job runs here, so start-up and requests do not wait for it.
	if !rederiveDone(ctx, st) {
		s.runReresolve(ctx, st)
	}
	for {
		select {
		case <-ctx.Done():
			return
		case <-reresolve:
			s.runReresolve(ctx, st)
		case <-first.C:
			s.runRetention(ctx, st)
		case <-ticker.C:
			s.runRetention(ctx, st)
		}
	}
}

func (s *Service) runRetention(ctx context.Context, st store.Store) {
	if err := RunRetention(ctx, st, s.Settings(), s.opts.Now()); err != nil {
		log.WithError(err).Warn("traces: retention job failed")
	}
}

// RunRetention drops content of old turns (by age, then size), deletes
// unreferenced blobs and orphan rows, and vacuums. Metadata rows stay.
func RunRetention(ctx context.Context, st store.Store, settings Settings, now time.Time) error {
	if settings.ContentMaxAge > 0 {
		if _, err := st.DropContentBefore(ctx, now.Add(-settings.ContentMaxAge)); err != nil {
			return err
		}
	}
	if err := st.CollectGarbage(ctx, now.Add(-24*time.Hour)); err != nil {
		return err
	}
	for i := 0; i < 1000 && settings.ContentMaxSize > 0; i++ {
		stats, err := st.Stats(ctx)
		if err != nil {
			return err
		}
		if stats.ContentBytes <= settings.ContentMaxSize {
			break
		}
		n, err := st.DropOldestContent(ctx, 200)
		if err != nil {
			return err
		}
		if err = st.CollectGarbage(ctx, now.Add(-24*time.Hour)); err != nil {
			return err
		}
		if n == 0 {
			break
		}
	}
	return nil
}

func (s *Service) loadCachedPrices(ctx context.Context) {
	if s.st == nil {
		return
	}
	data, _, err := s.st.GetKV(ctx, priceCacheKey)
	if err != nil || len(data) == 0 {
		return
	}
	if errImport := s.prices.ImportCatalog(data); errImport != nil {
		log.WithError(errImport).Debug("traces: cached price catalog ignored")
	}
}

func (s *Service) priceLoop(ctx context.Context, st store.Store, kick chan struct{}) {
	defer s.wg.Done()
	for {
		settings := s.Settings()
		wait := settings.PriceRefresh
		if settings.PriceSource != "" {
			source, updated := s.prices.Info()
			due := source != settings.PriceSource || updated.IsZero() || s.opts.Now().Sub(updated) >= settings.PriceRefresh
			if due {
				if err := s.RefreshPrices(ctx, st); err != nil {
					log.WithError(err).Warn("traces: price catalog refresh failed; keeping the cached prices")
					wait = time.Hour
				}
			} else {
				wait = settings.PriceRefresh - s.opts.Now().Sub(updated)
			}
		}
		if wait <= 0 {
			wait = time.Hour
		}
		timer := time.NewTimer(wait)
		select {
		case <-ctx.Done():
			timer.Stop()
			return
		case <-kick:
			timer.Stop()
		case <-timer.C:
		}
	}
}

// RefreshPrices fetches the catalog now and caches it in the trace database.
func (s *Service) RefreshPrices(ctx context.Context, st store.Store) error {
	settings := s.Settings()
	if settings.PriceSource == "" {
		return nil
	}
	fetch := s.opts.Fetcher
	if fetch == nil {
		fetch = defaultFetcher
	}
	data, err := fetch(ctx, settings.PriceSource)
	if err != nil {
		return err
	}
	rows, err := price.ParseModelsDev(data)
	if err != nil {
		return err
	}
	s.prices.SetCatalog(rows, settings.PriceSource, s.opts.Now().UTC())
	if st != nil {
		if snapshot, errExport := s.prices.ExportCatalog(); errExport == nil {
			if errPut := st.PutKV(ctx, priceCacheKey, snapshot); errPut != nil {
				log.WithError(errPut).Debug("traces: could not cache the price catalog")
			}
		}
	}
	return nil
}

var proxyURLForFetch atomic.Pointer[string]

func defaultFetcher(ctx context.Context, url string) ([]byte, error) {
	client := &http.Client{Timeout: 60 * time.Second}
	if p := proxyURLForFetch.Load(); p != nil && *p != "" {
		util.SetProxy(&sdkconfig.SDKConfig{ProxyURL: *p}, client)
	}
	return httpfetch.GetBytes(ctx, client, url, map[string]string{"Accept": "application/json"}, priceFetchLimit)
}

// SetProxyURL sets the outbound proxy used for the price catalog download.
func SetProxyURL(proxyURL string) { proxyURLForFetch.Store(&proxyURL) }
