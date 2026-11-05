package config

// TracesConfig configures request tracing (YAML: traces). Every field except Dir
// is applied on hot reload. Values stay in their YAML spelling here; the traces
// runtime parses durations and sizes so this package keeps no tracing logic.
type TracesConfig struct {
	// Enabled turns recording on. Default is false.
	Enabled bool `yaml:"enabled,omitempty" json:"enabled"`
	// Dir is the trace database folder. Relative paths resolve under auth-dir.
	Dir string `yaml:"dir,omitempty" json:"dir,omitempty"`
	// Capture is "metadata" (default) or "full" (message content too).
	Capture string `yaml:"capture,omitempty" json:"capture,omitempty"`
	// CaptureByKey overrides Capture per raw client API key.
	CaptureByKey map[string]string `yaml:"capture-by-key,omitempty" json:"capture-by-key,omitempty"`
	// IdleGap is the longest pause between turns that still counts as active time.
	IdleGap string `yaml:"idle-gap,omitempty" json:"idle-gap,omitempty"`
	// QueueSize bounds the in-memory event queue.
	QueueSize int `yaml:"queue-size,omitempty" json:"queue-size,omitempty"`
	// MaxStreamCapture bounds the reply bytes kept per turn, for example "8MB".
	MaxStreamCapture string `yaml:"max-stream-capture,omitempty" json:"max-stream-capture,omitempty"`
	// Retention bounds stored message content. Metadata rows are kept.
	Retention TracesRetentionConfig `yaml:"retention,omitempty" json:"retention"`
	// RedactSecrets replaces known secret patterns before content is stored. Default is true.
	RedactSecrets *bool `yaml:"redact-secrets,omitempty" json:"redact-secrets,omitempty"`
	// Prices configures the model price table used for API-equivalent cost.
	Prices TracesPricesConfig `yaml:"prices,omitempty" json:"prices"`
	// Projects configures folder to project rules.
	Projects TracesProjectsConfig `yaml:"projects,omitempty" json:"projects"`
	// ClientLabels maps a client IP or CIDR to a machine label.
	ClientLabels map[string]string `yaml:"client-labels,omitempty" json:"client-labels,omitempty"`
}

// TracesRetentionConfig bounds stored message content.
type TracesRetentionConfig struct {
	// ContentMaxAge accepts Go durations plus a "d" (day) suffix, for example "365d".
	ContentMaxAge string `yaml:"content-max-age,omitempty" json:"content-max-age,omitempty"`
	// ContentMaxSize accepts byte sizes such as "20GB".
	ContentMaxSize string `yaml:"content-max-size,omitempty" json:"content-max-size,omitempty"`
}

// TracesPricesConfig configures the price catalog and local overrides.
type TracesPricesConfig struct {
	// Source is the catalog URL. Nil uses the default catalog; an empty string disables fetching.
	Source *string `yaml:"source,omitempty" json:"source,omitempty"`
	// Refresh is the catalog refresh interval, for example "24h".
	Refresh string `yaml:"refresh,omitempty" json:"refresh,omitempty"`
	// Overrides win over the catalog. Model names may use * globs.
	Overrides []TracesPriceOverride `yaml:"overrides,omitempty" json:"overrides,omitempty"`
}

// TracesPriceOverride is one price row in USD per one million tokens.
type TracesPriceOverride struct {
	Model        string   `yaml:"model" json:"model"`
	Input        *float64 `yaml:"input,omitempty" json:"input,omitempty"`
	Output       *float64 `yaml:"output,omitempty" json:"output,omitempty"`
	CacheRead    *float64 `yaml:"cache-read,omitempty" json:"cache-read,omitempty"`
	CacheWrite5m *float64 `yaml:"cache-write-5m,omitempty" json:"cache-write-5m,omitempty"`
	CacheWrite1h *float64 `yaml:"cache-write-1h,omitempty" json:"cache-write-1h,omitempty"`
}

// TracesProjectsConfig holds ordered folder to project rules.
type TracesProjectsConfig struct {
	Rules []TracesProjectRule `yaml:"rules,omitempty" json:"rules,omitempty"`
}

// TracesProjectRule maps a folder pattern to a project name. "{name}" in Match
// captures one path segment; the first matching rule wins.
type TracesProjectRule struct {
	Match   string `yaml:"match" json:"match"`
	Project string `yaml:"project" json:"project"`
}
