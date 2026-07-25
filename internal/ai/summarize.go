package ai

import (
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
)

// RepoSummary is the compact, secret-free view of a repository that donkey
// sends to OpenAI.
type RepoSummary struct {
	Tree  string
	Files map[string]string
}

const (
	maxTreeEntries = 400
	maxTreeDepth   = 4
	maxFileBytes   = 4 * 1024
	maxTotalBytes  = 24 * 1024
)

// skipDirs never get walked.
var skipDirs = map[string]bool{
	".git": true, "node_modules": true, "vendor": true, ".venv": true, "venv": true,
	"__pycache__": true, "dist": true, "build": true, "target": true, ".next": true,
	".donkey": true, ".idea": true, ".vscode": true,
}

// manifestFiles get their content included (first maxFileBytes each).
// Dockerfiles and compose files earn their place twice over: they name the
// working directory paths resolve against, and their volumes are the app
// author's own statement about which folders hold data worth keeping.
var manifestFiles = map[string]bool{
	"package.json": true, "requirements.txt": true, "pyproject.toml": true,
	"Pipfile": true, "go.mod": true, "Gemfile": true, "composer.json": true,
	"pom.xml": true, "build.gradle": true, "Cargo.toml": true, "mix.exs": true,
	"Procfile": true, "Makefile": true, "README.md": true, "readme.md": true,
	"Dockerfile": true, "docker-compose.yml": true, "docker-compose.yaml": true,
	"compose.yml": true, "compose.yaml": true, "donkey.yaml": true,
}

// entrypointStems/sourceExts include likely server entry points — that's
// where listen ports and health routes live.
var entrypointStems = map[string]bool{
	"main": true, "app": true, "server": true, "index": true, "cli": true, "wsgi": true, "asgi": true,
}
var sourceExts = map[string]bool{
	".py": true, ".js": true, ".ts": true, ".mjs": true, ".cjs": true,
	".go": true, ".rb": true, ".php": true,
}

// interestingFile decides whether a file's content belongs in the summary.
func interestingFile(base string) bool {
	if manifestFiles[base] {
		return true
	}
	ext := strings.ToLower(filepath.Ext(base))
	stem := strings.ToLower(strings.TrimSuffix(base, filepath.Ext(base)))
	if entrypointStems[stem] && sourceExts[ext] {
		return true
	}
	// Framework configs often pin ports (vite.config.ts, next.config.js, …).
	if strings.HasSuffix(stem, ".config") && sourceExts[ext] {
		return true
	}
	return false
}

// deniedFile matches files whose content must never be sent, regardless of
// any other rule.
func deniedFile(name string) bool {
	lower := strings.ToLower(name)
	if strings.HasPrefix(lower, ".env") || strings.HasPrefix(lower, "id_") {
		return true
	}
	for _, pat := range []string{"secret", "credential", "password", "token"} {
		if strings.Contains(lower, pat) {
			return true
		}
	}
	for _, ext := range []string{".pem", ".key", ".p12", ".pfx", ".crt"} {
		if strings.HasSuffix(lower, ext) {
			return true
		}
	}
	return false
}

// Summarize walks dir and produces the capped summary.
func Summarize(dir string) (*RepoSummary, error) {
	var tree []string
	files := map[string]string{}
	total := 0

	err := filepath.WalkDir(dir, func(path string, d os.DirEntry, err error) error {
		if err != nil {
			return nil
		}
		rel, err := filepath.Rel(dir, path)
		if err != nil || rel == "." {
			return nil
		}
		if d.IsDir() {
			if skipDirs[d.Name()] {
				return filepath.SkipDir
			}
			if strings.Count(rel, string(filepath.Separator)) >= maxTreeDepth {
				return filepath.SkipDir
			}
			return nil
		}
		if len(tree) >= maxTreeEntries {
			return filepath.SkipAll
		}
		tree = append(tree, filepath.ToSlash(rel))

		base := d.Name()
		if interestingFile(base) && !deniedFile(base) && total < maxTotalBytes {
			if b, err := os.ReadFile(path); err == nil {
				content := string(b)
				if len(content) > maxFileBytes {
					content = content[:maxFileBytes] + "\n…(truncated)"
				}
				if total+len(content) <= maxTotalBytes {
					files[filepath.ToSlash(rel)] = content
					total += len(content)
				}
			}
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	sort.Strings(tree)
	return &RepoSummary{Tree: strings.Join(tree, "\n"), Files: files}, nil
}

// Render formats the summary as the prompt's user content.
func (s *RepoSummary) Render() string {
	var b strings.Builder
	b.WriteString("File tree:\n")
	b.WriteString(s.Tree)
	b.WriteString("\n")
	names := make([]string, 0, len(s.Files))
	for n := range s.Files {
		names = append(names, n)
	}
	sort.Strings(names)
	for _, n := range names {
		fmt.Fprintf(&b, "\n--- %s ---\n%s\n", n, s.Files[n])
	}
	return b.String()
}
