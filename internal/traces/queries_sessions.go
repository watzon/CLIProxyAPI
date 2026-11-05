package traces

import (
	"context"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v8/internal/traces/store"
)

// SessionQuery holds the list options of GET /traces/sessions.
type SessionQuery struct {
	Sort             string
	Order            string
	Limit            int
	Offset           int
	IncludeSubagents bool
}

type sessionAgg struct {
	sess  *store.Session
	agg   *aggregate
	errs  int
	subs  map[string]bool
	turns []*store.Turn
}

// collectSessions groups the turns of the matched sessions. Top-level rows
// cover the whole session tree; subagent rows (when included) cover their own turns.
func (s *Service) collectSessions(ctx context.Context, f store.TurnFilter, includeSubagents bool) ([]*sessionAgg, error) {
	matched, err := s.queryTurns(ctx, f)
	if err != nil {
		return nil, err
	}
	rootSet := map[string]bool{}
	subSet := map[string]bool{}
	for i := range matched {
		rootSet[matched[i].RootID] = true
		if includeSubagents && matched[i].SessionID != matched[i].RootID {
			subSet[matched[i].SessionID] = true
		}
	}
	rootIDs := make([]string, 0, len(rootSet))
	for id := range rootSet {
		rootIDs = append(rootIDs, id)
	}
	sort.Strings(rootIDs)
	all, err := s.queryTurnsByRoot(ctx, rootIDs)
	if err != nil {
		return nil, err
	}
	st := s.openStore()
	sessions := map[string]*store.Session{}
	if st != nil {
		for start := 0; start < len(rootIDs); start += queryChunk {
			end := min(start+queryChunk, len(rootIDs))
			rows, errSessions := st.SessionsByRoot(ctx, rootIDs[start:end])
			if errSessions != nil {
				return nil, errSessions
			}
			for i := range rows {
				sessions[rows[i].ID] = &rows[i]
			}
		}
	}
	idle := s.Settings().IdleGap
	out := map[string]*sessionAgg{}
	get := func(id string) *sessionAgg {
		a := out[id]
		if a == nil {
			sess := sessions[id]
			if sess == nil {
				sess = &store.Session{ID: id, RootID: id}
			}
			a = &sessionAgg{sess: sess, agg: newAggregate(), subs: map[string]bool{}}
			out[id] = a
		}
		return a
	}
	for i := range all {
		t := &all[i]
		cost, _ := s.turnCost(t)
		active := activeContribution(t, idle)
		targets := []string{}
		if rootSet[t.RootID] {
			targets = append(targets, t.RootID)
		}
		if subSet[t.SessionID] {
			targets = append(targets, t.SessionID)
		}
		for _, id := range targets {
			a := get(id)
			a.agg.add(t, cost, active)
			a.turns = append(a.turns, t)
			if t.Status == StatusError {
				a.errs++
			}
			if t.SessionID != id {
				a.subs[t.SessionID] = true
			}
		}
	}
	list := make([]*sessionAgg, 0, len(out))
	for _, a := range out {
		list = append(list, a)
	}
	return list, nil
}

// projectSourceView maps a stored source to the API enum. Rows written before
// the re-derive job ran may still hold the old spellings or nothing.
func projectSourceView(source string) string {
	switch source {
	case SourceHeader, SourcePath, SourceRule, SourceRemote, SourceFolder, SourceInherited, SourceNone:
		return source
	case "git":
		return SourceRemote
	case legacySourceInherit:
		return SourceInherited
	}
	return SourceNone
}

func (s *Service) sessionView(a *sessionAgg) SessionView {
	sess := a.sess
	v := SessionView{
		ID: sess.ID, Title: sess.Title, Project: sess.ProjectName, ProjectSource: projectSourceView(sess.ProjectSource), Folder: sess.Folder,
		Client: sess.Client, ClientVersion: sess.ClientVersion, Model: a.agg.topModel(), KeyLabel: sess.KeyLabel,
		Tags: sess.Tags, Machine: sess.Machine, Turns: a.agg.turns, Tokens: a.agg.tokens, CostUSD: a.agg.costPtr(),
		ActiveMs: a.agg.activeMs, ModelMs: a.agg.modelMs, Errors: a.errs, Subagents: len(a.subs),
		StartedAt: sess.StartedAt, LastAt: sess.LastAt,
	}
	if v.Tags == nil {
		v.Tags = []string{}
	}
	if v.Model == "" {
		v.Model = sess.Model
	}
	if sess.ParentID != "" {
		parent := sess.ParentID
		v.ParentID = &parent
	}
	if len(a.turns) > 0 {
		first, last := a.turns[0].StartedAt, a.turns[0].EndedAt
		for _, t := range a.turns {
			if t.StartedAt.Before(first) {
				first = t.StartedAt
			}
			if t.EndedAt.After(last) {
				last = t.EndedAt
			}
		}
		v.StartedAt, v.LastAt = first, last
	}
	return v
}

// Sessions answers GET /traces/sessions.
func (s *Service) Sessions(ctx context.Context, f store.TurnFilter, q SessionQuery) (SessionsResponse, error) {
	resp := SessionsResponse{Sessions: []SessionView{}}
	list, err := s.collectSessions(ctx, f, q.IncludeSubagents)
	if err != nil {
		return resp, err
	}
	views := make([]SessionView, 0, len(list))
	summary := newAggregate()
	for _, a := range list {
		views = append(views, s.sessionView(a))
	}
	for _, v := range views {
		summary.turns += v.Turns
		summary.activeMs += v.ActiveMs
		if v.CostUSD != nil {
			summary.cost += *v.CostUSD
			summary.priced++
		} else {
			summary.unpriced++
		}
	}
	sortSessions(views, q.Sort, q.Order)
	resp.Total = len(views)
	resp.Summary = SessionsSummary{Turns: summary.turns, ActiveMs: summary.activeMs, CostUSD: summary.costPtr()}
	if resp.Total == 0 {
		zero := 0.0
		resp.Summary.CostUSD = &zero
	}
	offset := max(q.Offset, 0)
	limit := q.Limit
	if offset < len(views) {
		end := len(views)
		if limit > 0 && offset+limit < end {
			end = offset + limit
		}
		resp.Sessions = append(resp.Sessions, views[offset:end]...)
	}
	return resp, nil
}

func sortSessions(views []SessionView, by, order string) {
	desc := order != "asc"
	costOf := func(v SessionView) float64 {
		if v.CostUSD == nil {
			return -1
		}
		return *v.CostUSD
	}
	sort.SliceStable(views, func(i, j int) bool {
		a, b := views[i], views[j]
		var less bool
		switch by {
		case "cost":
			less = costOf(a) < costOf(b)
		case "tokens":
			less = a.Tokens.Total < b.Tokens.Total
		case "active":
			less = a.ActiveMs < b.ActiveMs
		case "turns":
			less = a.Turns < b.Turns
		default:
			less = a.StartedAt.Before(b.StartedAt)
		}
		if desc {
			return !less && !equalSessionKey(a, b, by)
		}
		return less
	})
}

func equalSessionKey(a, b SessionView, by string) bool {
	switch by {
	case "cost":
		ac, bc := -1.0, -1.0
		if a.CostUSD != nil {
			ac = *a.CostUSD
		}
		if b.CostUSD != nil {
			bc = *b.CostUSD
		}
		return ac == bc
	case "tokens":
		return a.Tokens.Total == b.Tokens.Total
	case "active":
		return a.ActiveMs == b.ActiveMs
	case "turns":
		return a.Turns == b.Turns
	default:
		return a.StartedAt.Equal(b.StartedAt)
	}
}

func (s *Service) turnView(t *store.Turn) TurnView {
	cost, _ := s.turnCost(t)
	v := TurnView{
		ID: t.ID, Seq: t.Seq, SessionID: t.SessionID, StartedAt: t.StartedAt, EndedAt: t.EndedAt,
		LatencyMs: t.LatencyMs, TTFTMs: t.TTFTMs, Model: t.Model, ServedModel: t.ServedModel, Provider: t.Provider,
		Account: t.Account, Status: t.Status, HTTPStatus: t.HTTPStatus, Attempts: t.Attempts, Tokens: t.Tokens,
		CostUSD: cost, ToolNames: t.ToolNames, UpstreamCostUSD: t.UpstreamCostUSD,
	}
	if v.ToolNames == nil {
		v.ToolNames = []string{}
	}
	if t.Error != "" {
		e := t.Error
		v.Error = &e
	}
	return v
}

// treeSessions returns the session and every session below it.
func treeSessions(all []store.Session, id string) []*store.Session {
	children := map[string][]*store.Session{}
	var self *store.Session
	for i := range all {
		if all[i].ID == id {
			self = &all[i]
		}
		children[all[i].ParentID] = append(children[all[i].ParentID], &all[i])
	}
	if self == nil {
		return nil
	}
	out := []*store.Session{self}
	seen := map[string]bool{id: true}
	for i := 0; i < len(out); i++ {
		for _, c := range children[out[i].ID] {
			if !seen[c.ID] {
				seen[c.ID] = true
				out = append(out, c)
			}
		}
	}
	return out
}

// parentTurnOf returns the turn of the parent session that started last
// before the first turn of the subagent session.
func parentTurnOf(parentTurns []*store.Turn, firstStart time.Time) *string {
	var best *store.Turn
	for _, t := range parentTurns {
		if !t.StartedAt.After(firstStart) && (best == nil || t.StartedAt.After(best.StartedAt)) {
			best = t
		}
	}
	if best == nil {
		return nil
	}
	id := best.ID
	return &id
}

// SessionDetail answers GET /traces/sessions/:id. It returns nil when the session is unknown.
func (s *Service) SessionDetail(ctx context.Context, id string) (*SessionDetailResponse, error) {
	st := s.openStore()
	if st == nil {
		return nil, nil
	}
	found, err := st.Sessions(ctx, []string{id})
	if err != nil || len(found) == 0 {
		return nil, err
	}
	treeAll, err := st.SessionsByRoot(ctx, []string{found[0].RootID})
	if err != nil {
		return nil, err
	}
	tree := treeSessions(treeAll, id)
	if len(tree) == 0 {
		tree = []*store.Session{&found[0]}
	}
	ids := make([]string, 0, len(tree))
	for _, sess := range tree {
		ids = append(ids, sess.ID)
	}
	turns, err := s.queryTurns(ctx, store.TurnFilter{SessionIDs: ids})
	if err != nil {
		return nil, err
	}
	sort.SliceStable(turns, func(i, j int) bool { return turns[i].StartedAt.Before(turns[j].StartedAt) })
	idle := s.Settings().IdleGap
	agg := &sessionAgg{sess: tree[0], agg: newAggregate(), subs: map[string]bool{}}
	bySession := map[string][]*store.Turn{}
	resp := &SessionDetailResponse{Turns: []TurnView{}, Lanes: []Lane{}, Breaks: []Break{}}
	for i := range turns {
		t := &turns[i]
		cost, _ := s.turnCost(t)
		agg.agg.add(t, cost, activeContribution(t, idle))
		agg.turns = append(agg.turns, t)
		if t.Status == StatusError {
			agg.errs++
		}
		if t.SessionID != id {
			agg.subs[t.SessionID] = true
		}
		bySession[t.SessionID] = append(bySession[t.SessionID], t)
		resp.Turns = append(resp.Turns, s.turnView(t))
	}
	view := SessionDetailView{SessionView: s.sessionView(agg)}
	if tree[0].DeviceID != "" {
		d := tree[0].DeviceID
		view.DeviceID = &d
	}
	calls, err := st.ToolCalls(ctx, ids)
	if err != nil {
		return nil, err
	}
	files := map[string]bool{}
	for _, c := range calls {
		if !c.HasDiff || c.Failed {
			continue
		}
		files[c.File] = true
		view.LinesAdded += c.Added
		view.LinesRemoved += c.Removed
	}
	view.FilesChanged = len(files)
	resp.Session = view
	for i, sess := range tree {
		lane := Lane{SessionID: sess.ID, Turns: len(bySession[sess.ID])}
		if i == 0 {
			lane.Label = "Main"
		} else {
			lane.Label = subagentLabel(sess, i)
			if first := bySession[sess.ID]; len(first) > 0 {
				lane.ParentTurnID = parentTurnOf(bySession[sess.ParentID], first[0].StartedAt)
			}
		}
		resp.Lanes = append(resp.Lanes, lane)
	}
	for i := 1; i < len(turns); i++ {
		prev, next := &turns[i-1], &turns[i]
		if next.StartedAt.Sub(prev.StartedAt) > idle {
			from := prev.EndedAt
			if from.After(next.StartedAt) || from.Before(prev.StartedAt) {
				from = prev.StartedAt
			}
			resp.Breaks = append(resp.Breaks, Break{From: from, To: next.StartedAt})
		}
	}
	return resp, nil
}

func subagentLabel(sess *store.Session, n int) string {
	title := strings.TrimSpace(sess.Title)
	if title == "" {
		return "Subagent " + strconv.Itoa(n)
	}
	if r := []rune(title); len(r) > 60 {
		title = string(r[:60])
	}
	return title
}
