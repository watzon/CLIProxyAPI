package traces

import (
	"context"
	"encoding/json"
	"fmt"
	"sort"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/router-for-me/CLIProxyAPI/v8/internal/traces/extract"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/traces/price"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/traces/store"
)

// MaxBlockText is the block text size above which turn detail truncates.
const MaxBlockText = 20 << 10

// TurnDetail answers GET /traces/turns/:id. It returns nil for an unknown turn.
func (s *Service) TurnDetail(ctx context.Context, id string) (*TurnDetailResponse, error) {
	st := s.openStore()
	if st == nil {
		return nil, nil
	}
	return s.turnDetail(ctx, st, id, true)
}

func (s *Service) turnDetail(ctx context.Context, st store.Store, id string, truncate bool) (*TurnDetailResponse, error) {
	turn, err := st.Turn(ctx, id)
	if err != nil || turn == nil {
		return nil, err
	}
	sessions, err := st.Sessions(ctx, []string{turn.SessionID})
	if err != nil {
		return nil, err
	}
	sess := &store.Session{ID: turn.SessionID}
	if len(sessions) > 0 {
		sess = &sessions[0]
	}
	resp := &TurnDetailResponse{Turn: s.turnView(turn), Attempts: []AttemptView{}, CostBreakdown: []CostPart{}, Blocks: []BlockView{}}
	attempts, err := st.AttemptsForTurns(ctx, []string{id})
	if err != nil {
		return nil, err
	}
	prev := turn.StartedAt
	for i, a := range attempts {
		v := AttemptView{N: i + 1, Account: a.AuthID, HTTPStatus: a.HTTPStatus, Status: "ok", LatencyMs: max(a.At.Sub(prev).Milliseconds(), 0)}
		if !a.Success {
			v.Status = "error"
			if a.Error != "" {
				e := a.Error
				v.Error = &e
			}
		}
		prev = a.At
		resp.Attempts = append(resp.Attempts, v)
	}
	if len(resp.Attempts) == 0 {
		v := AttemptView{N: 1, Account: turn.Account, HTTPStatus: turn.HTTPStatus, Status: "ok", LatencyMs: turn.LatencyMs}
		if turn.Status == StatusError {
			v.Status = "error"
			if turn.Error != "" {
				e := turn.Error
				v.Error = &e
			}
		}
		resp.Attempts = append(resp.Attempts, v)
	}
	_, row := s.turnCost(turn)
	parts := []struct {
		kind   string
		tokens int64
	}{
		{"input", turn.Tokens.Input}, {"cache_read", turn.Tokens.CacheRead}, {"cache_write_5m", turn.Tokens.CacheWrite5m},
		{"cache_write_1h", turn.Tokens.CacheWrite1h}, {"output", turn.Tokens.Output},
	}
	var costs [5]float64
	if row != nil {
		costs[0], costs[1], costs[2], costs[3], costs[4] = row.Parts(priceTokens(turn.Tokens))
	}
	for i, p := range parts {
		part := CostPart{Type: p.kind, Tokens: p.tokens}
		if row != nil || turn.Tokens.Total == 0 {
			c := costs[i]
			part.CostUSD = &c
		}
		resp.CostBreakdown = append(resp.CostBreakdown, part)
	}
	resp.Facts = TurnFacts{RequestID: turn.RequestID, Folder: sess.Folder, Client: clientDisplay(sess), Machine: sess.Machine}
	if turn.StopReason != "" {
		sr := turn.StopReason
		resp.Facts.StopReason = &sr
	}
	if sess.ParentID != "" {
		parentTurns, errParent := st.QueryTurns(ctx, store.TurnFilter{SessionIDs: []string{sess.ParentID}})
		if errParent != nil {
			return nil, errParent
		}
		own, errOwn := st.QueryTurns(ctx, store.TurnFilter{SessionIDs: []string{sess.ID}})
		if errOwn != nil {
			return nil, errOwn
		}
		if len(own) > 0 {
			ptrs := make([]*store.Turn, len(parentTurns))
			for i := range parentTurns {
				ptrs[i] = &parentTurns[i]
			}
			first := own[0].StartedAt
			for _, t := range own {
				if t.StartedAt.Before(first) {
					first = t.StartedAt
				}
			}
			resp.Facts.ParentTurnID = parentTurnOf(ptrs, first)
		}
	}
	resp.ContentAvailable = turn.ContentAvailable
	if turn.ContentAvailable {
		blocks, errBlocks := s.turnBlocks(ctx, st, turn, truncate)
		if errBlocks != nil {
			return nil, errBlocks
		}
		resp.Blocks = blocks
	}
	return resp, nil
}

func clientDisplay(sess *store.Session) string {
	if sess.ClientVersion == "" {
		return sess.Client
	}
	return sess.Client + " " + sess.ClientVersion
}

// turnBlocks returns the new units of the turn and its reply as blocks.
func (s *Service) turnBlocks(ctx context.Context, st store.Store, turn *store.Turn, truncate bool) ([]BlockView, error) {
	refs, err := st.TurnBlobRefs(ctx, turn.ID)
	if err != nil {
		return nil, err
	}
	var blocks []extract.Block
	for _, ref := range refs {
		if !ref.IsNew || ref.Role == store.RoleTools {
			continue
		}
		raw, errBlob := st.Blob(ctx, ref.Hash)
		if errBlob != nil {
			return nil, errBlob
		}
		if raw == nil {
			continue
		}
		var msg extract.Message
		if errDecode := json.Unmarshal(raw, &msg); errDecode != nil {
			return nil, fmt.Errorf("decode content blob: %w", errDecode)
		}
		blocks = append(blocks, msg.Blocks...)
	}
	var callIDs []string
	for _, b := range blocks {
		if b.ToolCallID != "" {
			callIDs = append(callIDs, b.ToolCallID)
		}
	}
	calls, err := st.ToolCallsByID(ctx, turn.RootID, callIDs)
	if err != nil {
		return nil, err
	}
	byID := map[string]store.ToolCall{}
	for _, c := range calls {
		byID[c.CallID] = c
	}
	out := make([]BlockView, 0, len(blocks))
	for _, b := range blocks {
		out = append(out, blockView(b, byID, truncate))
	}
	return out, nil
}

func blockView(b extract.Block, calls map[string]store.ToolCall, truncate bool) BlockView {
	v := BlockView{Kind: b.Kind, Text: b.Text, FullSize: len(b.Text), IsError: b.IsError}
	if b.ToolCallID != "" {
		id := b.ToolCallID
		v.ToolCallID = &id
	}
	name := b.ToolName
	call, known := calls[b.ToolCallID]
	if name == "" && known {
		name = call.Name
	}
	if name != "" {
		v.ToolName = &name
	}
	switch b.Kind {
	case extract.KindToolResult:
		v.IsError = extract.ResultFailed(b.Text, b.IsError)
	case extract.KindToolCall:
		v.IsError = known && call.Failed
		if diff := extract.DiffFromToolCall(b.ToolName, b.Text); diff != nil {
			dv := &DiffView{File: diff.File}
			if diff.Patch != "" {
				patch, cut := cutText(diff.Patch, truncate)
				dv.Patch = &patch
				v.Truncated = v.Truncated || cut
			} else {
				oldText, cutOld := cutText(diff.Old, truncate)
				newText, cutNew := cutText(diff.New, truncate)
				dv.Old, dv.New = &oldText, &newText
				v.Truncated = v.Truncated || cutOld || cutNew
			}
			v.Diff = dv
		}
	}
	text, cut := cutText(b.Text, truncate)
	v.Text = text
	v.Truncated = v.Truncated || cut
	return v
}

func cutText(s string, truncate bool) (string, bool) {
	if !truncate || len(s) <= MaxBlockText {
		return s, false
	}
	n := MaxBlockText
	for n > 0 && !utf8.RuneStart(s[n]) {
		n--
	}
	return s[:n], true
}

// TurnBlock answers GET /traces/turns/:id/blocks/:index with the full block.
func (s *Service) TurnBlock(ctx context.Context, id string, index int) (*BlockView, error) {
	st := s.openStore()
	if st == nil {
		return nil, nil
	}
	turn, err := st.Turn(ctx, id)
	if err != nil || turn == nil || !turn.ContentAvailable {
		return nil, err
	}
	blocks, err := s.turnBlocks(ctx, st, turn, false)
	if err != nil {
		return nil, err
	}
	if index < 0 || index >= len(blocks) {
		return nil, nil
	}
	return &blocks[index], nil
}

// ErrorType classifies an error for grouping.
func ErrorType(status int, message string) string {
	lower := strings.ToLower(message)
	switch {
	case status == 429 || strings.Contains(lower, "rate limit"):
		return "rate_limit"
	case status == 529 || strings.Contains(lower, "overloaded"):
		return "overloaded"
	case status == 401 || status == 403:
		return "auth"
	case status == 408 || status == 504 || strings.Contains(lower, "timeout") || strings.Contains(lower, "deadline"):
		return "timeout"
	case status >= 500:
		return "server"
	case status >= 400:
		return "bad_request"
	case status == 0:
		return "network"
	}
	return "other"
}

// Errors answers GET /traces/errors: every failed attempt and every failed
// turn without a failed attempt, newest first.
func (s *Service) Errors(ctx context.Context, f store.TurnFilter, limit, offset int) (ErrorsResponse, error) {
	resp := ErrorsResponse{Groups: []ErrorGroup{}, Items: []ErrorItem{}}
	turns, err := s.queryTurns(ctx, f)
	if err != nil {
		return resp, err
	}
	st := s.openStore()
	byTurn := map[string][]store.Attempt{}
	candidates := make([]string, 0)
	for i := range turns {
		if turns[i].Status != StatusOK || turns[i].Attempts > 1 {
			candidates = append(candidates, turns[i].ID)
		}
	}
	for start := 0; st != nil && start < len(candidates); start += queryChunk {
		end := min(start+queryChunk, len(candidates))
		attempts, errAttempts := st.AttemptsForTurns(ctx, candidates[start:end])
		if errAttempts != nil {
			return resp, errAttempts
		}
		for _, a := range attempts {
			byTurn[a.TurnID] = append(byTurn[a.TurnID], a)
		}
	}
	var items []ErrorItem
	for i := range turns {
		t := &turns[i]
		title := t.Session.Title
		if title == "" {
			title = t.RootTitle
		}
		base := ErrorItem{TurnID: t.ID, SessionID: t.SessionID, Title: title, Project: t.Session.ProjectName, Recovered: t.Status != StatusError}
		failedAttempt := false
		for _, a := range byTurn[t.ID] {
			if a.Success {
				continue
			}
			failedAttempt = true
			item := base
			item.At, item.Account, item.HTTPStatus, item.Error = a.At, a.AuthID, a.HTTPStatus, a.Error
			items = append(items, item)
		}
		if !failedAttempt && t.Status == StatusError {
			item := base
			item.At, item.Account, item.HTTPStatus, item.Error = t.StartedAt, t.Account, t.HTTPStatus, t.Error
			items = append(items, item)
		}
	}
	sort.SliceStable(items, func(i, j int) bool { return items[i].At.After(items[j].At) })
	groups := map[[2]string]*ErrorGroup{}
	for _, item := range items {
		key := [2]string{ErrorType(item.HTTPStatus, item.Error), item.Account}
		g := groups[key]
		if g == nil {
			g = &ErrorGroup{Type: key[0], Account: key[1]}
			groups[key] = g
		}
		g.Count++
		if item.At.After(g.LastAt) {
			g.LastAt = item.At
		}
	}
	for _, g := range groups {
		resp.Groups = append(resp.Groups, *g)
	}
	sort.Slice(resp.Groups, func(i, j int) bool {
		if resp.Groups[i].Count != resp.Groups[j].Count {
			return resp.Groups[i].Count > resp.Groups[j].Count
		}
		return resp.Groups[i].LastAt.After(resp.Groups[j].LastAt)
	})
	resp.Total = len(items)
	offset = max(offset, 0)
	if offset < len(items) {
		end := len(items)
		if limit > 0 && offset+limit < end {
			end = offset + limit
		}
		resp.Items = append(resp.Items, items[offset:end]...)
	}
	return resp, nil
}

// Limits answers GET /traces/limits.
func (s *Service) Limits(ctx context.Context, f store.TurnFilter) (LimitsResponse, error) {
	resp := LimitsResponse{Accounts: []AccountLimits{}}
	st := s.openStore()
	if st == nil {
		return resp, nil
	}
	accounts := map[string]*AccountLimits{}
	get := func(id string) *AccountLimits {
		a := accounts[id]
		if a == nil {
			a = &AccountLimits{AccountID: id, Label: id, Tags: []string{}, Windows: []LimitWindowView{}, History: []LimitHistoryPoint{}}
			accounts[id] = a
		}
		return a
	}
	if s.opts.Accounts != nil {
		for _, info := range s.opts.Accounts() {
			a := get(info.ID)
			if info.Label != "" {
				a.Label = info.Label
			} else if info.Email != "" {
				a.Label = info.Email
			}
			a.Email, a.Provider = info.Email, info.Provider
			if len(info.Tags) > 0 {
				a.Tags = append([]string(nil), info.Tags...)
			}
			if info.Plan != "" {
				plan := info.Plan
				a.Plan = &plan
			}
		}
	}
	samples, err := st.LimitSamples(ctx, time.Time{})
	if err != nil {
		return resp, err
	}
	historyFrom := s.opts.Now().Add(-14 * 24 * time.Hour)
	latest := map[[2]string]store.LimitSample{}
	for _, sample := range samples {
		a := get(sample.AccountID)
		if a.Provider == "" {
			a.Provider = sample.Provider
		}
		latest[[2]string{sample.AccountID, sample.Window}] = sample
		if !sample.At.Before(historyFrom) {
			a.History = append(a.History, LimitHistoryPoint{At: sample.At, Window: sample.Window, UsedPct: sample.UsedPct})
		}
		if sample.Plan != "" && a.Plan == nil {
			plan := sample.Plan
			a.Plan = &plan
		}
	}
	for key, sample := range latest {
		a := get(key[0])
		a.Windows = append(a.Windows, LimitWindowView{Name: sample.Window, UsedPct: sample.UsedPct, Status: sample.Status, ResetsAt: sample.ResetsAt})
		if sample.OverageStatus != "" {
			o := sample.OverageStatus
			a.OverageStatus = &o
		}
	}
	stored, err := st.Accounts(ctx)
	if err != nil {
		return resp, err
	}
	for _, row := range stored {
		a := get(row.AccountID)
		a.PlanPriceUSD = row.PlanPriceUSD
		if row.Plan != "" {
			plan := row.Plan
			a.Plan = &plan
		}
	}
	turns, err := s.queryTurns(ctx, f)
	if err != nil {
		return resp, err
	}
	usage := map[string]*aggregate{}
	for i := range turns {
		t := &turns[i]
		if t.Account == "" {
			continue
		}
		u := usage[t.Account]
		if u == nil {
			u = newAggregate()
			usage[t.Account] = u
		}
		cost, _ := s.turnCost(t)
		u.add(t, cost, 0)
		get(t.Account)
	}
	for id, a := range accounts {
		if u := usage[id]; u != nil {
			a.Usage = AccountUsage{Turns: u.turns, Tokens: u.tokens.Total, CostUSD: u.costPtr()}
		} else {
			zero := 0.0
			a.Usage = AccountUsage{CostUSD: &zero}
		}
		sort.Slice(a.Windows, func(i, j int) bool { return a.Windows[i].Name < a.Windows[j].Name })
		resp.Accounts = append(resp.Accounts, *a)
	}
	sort.Slice(resp.Accounts, func(i, j int) bool {
		if resp.Accounts[i].Provider != resp.Accounts[j].Provider {
			return resp.Accounts[i].Provider < resp.Accounts[j].Provider
		}
		return resp.Accounts[i].Label < resp.Accounts[j].Label
	})
	return resp, nil
}

// SetPlanPrice stores the monthly plan price of an account.
func (s *Service) SetPlanPrice(ctx context.Context, accountID string, price *float64) error {
	st := s.openStore()
	if st == nil {
		return fmt.Errorf("traces are disabled")
	}
	return st.SetPlanPrice(ctx, accountID, price)
}

// PricesView answers GET /traces/prices: models seen in traces plus overrides.
// A seen model without any price is listed with null prices and origin "unpriced".
func (s *Service) PricesView(ctx context.Context) (PricesResponse, error) {
	source, updated := s.prices.Info()
	resp := PricesResponse{Source: s.Settings().PriceSource, Rows: []PriceRowView{}}
	if source != "" {
		resp.Source = source
	}
	if !updated.IsZero() {
		u := updated.UTC()
		resp.UpdatedAt = &u
	}
	seen := map[string]bool{}
	var models []string
	if st := s.openStore(); st != nil {
		list, err := st.SeenModels(ctx)
		if err != nil {
			return resp, err
		}
		models = list
	}
	// Config overrides come first and exactly as written, so the panel can edit
	// and save them without adding entries the user never created.
	for _, o := range s.prices.Overrides() {
		if seen[strings.ToLower(o.Model)] {
			continue
		}
		seen[strings.ToLower(o.Model)] = true
		resp.Rows = append(resp.Rows, PriceRowView{Model: o.Model, Input: o.Input, Output: o.Output, CacheRead: o.CacheRead, CacheWrite5m: o.CacheWrite5m, CacheWrite1h: o.CacheWrite1h, Origin: "override"})
	}
	for _, m := range models {
		if seen[strings.ToLower(m)] {
			continue
		}
		seen[strings.ToLower(m)] = true
		row, ok := s.prices.Lookup(m)
		if !ok {
			resp.Rows = append(resp.Rows, PriceRowView{Model: m, Origin: "unpriced"})
			continue
		}
		if row.Origin == price.OriginOverride {
			// Priced by a glob override, which is already listed above.
			continue
		}
		resp.Rows = append(resp.Rows, priceRowView(m, row.Input, row.Output, row.CacheRead, row.CacheWrite5m, row.CacheWrite1h, row.Origin))
	}
	return resp, nil
}

func priceRowView(model string, in, out, cr, w5, w1 float64, origin string) PriceRowView {
	return PriceRowView{Model: model, Input: &in, Output: &out, CacheRead: &cr, CacheWrite5m: &w5, CacheWrite1h: &w1, Origin: origin}
}

// Delete answers DELETE /traces and frees unreferenced content.
func (s *Service) Delete(ctx context.Context, f store.DeleteFilter) (DeleteResponse, error) {
	st := s.openStore()
	if st == nil {
		return DeleteResponse{}, nil
	}
	sessions, turns, err := st.Delete(ctx, f)
	if err != nil {
		return DeleteResponse{}, err
	}
	if errGC := st.CollectGarbage(ctx, s.opts.Now().Add(-24*time.Hour)); errGC != nil {
		return DeleteResponse{DeletedSessions: sessions, DeletedTurns: turns}, errGC
	}
	return DeleteResponse{DeletedSessions: sessions, DeletedTurns: turns}, nil
}
