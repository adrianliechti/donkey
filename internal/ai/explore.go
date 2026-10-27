package ai

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"io/fs"
	"os"
	"path"
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
//   - read-only planning; scoped edits only in a disposable build workspace.
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
	maxExploreTurns = 16
	// maxBuildExploreTurns bounds writing a Dockerfile, which starts from
	// nothing and has to establish the toolchain, the install, the build and
	// the start command. It is the difference between a model that reads the
	// vite config and one that guesses the output directory.
	maxBuildExploreTurns = 32
	maxToolOutput        = 48 * 1024
	maxReadFileLines     = 2000
)

// explorer answers tool calls against one folder.
type explorer struct {
	root       *os.Root
	fsys       fs.FS
	writable   bool
	readHashes map[string][32]byte
	originals  map[string]string
}

// newExplorer opens the folder as a root. Everything the assistant can reach
// goes through it.
func newExplorer(dir string) (*explorer, error) {
	root, err := os.OpenRoot(dir)
	if err != nil {
		return nil, err
	}
	return &explorer{root: root, fsys: root.FS(), readHashes: map[string][32]byte{}, originals: map[string]string{}}, nil
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
		responses.ToolParamOfFunction("read", schema(map[string]any{
			"file_path": str("File to read, relative to the uploaded workspace."),
			"offset":    map[string]any{"type": []string{"integer", "null"}, "description": "1-based starting line. Null starts at line 1."},
			"limit":     map[string]any{"type": []string{"integer", "null"}, "description": "Number of lines, up to 2000. Null uses 2000."},
		}, []string{"file_path", "offset", "limit"}), true),
	}
	for i, desc := range []string{
		"Read a file with 1-based line numbers. Use offset and limit to read known ranges; truncated output tells you where to continue.",
	} {
		tools[i].OfFunction.Description = openai.String(desc)
	}
	tools = append(tools, searchTools()...)
	if e.writable {
		tools = append(tools, editTool())
	}
	return tools
}

// call runs one tool. Every failure comes back as text the model can act on:
// an error string that says "no such file" is a useful observation, while a
// hard failure would end an investigation over a wrong guess.
func (e *explorer) call(name, args string) string {
	var a struct {
		Path     string `json:"path"`
		FilePath string `json:"file_path"`
		Offset   *int   `json:"offset"`
		Limit    *int   `json:"limit"`
	}
	if args != "" {
		if err := json.Unmarshal([]byte(args), &a); err != nil {
			return "Could not read those arguments: " + err.Error()
		}
	}
	switch name {
	case "read":
		if a.FilePath != "" {
			a.Path = a.FilePath
		}
		return e.readRange(a.Path, a.Offset, a.Limit)
	case "glob", "grep":
		return e.query(name, args)
	case "edit":
		if !e.writable {
			return "This investigation is read-only."
		}
		return e.edit(args)
	default:
		return "There is no tool called " + name + "."
	}
}

func (e *explorer) read(rel string) string {
	return e.readRange(rel, nil, nil)
}

func (e *explorer) readRange(rel string, offset, limit *int) string {
	if rel == "" {
		return "Give a file path."
	}
	name := fsPath(rel)
	if !readablePath(name) || !e.noSymlinks(name) {
		return "That path is excluded or contains a symlink."
	}
	info, err := e.root.Stat(name)
	if err != nil {
		return "No such file in this app: " + rel
	}
	if info.IsDir() {
		return rel + " is a folder — use glob."
	}
	if !info.Mode().IsRegular() {
		return "Read a regular text file."
	}
	b, err := e.root.ReadFile(name)
	if err != nil {
		return "Could not read " + rel + ": " + err.Error()
	}
	if !utf8.Valid(b) || strings.ContainsRune(string(b), '\x00') {
		return rel + " is not a text file."
	}
	start, count := 1, maxReadFileLines
	if offset != nil {
		start = *offset
	}
	if limit != nil {
		count = *limit
	}
	if start < 1 || count < 1 || count > maxReadFileLines {
		return "offset must be a positive 1-based line number and limit must be between 1 and 2000."
	}
	lines := strings.Split(strings.TrimSuffix(string(b), "\n"), "\n")
	if start > len(lines) {
		return fmt.Sprintf("This file has %d lines; offset %d is beyond its end.", len(lines), start)
	}
	var sb strings.Builder
	end := min(len(lines), start-1+count)
	next := start
	for i := start - 1; i < end; i++ {
		line := fmt.Sprintf("%d\t%s\n", i+1, lines[i])
		if sb.Len()+len(line) > maxToolOutput-200 {
			if sb.Len() == 0 {
				return fmt.Sprintf("Line %d exceeds the read output limit; use grep for a targeted excerpt.", i+1)
			}
			break
		}
		sb.WriteString(line)
		next = i + 2
	}
	if next <= len(lines) {
		fmt.Fprintf(&sb, "[Showing lines %d-%d of %d; use offset=%d to continue.]\n", start, next-1, len(lines), next)
	}
	e.readHashes[name] = sha256.Sum256(b)
	return sb.String()
}

// investigate lets the model read the app before it answers. It returns the
// transcript of what was looked at, which then goes into the call that asks
// for the structured proposal — so the conclusion is drawn from files that
// were actually read, and donkey can show which ones those were.
type exploreOptions struct {
	Writable     bool
	Progress     func(string)
	Changes      *[]SourceChange
	KnownSecrets []string
	CheckImage   func(context.Context, string) (string, error)
}

func (o *openAI) investigate(ctx context.Context, root, instructions, task string, maxTurns int, options ...exploreOptions) []responses.ResponseInputItemUnionParam {
	input := []responses.ResponseInputItemUnionParam{
		responses.ResponseInputItemParamOfMessage(task, responses.EasyInputMessageRoleUser),
	}
	ex, err := newExplorer(root)
	if err != nil {
		return input
	}
	defer ex.Close()
	var opt exploreOptions
	if len(options) > 0 {
		opt = options[0]
	}
	ex.writable = opt.Writable
	defer func() {
		if opt.Changes != nil {
			*opt.Changes = ex.changes()
		}
	}()
	tools := ex.tools()
	if opt.CheckImage != nil {
		tool := responses.ToolParamOfFunction("check_image", map[string]any{
			"type": "object", "properties": map[string]any{"image": map[string]any{"type": "string", "description": "Candidate base image reference, with a tag or digest."}},
			"required": []string{"image"}, "additionalProperties": false,
		}, true)
		tool.OfFunction.Description = openai.String("Resolve and scan a base image using the build engine's mirrors, target platform and current vulnerability database. Returns its exact digest, declared runtime versions, OS support status and blocking package versions. Checks are bounded and cached for this deployment; this does not approve the final image.")
		tools = append(tools, tool)
	}

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
			if item.Type == "reasoning" {
				reasoning := item.AsReasoning().ToParam()
				input = append(input, responses.ResponseInputItemUnionParam{OfReasoning: &reasoning})
				continue
			}
			if item.Type == "message" {
				message := item.AsMessage().ToParam()
				input = append(input, responses.ResponseInputItemUnionParam{OfOutputMessage: &message})
				continue
			}
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
			var out string
			if fc.Name == "check_image" && opt.CheckImage != nil {
				var args struct {
					Image string `json:"image"`
				}
				if err := json.Unmarshal([]byte(fc.Arguments), &args); err != nil || args.Image == "" {
					out = "Supply a valid image reference."
				} else {
					if opt.Progress != nil {
						opt.Progress("Checking base image " + RedactEvidence(args.Image, opt.KnownSecrets...))
					}
					var err error
					out, err = opt.CheckImage(ctx, args.Image)
					if err != nil {
						out = "Image check unavailable: " + err.Error()
					}
				}
			} else {
				out = ex.call(fc.Name, fc.Arguments)
			}
			if opt.Progress != nil {
				var args struct {
					Path     string `json:"path"`
					FilePath string `json:"file_path"`
				}
				_ = json.Unmarshal([]byte(fc.Arguments), &args)
				if args.FilePath != "" {
					args.Path = args.FilePath
				}
				if fc.Name == "edit" {
					opt.Progress(out)
				} else if args.Path != "" {
					opt.Progress(fc.Name + ": " + args.Path)
				}
			}
			if len(out) > maxToolOutput {
				out = out[:maxToolOutput] + "\n…(truncated)"
			}
			if opt.Writable || len(opt.KnownSecrets) > 0 {
				out = RedactEvidence(out, opt.KnownSecrets...)
			}
			toolOutput := responses.ResponseInputItemParamOfFunctionCallOutput(out)
			toolOutput.OfFunctionCallOutput.CallID = openai.String(fc.CallID)
			input = append(input, toolOutput)
		}
		if calls == 0 {
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
		if fc == nil || fc.Name != "read" {
			continue
		}
		var a struct {
			Path     string `json:"path"`
			FilePath string `json:"file_path"`
		}
		if json.Unmarshal([]byte(fc.Arguments), &a) != nil {
			continue
		}
		if a.FilePath != "" {
			a.Path = a.FilePath
		}
		if a.Path == "" {
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
