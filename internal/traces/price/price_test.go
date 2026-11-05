package price

import (
	"math"
	"testing"
	"time"
)

const catalog = `{
  "openrouter": {"id":"openrouter","models":{
    "claude-sonnet-4-5": {"id":"claude-sonnet-4-5","cost":{"input":9,"output":99}}
  }},
  "anthropic": {"id":"anthropic","models":{
    "claude-sonnet-4-5": {"id":"claude-sonnet-4-5","cost":{"input":3,"output":15,"cache_read":0.3,"cache_write":3.75}},
    "claude-opus-4-1-20250805": {"id":"claude-opus-4-1-20250805","cost":{"input":15,"output":75,"cache_read":1.5,"cache_write":18.75,"cache_write_1h":25}}
  }},
  "openai": {"id":"openai","models":{
    "gpt-5": {"id":"gpt-5","cost":{"input":1.25,"output":10,"cache_read":0.125}},
    "free-model": {"id":"free-model"}
  }}
}`

func near(a, b float64) bool { return math.Abs(a-b) < 1e-12 }

func f(v float64) *float64 { return &v }

func TestParseModelsDev(t *testing.T) {
	rows, err := ParseModelsDev([]byte(catalog))
	if err != nil {
		t.Fatal(err)
	}
	sonnet := rows["claude-sonnet-4-5"]
	if sonnet.Input != 3 || sonnet.CacheWrite5m != 3.75 || sonnet.CacheWrite1h != 6 || sonnet.Origin != OriginCatalog {
		t.Errorf("first-party provider must win and 1h defaults to 2x input: %+v", sonnet)
	}
	if opus := rows["claude-opus-4-1-20250805"]; opus.CacheWrite1h != 25 {
		t.Errorf("explicit 1h price ignored: %+v", opus)
	}
	if gpt := rows["gpt-5"]; gpt.CacheWrite5m != 1.25 || gpt.CacheRead != 0.125 {
		t.Errorf("missing cache_write must fall back to input: %+v", gpt)
	}
	if _, ok := rows["free-model"]; ok {
		t.Errorf("a model without cost must be skipped")
	}
	if _, err = ParseModelsDev([]byte(`[]`)); err == nil {
		t.Errorf("a non-object catalog must fail")
	}
}

func TestLookupOrder(t *testing.T) {
	rows, err := ParseModelsDev([]byte(catalog))
	if err != nil {
		t.Fatal(err)
	}
	table := NewTable()
	table.SetCatalog(rows, "test", time.Unix(100, 0))
	table.SetOverrides([]Override{
		{Model: "gpt-5", Output: f(8)},
		{Model: "local-*", Input: f(0), Output: f(0)},
		{Model: "claude-*", Input: f(1), Output: f(1)},
	})
	cases := []struct {
		model  string
		input  float64
		output float64
		origin string
	}{
		{"gpt-5", 1.25, 8, OriginOverride},                       // exact override merges with catalog
		{"claude-sonnet-4-5", 3, 15, OriginCatalog},              // exact catalog beats glob override
		{"anthropic/claude-sonnet-4-5", 3, 15, OriginCatalog},    // provider-independent id
		{"claude-opus-4-1", 15, 75, OriginCatalog},               // date suffix removed on both sides
		{"claude-sonnet-4-5-20250929[1m]", 3, 15, OriginCatalog}, // pins and suffixes removed
		{"claude-haiku-9", 1, 1, OriginOverride},                 // glob override last
		{"local-llama", 0, 0, OriginOverride},
	}
	for _, c := range cases {
		row, ok := table.Lookup(c.model)
		if !ok || row.Input != c.input || row.Output != c.output || row.Origin != c.origin {
			t.Errorf("Lookup(%q) = %+v %v, want in=%v out=%v origin=%s", c.model, row, ok, c.input, c.output, c.origin)
		}
	}
	if _, ok := table.Lookup("mystery-model"); ok {
		t.Errorf("unknown model must not be priced")
	}
	if _, ok := table.Lookup(""); ok {
		t.Errorf("empty model must not be priced")
	}
}

func TestCostMath(t *testing.T) {
	row := Row{Input: 3, Output: 15, CacheRead: 0.3, CacheWrite5m: 3.75, CacheWrite1h: 6}
	tokens := Tokens{Input: 1000, Output: 2000, CacheRead: 100000, CacheWrite5m: 4000, CacheWrite1h: 1000}
	want := (1000*3 + 100000*0.3 + 4000*3.75 + 1000*6 + 2000*15) / 1e6
	if got := row.Cost(tokens); !near(got, want) {
		t.Errorf("Cost = %v, want %v", got, want)
	}
	in, cr, w5, w1, out := row.Parts(tokens)
	if !near(in+cr+w5+w1+out, want) || !near(out, 0.03) {
		t.Errorf("Parts = %v %v %v %v %v", in, cr, w5, w1, out)
	}
}

func TestCatalogCacheRoundTrip(t *testing.T) {
	rows, _ := ParseModelsDev([]byte(catalog))
	a := NewTable()
	a.SetCatalog(rows, "src", time.Unix(42, 0).UTC())
	data, err := a.ExportCatalog()
	if err != nil {
		t.Fatal(err)
	}
	b := NewTable()
	if err = b.ImportCatalog(data); err != nil {
		t.Fatal(err)
	}
	source, updated := b.Info()
	if source != "src" || !updated.Equal(time.Unix(42, 0)) {
		t.Errorf("info = %s %v", source, updated)
	}
	if row, ok := b.Lookup("gpt-5"); !ok || row.Input != 1.25 {
		t.Errorf("imported lookup = %+v %v", row, ok)
	}
	if err = b.ImportCatalog([]byte(`{"rows":{}}`)); err == nil {
		t.Errorf("empty cache must be rejected")
	}
}

func TestGlobMatch(t *testing.T) {
	for _, c := range []struct {
		p, s string
		ok   bool
	}{
		{"a*", "abc", true}, {"*c", "abc", true}, {"a*c", "abc", true}, {"a*b*c", "aXbYc", true},
		{"*a*a", "xa", false}, {"abc", "abc", true}, {"abc", "abd", false}, {"a*", "b", false},
	} {
		if got := GlobMatch(c.p, c.s); got != c.ok {
			t.Errorf("GlobMatch(%q, %q) = %v", c.p, c.s, got)
		}
	}
}

func TestOverrideMissingPricesAreNotZero(t *testing.T) {
	rows, _ := ParseModelsDev([]byte(catalog))
	table := NewTable()
	table.SetCatalog(rows, "test", time.Unix(1, 0))
	table.SetOverrides([]Override{
		{Model: "half-*", Input: f(2)},                               // no output and no catalog row
		{Model: "own-model", Input: f(2), Output: f(6)},              // no cache prices
		{Model: "claude-sonnet-4-5", CacheWrite1h: f(9)},             // only one field over the catalog
		{Model: "gpt-*", Input: f(1), Output: f(1), CacheRead: f(0)}, // explicit 0 stays 0
	})
	if row, ok := table.Lookup("half-model"); ok {
		t.Errorf("an override without an output price priced the model: %+v", row)
	}
	own, ok := table.Lookup("own-model")
	if !ok || own.CacheRead != 2 || own.CacheWrite5m != 2 || own.CacheWrite1h != 4 || own.Origin != OriginOverride {
		t.Errorf("missing cache prices must derive from input: %+v %v", own, ok)
	}
	sonnet, _ := table.Lookup("claude-sonnet-4-5")
	if sonnet.Input != 3 || sonnet.Output != 15 || sonnet.CacheRead != 0.3 || sonnet.CacheWrite1h != 9 {
		t.Errorf("missing fields must keep the catalog prices: %+v", sonnet)
	}
	if gpt, _ := table.Lookup("gpt-9"); gpt.CacheRead != 0 || gpt.CacheWrite5m != 1 {
		t.Errorf("explicit zero lost: %+v", gpt)
	}
}
