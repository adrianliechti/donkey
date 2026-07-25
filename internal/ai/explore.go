package ai

import (
	"context"
	"encoding/json"
	"fmt"
	"io/fs"
	"os"
	"path"
	"sort"
	"strings"
	"unicode/utf8"

	"github.com/openai/openai-go/v3"
	"github.com/openai/openai-go/v3/responses"
	"github.com/openai/openai-go/v3/shared"
)

// Investigating a failure is a search, not a lookup: the trace names a file,
// that file imports another, and the setting that actually broke the deploy
// is a third one nobody mentioned. Handing over a fixed set of files guessed
// up front gets the common case and misses exactly the cases worth having an
// assistant for.
//
// So the assistant can read the app for itself, within hard limits:
//
//   - three tools, all read-only. There is no shell, no write, no network.
//     The worst a confused model can do is read a file it did not need.
//   - containment is the operating system's job, not this file's. Every path
//     goes through an *os.Root opened on the upload, which refuses ".." and
//     symlinks that leave the tree at the syscall level — no string check to
//     get wrong, and no window between checking a path and using it. The
//     upload is a copy in a temp dir, never the user's working tree.
//   - the same exclusions as everywhere else (deniedFile, skipDirs), so .env
//     files and keys stay invisible.
//   - a turn budget. Exploration ends, then a separate call asks for the
//     structured answer, so tool use and strict JSON never mix.
const (
	maxExploreTurns   = 6
	maxToolOutput     = 8 * 1024
	maxListEntries    = 200
	maxSearchMatches  = 40
	maxReadFileLines  = 400
	maxSearchFileSize = 512 * 1024
)

// explorer answers tool calls against one folder.
type explorer struct {
	root *os.Root
	fsys fs.FS
}

// newExplorer opens the folder as a root. Everything the assistant can reach
// goes through it.
func newExplorer(dir string) (*explorer, error) {
	root, err := os.OpenRoot(dir)
	if err != nil {
		return nil, err
	}
	return &explorer{root: root, fsys: root.FS()}, nil
}

func (e *explorer) Close() error { return e.root.Close() }

// fsPath maps what the assistant typed to a path inside the root. It is a
// convenience, not a boundary: the two prefixes below are the ones a model
// reasonably produces (the container's working directory is the app root),
// and anything it gets wrong is refused by *os.Root rather than by this.
func fsPath(rel string) string {
	s := strings.TrimSpace(rel)
	s = strings.TrimPrefix(s, "/app/")
	s = strings.TrimPrefix(s, "./")
	s = strings.Trim(s, "/")
	if s == "" {
		return "."
	}
	return path.Clean(s)
}

func (e *explorer) tools() []responses.ToolUnionParam {
	str := func(desc string) map[string]any {
		return map[string]any{"type": "string", "description": desc}
	}
	object := func(prop string, desc string) map[string]any {
		return map[string]any{
			"type":                 "object",
			"properties":           map[string]any{prop: str(desc)},
			"required":             []string{prop},
			"additionalProperties": false,
		}
	}
	tools := []responses.ToolUnionParam{
		responses.ToolParamOfFunction("list_files",
			object("path", "Folder to list, relative to the app root. Empty for the root."), true),
		responses.ToolParamOfFunction("read_file",
			object("path", "File to read, relative to the app root."), true),
		responses.ToolParamOfFunction("search_files",
			object("query", "Literal text to look for (not a regular expression)."), true),
	}
	for i, desc := range []string{
		"List files in the app, optionally under a subfolder. Start here when you don't know the layout.",
		"Read one file from the app, with line numbers. Read a file before proposing any change to it.",
		"Find a literal string across the app's files. Use it to locate a setting, a port, a route or a symbol when you don't know which file holds it.",
	} {
		tools[i].OfFunction.Description = openai.String(desc)
	}
	return tools
}

// call runs one tool. Every failure comes back as text the model can act on:
// an error string that says "no such file" is a useful observation, while a
// hard failure would end an investigation over a wrong guess.
func (e *explorer) call(name, args string) string {
	var a struct {
		Path  string `json:"path"`
		Query string `json:"query"`
	}
	if args != "" {
		if err := json.Unmarshal([]byte(args), &a); err != nil {
			return "Could not read those arguments: " + err.Error()
		}
	}
	switch name {
	case "list_files":
		return e.list(a.Path)
	case "read_file":
		return e.read(a.Path)
	case "search_files":
		return e.search(a.Query)
	default:
		return "There is no tool called " + name + "."
	}
}

func (e *explorer) list(rel string) string {
	var lines []string
	n := 0
	err := fs.WalkDir(e.fsys, fsPath(rel), func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return nil
		}
		if d.IsDir() {
			if skipDirs[d.Name()] {
				return fs.SkipDir
			}
			return nil
		}
		if deniedFile(d.Name()) {
			return nil
		}
		if n >= maxListEntries {
			return fs.SkipAll
		}
		n++
		lines = append(lines, p)
		return nil
	})
	if err != nil || len(lines) == 0 {
		return "No files there."
	}
	sort.Strings(lines)
	out := strings.Join(lines, "\n")
	if n >= maxListEntries {
		out += "\n…(more files not shown)"
	}
	return out
}

func (e *explorer) read(rel string) string {
	if rel == "" {
		return "Give a file path."
	}
	name := fsPath(rel)
	if deniedFile(path.Base(name)) {
		return "That file is not readable — donkey never sends secrets to the assistant."
	}
	info, err := e.root.Stat(name)
	if err != nil {
		return "No such file in this app: " + rel
	}
	if info.IsDir() {
		return rel + " is a folder — use list_files."
	}
	b, err := e.root.ReadFile(name)
	if err != nil {
		return "Could not read " + rel + ": " + err.Error()
	}
	if !utf8.Valid(b) {
		return rel + " is not a text file."
	}
	lines := strings.Split(string(b), "\n")
	truncated := false
	if len(lines) > maxReadFileLines {
		lines = lines[:maxReadFileLines]
		truncated = true
	}
	var sb strings.Builder
	for i, l := range lines {
		fmt.Fprintf(&sb, "%d\t%s\n", i+1, l)
		if sb.Len() > maxToolOutput {
			truncated = true
			break
		}
	}
	if truncated {
		sb.WriteString("…(file continues)\n")
	}
	return sb.String()
}

func (e *explorer) search(query string) string {
	if query == "" {
		return "Give something to search for."
	}
	var hits []string
	fs.WalkDir(e.fsys, ".", func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return nil
		}
		if d.IsDir() {
			if skipDirs[d.Name()] {
				return fs.SkipDir
			}
			return nil
		}
		if len(hits) >= maxSearchMatches {
			return fs.SkipAll
		}
		if deniedFile(d.Name()) {
			return nil
		}
		info, err := d.Info()
		if err != nil || info.Size() > maxSearchFileSize {
			return nil
		}
		b, err := e.root.ReadFile(p)
		if err != nil || !utf8.Valid(b) {
			return nil
		}
		rel := p
		for i, line := range strings.Split(string(b), "\n") {
			if !strings.Contains(line, query) {
				continue
			}
			if len(line) > 200 {
				line = line[:200] + "…"
			}
			hits = append(hits, fmt.Sprintf("%s:%d: %s", rel, i+1, strings.TrimSpace(line)))
			if len(hits) >= maxSearchMatches {
				break
			}
		}
		return nil
	})
	if len(hits) == 0 {
		return "No file contains that."
	}
	return strings.Join(hits, "\n")
}

// investigate lets the model read the app before it answers. It returns the
// transcript of what was looked at, which then goes into the call that asks
// for the structured proposal — so the conclusion is drawn from files that
// were actually read, and donkey can show which ones those were.
func (o *openAI) investigate(ctx context.Context, root, instructions, task string) []responses.ResponseInputItemUnionParam {
	input := []responses.ResponseInputItemUnionParam{
		responses.ResponseInputItemParamOfMessage(task, responses.EasyInputMessageRoleUser),
	}
	ex, err := newExplorer(root)
	if err != nil {
		return input
	}
	defer ex.Close()
	tools := ex.tools()

	for turn := 0; turn < maxExploreTurns; turn++ {
		resp, rerr := o.client.Responses.New(ctx, responses.ResponseNewParams{
			Model:        shared.ResponsesModel(o.model),
			Instructions: openai.String(instructions),
			Input:        responses.ResponseNewParamsInputUnion{OfInputItemList: input},
			Tools:        tools,
		})
		// A failed exploration is not a failed diagnosis: whatever was
		// gathered so far still goes to the answer call.
		if rerr != nil {
			break
		}
		calls := 0
		// The exchange is carried in full rather than by PreviousResponseID:
		// donkey talks to whatever OpenAI-compatible endpoint the operator
		// configured, and server-side conversation state is the part such
		// endpoints most often do not implement.
		for _, item := range resp.Output {
			if item.Type != "function_call" {
				continue
			}
			calls++
			fc := item.AsFunctionCall()
			input = append(input, responses.ResponseInputItemUnionParam{OfFunctionCall: &responses.ResponseFunctionToolCallParam{
				CallID:    fc.CallID,
				Name:      fc.Name,
				Arguments: fc.Arguments,
			}})
			out := ex.call(fc.Name, fc.Arguments)
			if len(out) > maxToolOutput {
				out = out[:maxToolOutput] + "\n…(truncated)"
			}
			input = append(input, responses.ResponseInputItemParamOfFunctionCallOutput(fc.CallID, out))
		}
		if calls == 0 {
			// Nothing more to look at — whatever it said, the answer call is
			// what turns it into a proposal.
			if text := resp.OutputText(); text != "" {
				input = append(input, responses.ResponseInputItemParamOfMessage(text, responses.EasyInputMessageRoleAssistant))
			}
			break
		}
	}
	return input
}
