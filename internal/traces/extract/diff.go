package extract

import (
	"regexp"
	"strings"

	"github.com/MoeForge/moego/ojson"
)

// Diff is the file change made by one tool call. Either Old/New or Patch is set.
type Diff struct {
	File    string
	Old     string
	New     string
	Patch   string
	Added   int
	Removed int
}

// DiffFromToolCall derives the file change of a tool call from its name and
// input (canonical JSON or raw text). It returns nil for tools that do not
// change files.
func DiffFromToolCall(name, input string) *Diff {
	lower := strings.ToLower(strings.TrimSpace(name))
	var args ojson.Result
	isJSON := ojson.Valid(input)
	if isJSON {
		args = ojson.Parse(input)
	}
	switch lower {
	case "edit", "str_replace", "replace", "str_replace_editor", "str_replace_based_edit_tool", "edit_file":
		if !isJSON {
			return nil
		}
		if cmd := args.Get("command").String(); cmd != "" && cmd != "str_replace" {
			if cmd == "create" {
				return writeDiff(fileField(args), firstString(args, "file_text", "content"))
			}
			return nil
		}
		return editDiff(fileField(args), firstString(args, "old_string", "oldString", "old_str"), firstString(args, "new_string", "newString", "new_str"))
	case "multiedit", "multi_edit":
		if !isJSON {
			return nil
		}
		return multiEditDiff(fileField(args), args.Get("edits"))
	case "write", "create", "write_file", "create_file":
		if !isJSON {
			return nil
		}
		return writeDiff(fileField(args), firstString(args, "content", "file_text", "text"))
	case "notebookedit", "notebook_edit":
		if !isJSON {
			return nil
		}
		return writeDiff(firstString(args, "notebook_path", "notebookPath"), args.Get("new_source").String())
	case "apply_patch", "patch", "applypatch":
		patch := input
		if isJSON {
			patch = firstString(args, "input", "patch", "patchText", "patch_text")
		}
		return patchDiff(patch)
	case "shell", "local_shell", "exec_command", "bash", "container.exec":
		if !isJSON {
			return nil
		}
		return shellPatchDiff(args)
	}
	return nil
}

func fileField(args ojson.Result) string {
	return firstString(args, "file_path", "filePath", "path", "file", "target_file")
}

func editDiff(file, oldText, newText string) *Diff {
	if file == "" {
		return nil
	}
	added, removed := LineDiffCounts(oldText, newText)
	return &Diff{File: file, Old: oldText, New: newText, Added: added, Removed: removed}
}

func writeDiff(file, content string) *Diff {
	if file == "" {
		return nil
	}
	return &Diff{File: file, New: content, Added: countLines(content)}
}

func multiEditDiff(file string, edits ojson.Result) *Diff {
	if file == "" || !edits.IsArray() {
		return nil
	}
	var sb strings.Builder
	added, removed := 0, 0
	for i, e := range edits.Array() {
		oldText := firstString(e, "old_string", "oldString")
		newText := firstString(e, "new_string", "newString")
		a, r := LineDiffCounts(oldText, newText)
		added += a
		removed += r
		sb.WriteString("@@ edit " + itoa(i+1) + " @@\n")
		for _, line := range splitLines(oldText) {
			sb.WriteString("-" + line + "\n")
		}
		for _, line := range splitLines(newText) {
			sb.WriteString("+" + line + "\n")
		}
	}
	return &Diff{File: file, Patch: sb.String(), Added: added, Removed: removed}
}

var (
	rePatchFile   = regexp.MustCompile(`(?m)^\*\*\* (?:Update|Add|Delete) File:\s*(.+?)\s*$`)
	reUnifiedFile = regexp.MustCompile(`(?m)^\+\+\+ (?:b/)?(.+?)\s*$`)
)

func patchDiff(patch string) *Diff {
	if strings.TrimSpace(patch) == "" {
		return nil
	}
	var files []string
	for _, m := range rePatchFile.FindAllStringSubmatch(patch, -1) {
		files = append(files, m[1])
	}
	if len(files) == 0 {
		for _, m := range reUnifiedFile.FindAllStringSubmatch(patch, -1) {
			if m[1] != "/dev/null" {
				files = append(files, m[1])
			}
		}
	}
	if len(files) == 0 {
		return nil
	}
	added, removed := 0, 0
	for _, line := range strings.Split(patch, "\n") {
		switch {
		case strings.HasPrefix(line, "+++"), strings.HasPrefix(line, "---"), strings.HasPrefix(line, "***"):
		case strings.HasPrefix(line, "+"):
			added++
		case strings.HasPrefix(line, "-"):
			removed++
		}
	}
	return &Diff{File: strings.Join(files, ", "), Patch: patch, Added: added, Removed: removed}
}

func shellPatchDiff(args ojson.Result) *Diff {
	cmd := args.Get("command")
	if !cmd.Exists() {
		cmd = args.Get("cmd")
	}
	var parts []string
	if cmd.IsArray() {
		for _, p := range cmd.Array() {
			parts = append(parts, p.String())
		}
	} else {
		parts = []string{cmd.String()}
	}
	for i, p := range parts {
		if strings.Contains(p, "*** Begin Patch") {
			return patchDiff(p[strings.Index(p, "*** Begin Patch"):])
		}
		if (p == "apply_patch" || p == "applypatch") && i+1 < len(parts) {
			return patchDiff(parts[i+1])
		}
	}
	return nil
}

func splitLines(s string) []string {
	if s == "" {
		return nil
	}
	return strings.Split(strings.TrimSuffix(s, "\n"), "\n")
}

func countLines(s string) int { return len(splitLines(s)) }

// maxLCSCells bounds the line LCS work; larger inputs fall back to line counts.
const maxLCSCells = 4_000_000

// LineDiffCounts returns lines added and removed between oldText and newText.
func LineDiffCounts(oldText, newText string) (added, removed int) {
	a, b := splitLines(oldText), splitLines(newText)
	if len(a) == 0 || len(b) == 0 || len(a)*len(b) > maxLCSCells {
		return len(b), len(a)
	}
	prev := make([]int, len(b)+1)
	cur := make([]int, len(b)+1)
	for i := 1; i <= len(a); i++ {
		for j := 1; j <= len(b); j++ {
			switch {
			case a[i-1] == b[j-1]:
				cur[j] = prev[j-1] + 1
			case prev[j] >= cur[j-1]:
				cur[j] = prev[j]
			default:
				cur[j] = cur[j-1]
			}
		}
		prev, cur = cur, prev
	}
	common := prev[len(b)]
	return len(b) - common, len(a) - common
}

var reExitCode = regexp.MustCompile(`(?i)(?:exit code:?|exited with code|exit status)\s*(-?\d+)`)

// ResultFailed reports whether a tool result marks its call as failed: an
// explicit error flag, or a non-zero exit code near the start of the text.
func ResultFailed(text string, isError bool) bool {
	if isError {
		return true
	}
	head := text
	if len(head) > 300 {
		head = head[:300]
	}
	if m := reExitCode.FindStringSubmatch(head); m != nil {
		return m[1] != "0"
	}
	return false
}
