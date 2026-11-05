package extract

import (
	"net/http"
	"regexp"
	"strings"

	"github.com/MoeForge/moego/ojson"
)

// Client labels for known coding agents.
const (
	ClientClaudeCode = "claude-code"
	ClientCodex      = "codex"
	ClientOpenCode   = "opencode"
	ClientOMP        = "omp"
	ClientUnknown    = "unknown"
)

// Derived is the trace metadata taken from one request.
type Derived struct {
	Client            string
	ClientVersion     string
	Folder            string
	GitRemote         string
	ClientProjectHash string
	DeviceID          string
	HeaderProject     string
	// PathProject is the project named in the base URL (/p/<project>).
	PathProject string
}

// Client header names read by Derive. The proxy never forwards them upstream.
const (
	HeaderProject   = "X-CPA-Project"
	HeaderCwd       = "X-CPA-Cwd"
	HeaderClient    = "X-CPA-Client"
	HeaderGitRemote = "X-CPA-Git-Remote"
	// HeaderPathProject is set by the proxy itself from the /p/<project> base URL.
	HeaderPathProject = "X-CPA-Path-Project"
)

// FolderExtractor finds the working folder (and git remote) of a request.
type FolderExtractor func(headers http.Header, req *Request) (folder, remote string)

// folderExtractors is the per-client registry. Clients without an entry
// (generic clients) get no folder unless they send X-CPA-Cwd.
var folderExtractors = map[string]FolderExtractor{
	ClientClaudeCode: claudeCodeFolder,
	ClientOMP:        ompFolder,
	ClientCodex:      codexFolder,
	ClientOpenCode:   openCodeFolder,
}

// Derive computes client, folder, remote and IDs for one request.
func Derive(headers http.Header, req *Request) Derived {
	if headers == nil {
		headers = http.Header{}
	}
	var d Derived
	d.Client, d.ClientVersion = DetectClient(headers)
	d.HeaderProject = strings.TrimSpace(headers.Get(HeaderProject))
	d.PathProject = strings.TrimSpace(headers.Get(HeaderPathProject))
	if req != nil {
		d.Folder, d.GitRemote = DeriveFolder(d.Client, headers, req)
	}
	if cwd := strings.TrimSpace(headers.Get(HeaderCwd)); cwd != "" {
		d.Folder = NormalizePath(cwd)
	}
	if remote := NormalizeGitRemote(headers.Get(HeaderGitRemote)); remote != "" {
		d.GitRemote = remote
	}
	if label := strings.TrimSpace(headers.Get(HeaderClient)); label != "" {
		d.Client = label
	}
	if d.Client == ClientOpenCode || headers.Get("X-Opencode-Project") != "" {
		d.ClientProjectHash = strings.TrimSpace(headers.Get("X-Opencode-Project"))
	}
	if req != nil {
		d.DeviceID = claudeDeviceID(req.ClaudeUserID)
	}
	return d
}

// DeriveFolder returns the normalized working folder and git remote that the
// client's own extractor finds in a request. Clients without an extractor
// give empty values.
func DeriveFolder(client string, headers http.Header, req *Request) (folder, remote string) {
	extractor := folderExtractors[client]
	if extractor == nil || req == nil {
		return "", ""
	}
	if headers == nil {
		headers = http.Header{}
	}
	f, r := extractor(headers, req)
	return NormalizePath(cleanFolderText(f)), NormalizeGitRemote(r)
}

// HasFolderExtractor reports whether Derive can find a folder for the client.
func HasFolderExtractor(client string) bool { return folderExtractors[client] != nil }

// DetectClient maps User-Agent and originator headers to a client label and version.
func DetectClient(headers http.Header) (string, string) {
	ua := strings.TrimSpace(headers.Get("User-Agent"))
	product, version := firstProduct(ua)
	lower := strings.ToLower(product)
	originator := strings.ToLower(strings.TrimSpace(headers.Get("Originator")))
	switch {
	case lower == "claude-cli" || lower == "claude-code":
		return ClientClaudeCode, version
	case lower == "omp":
		return ClientOMP, version
	case strings.HasPrefix(lower, "codex") || strings.HasPrefix(originator, "codex"):
		if !strings.HasPrefix(lower, "codex") {
			version = ""
		}
		return ClientCodex, version
	case lower == "opencode":
		return ClientOpenCode, version
	case lower == "":
		return ClientUnknown, ""
	}
	return lower, version
}

func firstProduct(ua string) (string, string) {
	token := ua
	if i := strings.IndexAny(token, " \t("); i >= 0 {
		token = token[:i]
	}
	name, version, _ := strings.Cut(token, "/")
	return strings.TrimSpace(name), strings.TrimSpace(version)
}

var (
	reClaudePrimaryDir = regexp.MustCompile(`(?m)Primary working directory:\s*(.+?)\s*$`)
	reWorkingDir       = regexp.MustCompile(`(?m)Working directory:\s*(.+?)\s*$`)
	reOMPDir           = regexp.MustCompile(`current working directory:\s*'([^']+)'`)
	reCodexCwd         = regexp.MustCompile(`<cwd>\s*([^<]+?)\s*</cwd>`)
	// reFolderWas matches the " (was X)" tail of an "# Environment update" line.
	reFolderWas = regexp.MustCompile(`^(.*?)\s+\(was\s+(.+)\)$`)
)

// cleanFolderText removes the " (was X)" tail of an environment update line.
// X is the folder before the shell moved, so it replaces the new folder.
func cleanFolderText(text string) string {
	text = strings.TrimSpace(text)
	if m := reFolderWas.FindStringSubmatch(text); m != nil {
		return strings.TrimSpace(m[2])
	}
	return text
}

func claudeCodeFolder(_ http.Header, req *Request) (string, string) {
	system := req.SystemText()
	if m := reClaudePrimaryDir.FindStringSubmatch(system); m != nil {
		return m[1], ""
	}
	if req.LiveCwd != "" {
		return req.LiveCwd, ""
	}
	if m := reWorkingDir.FindStringSubmatch(system); m != nil {
		return m[1], ""
	}
	return claudeMessageFolder(req), ""
}

// claudeMessageFolder finds the folder of an SDK session. The SDK sends no
// "Primary working directory" line in the system prompt. The line arrives in
// user text instead: in the SessionStart hook context, and in every
// "# Environment update" when the shell changes folder. The earliest line
// wins, and a " (was X)" tail names the older folder X.
func claudeMessageFolder(req *Request) string {
	for _, msg := range req.Messages {
		for _, b := range msg.Blocks {
			if b.Kind != KindUser {
				continue
			}
			if m := reClaudePrimaryDir.FindStringSubmatch(b.Text); m != nil {
				return cleanFolderText(m[1])
			}
		}
	}
	return ""
}

func ompFolder(_ http.Header, req *Request) (string, string) {
	for _, msg := range req.Messages {
		for _, b := range msg.Blocks {
			if b.Kind != KindUser {
				continue
			}
			if m := reOMPDir.FindStringSubmatch(b.Text); m != nil {
				return m[1], ""
			}
			// Only the first user message carries the folder.
			return "", ""
		}
	}
	return "", ""
}

func codexFolder(headers http.Header, req *Request) (string, string) {
	if raw := strings.TrimSpace(headers.Get("X-Codex-Turn-Metadata")); raw != "" && ojson.Valid(raw) {
		meta := ojson.Parse(raw)
		folder, remote := "", ""
		meta.Get("workspaces").ForEach(func(key, value ojson.Result) bool {
			folder = key.String()
			remote = value.Get("associated_remote_urls.origin").String()
			return false
		})
		if remote == "" {
			remote = meta.Get("associated_remote_urls.origin").String()
		}
		if folder != "" {
			return folder, remote
		}
	}
	for _, msg := range req.Messages {
		for _, b := range msg.Blocks {
			if m := reCodexCwd.FindStringSubmatch(b.Text); m != nil {
				return m[1], ""
			}
		}
	}
	return "", ""
}

func openCodeFolder(_ http.Header, req *Request) (string, string) {
	if m := reWorkingDir.FindStringSubmatch(req.SystemText()); m != nil {
		return m[1], ""
	}
	return "", ""
}

// claudeDeviceID reads device_id from Claude Code metadata.user_id JSON.
func claudeDeviceID(userID string) string {
	userID = strings.TrimSpace(userID)
	if userID == "" || !ojson.Valid(userID) {
		return ""
	}
	return ojson.Parse(userID).Get("device_id").String()
}

var (
	reHomeUnix    = regexp.MustCompile(`^/(?:Users|home)/[^/]+`)
	reHomeWindows = regexp.MustCompile(`^(?i:[a-z]):/Users/[^/]+`)
	reDrive       = regexp.MustCompile(`^([A-Za-z]):`)
)

// NormalizePath makes folders comparable across machines: home folders become
// "~", macOS private prefixes are dropped, separators become "/", drive
// letters become lowercase, and trailing separators are removed.
func NormalizePath(p string) string {
	p = strings.TrimSpace(p)
	p = strings.Trim(p, `"'`)
	if p == "" {
		return ""
	}
	p = strings.ReplaceAll(p, `\`, "/")
	if strings.HasPrefix(p, "/private/tmp") {
		p = "/tmp" + strings.TrimPrefix(p, "/private/tmp")
	} else if strings.HasPrefix(p, "/private/var") {
		p = "/var" + strings.TrimPrefix(p, "/private/var")
	}
	if loc := reHomeUnix.FindStringIndex(p); loc != nil {
		p = "~" + p[loc[1]:]
	} else if loc := reHomeWindows.FindStringIndex(p); loc != nil {
		p = "~" + p[loc[1]:]
	} else if m := reDrive.FindStringSubmatch(p); m != nil {
		p = strings.ToLower(m[1]) + p[1:]
	}
	for len(p) > 1 && strings.HasSuffix(p, "/") {
		p = strings.TrimSuffix(p, "/")
	}
	return p
}

// NormalizeGitRemote returns host/org/repo for SSH and HTTPS remotes.
func NormalizeGitRemote(remote string) string {
	r := strings.TrimSpace(remote)
	if r == "" {
		return ""
	}
	switch {
	case strings.Contains(r, "://"):
		_, rest, _ := strings.Cut(r, "://")
		if at := strings.LastIndex(rest, "@"); at >= 0 && at < strings.Index(rest+"/", "/") {
			rest = rest[at+1:]
		}
		host, path, _ := strings.Cut(rest, "/")
		if i := strings.Index(host, ":"); i >= 0 {
			host = host[:i]
		}
		r = host + "/" + path
	case strings.Contains(r, ":"):
		// scp-like syntax: git@host:org/repo.git
		hostPart, path, _ := strings.Cut(r, ":")
		if at := strings.LastIndex(hostPart, "@"); at >= 0 {
			hostPart = hostPart[at+1:]
		}
		r = hostPart + "/" + strings.TrimPrefix(path, "/")
	}
	r = strings.TrimSuffix(strings.TrimRight(r, "/"), ".git")
	host, path, _ := strings.Cut(r, "/")
	return strings.ToLower(host) + "/" + path
}

// ProjectNameFromRemote returns the repository name of a normalized remote.
func ProjectNameFromRemote(remote string) string {
	remote = strings.TrimRight(remote, "/")
	if i := strings.LastIndex(remote, "/"); i >= 0 {
		return remote[i+1:]
	}
	return remote
}

// ProjectNameFromFolder returns the last segment of a normalized folder.
func ProjectNameFromFolder(folder string) string {
	folder = strings.TrimRight(folder, "/")
	if i := strings.LastIndex(folder, "/"); i >= 0 && i < len(folder)-1 {
		return folder[i+1:]
	}
	return folder
}
