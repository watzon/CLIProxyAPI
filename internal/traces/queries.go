package traces

import (
	"context"
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v8/internal/traces/price"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/traces/store"
)

// Group keys for merged and empty groups.
const (
	GroupOther    = "__other__"
	GroupUntagged = store.UntaggedFilter
	maxGroups     = 8
	maxBuckets    = 5000
	queryChunk    = 400
)

func (s *Service) openStore() store.Store {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.st
}

// Health returns the health object. It answers while disabled.
func (s *Service) Health(ctx context.Context) Health {
	settings := s.Settings()
	depth, size, droppedEvents, droppedContent := s.QueueStats()
	source, updated := s.prices.Info()
	h := Health{
		Enabled:        s.Enabled(),
		Capture:        settings.Capture,
		QueueDepth:     depth,
		QueueSize:      size,
		DroppedEvents:  droppedEvents,
		DroppedContent: droppedContent,
		LastWriteAt:    s.LastWriteAt(),
		PricesSource:   settings.PriceSource,
	}
	if source != "" {
		h.PricesSource = source
	}
	if !updated.IsZero() {
		u := updated.UTC()
		h.PricesUpdatedAt = &u
	}
	if st := s.openStore(); st != nil {
		if stats, err := st.Stats(ctx); err == nil {
			h.DBBytes = stats.DBBytes
			h.ContentBytes = stats.ContentBytes
			h.OldestTurnAt = stats.OldestTurnAt
		}
	}
	return h
}

// turnCost prices a turn from its tokens and the current price table.
// It returns nil when the model has no price.
func (s *Service) turnCost(t *store.Turn) (*float64, *price.Row) {
	if t.Tokens.Total == 0 {
		zero := 0.0
		return &zero, nil
	}
	for _, model := range []string{t.ServedModel, t.Model} {
		if row, ok := s.prices.Lookup(model); ok {
			c := row.Cost(priceTokens(t.Tokens))
			return &c, &row
		}
	}
	return nil, nil
}

func priceTokens(t store.Tokens) price.Tokens {
	return price.Tokens{Input: t.Input, Output: t.Output, CacheRead: t.CacheRead, CacheWrite5m: t.CacheWrite5m, CacheWrite1h: t.CacheWrite1h}
}

// activeContribution is the part of session active time credited to a turn:
// the gap to the next turn start when it is within idle-gap, the turn latency
// for the last turn, and nothing before a break.
func activeContribution(t *store.Turn, idle time.Duration) int64 {
	if t.GapNextMs == nil {
		return t.LatencyMs
	}
	if *t.GapNextMs <= idle.Milliseconds() {
		return *t.GapNextMs
	}
	return 0
}

type aggregate struct {
	cost        float64
	priced      int
	unpriced    int
	tokens      store.Tokens
	activeMs    int64
	modelMs     int64
	turns       int
	failed      int
	recovered   int
	roots       map[string]bool
	subsessions map[string]bool
	models      map[string]float64
	lastAt      time.Time
}

func newAggregate() *aggregate {
	return &aggregate{roots: map[string]bool{}, subsessions: map[string]bool{}, models: map[string]float64{}}
}

func (a *aggregate) add(t *store.Turn, cost *float64, active int64) {
	a.turns++
	a.tokens.Add(t.Tokens)
	a.activeMs += active
	a.modelMs += t.LatencyMs
	if cost != nil {
		a.cost += *cost
		a.priced++
	} else {
		a.unpriced++
	}
	switch t.Status {
	case StatusError:
		a.failed++
	case StatusRetried:
		a.recovered++
	}
	if t.SessionID == t.RootID {
		a.roots[t.SessionID] = true
	} else {
		a.subsessions[t.SessionID] = true
		a.roots[t.RootID] = true
	}
	model := t.ServedModel
	if model == "" {
		model = t.Model
	}
	weight := 1.0
	if cost != nil {
		weight += *cost * 1000
	}
	a.models[model] += weight
	if t.EndedAt.After(a.lastAt) {
		a.lastAt = t.EndedAt
	}
}

func (a *aggregate) costPtr() *float64 {
	if a.priced == 0 && a.unpriced > 0 {
		return nil
	}
	c := a.cost
	return &c
}

func (a *aggregate) topModel() string {
	best, bestWeight := "", -1.0
	for m, w := range a.models {
		if w > bestWeight || (w == bestWeight && m < best) {
			best, bestWeight = m, w
		}
	}
	return best
}

func (a *aggregate) totals() Totals {
	return Totals{
		CostUSD: a.costPtr(), Tokens: a.tokens, ActiveMs: a.activeMs, ModelMs: a.modelMs,
		Sessions: len(a.roots), SubagentSessions: len(a.subsessions), Turns: a.turns,
		FailedTurns: a.failed, RecoveredTurns: a.recovered, UnpricedTurns: a.unpriced,
	}
}

func (s *Service) queryTurns(ctx context.Context, f store.TurnFilter) ([]store.Turn, error) {
	st := s.openStore()
	if st == nil {
		return nil, nil
	}
	return st.QueryTurns(ctx, f)
}

// queryTurnsByRoot loads every turn of the given session trees.
func (s *Service) queryTurnsByRoot(ctx context.Context, rootIDs []string) ([]store.Turn, error) {
	var out []store.Turn
	for start := 0; start < len(rootIDs); start += queryChunk {
		end := min(start+queryChunk, len(rootIDs))
		turns, err := s.queryTurns(ctx, store.TurnFilter{RootIDs: rootIDs[start:end]})
		if err != nil {
			return nil, err
		}
		out = append(out, turns...)
	}
	return out, nil
}

// Stats answers GET /traces/stats.
func (s *Service) Stats(ctx context.Context, f store.TurnFilter, group, bucket string) (StatsResponse, error) {
	resp := StatsResponse{From: f.From, To: f.To, Groups: []StatsGroup{}, Buckets: []Bucket{}}
	idle := s.Settings().IdleGap
	turns, err := s.queryTurns(ctx, f)
	if err != nil {
		return resp, err
	}
	prevFilter := f
	prevFilter.From = f.From.Add(-f.To.Sub(f.From))
	prevFilter.To = f.From
	prevTurns, err := s.queryTurns(ctx, prevFilter)
	if err != nil {
		return resp, err
	}
	total, previous := newAggregate(), newAggregate()
	for i := range prevTurns {
		cost, _ := s.turnCost(&prevTurns[i])
		previous.add(&prevTurns[i], cost, activeContribution(&prevTurns[i], idle))
	}
	labels := s.accountLabels()
	groups := map[string]*aggregate{}
	groupLabels := map[string]string{}
	type turnKeys struct {
		keys   []string
		cost   *float64
		active int64
	}
	perTurn := make([]turnKeys, len(turns))
	for i := range turns {
		t := &turns[i]
		cost, _ := s.turnCost(t)
		active := activeContribution(t, idle)
		total.add(t, cost, active)
		keys, keyLabels := groupKeys(group, t, labels)
		for j, k := range keys {
			g := groups[k]
			if g == nil {
				g = newAggregate()
				groups[k] = g
				groupLabels[k] = keyLabels[j]
			}
			g.add(t, cost, active)
		}
		perTurn[i] = turnKeys{keys: keys, cost: cost, active: active}
	}
	resp.Totals = total.totals()
	resp.Previous = previous.totals()

	ordered := make([]string, 0, len(groups))
	for k := range groups {
		ordered = append(ordered, k)
	}
	sort.Slice(ordered, func(i, j int) bool {
		a, b := groups[ordered[i]], groups[ordered[j]]
		if a.cost != b.cost {
			return a.cost > b.cost
		}
		if a.tokens.Total != b.tokens.Total {
			return a.tokens.Total > b.tokens.Total
		}
		return ordered[i] < ordered[j]
	})
	mapping := map[string]string{}
	var other *aggregate
	otherCount := 0
	for i, k := range ordered {
		if i < maxGroups {
			mapping[k] = k
			g := groups[k]
			resp.Groups = append(resp.Groups, StatsGroup{Key: k, Label: groupLabels[k], CostUSD: g.costPtr(), Tokens: g.tokens, Turns: g.turns, Sessions: len(g.roots), ActiveMs: g.activeMs})
			continue
		}
		mapping[k] = GroupOther
		otherCount++
		if other == nil {
			other = newAggregate()
		}
		g := groups[k]
		other.cost += g.cost
		other.priced += g.priced
		other.unpriced += g.unpriced
		other.tokens.Add(g.tokens)
		other.turns += g.turns
		other.activeMs += g.activeMs
		for r := range g.roots {
			other.roots[r] = true
		}
	}
	if other != nil {
		resp.Groups = append(resp.Groups, StatsGroup{Key: GroupOther, Label: fmt.Sprintf("Other (%d)", otherCount), CostUSD: other.costPtr(), Tokens: other.tokens, Turns: other.turns, Sessions: len(other.roots), ActiveMs: other.activeMs})
	}

	starts := bucketStarts(f.From, f.To, bucket)
	index := make(map[int64]int, len(starts))
	for i, start := range starts {
		resp.Buckets = append(resp.Buckets, Bucket{Start: start, Groups: map[string]BucketValue{}})
		index[start.Unix()] = i
	}
	type cell struct {
		cost            float64
		priced, missing int
		tokens, active  int64
	}
	cells := map[int]map[string]*cell{}
	for i := range turns {
		bi, ok := index[alignBucket(turns[i].StartedAt, bucket).Unix()]
		if !ok {
			continue
		}
		if cells[bi] == nil {
			cells[bi] = map[string]*cell{}
		}
		for _, k := range perTurn[i].keys {
			key := mapping[k]
			c := cells[bi][key]
			if c == nil {
				c = &cell{}
				cells[bi][key] = c
			}
			if perTurn[i].cost != nil {
				c.cost += *perTurn[i].cost
				c.priced++
			} else {
				c.missing++
			}
			c.tokens += turns[i].Tokens.Total
			c.active += perTurn[i].active
		}
	}
	for bi, byKey := range cells {
		for key, c := range byKey {
			var cost *float64
			if c.priced > 0 || c.missing == 0 {
				v := c.cost
				cost = &v
			}
			resp.Buckets[bi].Groups[key] = BucketValue{CostUSD: cost, Tokens: c.tokens, ActiveMs: c.active}
		}
	}
	return resp, nil
}

func groupKeys(group string, t *store.Turn, accountLabels map[string]string) ([]string, []string) {
	sess := t.Session
	if sess == nil {
		sess = &store.Session{}
	}
	switch group {
	case "model":
		m := t.ServedModel
		if m == "" {
			m = t.Model
		}
		return []string{m}, []string{m}
	case "client":
		return []string{sess.Client}, []string{sess.Client}
	case "key":
		return []string{sess.KeyLabel}, []string{sess.KeyLabel}
	case "tag":
		if len(sess.Tags) == 0 {
			return []string{GroupUntagged}, []string{"Untagged"}
		}
		return sess.Tags, sess.Tags
	case "account":
		label := accountLabels[t.Account]
		if label == "" {
			label = t.Account
		}
		return []string{t.Account}, []string{label}
	case "machine":
		return []string{sess.Machine}, []string{sess.Machine}
	default:
		return []string{sess.ProjectID}, []string{sess.ProjectName}
	}
}

func alignBucket(t time.Time, bucket string) time.Time {
	t = t.UTC()
	switch bucket {
	case "hour":
		return t.Truncate(time.Hour)
	case "week":
		day := time.Date(t.Year(), t.Month(), t.Day(), 0, 0, 0, 0, time.UTC)
		offset := (int(day.Weekday()) + 6) % 7
		return day.AddDate(0, 0, -offset)
	default:
		return time.Date(t.Year(), t.Month(), t.Day(), 0, 0, 0, 0, time.UTC)
	}
}

func bucketStarts(from, to time.Time, bucket string) []time.Time {
	var out []time.Time
	if !to.After(from) {
		return out
	}
	for cur := alignBucket(from, bucket); cur.Before(to) && len(out) < maxBuckets; {
		out = append(out, cur)
		switch bucket {
		case "hour":
			cur = cur.Add(time.Hour)
		case "week":
			cur = cur.AddDate(0, 0, 7)
		default:
			cur = cur.AddDate(0, 0, 1)
		}
	}
	return out
}

func (s *Service) accountLabels() map[string]string {
	out := map[string]string{}
	if s.opts.Accounts == nil {
		return out
	}
	for _, a := range s.opts.Accounts() {
		label := a.Label
		if label == "" {
			label = a.Email
		}
		out[a.ID] = label
	}
	return out
}

// Projects answers GET /traces/projects.
func (s *Service) Projects(ctx context.Context, f store.TurnFilter) (ProjectsResponse, error) {
	resp := ProjectsResponse{Projects: []Project{}, Rules: []RuleHits{}}
	settings := s.Settings()
	turns, err := s.queryTurns(ctx, f)
	if err != nil {
		return resp, err
	}
	type projectAgg struct {
		agg     *aggregate
		name    string
		key     string
		keyAt   time.Time
		daily   map[string]*aggregate
		dayList []string
	}
	projects := map[string]*projectAgg{}
	unassigned := map[string]bool{}
	for i := range turns {
		t := &turns[i]
		sess := t.Session
		p := projects[sess.ProjectID]
		if p == nil {
			p = &projectAgg{agg: newAggregate(), name: sess.ProjectName, daily: map[string]*aggregate{}}
			projects[sess.ProjectID] = p
		}
		if !sess.LastAt.Before(p.keyAt) {
			p.key = sess.ProjectKey
			p.keyAt = sess.LastAt
		}
		cost, _ := s.turnCost(t)
		active := activeContribution(t, settings.IdleGap)
		p.agg.add(t, cost, active)
		day := t.StartedAt.UTC().Format("2006-01-02")
		d := p.daily[day]
		if d == nil {
			d = newAggregate()
			p.daily[day] = d
		}
		d.add(t, cost, active)
		if sess.ProjectName == UnassignedName && t.SessionID == t.RootID {
			unassigned[t.SessionID] = true
		}
	}
	days := dayList(f.From, f.To)
	for id, p := range projects {
		proj := Project{
			ID: id, Name: p.name, Key: p.key, Sessions: len(p.agg.roots), Turns: p.agg.turns,
			ActiveMs: p.agg.activeMs, ModelMs: p.agg.modelMs, Tokens: p.agg.tokens, CostUSD: p.agg.costPtr(),
			TopModel: p.agg.topModel(), LastActiveAt: p.agg.lastAt, Daily: []DailyValue{},
		}
		for _, day := range days {
			if d := p.daily[day]; d != nil {
				proj.Daily = append(proj.Daily, DailyValue{Day: day, CostUSD: d.costPtr(), ActiveMs: d.activeMs})
			} else {
				zero := 0.0
				proj.Daily = append(proj.Daily, DailyValue{Day: day, CostUSD: &zero})
			}
		}
		resp.Projects = append(resp.Projects, proj)
	}
	sort.Slice(resp.Projects, func(i, j int) bool {
		a, b := resp.Projects[i], resp.Projects[j]
		ac, bc := 0.0, 0.0
		if a.CostUSD != nil {
			ac = *a.CostUSD
		}
		if b.CostUSD != nil {
			bc = *b.CostUSD
		}
		if ac != bc {
			return ac > bc
		}
		return a.LastActiveAt.After(b.LastActiveAt)
	})
	resp.UnassignedSessions = len(unassigned)
	hits := map[string]int{}
	if st := s.openStore(); st != nil {
		sessions, errSessions := st.AllSessions(ctx)
		if errSessions != nil {
			return resp, errSessions
		}
		for _, sess := range sessions {
			if sess.ProjectRule != "" {
				hits[sess.ProjectRule]++
			}
		}
	}
	for _, r := range settings.Rules {
		resp.Rules = append(resp.Rules, RuleHits{Match: r.Match, Project: r.Project, Hits: hits[r.Match]})
	}
	return resp, nil
}

func dayList(from, to time.Time) []string {
	var out []string
	for cur := alignBucket(from, "day"); cur.Before(to) && len(out) < 400; cur = cur.AddDate(0, 0, 1) {
		out = append(out, cur.Format("2006-01-02"))
	}
	return out
}

// ProjectFolders returns the project name and the folders of its sessions.
func (s *Service) ProjectFolders(ctx context.Context, projectID string) (string, []string, error) {
	st := s.openStore()
	if st == nil {
		return "", nil, nil
	}
	sessions, err := st.AllSessions(ctx)
	if err != nil {
		return "", nil, err
	}
	name := ""
	seen := map[string]bool{}
	var folders []string
	for _, sess := range sessions {
		if sess.ProjectID != projectID && sess.ProjectName != projectID {
			continue
		}
		name = sess.ProjectName
		if sess.Folder != "" && !seen[sess.Folder] {
			seen[sess.Folder] = true
			folders = append(folders, sess.Folder)
		}
	}
	sort.Strings(folders)
	return name, folders, nil
}

// FindProject returns the project with the given ID or name over all time.
func (s *Service) FindProject(ctx context.Context, idOrName string) (*Project, error) {
	resp, err := s.Projects(ctx, store.TurnFilter{From: time.Unix(0, 0), To: s.opts.Now().Add(time.Hour)})
	if err != nil {
		return nil, err
	}
	for i := range resp.Projects {
		p := &resp.Projects[i]
		if p.ID == idOrName || strings.EqualFold(p.Name, idOrName) {
			p.Daily = []DailyValue{}
			return p, nil
		}
	}
	return nil, nil
}
