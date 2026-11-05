package traces

import (
	"math"
	"net/http"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v8/internal/traces/store"
)

// LimitWindow is one plan-limit window read from upstream response headers.
type LimitWindow struct {
	Name     string
	UsedPct  *float64
	Status   string
	ResetsAt *time.Time
}

// LimitSnapshot is everything one response says about plan limits.
type LimitSnapshot struct {
	Windows       []LimitWindow
	OverageStatus string
	Plan          string
}

var (
	reClaudeWindow = regexp.MustCompile(`(?i)^anthropic-ratelimit-unified-(.+)-(status|utilization|reset)$`)
	reCodexWindow  = regexp.MustCompile(`(?i)^x-codex-(?:(additional-[a-z0-9_-]+?|code-review)-)?(primary|secondary)-(used-percent|reset-at|reset-after-seconds|window-minutes)$`)
)

// ParseLimitHeaders reads Anthropic unified rate-limit headers and Codex quota
// headers (the same names the executors parse in internal/runtime/executor/helps).
func ParseLimitHeaders(h http.Header, now time.Time) LimitSnapshot {
	var snap LimitSnapshot
	if len(h) == 0 {
		return snap
	}
	windows := map[string]*LimitWindow{}
	get := func(name string) *LimitWindow {
		w := windows[name]
		if w == nil {
			w = &LimitWindow{Name: name}
			windows[name] = w
		}
		return w
	}
	codexStatus := ""
	for key, values := range h {
		if len(values) == 0 {
			continue
		}
		value := strings.TrimSpace(values[0])
		lower := strings.ToLower(key)
		switch {
		case lower == "anthropic-ratelimit-unified-overage-status":
			snap.OverageStatus = value
		case reClaudeWindow.MatchString(key):
			m := reClaudeWindow.FindStringSubmatch(key)
			name := strings.ToLower(m[1])
			if name == "overage" || strings.HasPrefix(name, "overage-") {
				continue
			}
			w := get(name)
			switch strings.ToLower(m[2]) {
			case "status":
				w.Status = value
			case "utilization":
				if f, err := strconv.ParseFloat(value, 64); err == nil && !math.IsNaN(f) {
					pct := f * 100
					if f > 1.5 {
						pct = f
					}
					w.UsedPct = &pct
				}
			case "reset":
				if t, ok := parseEpoch(value); ok {
					w.ResetsAt = &t
				}
			}
		case lower == "x-codex-plan-type":
			snap.Plan = value
		case lower == "x-codex-limit-reached":
			if strings.EqualFold(value, "true") {
				codexStatus = "rejected"
			}
		case reCodexWindow.MatchString(key):
			m := reCodexWindow.FindStringSubmatch(key)
			name := strings.ToLower(m[2])
			if m[1] != "" {
				name = strings.ToLower(strings.TrimPrefix(strings.ToLower(m[1]), "additional-")) + "-" + name
			}
			w := get(name)
			switch strings.ToLower(m[3]) {
			case "used-percent":
				if f, err := strconv.ParseFloat(value, 64); err == nil && !math.IsNaN(f) {
					w.UsedPct = &f
				}
			case "reset-at":
				if t, ok := parseEpoch(value); ok {
					w.ResetsAt = &t
				}
			case "reset-after-seconds":
				if secs, err := strconv.ParseFloat(value, 64); err == nil && w.ResetsAt == nil {
					t := now.Add(time.Duration(secs * float64(time.Second))).UTC().Truncate(time.Minute)
					w.ResetsAt = &t
				}
			}
		}
	}
	for _, w := range windows {
		if w.Status == "" && w.UsedPct != nil {
			w.Status = "allowed"
			if codexStatus != "" || *w.UsedPct >= 100 {
				w.Status = "rejected"
			}
		}
		if w.Status == "" && w.UsedPct == nil && w.ResetsAt == nil {
			continue
		}
		snap.Windows = append(snap.Windows, *w)
	}
	sort.Slice(snap.Windows, func(i, j int) bool { return snap.Windows[i].Name < snap.Windows[j].Name })
	return snap
}

func parseEpoch(v string) (time.Time, bool) {
	v = strings.TrimSpace(v)
	if v == "" {
		return time.Time{}, false
	}
	if n, err := strconv.ParseFloat(v, 64); err == nil && n > 0 {
		if n > 1e12 {
			return time.UnixMilli(int64(n)).UTC(), true
		}
		return time.Unix(int64(n), 0).UTC(), true
	}
	if t, err := time.Parse(time.RFC3339, v); err == nil {
		return t.UTC(), true
	}
	return time.Time{}, false
}

// sampleLimits stores a sample for each window whose values changed.
func sampleLimits(tx store.Tx, accountID, provider string, h http.Header, at time.Time) error {
	if accountID == "" || len(h) == 0 {
		return nil
	}
	snap := ParseLimitHeaders(h, at)
	if snap.Plan != "" {
		if err := tx.SetAccountPlan(accountID, snap.Plan); err != nil {
			return err
		}
	}
	for _, w := range snap.Windows {
		last, err := tx.LastLimitSample(accountID, w.Name)
		if err != nil {
			return err
		}
		if last != nil && sameSample(last, w, snap.OverageStatus) {
			continue
		}
		sample := &store.LimitSample{
			AccountID: accountID, At: at.UTC(), Window: w.Name, UsedPct: w.UsedPct, Status: w.Status,
			ResetsAt: w.ResetsAt, OverageStatus: snap.OverageStatus, Provider: provider, Plan: snap.Plan,
		}
		if err = tx.InsertLimitSample(sample); err != nil {
			return err
		}
	}
	return nil
}

func sameSample(last *store.LimitSample, w LimitWindow, overage string) bool {
	if last.Status != w.Status || last.OverageStatus != overage {
		return false
	}
	if (last.UsedPct == nil) != (w.UsedPct == nil) {
		return false
	}
	if last.UsedPct != nil && math.Abs(*last.UsedPct-*w.UsedPct) >= 0.5 {
		return false
	}
	if (last.ResetsAt == nil) != (w.ResetsAt == nil) {
		return false
	}
	if last.ResetsAt != nil && last.ResetsAt.Sub(*w.ResetsAt).Abs() > 2*time.Minute {
		return false
	}
	return true
}
