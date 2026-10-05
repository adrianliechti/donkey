package ai

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// write lays out a repository from a path→content map.
func write(t *testing.T, files map[string]string) string {
	t.Helper()
	dir := t.TempDir()
	for rel, body := range files {
		full := filepath.Join(dir, filepath.FromSlash(rel))
		if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(full, []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	return dir
}

// values flattens signals to their text, for the "did it notice X" assertions.
func values(sigs []Signal) string {
	var b strings.Builder
	for _, s := range sigs {
		b.WriteString(s.Value + " [" + s.Evidence + "]\n")
	}
	return b.String()
}

// The lockfile decides the package manager, and the manifest's own
// packageManager field decides it harder. Getting this wrong means an install
// command that resolves nothing.
func TestPackageManagerPrecedence(t *testing.T) {
	for name, tc := range map[string]struct {
		files map[string]string
		want  string
	}{
		"pnpm lockfile": {
			files: map[string]string{"package.json": `{}`, "pnpm-lock.yaml": ""},
			want:  "Node: pnpm",
		},
		"bun lockfile": {
			files: map[string]string{"package.json": `{}`, "bun.lock": ""},
			want:  "Node: bun",
		},
		"yarn berry via yarnrc": {
			files: map[string]string{"package.json": `{}`, ".yarnrc.yml": "", "yarn.lock": ""},
			want:  "Node: yarn (berry, v2+)",
		},
		"yarn 1 via lockfile alone": {
			files: map[string]string{"package.json": `{}`, "yarn.lock": ""},
			want:  "Node: yarn 1",
		},
		"npm lockfile": {
			files: map[string]string{"package.json": `{}`, "package-lock.json": ""},
			want:  "Node: npm",
		},
		"no lockfile warns against npm ci": {
			files: map[string]string{"package.json": `{}`},
			want:  "no lockfile found",
		},
		"declared packageManager wins": {
			files: map[string]string{
				"package.json": `{"packageManager":"pnpm@9.1.0"}`,
				"yarn.lock":    "",
			},
			want: "pnpm@9.1.0 (declared, wins over lockfiles)",
		},
		"devEngines names the manager as an object": {
			files: map[string]string{
				"package.json": `{"devEngines":{"packageManager":{"name":"pnpm","version":"10.4.1"}}}`,
			},
			want: "Node: pnpm@10.4.1 (declared in devEngines, wins over lockfiles)",
		},
		"devEngines names the manager in a list": {
			files: map[string]string{
				"package.json": `{"devEngines":{"packageManager":[{"name":"bun"}]}}`,
				"yarn.lock":    "",
			},
			want: "Node: bun (declared in devEngines, wins over lockfiles)",
		},
		"uv lock": {
			files: map[string]string{"pyproject.toml": "", "uv.lock": ""},
			want:  "Python: uv",
		},
		"poetry lock": {
			files: map[string]string{"pyproject.toml": "", "poetry.lock": ""},
			want:  "Python: poetry",
		},
		"plain pip": {
			files: map[string]string{"requirements.txt": "flask\n"},
			want:  "Python: pip",
		},
	} {
		t.Run(name, func(t *testing.T) {
			got := values(Scan(write(t, tc.files)).PackageManager)
			if !strings.Contains(got, tc.want) {
				t.Errorf("want a signal containing %q, got:\n%s", tc.want, got)
			}
		})
	}
}

// A lockfile that predates an edit to package.json makes `npm ci` fail before
// it installs anything, and the build log only says the two disagree. The
// scanner reads the lockfile's own record of the manifest and says which
// dependencies it is missing.
func TestNpmLockfileOutOfSync(t *testing.T) {
	const lock = `{"name":"app","lockfileVersion":3,"packages":{"":{"dependencies":{"dayjs":"1.11.13"},"devDependencies":{"vite":"^7"}},"node_modules/dayjs":{"version":"1.11.13"}}}`
	for name, tc := range map[string]struct {
		pkg, lock string
		want      string // empty: the lockfile must not be called out of sync
	}{
		"a dependency added without reinstalling": {
			pkg:  `{"dependencies":{"dayjs":"1.11.13","is-number":"7.0.0"},"devDependencies":{"vite":"^7"}}`,
			lock: lock,
			want: "does not record is-number",
		},
		"a missing devDependency counts too": {
			pkg:  `{"dependencies":{"dayjs":"1.11.13"},"devDependencies":{"vite":"^7","vitest":"^3"}}`,
			lock: lock,
			want: "does not record vitest",
		},
		"in sync": {
			pkg:  `{"dependencies":{"dayjs":"1.11.13"},"devDependencies":{"vite":"^7"}}`,
			lock: lock,
		},
		// Removing a dependency leaves an extra entry behind, which npm ci
		// tolerates.
		"a removed dependency is not a conflict": {
			pkg:  `{"dependencies":{"dayjs":"1.11.13"}}`,
			lock: lock,
		},
		// npm 6 lockfiles have no packages[""] entry to compare against.
		"a v1 lockfile is not judged": {
			pkg:  `{"dependencies":{"dayjs":"1.11.13","is-number":"7.0.0"}}`,
			lock: `{"name":"app","lockfileVersion":1,"dependencies":{"dayjs":{"version":"1.11.13"}}}`,
		},
		"an unreadable lockfile is not judged": {
			pkg:  `{"dependencies":{"dayjs":"1.11.13"}}`,
			lock: `{"packages":{"": `,
		},
	} {
		t.Run(name, func(t *testing.T) {
			got := values(Scan(write(t, map[string]string{
				"package.json":      tc.pkg,
				"package-lock.json": tc.lock,
			})).PackageManager)
			if !strings.Contains(got, "Node: npm") {
				t.Fatalf("npm should still be the package manager:\n%s", got)
			}
			if tc.want == "" {
				if strings.Contains(got, "out of sync") {
					t.Errorf("the lockfile should not be called out of sync:\n%s", got)
				}
				return
			}
			if !strings.Contains(got, tc.want) || !strings.Contains(got, "use `npm install`") {
				t.Errorf("want %q and the npm install advice, got:\n%s", tc.want, got)
			}
		})
	}
}

// devEngines is free-form enough that a manifest can carry a shape npm would
// reject. That must cost the devEngines signal, not everything else in the
// file.
func TestMalformedDevEnginesKeepsTheManifest(t *testing.T) {
	got := Scan(write(t, map[string]string{
		"package.json": `{"scripts":{"start":"node app.js"},"devEngines":{"packageManager":"pnpm","runtime":[1,2]}}`,
	}))
	if !strings.Contains(values(got.StartCommands), "node app.js") {
		t.Errorf("the start script should survive a malformed devEngines:\n%s", values(got.StartCommands))
	}
	if strings.Contains(values(got.PackageManager), "devEngines") {
		t.Errorf("a malformed devEngines should not be reported:\n%s", values(got.PackageManager))
	}
}

// Version pins are scattered across a dozen conventions and each one names a
// different file. Every hit has to carry the file it came from, because the
// files disagree and only the model can weigh them.
func TestVersionPins(t *testing.T) {
	dir := write(t, map[string]string{
		".nvmrc":         "20.11.0\n",
		"package.json":   `{"engines":{"node":">=22"},"devEngines":{"runtime":{"name":"node","version":"22.11.0"}}}`,
		"go.mod":         "module x\n\ngo 1.23.4\n",
		"pyproject.toml": `requires-python = ">=3.11"`,
		".tool-versions": "python 3.12.1\nruby 3.3.0\n",
		"Gemfile":        "source 'https://rubygems.org'\nruby '3.3.0'\n",
	})
	got := values(Scan(dir).Versions)
	for _, want := range []string{
		"node 20.11.0 [.nvmrc]",
		"node >=22 [package.json > engines.node]",
		"node 22.11.0 [package.json > devEngines.runtime]",
		"go 1.23.4 [go.mod]",
		"python >=3.11 [pyproject.toml > requires-python]",
		"python 3.12.1 [.tool-versions]",
		"ruby 3.3.0 [.tool-versions]",
		"ruby 3.3.0 [Gemfile]",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("missing %q in:\n%s", want, got)
		}
	}
}

// Static site detection is the highest-value and highest-risk call the scanner
// makes: right, and the image is a web server holding a folder; wrong, and
// either a server app gets flattened to files or a static site ships a whole
// Node runtime.
func TestStaticSiteDetection(t *testing.T) {
	for name, tc := range map[string]struct {
		files      map[string]string
		wantFrame  string
		wantOutDir string
		wantNone   bool
		wantCustom string
	}{
		"vite default output": {
			files: map[string]string{
				"package.json":   `{"devDependencies":{"vite":"^5"}}`,
				"vite.config.ts": `export default { plugins: [] }`,
			},
			wantFrame: "Vite", wantOutDir: "dist",
		},
		"vite with a moved output directory": {
			files: map[string]string{
				"package.json":   `{"devDependencies":{"vite":"^5"}}`,
				"vite.config.ts": "export default {\n  build: { outDir: 'www' }\n}",
			},
			wantFrame: "Vite", wantOutDir: "www",
		},
		"vite but the author wrote a server": {
			files: map[string]string{
				"package.json":   `{"devDependencies":{"vite":"^5"},"scripts":{"start":"node server.js"}}`,
				"vite.config.ts": "export default {}",
			},
			wantFrame: "Vite", wantOutDir: "dist", wantCustom: "node server.js",
		},
		"next without export is a server": {
			files: map[string]string{
				"package.json":   `{"dependencies":{"next":"^14"}}`,
				"next.config.js": "module.exports = {}",
			},
			wantNone: true,
		},
		"next with export is static": {
			files: map[string]string{
				"package.json":   `{"dependencies":{"next":"^14"}}`,
				"next.config.js": "module.exports = { output: 'export' }",
			},
			wantFrame: "Next.js (static export)", wantOutDir: "out",
		},
		// Every one of these carries vite as a direct dependency, which is the
		// trap: recognizing the framework and concluding "server" has to STOP
		// the search, or the generic Vite rule below flattens a server app
		// into a folder of files.
		"react router defaults to ssr, so not static": {
			files: map[string]string{
				"package.json":           `{"devDependencies":{"@react-router/dev":"^7","vite":"^5"}}`,
				"react-router.config.ts": "export default { }",
			},
			wantNone: true,
		},
		"react router with no config at all defaults to ssr": {
			files: map[string]string{
				"package.json": `{"devDependencies":{"@react-router/dev":"^7","vite":"^5"}}`,
			},
			wantNone: true,
		},
		"astro server output is not rescued by its vite dependency": {
			files: map[string]string{
				"package.json":     `{"dependencies":{"astro":"^4","vite":"^5"}}`,
				"astro.config.mjs": "export default { output: 'server' }",
			},
			wantNone: true,
		},
		"next without export is not rescued by its vite dependency": {
			files: map[string]string{
				"package.json":   `{"dependencies":{"next":"^14","vite":"^5"}}`,
				"next.config.js": "module.exports = {}",
			},
			wantNone: true,
		},
		"react router with ssr off is static": {
			files: map[string]string{
				"package.json":           `{"devDependencies":{"@react-router/dev":"^7","vite":"^5"}}`,
				"react-router.config.ts": "export default { ssr: false }",
			},
			wantFrame: "React Router (SPA mode)", wantOutDir: "build/client",
		},
		"astro server output is not static": {
			files: map[string]string{
				"package.json":     `{"dependencies":{"astro":"^4"}}`,
				"astro.config.mjs": "export default { output: 'server' }",
			},
			wantNone: true,
		},
		"astro static": {
			files: map[string]string{
				"package.json":     `{"dependencies":{"astro":"^4"}}`,
				"astro.config.mjs": "export default { }",
			},
			wantFrame: "Astro (static)", wantOutDir: "dist",
		},
		"nuxt always emits a server": {
			files: map[string]string{
				"package.json": `{"dependencies":{"nuxt":"^3","vite":"^5"}}`,
			},
			wantNone: true,
		},
		// railpack serves this one as a static site and suggests moving the
		// dependency. The build output says otherwise: dist/client and
		// dist/server, and no index.html at the top of either.
		"tanstack start as a devDependency still builds a server": {
			files: map[string]string{
				"package.json":   `{"dependencies":{"react":"^19"},"devDependencies":{"@tanstack/react-start":"^1","vite":"^7"}}`,
				"vite.config.ts": "export default { plugins: [tanstackStart()] }",
			},
			wantNone: true,
		},
		"remix always emits a server": {
			files: map[string]string{
				"package.json": `{"dependencies":{"@remix-run/node":"^2","vite":"^5"}}`,
			},
			wantNone: true,
		},
		"create react app": {
			files: map[string]string{
				"package.json": `{"dependencies":{"react-scripts":"5.0.1"}}`,
			},
			wantFrame: "Create React App", wantOutDir: "build",
		},
		"sveltekit needs the static adapter": {
			files: map[string]string{
				"package.json":     `{"devDependencies":{"@sveltejs/kit":"^2","vite":"^5"}}`,
				"svelte.config.js": "import adapter from '@sveltejs/adapter-node';",
			},
			wantNone: true,
		},
		"sveltekit with adapter-static": {
			files: map[string]string{
				"package.json":     `{"devDependencies":{"@sveltejs/kit":"^2","vite":"^5"}}`,
				"svelte.config.js": "import adapter from '@sveltejs/adapter-static';",
			},
			wantFrame: "SvelteKit (static adapter)", wantOutDir: "build",
		},
		"plain html with no manifest": {
			files:     map[string]string{"index.html": "<html></html>"},
			wantFrame: "plain HTML", wantOutDir: ".",
		},
	} {
		t.Run(name, func(t *testing.T) {
			got := Scan(write(t, tc.files)).Static
			if tc.wantNone {
				if got != nil {
					t.Fatalf("expected no static site, got %+v", got)
				}
				return
			}
			if got == nil {
				t.Fatal("expected a static site, got none")
			}
			if got.Framework != tc.wantFrame {
				t.Errorf("framework = %q, want %q", got.Framework, tc.wantFrame)
			}
			if got.OutputDir != tc.wantOutDir {
				t.Errorf("output dir = %q, want %q", got.OutputDir, tc.wantOutDir)
			}
			if got.CustomStart != tc.wantCustom {
				t.Errorf("custom start = %q, want %q", got.CustomStart, tc.wantCustom)
			}
		})
	}
}

// Angular 17 moved the browser bundle into a browser/ subdirectory. Serving
// the parent gives a visitor a directory listing instead of the app, and the
// build is green either way — so the version has to decide it.
func TestAngularOutputDirectoryDependsOnVersion(t *testing.T) {
	const angularJSON = `{"projects":{"app":{"architect":{"build":{"options":{"outputPath":"dist/app"}}}}}}`
	for spec, want := range map[string]string{
		"^17.0.0": "dist/app/browser",
		"^19.2.1": "dist/app/browser",
		"~16.2.0": "dist/app",
		"15.0.0":  "dist/app",
	} {
		dir := write(t, map[string]string{
			"package.json": `{"dependencies":{"@angular/core":"` + spec + `"}}`,
			"angular.json": angularJSON,
		})
		got := Scan(dir).Static
		if got == nil || got.OutputDir != want {
			t.Errorf("@angular/core %s: want %s, got %+v", spec, want, got)
		}
	}
}

// These are the failures that cost a whole build to discover, and whose error
// messages never name the missing package.
func TestSystemPackages(t *testing.T) {
	dir := write(t, map[string]string{
		"requirements.txt": "psycopg2==2.9.9\nPillow\nlxml\nweasyprint\n",
	})
	got := Scan(dir).SystemPackages
	found := map[string]SystemPackage{}
	for _, p := range got {
		found[p.Dependency] = p
	}
	psy, ok := found["psycopg2"]
	if !ok {
		t.Fatalf("psycopg2 not flagged, got %+v", got)
	}
	if !strings.Contains(strings.Join(psy.Build, " "), "libpq-dev") {
		t.Errorf("psycopg2 should need libpq-dev to build, got %v", psy.Build)
	}
	// The runtime package differs from the build one, which is the whole
	// point: an image with only libpq-dev in a discarded stage dies on import.
	if !strings.Contains(strings.Join(psy.Runtime, " "), "libpq5") {
		t.Errorf("psycopg2 should need libpq5 at runtime, got %v", psy.Runtime)
	}
	for _, dep := range []string{"pillow", "lxml", "weasyprint"} {
		if _, ok := found[dep]; !ok {
			t.Errorf("%s not flagged", dep)
		}
	}
}

func TestNodeSystemPackagesAreNotMatchedAgainstPython(t *testing.T) {
	// "canvas" and "sqlite3" appear in Python projects too, under different
	// meanings. A Node rule must only fire on a Node dependency list.
	dir := write(t, map[string]string{"requirements.txt": "canvas\nsqlite3\n"})
	for _, p := range Scan(dir).SystemPackages {
		if p.Dependency == "canvas" || p.Dependency == "sqlite3" {
			t.Errorf("node rule %q fired on a Python project", p.Dependency)
		}
	}
}

// A Procfile is the author stating the start command outright, so it outranks
// everything the scanner could infer.
func TestStartCommandOrder(t *testing.T) {
	dir := write(t, map[string]string{
		"Procfile":     "web: gunicorn app:server --bind 0.0.0.0:$PORT\nworker: celery -A app worker\n",
		"package.json": `{"scripts":{"start":"node dist/main.js"},"main":"index.js"}`,
	})
	got := Scan(dir).StartCommands
	if len(got) == 0 {
		t.Fatal("no start commands found")
	}
	if !strings.Contains(got[0].Value, "gunicorn app:server") {
		t.Errorf("the Procfile web line should come first, got %q", got[0].Value)
	}
	if !strings.Contains(values(got), "node dist/main.js") {
		t.Errorf("the start script should still be listed:\n%s", values(got))
	}
}

// TanStack Start has no start script by default and its server lands where no
// generic rule looks, so the scanner names it — and says when a devDependency
// would take the server away with the pruned dev dependencies.
func TestTanstackStartCommand(t *testing.T) {
	for name, tc := range map[string]struct {
		pkg   string
		want  string
		avoid string
	}{
		"srvx serves the default build": {
			pkg:   `{"dependencies":{"@tanstack/react-start":"^1"}}`,
			want:  "npx srvx --prod -s ../client dist/server/server.js",
			avoid: "devDependency",
		},
		"nitro builds its own server": {
			pkg:   `{"dependencies":{"@tanstack/react-start":"^1","nitro":"^3"}}`,
			want:  "node .output/server/index.mjs",
			avoid: "srvx",
		},
		"a devDependency has to survive into the runtime image": {
			pkg:  `{"devDependencies":{"@tanstack/react-start":"^1","vite":"^7"}}`,
			want: "only a devDependency, and srvx arrives with it",
		},
	} {
		t.Run(name, func(t *testing.T) {
			got := values(Scan(write(t, map[string]string{"package.json": tc.pkg})).StartCommands)
			if !strings.Contains(got, tc.want) {
				t.Errorf("want %q in:\n%s", tc.want, got)
			}
			if tc.avoid != "" && strings.Contains(got, tc.avoid) {
				t.Errorf("did not want %q in:\n%s", tc.avoid, got)
			}
		})
	}
}

func TestDjangoIsRecognized(t *testing.T) {
	dir := write(t, map[string]string{
		"manage.py":        "#!/usr/bin/env python\n",
		"requirements.txt": "Django==5.0\ngunicorn\npsycopg2-binary\n",
		"blog/settings.py": "ALLOWED_HOSTS = []\n",
	})
	s := Scan(dir)
	if !strings.Contains(values(s.Frameworks), "Django") {
		t.Errorf("Django not detected:\n%s", values(s.Frameworks))
	}
	// runserver is a development server and must never be the start command.
	if !strings.Contains(values(s.StartCommands), "runserver is a development server") {
		t.Errorf("the runserver warning should be present:\n%s", values(s.StartCommands))
	}
}

func TestWorkspaceLayout(t *testing.T) {
	dir := write(t, map[string]string{
		"package.json":           `{"workspaces":["apps/*"]}`,
		"pnpm-workspace.yaml":    "packages:\n  - apps/*\n",
		"turbo.json":             "{}",
		"apps/web/package.json":  `{}`,
		"apps/api/package.json":  `{}`,
		"apps/docs/package.json": `{}`,
	})
	got := values(Scan(dir).Workspace)
	for _, want := range []string{"pnpm workspace", "Turborepo", "npm/yarn workspaces", "may not be at the root"} {
		if !strings.Contains(got, want) {
			t.Errorf("missing %q in:\n%s", want, got)
		}
	}
}

// An existing Dockerfile is the author having already solved this. Never
// noticing it is the worst possible outcome.
func TestExistingBuildFilesAreSurfaced(t *testing.T) {
	dir := write(t, map[string]string{
		"Dockerfile":         "FROM node:22\n",
		"docker-compose.yml": "services:\n  web:\n    ports: ['3000:3000']\n",
		".dockerignore":      "node_modules\n",
		"package.json":       `{}`,
	})
	got := values(Scan(dir).BuildFiles)
	for _, want := range []string{"existing Dockerfile", "compose file", ".dockerignore"} {
		if !strings.Contains(got, want) {
			t.Errorf("missing %q in:\n%s", want, got)
		}
	}
}

// The scanner runs on every AI build and must never be the thing that fails
// one.
func TestScanSurvivesBrokenInput(t *testing.T) {
	dir := write(t, map[string]string{
		"package.json":   `{"scripts": {"start": }`, // invalid JSON
		"pyproject.toml": "\x00\x01 not toml",
		"angular.json":   "",
	})
	s := Scan(dir)
	if s == nil {
		t.Fatal("Scan returned nil")
	}
	// A manifest that will not parse is still proof of a Node app.
	if !strings.Contains(values(s.Runtimes), "Node.js") {
		t.Errorf("an unparseable package.json should still mean Node:\n%s", values(s.Runtimes))
	}
	_ = s.Render()
}

func TestScanOnEmptyDirectory(t *testing.T) {
	s := Scan(t.TempDir())
	if !s.IsEmpty() {
		t.Errorf("an empty folder should produce no signals, got:\n%s", s.Render())
	}
}

// Render is what actually reaches the model, so the facts have to survive it.
func TestRenderCarriesEvidence(t *testing.T) {
	dir := write(t, map[string]string{
		"package.json":     `{"dependencies":{"astro":"^4"}}`,
		"pnpm-lock.yaml":   "",
		"astro.config.mjs": "export default {}",
		".nvmrc":           "20\n",
	})
	out := Scan(dir).Render()
	for _, want := range []string{
		"pnpm-lock.yaml", "node 20", ".nvmrc", "Astro", "./dist",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("Render() dropped %q:\n%s", want, out)
		}
	}
}
