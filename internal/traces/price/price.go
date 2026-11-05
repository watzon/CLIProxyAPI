// Package price holds the model price table used for API-equivalent cost.
// Prices are USD per one million tokens.
package price

import (
	"encoding/json"
	"fmt"
	"regexp"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/MoeForge/moego/ojson"
)

// Origins of a price row.
const (
	OriginCatalog  = "catalog"
	OriginOverride = "override"
)

// Row is one model price in USD per one million tokens.
type Row struct {
	Model        string  `json:"model"`
	Input        float64 `json:"input"`
	Output       float64 `json:"output"`
	CacheRead    float64 `json:"cache_read"`
	CacheWrite5m float64 `json:"cache_write_5m"`
	CacheWrite1h float64 `json:"cache_write_1h"`
	Origin       string  `json:"origin"`
}

// Override is a configured price. Nil fields are not 0; see applyOverride.
type Override struct {
	Model        string
	Input        *float64
	Output       *float64
	CacheRead    *float64
	CacheWrite5m *float64
	CacheWrite1h *float64
}

// Tokens is a normalized token count for pricing. Output includes reasoning.
type Tokens struct {
	Input        int64
	Output       int64
	CacheRead    int64
	CacheWrite5m int64
	CacheWrite1h int64
}

// Parts returns the cost of each token type.
func (r Row) Parts(t Tokens) (input, cacheRead, cacheWrite5m, cacheWrite1h, output float64) {
	return float64(t.Input) * r.Input / 1e6,
		float64(t.CacheRead) * r.CacheRead / 1e6,
		float64(t.CacheWrite5m) * r.CacheWrite5m / 1e6,
		float64(t.CacheWrite1h) * r.CacheWrite1h / 1e6,
		float64(t.Output) * r.Output / 1e6
}

// Cost returns (u*in + r*cache_read + w5*cw5 + w1*cw1 + o*out) / 1e6.
func (r Row) Cost(t Tokens) float64 {
	a, b, c, d, e := r.Parts(t)
	return a + b + c + d + e
}

// preferredProviders win when several catalog providers list the same model id.
var preferredProviders = []string{"anthropic", "openai", "google", "xai", "deepseek", "mistral", "moonshotai", "alibaba", "zai", "zhipuai", "meta", "amazon-bedrock", "google-vertex"}

// ParseModelsDev reads a models.dev api.json catalog into rows keyed by
// lowercase model id.
func ParseModelsDev(data []byte) (map[string]Row, error) {
	if !ojson.ValidBytes(data) {
		return nil, fmt.Errorf("price catalog is not valid JSON")
	}
	root := ojson.ParseBytes(data)
	if !root.IsObject() {
		return nil, fmt.Errorf("price catalog is not a JSON object")
	}
	providers := map[string]ojson.Result{}
	var names []string
	root.ForEach(func(key, value ojson.Result) bool {
		if value.Get("models").IsObject() {
			providers[key.String()] = value
			names = append(names, key.String())
		}
		return true
	})
	rank := func(name string) int {
		for i, p := range preferredProviders {
			if p == name {
				return i
			}
		}
		return len(preferredProviders)
	}
	sort.SliceStable(names, func(i, j int) bool {
		ri, rj := rank(names[i]), rank(names[j])
		if ri != rj {
			return ri < rj
		}
		return names[i] < names[j]
	})
	rows := map[string]Row{}
	for _, name := range names {
		providers[name].Get("models").ForEach(func(key, model ojson.Result) bool {
			cost := model.Get("cost")
			if !cost.IsObject() || !cost.Get("input").Exists() {
				return true
			}
			id := model.Get("id").String()
			if id == "" {
				id = key.String()
			}
			lower := strings.ToLower(id)
			if _, exists := rows[lower]; exists {
				return true
			}
			rows[lower] = rowFromCost(id, cost)
			return true
		})
	}
	if len(rows) == 0 {
		return nil, fmt.Errorf("price catalog has no priced models")
	}
	return rows, nil
}

func rowFromCost(id string, cost ojson.Result) Row {
	in := cost.Get("input").Float()
	row := Row{Model: id, Input: in, Output: cost.Get("output").Float(), Origin: OriginCatalog}
	row.CacheRead = in
	if v := cost.Get("cache_read"); v.Exists() {
		row.CacheRead = v.Float()
	}
	row.CacheWrite5m = in
	if v := cost.Get("cache_write"); v.Exists() {
		row.CacheWrite5m = v.Float()
	}
	row.CacheWrite1h = 2 * in
	for _, key := range []string{"cache_write_1h", "cache_write_one_hour"} {
		if v := cost.Get(key); v.Exists() {
			row.CacheWrite1h = v.Float()
			break
		}
	}
	return row
}

var (
	reDateSuffix = regexp.MustCompile(`-(?:20\d{6}|20\d{2}-\d{2}-\d{2})$`)
	reBracket    = regexp.MustCompile(`\[[^\]]*\]$`)
)

// NormalizeModelID strips provider prefixes, version pins and date suffixes
// so the same model matches across providers.
func NormalizeModelID(model string) string {
	m := strings.ToLower(strings.TrimSpace(model))
	if i := strings.LastIndex(m, "/"); i >= 0 {
		m = m[i+1:]
	}
	if i := strings.Index(m, "@"); i >= 0 {
		m = m[:i]
	}
	if i := strings.Index(m, ":"); i >= 0 {
		m = m[:i]
	}
	m = reBracket.ReplaceAllString(m, "")
	m = reDateSuffix.ReplaceAllString(m, "")
	m = strings.TrimSuffix(m, "-latest")
	return m
}

// Table is the merged catalog and overrides. It is safe for concurrent use.
type Table struct {
	mu         sync.RWMutex
	catalog    map[string]Row
	normalized map[string]Row
	overrides  []Override
	updatedAt  time.Time
	source     string
}

// NewTable returns an empty table.
func NewTable() *Table { return &Table{} }

// SetCatalog replaces the catalog rows.
func (t *Table) SetCatalog(rows map[string]Row, source string, updatedAt time.Time) {
	normalized := make(map[string]Row, len(rows))
	keys := make([]string, 0, len(rows))
	for k := range rows {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		n := NormalizeModelID(k)
		if _, exists := normalized[n]; !exists {
			normalized[n] = rows[k]
		}
	}
	t.mu.Lock()
	t.catalog = rows
	t.normalized = normalized
	t.source = source
	t.updatedAt = updatedAt
	t.mu.Unlock()
}

// SetOverrides replaces the configured overrides.
func (t *Table) SetOverrides(overrides []Override) {
	t.mu.Lock()
	t.overrides = append([]Override(nil), overrides...)
	t.mu.Unlock()
}

// Overrides returns a copy of the configured overrides.
func (t *Table) Overrides() []Override {
	t.mu.RLock()
	defer t.mu.RUnlock()
	return append([]Override(nil), t.overrides...)
}

// Info returns the catalog source and update time.
func (t *Table) Info() (string, time.Time) {
	t.mu.RLock()
	defer t.mu.RUnlock()
	return t.source, t.updatedAt
}

// Lookup returns the price of model: an exact override, the catalog by exact
// id, the catalog by provider-independent id, then a glob override.
func (t *Table) Lookup(model string) (Row, bool) {
	model = strings.TrimSpace(model)
	if model == "" {
		return Row{}, false
	}
	lower := strings.ToLower(model)
	t.mu.RLock()
	defer t.mu.RUnlock()
	base, hasBase := t.catalog[lower]
	if !hasBase {
		base, hasBase = t.normalized[NormalizeModelID(model)]
	}
	for _, o := range t.overrides {
		if !strings.Contains(o.Model, "*") && strings.EqualFold(o.Model, model) {
			if row, ok := applyOverride(model, base, hasBase, o); ok {
				return row, true
			}
		}
	}
	if hasBase {
		base.Model = model
		return base, true
	}
	for _, o := range t.overrides {
		if strings.Contains(o.Model, "*") && GlobMatch(strings.ToLower(o.Model), lower) {
			if row, ok := applyOverride(model, Row{}, false, o); ok {
				return row, true
			}
		}
	}
	return Row{}, false
}

// applyOverride merges an override onto the catalog row. A missing price is
// never read as 0: it falls back to the catalog row, and without one the cache
// prices derive from the input price like catalog rows do. An override with no
// catalog row needs both an input and an output price to price the model.
func applyOverride(model string, base Row, hasBase bool, o Override) (Row, bool) {
	row := base
	row.Model = model
	row.Origin = OriginOverride
	set := func(dst *float64, v *float64) {
		if v != nil {
			*dst = *v
		}
	}
	set(&row.Input, o.Input)
	set(&row.Output, o.Output)
	if !hasBase {
		if o.Input == nil || o.Output == nil {
			return Row{}, false
		}
		row.CacheRead = row.Input
		row.CacheWrite5m = row.Input
		row.CacheWrite1h = 2 * row.Input
	}
	set(&row.CacheRead, o.CacheRead)
	set(&row.CacheWrite5m, o.CacheWrite5m)
	set(&row.CacheWrite1h, o.CacheWrite1h)
	return row, true
}

// GlobMatch matches s against a pattern where "*" matches any run of characters.
func GlobMatch(pattern, s string) bool {
	parts := strings.Split(pattern, "*")
	if len(parts) == 1 {
		return pattern == s
	}
	if !strings.HasPrefix(s, parts[0]) {
		return false
	}
	s = s[len(parts[0]):]
	for i := 1; i < len(parts)-1; i++ {
		idx := strings.Index(s, parts[i])
		if idx < 0 {
			return false
		}
		s = s[idx+len(parts[i]):]
	}
	return strings.HasSuffix(s, parts[len(parts)-1])
}

// snapshot is the persisted form of the catalog.
type snapshot struct {
	Source    string         `json:"source"`
	UpdatedAt time.Time      `json:"updated_at"`
	Rows      map[string]Row `json:"rows"`
}

// ExportCatalog serializes the catalog for the trace DB cache.
func (t *Table) ExportCatalog() ([]byte, error) {
	t.mu.RLock()
	defer t.mu.RUnlock()
	return json.Marshal(snapshot{Source: t.source, UpdatedAt: t.updatedAt, Rows: t.catalog})
}

// ImportCatalog restores a catalog cached by ExportCatalog.
func (t *Table) ImportCatalog(data []byte) error {
	var snap snapshot
	if err := json.Unmarshal(data, &snap); err != nil {
		return fmt.Errorf("decode cached price catalog: %w", err)
	}
	if len(snap.Rows) == 0 {
		return fmt.Errorf("cached price catalog is empty")
	}
	t.SetCatalog(snap.Rows, snap.Source, snap.UpdatedAt)
	return nil
}
