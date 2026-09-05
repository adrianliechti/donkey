package ai

import (
	"bufio"
	"encoding/json"
	"fmt"
	"io/fs"
	"path"
	"regexp"
	"sort"
	"strings"
	"unicode/utf8"

	"github.com/openai/openai-go/v3"
	"github.com/openai/openai-go/v3/responses"
)

func searchTools() []responses.ToolUnionParam {
	var tools []responses.ToolUnionParam
	for _, name := range []string{"glob", "grep"} {
		props := map[string]any{
			"pattern":    map[string]any{"type": "string", "description": "glob: filename/path pattern with *, ?, [] and ** directories. grep: Go RE2 regular expression."},
			"path":       map[string]any{"type": []string{"string", "null"}, "description": "Relative directory to search; grep also accepts a single file. Null uses the workspace."},
			"skip":       map[string]any{"type": []string{"integer", "null"}, "description": "Zero-based result offset, as reported by truncated results. Null uses 0."},
			"head_limit": map[string]any{"type": []string{"integer", "null"}, "description": "Maximum results from 1 to 200; null uses 50."},
		}
		required := []string{"pattern", "path", "skip", "head_limit"}
		desc := "Find files by name or path glob, including ** for recursive directories. Results are shallowest first and paginated; use path to scope a monorepo. Use grep to search content."
		if name == "grep" {
			props["glob"] = map[string]any{"type": []string{"string", "null"}, "description": "Optional filename/path glob filter."}
			props["output_mode"] = map[string]any{"type": []string{"string", "null"}, "enum": []any{"content", "files_with_matches", "count", nil}}
			props["-C"] = map[string]any{"type": []string{"integer", "null"}, "description": "Surrounding lines, 0 to 10. Null uses 0."}
			props["-i"] = map[string]any{"type": []string{"boolean", "null"}, "description": "Case-insensitive search; null is false."}
			required = append(required, "glob", "output_mode", "-C", "-i")
			desc = "Search source with Go RE2 regex (no lookaround/backreferences). Scope to a directory or single file, filter by glob, and choose content with line numbers/context, files_with_matches (default), or count. Results support zero-based skip pagination. Use read with the reported 1-based line number for a larger excerpt."
		}
		t := responses.ToolParamOfFunction(name, map[string]any{"type": "object", "properties": props, "required": required, "additionalProperties": false}, true)
		t.OfFunction.Description = openai.String(desc)
		tools = append(tools, t)
	}
	return tools
}

// matchPath gives ** its directory meaning while delegating ordinary segment
// syntax to path.Match. Patterns without slashes match basenames anywhere.
func matchPath(pattern, name string) bool {
	if !strings.Contains(pattern, "/") {
		ok, _ := path.Match(pattern, path.Base(name))
		return ok
	}
	pat, parts := strings.Split(pattern, "/"), strings.Split(name, "/")
	seen := map[[2]int]bool{}
	var match func(int, int) bool
	match = func(i, j int) bool {
		key := [2]int{i, j}
		if seen[key] {
			return false
		}
		seen[key] = true
		if i == len(pat) {
			return j == len(parts)
		}
		if pat[i] == "**" {
			return match(i+1, j) || j < len(parts) && match(i, j+1)
		}
		if j == len(parts) {
			return false
		}
		ok, _ := path.Match(pat[i], parts[j])
		return ok && match(i+1, j+1)
	}
	return match(0, 0)
}

func (e *explorer) query(tool, args string) string {
	var a struct {
		Pattern    string `json:"pattern"`
		Path       string `json:"path"`
		Glob       string `json:"glob"`
		Mode       string `json:"output_mode"`
		Context    int    `json:"-C"`
		IgnoreCase bool   `json:"-i"`
		Offset     int    `json:"skip"`
		Limit      *int   `json:"head_limit"`
	}
	if err := json.Unmarshal([]byte(args), &a); err != nil {
		return "Invalid search: " + err.Error()
	}
	limit := 50
	if a.Limit != nil {
		limit = *a.Limit
	}
	if a.Pattern == "" || a.Offset < 0 || limit < 1 || limit > 200 || a.Context < 0 || a.Context > 10 {
		return "Supply a pattern, non-negative skip, head_limit 1–200 and -C 0–10."
	}
	if a.Mode == "" {
		a.Mode = "files_with_matches"
	}
	if a.Mode != "content" && a.Mode != "files_with_matches" && a.Mode != "count" {
		return "output_mode must be content, files_with_matches or count."
	}
	var re *regexp.Regexp
	if tool == "grep" {
		pattern := a.Pattern
		if a.IgnoreCase {
			pattern = "(?i)" + pattern
		}
		var err error
		re, err = regexp.Compile(pattern)
		if err != nil {
			return "Invalid RE2 pattern: " + err.Error()
		}
	} else {
		a.Glob = a.Pattern
	}
	if a.Glob != "" {
		if _, err := path.Match(a.Glob, ""); err != nil {
			return "Invalid glob: " + err.Error()
		}
	}
	scope := fsPath(a.Path)
	if scope != "." && (!readablePath(scope) || !e.noSymlinks(scope)) {
		return "That search path is excluded or contains a symlink."
	}
	info, err := e.root.Stat(scope)
	if err != nil {
		return "No such search path: " + a.Path
	}
	if !info.IsDir() && tool == "glob" {
		return "glob path must be a directory."
	}
	var files []string
	_ = fs.WalkDir(e.fsys, scope, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return nil
		}
		if d.IsDir() {
			if p != scope && skipDirs[d.Name()] {
				return fs.SkipDir
			}
			return nil
		}
		if !d.Type().IsRegular() || !readablePath(p) {
			return nil
		}
		rel := p
		if scope != "." && info.IsDir() {
			rel = strings.TrimPrefix(p, scope+"/")
		}
		if a.Glob == "" || matchPath(a.Glob, rel) {
			files = append(files, p)
		}
		return nil
	})
	sort.Slice(files, func(i, j int) bool {
		if di, dj := strings.Count(files[i], "/"), strings.Count(files[j], "/"); di != dj {
			return di < dj
		}
		return files[i] < files[j]
	})
	var results []string
	oversized := 0
	for _, name := range files {
		if tool == "glob" {
			results = append(results, name)
			continue
		}
		f, err := e.root.Open(name)
		if err != nil {
			continue
		}
		var lines []string
		var hits []int
		scan := bufio.NewScanner(f)
		scan.Buffer(make([]byte, 64<<10), 1<<20)
		bytes := 0
		for scan.Scan() {
			if len(lines) >= 250000 {
				oversized++
				break
			}
			line := scan.Text()
			bytes += len(line) + 1
			if bytes > 16<<20 {
				oversized++
				break
			}
			if !utf8.ValidString(line) || strings.ContainsRune(line, '\x00') {
				break
			}
			if re.MatchString(line) {
				hits = append(hits, len(lines))
				if a.Mode == "files_with_matches" {
					break
				}
			}
			lines = append(lines, line)
		}
		if scan.Err() != nil {
			oversized++
		}
		f.Close()
		if len(hits) == 0 {
			continue
		}
		switch a.Mode {
		case "files_with_matches":
			results = append(results, name)
		case "count":
			results = append(results, fmt.Sprintf("%s:%d", name, len(hits)))
		case "content":
			for _, hit := range hits {
				if len(results) > a.Offset+limit {
					break
				}
				var b strings.Builder
				for i := max(0, hit-a.Context); i <= min(len(lines)-1, hit+a.Context); i++ {
					line := []rune(lines[i])
					if len(line) > 300 {
						line = append(line[:300], '…')
					}
					fmt.Fprintf(&b, "%s:%d: %s\n", name, i+1, string(line))
				}
				results = append(results, strings.TrimSuffix(b.String(), "\n"))
			}
		}
		// Enough to answer this page and prove that another result exists.
		if len(results) > a.Offset+limit {
			break
		}
	}
	if len(results) == 0 {
		return fmt.Sprintf("No matches (%d files exceeded search limits).", oversized)
	}
	if a.Offset >= len(results) {
		return "No more results at that skip count."
	}
	var out strings.Builder
	next := a.Offset
	for ; next < min(len(results), a.Offset+limit); next++ {
		if out.Len()+len(results[next]) > maxToolOutput-200 && out.Len() > 0 {
			break
		}
		out.WriteString(results[next])
		out.WriteByte('\n')
	}
	if next < len(results) {
		fmt.Fprintf(&out, "[More results; use skip=%d to continue.]\n", next)
	}
	if oversized > 0 {
		fmt.Fprintf(&out, "[%d files exceeded search limits; narrow the search.]\n", oversized)
	}
	return out.String()
}
