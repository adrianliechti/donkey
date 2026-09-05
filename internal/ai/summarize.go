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
	// Signals is what donkey established deterministically. It goes first in
	// the rendered prompt: it is the part that is not a guess.
	Signals *Signals
}

const (
	maxTreeEntries = 600
	maxFileBytes   = 8 * 1024
	// The summary is the model's first look at the app, and it can read
	// anything it wants afterwards with its own tools — but what it never sees
	// it never thinks to ask about. The budget is set to fit a real repository
	// rather than to be small.
	maxTotalBytes = 96 * 1024
)

// skipDirs never get walked.
var skipDirs = map[string]bool{
	".git": true, "node_modules": true, "vendor": true, ".venv": true, "venv": true,
	"__pycache__": true, "dist": true, "build": true, "target": true, ".next": true,
	".donkey": true, ".idea": true, ".vscode": true, ".nuxt": true, ".svelte-kit": true,
	".turbo": true, ".pytest_cache": true, ".mypy_cache": true, ".gradle": true,
	".terraform": true, "bin": true, "obj": true, "coverage": true, ".yarn": true,
}

// manifestFiles get their content included (first maxFileBytes each). The set
// is wide on purpose: every entry changes what a correct build looks like, and
// leaving one out costs a whole failed attempt — a missing .nvmrc is the wrong
// Node, a missing pnpm-workspace.yaml an install that resolves nothing.
var manifestFiles = map[string]bool{
	// Dependency manifests.
	"package.json": true, "package.json5": true, "requirements.txt": true,
	"requirements.in": true, "pyproject.toml": true, "Pipfile": true, "setup.py": true,
	"setup.cfg": true, "go.mod": true, "go.work": true, "Gemfile": true,
	"composer.json": true, "pom.xml": true, "build.gradle": true,
	"build.gradle.kts": true, "settings.gradle": true, "settings.gradle.kts": true,
	"Cargo.toml": true, "mix.exs": true, "gleam.toml": true, "deno.json": true,
	"deno.jsonc": true, "pubspec.yaml": true, "CMakeLists.txt": true, "meson.build": true,

	// Toolchain version pins. Small files, and each one is the difference
	// between the right interpreter and a build that fails on syntax.
	".nvmrc": true, ".node-version": true, ".python-version": true,
	"runtime.txt": true, ".ruby-version": true, ".tool-versions": true,
	"mise.toml": true, ".mise.toml": true, "rust-toolchain.toml": true,
	"rust-toolchain": true, ".sdkmanrc": true,

	// Workspace and monorepo layout — which package is even the app.
	"pnpm-workspace.yaml": true, "nx.json": true, "turbo.json": true,
	"lerna.json": true, "rush.json": true, "bunfig.toml": true, ".yarnrc.yml": true,
	".npmrc": true,

	// Framework configuration. These decide the output directory of a static
	// build, whether a framework emits a server, and where the entry point is.
	"next.config.js": true, "next.config.mjs": true, "next.config.ts": true,
	"nuxt.config.ts": true, "nuxt.config.js": true,
	"astro.config.mjs": true, "astro.config.ts": true, "astro.config.js": true,
	"svelte.config.js": true, "react-router.config.ts": true,
	"remix.config.js": true, "angular.json": true, "nest-cli.json": true,
	"app.json": true, "expo.json": true, "quasar.config.js": true,
	"gatsby-config.js": true, "docusaurus.config.js": true,

	// Build and run instructions the author already wrote.
	"Procfile": true, "Makefile": true, "Taskfile.yml": true, "justfile": true,
	"Dockerfile": true, "docker-compose.yml": true, "docker-compose.yaml": true,
	"compose.yml": true, "compose.yaml": true, ".dockerignore": true,
	"donkey.yaml": true, "railpack.json": true, "nixpacks.toml": true,
	"Caddyfile": true, "Staticfile": true, "manage.py": true, "config.ru": true,
	"artisan": true, "start.sh": true, "entrypoint.sh": true, "run.sh": true,

	// Application settings that pin ports, hosts and health routes.
	"application.properties": true, "application.yml": true, "application.yaml": true,
	"README.md": true, "readme.md": true, "tsconfig.json": true,
}

// manifestGlobs catch the manifests whose names are not fixed.
var manifestGlobs = []string{"*.csproj", "*.fsproj", "*.sln", "requirements-*.txt"}

// lockFiles are listed in the tree but never sent: which one exists is the
// signal, and their contents are megabytes of noise.
var lockFiles = map[string]bool{
	"package-lock.json": true, "pnpm-lock.yaml": true, "yarn.lock": true,
	"bun.lockb": true, "bun.lock": true, "poetry.lock": true, "uv.lock": true,
	"Pipfile.lock": true, "pdm.lock": true, "Gemfile.lock": true,
	"composer.lock": true, "Cargo.lock": true, "mix.lock": true,
	"go.sum": true, "packages.lock.json": true,
}

// entrypointStems/sourceExts include likely server entry points — that's
// where listen ports and health routes live.
var entrypointStems = map[string]bool{
	"main": true, "app": true, "server": true, "index": true, "cli": true,
	"wsgi": true, "asgi": true, "manage": true, "worker": true, "bot": true,
	"run": true, "start": true, "settings": true, "application": true, "program": true,
}
var sourceExts = map[string]bool{
	".py": true, ".js": true, ".ts": true, ".mjs": true, ".cjs": true,
	".go": true, ".rb": true, ".php": true, ".rs": true, ".ex": true,
	".exs": true, ".cs": true, ".java": true, ".kt": true,
}

// interestingFile decides whether a file's content belongs in the summary.
func interestingFile(base string) bool {
	if manifestFiles[base] {
		return true
	}
	// A lockfile's existence is the signal — which package manager is in use.
	// Its contents are megabytes that say nothing more.
	if lockFiles[base] {
		return false
	}
	for _, g := range manifestGlobs {
		if ok, _ := filepath.Match(g, base); ok {
			return true
		}
	}
	ext := strings.ToLower(filepath.Ext(base))
	stem := strings.ToLower(strings.TrimSuffix(base, filepath.Ext(base)))
	if entrypointStems[stem] && sourceExts[ext] {
		return true
	}
	// Framework configs often pin ports and output directories
	// (vite.config.ts, next.config.js, …).
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

// Summarize caps the tree listing but collects manifests and entry points
// wherever the walk finds them — hundreds of assets in an early folder must
// not push out package.json. Contents go in by rank until the budget is spent.
func Summarize(dir string) (*RepoSummary, error) {
	type candidate struct {
		rel   string
		path  string
		depth int
		// rank orders candidates: 0 = manifest, 1 = entry point / config.
		rank int
	}
	var tree []string
	var candidates []candidate
	skipped := 0

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
			return nil
		}
		if len(tree) < maxTreeEntries {
			tree = append(tree, filepath.ToSlash(rel))
		} else {
			skipped++
		}
		base := d.Name()
		if interestingFile(base) && !deniedFile(base) {
			rank := 1
			if manifestFiles[base] {
				rank = 0
			}
			candidates = append(candidates, candidate{
				rel:   filepath.ToSlash(rel),
				path:  path,
				depth: strings.Count(rel, string(filepath.Separator)),
				rank:  rank,
			})
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	sort.Strings(tree)
	if skipped > 0 {
		tree = append(tree, fmt.Sprintf("… and %d more files", skipped))
	}

	sort.SliceStable(candidates, func(i, j int) bool {
		if candidates[i].rank != candidates[j].rank {
			return candidates[i].rank < candidates[j].rank
		}
		if candidates[i].depth != candidates[j].depth {
			return candidates[i].depth < candidates[j].depth
		}
		return candidates[i].rel < candidates[j].rel
	})

	files := map[string]string{}
	total := 0
	for _, c := range candidates {
		if total >= maxTotalBytes {
			break
		}
		b, err := os.ReadFile(c.path)
		if err != nil {
			continue
		}
		content := string(b)
		if len(content) > maxFileBytes {
			content = content[:maxFileBytes] + "\n…(truncated)"
		}
		if total+len(content) <= maxTotalBytes {
			files[c.rel] = content
			total += len(content)
		}
	}
	return &RepoSummary{
		Tree:    strings.Join(tree, "\n"),
		Files:   files,
		Signals: Scan(dir),
	}, nil
}

// Render formats the summary as the prompt's user content. What donkey
// established comes first: it is short, it is not a guess, and it is what the
// rest of the material should be read against.
func (s *RepoSummary) Render() string {
	var b strings.Builder
	if s.Signals != nil && !s.Signals.IsEmpty() {
		b.WriteString(s.Signals.Render())
		b.WriteString("\n")
	}
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
