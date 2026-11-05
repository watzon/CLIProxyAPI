package traces

import (
	"context"
	"encoding/csv"
	"io"
	"sort"
	"strconv"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v8/internal/traces/store"
)

// ExportCSV writes GET /traces/export.csv. group is "project" (per period and
// project) or "session" (one row per top-level session).
func (s *Service) ExportCSV(ctx context.Context, f store.TurnFilter, group, period string, w io.Writer) error {
	out := csv.NewWriter(w)
	if group == "session" {
		if err := s.exportSessions(ctx, f, out); err != nil {
			return err
		}
	} else if err := s.exportProjects(ctx, f, period, out); err != nil {
		return err
	}
	out.Flush()
	return out.Error()
}

func hours(ms int64) string { return strconv.FormatFloat(float64(ms)/3.6e6, 'f', 3, 64) }

func costCell(c *float64) string {
	if c == nil {
		return ""
	}
	return strconv.FormatFloat(*c, 'f', 6, 64)
}

func periodStart(t time.Time, period string) time.Time {
	t = t.UTC()
	switch period {
	case "week":
		return alignBucket(t, "week")
	case "month":
		return time.Date(t.Year(), t.Month(), 1, 0, 0, 0, 0, time.UTC)
	default:
		return alignBucket(t, "day")
	}
}

func (s *Service) exportProjects(ctx context.Context, f store.TurnFilter, period string, out *csv.Writer) error {
	if err := out.Write([]string{"period_start", "project", "sessions", "turns", "active_hours", "model_hours", "input_tokens", "output_tokens", "cache_read_tokens", "cache_write_tokens", "cost_usd"}); err != nil {
		return err
	}
	turns, err := s.queryTurns(ctx, f)
	if err != nil {
		return err
	}
	idle := s.Settings().IdleGap
	type key struct {
		start   time.Time
		project string
	}
	groups := map[key]*aggregate{}
	for i := range turns {
		t := &turns[i]
		k := key{start: periodStart(t.StartedAt, period), project: t.Session.ProjectName}
		a := groups[k]
		if a == nil {
			a = newAggregate()
			groups[k] = a
		}
		cost, _ := s.turnCost(t)
		a.add(t, cost, activeContribution(t, idle))
	}
	keys := make([]key, 0, len(groups))
	for k := range groups {
		keys = append(keys, k)
	}
	sort.Slice(keys, func(i, j int) bool {
		if !keys[i].start.Equal(keys[j].start) {
			return keys[i].start.Before(keys[j].start)
		}
		return keys[i].project < keys[j].project
	})
	for _, k := range keys {
		a := groups[k]
		row := []string{
			k.start.Format("2006-01-02"), k.project, strconv.Itoa(len(a.roots)), strconv.Itoa(a.turns),
			hours(a.activeMs), hours(a.modelMs), strconv.FormatInt(a.tokens.Input, 10), strconv.FormatInt(a.tokens.Output, 10),
			strconv.FormatInt(a.tokens.CacheRead, 10), strconv.FormatInt(a.tokens.CacheWrite5m+a.tokens.CacheWrite1h, 10), costCell(a.costPtr()),
		}
		if err = out.Write(row); err != nil {
			return err
		}
	}
	return nil
}

func (s *Service) exportSessions(ctx context.Context, f store.TurnFilter, out *csv.Writer) error {
	if err := out.Write([]string{"session_id", "started_at", "project", "client", "model", "key", "turns", "active_hours", "tokens", "cost_usd", "title"}); err != nil {
		return err
	}
	list, err := s.collectSessions(ctx, f, false)
	if err != nil {
		return err
	}
	views := make([]SessionView, 0, len(list))
	for _, a := range list {
		views = append(views, s.sessionView(a))
	}
	sortSessions(views, "started_at", "asc")
	for _, v := range views {
		row := []string{
			v.ID, v.StartedAt.UTC().Format(time.RFC3339), v.Project, v.Client, v.Model, v.KeyLabel,
			strconv.Itoa(v.Turns), hours(v.ActiveMs), strconv.FormatInt(v.Tokens.Total, 10), costCell(v.CostUSD), v.Title,
		}
		if err = out.Write(row); err != nil {
			return err
		}
	}
	return nil
}
