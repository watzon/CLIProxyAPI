package traces

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"net"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v8/internal/config"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/traces/extract"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/traces/price"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/util"
)

// Capture levels.
const (
	CaptureMetadata = "metadata"
	CaptureFull     = "full"
)

// Defaults for the traces block.
const (
	DefaultDir              = "traces"
	DefaultIdleGap          = 10 * time.Minute
	DefaultQueueSize        = 10000
	DefaultMaxStreamCapture = 8 << 20
	DefaultContentMaxAge    = 365 * 24 * time.Hour
	DefaultContentMaxSize   = int64(20) << 30
	DefaultPriceSource      = "https://models.dev/api.json"
	DefaultPriceRefresh     = 24 * time.Hour
)

// DefaultConfigYAML is the traces block with every default, in config file
// spelling. Config reads fill unset keys from it.
const DefaultConfigYAML = `enabled: false
dir: "traces"
capture: metadata
capture-by-key: {}
idle-gap: 10m
queue-size: 10000
max-stream-capture: 8MB
retention:
  content-max-age: 365d
  content-max-size: 20GB
redact-secrets: true
prices:
  source: "https://models.dev/api.json"
  refresh: 24h
  overrides: []
projects:
  rules: []
client-labels: {}
`

// Settings is the parsed traces configuration.
type Settings struct {
	Enabled          bool
	Dir              string
	Capture          string
	CaptureByKey     map[string]string
	IdleGap          time.Duration
	QueueSize        int
	MaxStreamCapture int
	ContentMaxAge    time.Duration
	ContentMaxSize   int64
	RedactSecrets    bool
	PriceSource      string
	PriceRefresh     time.Duration
	Overrides        []price.Override
	Rules            []ProjectRule
	ClientLabels     []clientLabel
	KeyTags          map[string][]string
}

// ProjectRule is a compiled traces.projects.rules entry.
type ProjectRule struct {
	Match   string
	Project string
	pattern string
	before  string
	after   string
	hasName bool
}

type clientLabel struct {
	ip    net.IP
	cidr  *net.IPNet
	label string
}

// SettingsFromConfig parses the traces block. Invalid values fall back to
// defaults so a typo never stops the proxy.
func SettingsFromConfig(cfg *config.Config) Settings {
	s := Settings{
		Capture:          CaptureMetadata,
		IdleGap:          DefaultIdleGap,
		QueueSize:        DefaultQueueSize,
		MaxStreamCapture: DefaultMaxStreamCapture,
		ContentMaxAge:    DefaultContentMaxAge,
		ContentMaxSize:   DefaultContentMaxSize,
		RedactSecrets:    true,
		PriceSource:      DefaultPriceSource,
		PriceRefresh:     DefaultPriceRefresh,
	}
	if cfg == nil {
		return s
	}
	t := cfg.Traces
	s.Enabled = t.Enabled
	s.Dir = resolveDir(cfg.AuthDir, t.Dir)
	if c := normalizeCapture(t.Capture); c != "" {
		s.Capture = c
	}
	s.CaptureByKey = map[string]string{}
	for key, level := range t.CaptureByKey {
		if c := normalizeCapture(level); c != "" && strings.TrimSpace(key) != "" {
			s.CaptureByKey[strings.TrimSpace(key)] = c
		}
	}
	if d, err := ParseDuration(t.IdleGap); err == nil && d > 0 {
		s.IdleGap = d
	}
	if t.QueueSize > 0 {
		s.QueueSize = t.QueueSize
	}
	if n, err := ParseSize(t.MaxStreamCapture); err == nil && n > 0 {
		s.MaxStreamCapture = int(n)
	}
	if d, err := ParseDuration(t.Retention.ContentMaxAge); err == nil && d > 0 {
		s.ContentMaxAge = d
	}
	if n, err := ParseSize(t.Retention.ContentMaxSize); err == nil && n > 0 {
		s.ContentMaxSize = n
	}
	if t.RedactSecrets != nil {
		s.RedactSecrets = *t.RedactSecrets
	}
	if t.Prices.Source != nil {
		s.PriceSource = strings.TrimSpace(*t.Prices.Source)
	}
	if d, err := ParseDuration(t.Prices.Refresh); err == nil && d > 0 {
		s.PriceRefresh = d
	}
	for _, o := range t.Prices.Overrides {
		if strings.TrimSpace(o.Model) == "" {
			continue
		}
		s.Overrides = append(s.Overrides, price.Override{
			Model: strings.TrimSpace(o.Model), Input: o.Input, Output: o.Output,
			CacheRead: o.CacheRead, CacheWrite5m: o.CacheWrite5m, CacheWrite1h: o.CacheWrite1h,
		})
	}
	s.Rules = CompileRules(t.Projects.Rules)
	s.ClientLabels = compileClientLabels(t.ClientLabels)
	s.KeyTags = map[string][]string{}
	for key, tags := range cfg.APIKeyTags {
		if strings.TrimSpace(key) != "" && len(tags) > 0 {
			s.KeyTags[strings.TrimSpace(key)] = append([]string(nil), tags...)
		}
	}
	return s
}

func normalizeCapture(v string) string {
	switch strings.ToLower(strings.TrimSpace(v)) {
	case CaptureFull:
		return CaptureFull
	case CaptureMetadata:
		return CaptureMetadata
	}
	return ""
}

func resolveDir(authDir, dir string) string {
	dir = strings.TrimSpace(dir)
	if dir == "" {
		dir = DefaultDir
	}
	if strings.HasPrefix(dir, "~") {
		if resolved, err := util.ResolveAuthDir(dir); err == nil {
			return resolved
		}
	}
	if filepath.IsAbs(dir) {
		return filepath.Clean(dir)
	}
	base, err := util.ResolveAuthDir(authDir)
	if err != nil || base == "" {
		base = "."
	}
	return filepath.Join(base, dir)
}

// CaptureLevel returns the capture level for a raw client key.
func (s Settings) CaptureLevel(apiKey string) string {
	if level, ok := s.CaptureByKey[strings.TrimSpace(apiKey)]; ok {
		return level
	}
	return s.Capture
}

// KeyLabel returns the stored label of a client key: "key-" plus the first
// eight hex characters of its SHA-256. The raw key is never stored.
func KeyLabel(apiKey string) string {
	apiKey = strings.TrimSpace(apiKey)
	if apiKey == "" {
		return ""
	}
	sum := sha256.Sum256([]byte(apiKey))
	return "key-" + hex.EncodeToString(sum[:])[:8]
}

// Machine maps a client IP through client-labels.
func (s Settings) Machine(ip string) string {
	parsed := net.ParseIP(strings.TrimSpace(ip))
	if parsed == nil {
		return strings.TrimSpace(ip)
	}
	for _, l := range s.ClientLabels {
		if l.ip != nil && l.ip.Equal(parsed) {
			return l.label
		}
	}
	for _, l := range s.ClientLabels {
		if l.cidr != nil && l.cidr.Contains(parsed) {
			return l.label
		}
	}
	return parsed.String()
}

func compileClientLabels(labels map[string]string) []clientLabel {
	var out []clientLabel
	for key, label := range labels {
		key = strings.TrimSpace(key)
		label = strings.TrimSpace(label)
		if key == "" || label == "" {
			continue
		}
		if ip := net.ParseIP(key); ip != nil {
			out = append(out, clientLabel{ip: ip, label: label})
			continue
		}
		if _, cidr, err := net.ParseCIDR(key); err == nil {
			out = append(out, clientLabel{cidr: cidr, label: label})
		}
	}
	// Most specific CIDR first.
	sort.SliceStable(out, func(i, j int) bool {
		if out[i].cidr == nil || out[j].cidr == nil {
			return out[i].cidr == nil && out[j].cidr != nil
		}
		oi, _ := out[i].cidr.Mask.Size()
		oj, _ := out[j].cidr.Mask.Size()
		return oi > oj
	})
	return out
}

// CompileRules prepares project rules. Patterns are normalized like folders.
func CompileRules(rules []config.TracesProjectRule) []ProjectRule {
	var out []ProjectRule
	for _, r := range rules {
		match := strings.TrimSpace(r.Match)
		if match == "" || strings.TrimSpace(r.Project) == "" {
			continue
		}
		pr := ProjectRule{Match: match, Project: strings.TrimSpace(r.Project)}
		const placeholder = "{name}"
		if i := strings.Index(match, placeholder); i >= 0 {
			pr.hasName = true
			pr.before = extract.NormalizePath(match[:i])
			if match[:i] != "" && strings.HasSuffix(strings.ReplaceAll(match[:i], `\`, "/"), "/") {
				pr.before += "/"
			}
			pr.after = strings.ReplaceAll(match[i+len(placeholder):], `\`, "/")
		} else {
			pr.pattern = extract.NormalizePath(match)
		}
		out = append(out, pr)
	}
	return out
}

// Apply returns the project name for folder, or false when the rule does not match.
func (r ProjectRule) Apply(folder string) (string, bool) {
	if folder == "" {
		return "", false
	}
	if !r.hasName {
		if folder == r.pattern || strings.HasPrefix(folder, strings.TrimRight(r.pattern, "/")+"/") {
			return r.Project, true
		}
		return "", false
	}
	if !strings.HasPrefix(folder, r.before) {
		return "", false
	}
	rest := folder[len(r.before):]
	name, remainder, _ := strings.Cut(rest, "/")
	if name == "" {
		return "", false
	}
	if remainder != "" {
		remainder = "/" + remainder
	}
	if r.after != "" && !strings.HasPrefix(remainder+"/", strings.TrimRight(r.after, "/")+"/") {
		return "", false
	}
	return strings.ReplaceAll(r.Project, "{name}", name), true
}

// ParseDuration accepts Go durations plus a "d" (day) unit, for example "365d" or "1d12h".
func ParseDuration(v string) (time.Duration, error) {
	v = strings.TrimSpace(v)
	if v == "" {
		return 0, fmt.Errorf("empty duration")
	}
	var total time.Duration
	if i := strings.Index(v, "d"); i > 0 {
		days, err := strconv.ParseFloat(v[:i], 64)
		if err != nil {
			return 0, fmt.Errorf("invalid duration %q", v)
		}
		total = time.Duration(days * float64(24*time.Hour))
		v = v[i+1:]
		if v == "" {
			return total, nil
		}
	}
	d, err := time.ParseDuration(v)
	if err != nil {
		return 0, fmt.Errorf("invalid duration %q", v)
	}
	return total + d, nil
}

// ParseSize accepts byte sizes such as "8MB", "20GB", "512KiB" or a plain number of bytes.
// KB, MB, GB and TB are powers of 1024.
func ParseSize(v string) (int64, error) {
	v = strings.ToUpper(strings.TrimSpace(v))
	if v == "" {
		return 0, fmt.Errorf("empty size")
	}
	units := []struct {
		suffix string
		mult   int64
	}{
		{"TIB", 1 << 40}, {"GIB", 1 << 30}, {"MIB", 1 << 20}, {"KIB", 1 << 10},
		{"TB", 1 << 40}, {"GB", 1 << 30}, {"MB", 1 << 20}, {"KB", 1 << 10},
		{"T", 1 << 40}, {"G", 1 << 30}, {"M", 1 << 20}, {"K", 1 << 10}, {"B", 1},
	}
	mult := int64(1)
	for _, u := range units {
		if strings.HasSuffix(v, u.suffix) {
			mult = u.mult
			v = strings.TrimSpace(strings.TrimSuffix(v, u.suffix))
			break
		}
	}
	n, err := strconv.ParseFloat(v, 64)
	if err != nil || n < 0 {
		return 0, fmt.Errorf("invalid size %q", v)
	}
	return int64(n * float64(mult)), nil
}

// ProjectID returns the stable ID of a project name. It is the stats group
// key of the project and a valid project= filter value. The unassigned
// project keeps the literal ID "unassigned".
func ProjectID(name string) string {
	if strings.EqualFold(strings.TrimSpace(name), UnassignedName) {
		return UnassignedName
	}
	sum := sha256.Sum256([]byte(strings.ToLower(strings.TrimSpace(name))))
	return hex.EncodeToString(sum[:])[:12]
}
