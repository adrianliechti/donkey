package ai

import (
	"crypto/rand"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path"
	"sort"
	"strings"
	"unicode/utf8"

	"github.com/aymanbagabas/go-udiff"
	"github.com/openai/openai-go/v3"
	"github.com/openai/openai-go/v3/responses"
)

func readablePath(name string) bool {
	if !fs.ValidPath(name) || name == "." || deniedFile(path.Base(name)) {
		return false
	}
	for _, component := range strings.Split(name, "/") {
		if component == ".git" || component == ".donkey" || deniedFile(component) {
			return false
		}
	}
	return true
}

func (e *explorer) noSymlinks(name string) bool {
	for p := name; p != "."; p = path.Dir(p) {
		info, err := e.root.Lstat(p)
		if err == nil && info.Mode()&os.ModeSymlink != 0 {
			return false
		}
		if err != nil && !errors.Is(err, fs.ErrNotExist) {
			return false
		}
	}
	return true
}

func editTool() responses.ToolUnionParam {
	op := stringObject("file_path", "old_string", "new_string", "replace_all")
	op["properties"].(map[string]any)["replace_all"] = map[string]any{"type": "boolean"}
	t := responses.ToolParamOfFunction("edit", map[string]any{
		"type": "object", "properties": map[string]any{
			"edits": map[string]any{"type": "array", "minItems": 1, "items": op},
		}, "required": []string{"edits"}, "additionalProperties": false,
	}, true)
	t.OfFunction.Description = openai.String(`Create or edit files in the disposable build workspace using a batch of exact replacements. Read existing files first. Each old_string must occur exactly once unless replace_all is true. Empty old_string creates a file or fills an empty file. Entries for one file apply in order. All entries are validated and staged before writing; a failed precondition changes nothing. Preserve unrelated code and indentation. The result is authoritative; do not re-read merely to verify an edit. These changes never modify the user's repository.`)
	return t
}

type fileEdit struct {
	Path string  `json:"file_path"`
	Old  string  `json:"old_string"`
	New  *string `json:"new_string"`
	All  bool    `json:"replace_all"`
}

func (e *explorer) edit(args string) string {
	var batch struct {
		Edits []fileEdit `json:"edits"`
	}
	if err := json.Unmarshal([]byte(args), &batch); err != nil {
		return "Invalid edits: " + err.Error()
	}
	if len(batch.Edits) == 0 {
		return "Supply at least one edit."
	}
	before, after := map[string]string{}, map[string]string{}
	modes := map[string]fs.FileMode{}
	existed := map[string]bool{}
	var names []string
	for _, op := range batch.Edits {
		name := fsPath(op.Path)
		if op.Path == "" || !readablePath(name) || path.Base(name) == "donkey.yaml" {
			return "Cannot edit excluded or deployment-control file: " + op.Path
		}
		if op.New == nil {
			return "new_string is required for " + name
		}
		if !utf8.ValidString(*op.New) || strings.ContainsRune(*op.New, '\x00') {
			return "new_string must be UTF-8 text without NUL bytes."
		}
		if _, staged := after[name]; !staged {
			// Do not let an in-workspace symlink alias a denied file either.
			for p := name; p != "."; p = path.Dir(p) {
				info, err := e.root.Lstat(p)
				if err == nil && info.Mode()&os.ModeSymlink != 0 {
					return "Cannot edit a symlink path: " + name
				}
				if err != nil && !errors.Is(err, fs.ErrNotExist) {
					return "Cannot inspect " + name + ": " + err.Error()
				}
			}
			info, err := e.root.Stat(name)
			modes[name] = 0644
			if err == nil {
				if !info.Mode().IsRegular() {
					return "Cannot edit a non-regular file: " + name
				}
				data, err := e.root.ReadFile(name)
				if err != nil {
					return "Cannot read " + name + ": " + err.Error()
				}
				if !utf8.Valid(data) || strings.ContainsRune(string(data), '\x00') {
					return "Cannot edit a binary file: " + name
				}
				seen, read := e.readHashes[name]
				if !read || seen != sha256.Sum256(data) {
					return "Read " + name + " before editing it; it is unread or changed since the last read."
				}
				before[name], modes[name], existed[name] = string(data), info.Mode().Perm(), true
			} else if !errors.Is(err, fs.ErrNotExist) {
				return "Cannot read " + name + ": " + err.Error()
			}
			after[name] = before[name]
			names = append(names, name)
		}
		content := after[name]
		switch {
		case op.Old == "":
			if content != "" {
				return "Empty old_string cannot overwrite existing content in " + name
			}
			after[name] = *op.New
		case op.Old == *op.New:
			return "The replacement makes no change in " + name
		default:
			count := strings.Count(content, op.Old)
			if count == 0 || count > 1 && !op.All {
				return fmt.Sprintf("old_string matches %d times in %s; include enough context for one match or use replace_all.", count, name)
			}
			n := 1
			if op.All {
				n = -1
			}
			after[name] = strings.Replace(content, op.Old, *op.New, n)
		}
	}
	// Stage replacements beside their targets. Rename preserves executable
	// modes and avoids exposing a partially written file to the next tool.
	staged := map[string]string{}
	defer func() {
		for _, p := range staged {
			_ = e.root.Remove(p)
		}
	}()
	for _, name := range names {
		if err := e.root.MkdirAll(path.Dir(name), 0755); err != nil {
			return "Cannot prepare " + name + ": " + err.Error()
		}
		tmp := path.Join(path.Dir(name), ".donkey-edit-"+rand.Text())
		f, err := e.root.OpenFile(tmp, os.O_CREATE|os.O_EXCL|os.O_WRONLY, modes[name])
		if err != nil {
			return "Cannot stage " + name + ": " + err.Error()
		}
		staged[name] = tmp
		_, werr := f.WriteString(after[name])
		cerr := f.Close()
		if err := errors.Join(werr, cerr); err != nil {
			return "Cannot stage " + name + ": " + err.Error()
		}
	}
	var committed []string
	for _, name := range names {
		if err := e.root.Rename(staged[name], name); err != nil {
			var rollbackErr error
			for _, p := range committed {
				if !existed[p] {
					rollbackErr = errors.Join(rollbackErr, e.root.Remove(p))
					continue
				}
				rollbackErr = errors.Join(rollbackErr, e.root.WriteFile(p, []byte(before[p]), modes[p]))
			}
			return fmt.Sprintf("Could not commit edits: %v", errors.Join(err, rollbackErr))
		}
		committed = append(committed, name)
	}
	for _, name := range names {
		if _, tracked := e.originals[name]; !tracked {
			e.originals[name] = before[name]
		}
		e.readHashes[name] = sha256.Sum256([]byte(after[name]))
	}
	return "Edited " + strings.Join(names, ", ") + " in the build workspace."
}

func (e *explorer) changes() []SourceChange {
	names := make([]string, 0, len(e.originals))
	for name := range e.originals {
		names = append(names, name)
	}
	sort.Strings(names)
	var changes []SourceChange
	for _, name := range names {
		data, err := e.root.ReadFile(name)
		if err != nil || string(data) == e.originals[name] {
			continue
		}
		changes = append(changes, SourceChange{Path: name, Diff: Redact(udiff.Unified("a/"+name, "b/"+name, e.originals[name], string(data)))})
	}
	return changes
}
