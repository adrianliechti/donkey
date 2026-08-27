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

// Investigating an app is a search, not a lookup: the trace names one file,
// that file imports another, and the setting that broke the deploy is a third
// nobody mentioned. So the assistant reads the app itself, within hard limits:
//
//   - read-only tools. No shell, no write, no network.
//   - containment is the OS's job: every path goes through an *os.Root on the
//     upload, which refuses ".." and escaping symlinks at the syscall level —
//     no string check to get wrong, no check-then-use window. The upload is a
//     temp copy, never the user's working tree.
//   - the same deniedFile/skipDirs exclusions as everywhere else.
//   - a turn budget, and a separate call for the structured answer, so tool
//     use and strict JSON never mix.
const (
	// maxExploreTurns bounds a diagnosis, which starts from a stack trace and
	// only has to follow it.
	maxExploreTurns = 6
	// maxBuildExploreTurns bounds writing a Dockerfile, which starts from
	// nothing and has to establish the toolchain, the install, the build and
	// the start command. It is the difference between a model that reads the
	// vite config and one that guesses the output directory.
	maxBuildExploreTurns = 14
	maxToolOutput        = 12 * 1024
	maxListEntries       = 300
	maxSearchMatches     = 50
	maxFindMatches       = 100
	maxReadFileLines     = 500
	maxSearchFileSize    = 512 * 1024
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
	// Strict function schemas require every property to be listed in
	// `required`, so optional arguments are declared nullable instead and an
	// empty value means "not given".
	schema := func(props map[string]any, required []string) map[string]any {
		return map[string]any{
			"type":                 "object",
			"properties":           props,
			"required":             required,
			"additionalProperties": false,
		}
	}
	tools := []responses.ToolUnionParam{
		responses.ToolParamOfFunction("list_files", schema(map[string]any{
			"path": str("Folder to list, relative to the app root. Empty string for the root."),
		}, []string{"path"}), true),

		responses.ToolParamOfFunction("read_file", schema(map[string]any{
			"path": str("File to read, relative to the app root."),
		}, []string{"path"}), true),

		responses.ToolParamOfFunction("search_files", schema(map[string]any{
			"query": str("Literal text to look for (not a regular expression)."),
			"path":  str("Optional subfolder to search in. Empty string searches the whole app."),
		}, []string{"query", "path"}), true),

		responses.ToolParamOfFunction("find_files", schema(map[string]any{
			"pattern": str("Filename glob, matched against the file name only: '*.csproj', 'vite.config.*', 'Dockerfile*', '*.tsx'."),
		}, []string{"pattern"}), true),
	}
	for i, desc := range []string{
		"List files in the app, optionally under a subfolder. Start here when you don't know the layout.",
		"Read one file from the app, with line numbers.",
		"Find a literal string across the app's files. Use it to locate a listen port, a route, a setting or a symbol when you don't know which file holds it.",
		"Find files by name pattern anywhere in the app, shallowest first. Use it to locate a framework config, a project file or an entry point without listing the whole tree.",
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
		Path    string `json:"path"`
		Query   string `json:"query"`
		Pattern string `json:"pattern"`
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
		return e.search(a.Query, a.Path)
	case "find_files":
		return e.find(a.Pattern)
	default:
		return "There is no tool called " + name + "."
	}
}

// find locates files by name pattern. It is the tool that makes exploration
// cheap on a repository nobody has seen: "where is the vite config" is one
// call instead of a walk the model has to read.
func (e *explorer) find(pattern string) string {
	pattern = strings.TrimSpace(pattern)
	if pattern == "" {
		return "Give a filename pattern, for example '*.csproj'."
	}
	// A pattern with a slash is a path pattern; match it against the whole
	// relative path rather than the base name.
	full := strings.Contains(pattern, "/")
	if _, err := path.Match(pattern, "probe"); err != nil {
		return "That is not a valid pattern: " + err.Error()
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
		if len(hits) >= maxFindMatches {
			return fs.SkipAll
		}
		if deniedFile(d.Name()) {
			return nil
		}
		target := d.Name()
		if full {
			target = p
		}
		if ok, _ := path.Match(pattern, target); ok {
			hits = append(hits, p)
		}
		return nil
	})
	if len(hits) == 0 {
		return "No file matches " + pattern + "."
	}
	// Shallowest first: a manifest at the root almost always outranks the same
	// name buried in a fixture or an example folder.
	sort.SliceStable(hits, func(i, j int) bool {
		di, dj := strings.Count(hits[i], "/"), strings.Count(hits[j], "/")
		if di != dj {
			return di < dj
		}
		return hits[i] < hits[j]
	})
	out := strings.Join(hits, "\n")
	if len(hits) >= maxFindMatches {
		out += "\n…(more matches not shown — narrow the pattern)"
	}
	return out
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

// search finds a literal string, optionally under one subfolder. Scoping
// matters on a monorepo: the same import appears in a dozen packages and only
// the one being built is the answer.
func (e *explorer) search(query, scope string) string {
	if query == "" {
		return "Give something to search for."
	}
	root := fsPath(scope)
	if info, err := e.root.Stat(root); err != nil || !info.IsDir() {
		// A scope that is not a folder is a wrong guess, not a reason to give
		// up: search the whole app and say so.
		root = "."
	}
	var hits []string
	fs.WalkDir(e.fsys, root, func(p string, d fs.DirEntry, err error) error {
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
	out := strings.Join(hits, "\n")
	if len(hits) >= maxSearchMatches {
		out += "\n…(more matches not shown — narrow the search or scope it to a folder)"
	}
	return out
}

// investigate lets the model read the app before it answers. It returns the
// transcript of what was looked at, which then goes into the call that asks
// for the structured proposal — so the conclusion is drawn from files that
// were actually read, and donkey can show which ones those were.
func (o *openAI) investigate(ctx context.Context, root, instructions, task string, maxTurns int) []responses.ResponseInputItemUnionParam {
	input := []responses.ResponseInputItemUnionParam{
		responses.ResponseInputItemParamOfMessage(task, responses.EasyInputMessageRoleUser),
	}
	ex, err := newExplorer(root)
	if err != nil {
		return input
	}
	defer ex.Close()
	tools := ex.tools()

	for turn := 0; turn < maxTurns; turn++ {
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
			toolOutput := responses.ResponseInputItemParamOfFunctionCallOutput(out)
			toolOutput.OfFunctionCallOutput.CallID = openai.String(fc.CallID)
			input = append(input, toolOutput)
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

// exploredFiles lists the files an investigation actually opened, so donkey
// can say what it looked at. A build log that names the four files behind a
// Dockerfile is reviewable; one that just presents the Dockerfile is not.
func exploredFiles(history []responses.ResponseInputItemUnionParam) []string {
	var out []string
	seen := map[string]bool{}
	for _, item := range history {
		fc := item.OfFunctionCall
		if fc == nil || fc.Name != "read_file" {
			continue
		}
		var a struct {
			Path string `json:"path"`
		}
		if json.Unmarshal([]byte(fc.Arguments), &a) != nil || a.Path == "" {
			continue
		}
		p := fsPath(a.Path)
		if !seen[p] {
			seen[p] = true
			out = append(out, p)
		}
	}
	return out
}
