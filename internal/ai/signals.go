package ai

import (
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path"
	"path/filepath"
	"regexp"
	"slices"
	"sort"
	"strings"
)

// Signals is what donkey establishes about an app before the model is asked
// anything. The things worth knowing here are not judgement calls: the
// lockfile on disk decides the package manager, .python-version decides the
// interpreter, psycopg2 needs libpq-dev as a matter of fact.
//
// So donkey computes them and hands the model evidence rather than
// conclusions — "pnpm-lock.yaml exists", not "use pnpm". Every signal names
// its file, which is what lets the model overrule one, and what lets a person
// reading the build log see why donkey thought what it thought.
type Signals struct {
	// Runtimes are the language ecosystems found, most-evidenced first.
	Runtimes       []Signal
	PackageManager []Signal
	// Versions may hold more than one entry per tool — that the files disagree
	// is exactly what the model needs to see.
	Versions   []Signal
	Frameworks []Signal
	// Static is the most misbuilt app shape: the right answer is a web server
	// holding a folder, the tempting wrong one is a dev server in production.
	Static *StaticSite
	// StartCommands are candidates, best first.
	StartCommands []Signal
	Workspace     []Signal
	// SystemPackages surface as a linker error deep in a build or an
	// ImportError at startup, and neither names the package.
	SystemPackages []SystemPackage
	BuildFiles     []Signal
}

// Signal is one fact and the file that proves it.
type Signal struct {
	Value    string
	Evidence string
}

// StaticSite is a front end whose build output is served as files.
type StaticSite struct {
	Framework string
	// OutputDir is empty when the framework was recognized but its configured
	// output was not — still worth saying, so the model can go and look.
	OutputDir string
	Evidence  string
	// CustomStart: a start script that is not the framework's own preview
	// command, so the author intends a server and this must not be flattened.
	CustomStart string
}

// SystemPackage is an OS package a dependency needs. Build and Runtime differ,
// and confusing them is how an image builds cleanly then dies on first import.
type SystemPackage struct {
	Dependency string
	Build      []string
	Runtime    []string
}

const maxScanEntries = 20000

// Scan is best-effort by design: a signal donkey cannot compute is one the
// model looks up itself, so nothing here returns an error.
func Scan(dir string) *Signals {
	s := &scanner{dir: dir, files: map[string]bool{}, dirs: map[string]bool{}}
	s.walk()
	out := &Signals{}
	s.runtimes(out)
	s.packageManager(out)
	s.versions(out)
	s.frameworks(out)
	s.staticSite(out)
	s.startCommands(out)
	s.workspace(out)
	s.systemPackages(out)
	s.buildFiles(out)
	return out
}

type scanner struct {
	dir   string
	files map[string]bool
	dirs  map[string]bool
	// order keeps glob results stable.
	order []string

	pkgOnce bool
	pkg     *packageJSON
	pyOnce  bool
	pyText  string
}

func (s *scanner) walk() {
	n := 0
	filepath.WalkDir(s.dir, func(p string, d os.DirEntry, err error) error {
		if err != nil {
			return nil
		}
		rel, rerr := filepath.Rel(s.dir, p)
		if rerr != nil || rel == "." {
			return nil
		}
		rel = filepath.ToSlash(rel)
		if d.IsDir() {
			if skipDirs[d.Name()] {
				return filepath.SkipDir
			}
			s.dirs[rel] = true
			return nil
		}
		if n >= maxScanEntries {
			return filepath.SkipAll
		}
		n++
		s.files[rel] = true
		s.order = append(s.order, rel)
		return nil
	})
}

func (s *scanner) has(rel ...string) string {
	for _, r := range rel {
		if s.files[r] {
			return r
		}
	}
	return ""
}

// glob matches base names anywhere, shallowest first.
func (s *scanner) glob(pattern string) []string {
	var hits []string
	for _, f := range s.order {
		if ok, _ := path.Match(pattern, path.Base(f)); ok {
			hits = append(hits, f)
		}
	}
	sort.SliceStable(hits, func(i, j int) bool {
		return strings.Count(hits[i], "/") < strings.Count(hits[j], "/")
	})
	return hits
}

func (s *scanner) read(rel string) string {
	if rel == "" || deniedFile(path.Base(rel)) {
		return ""
	}
	b, err := os.ReadFile(filepath.Join(s.dir, filepath.FromSlash(rel)))
	if err != nil || len(b) > 1<<20 {
		return ""
	}
	return string(b)
}

// packageJSON is the part of a Node manifest that changes the build.
type packageJSON struct {
	Main                 string            `json:"main"`
	PackageManager       string            `json:"packageManager"`
	Scripts              map[string]string `json:"scripts"`
	Dependencies         map[string]string `json:"dependencies"`
	DevDependencies      map[string]string `json:"devDependencies"`
	OptionalDependencies map[string]string `json:"optionalDependencies"`
	Engines              map[string]string `json:"engines"`
	DevEngines           struct {
		Runtime        devEngines `json:"runtime"`
		PackageManager devEngines `json:"packageManager"`
	} `json:"devEngines"`
	Workspaces json.RawMessage `json:"workspaces"`
}

type devEngine struct {
	Name    string `json:"name"`
	Version string `json:"version"`
}

func (e devEngine) String() string {
	if e.Version == "" {
		return e.Name
	}
	return e.Name + "@" + e.Version
}

// devEngines is npm's devEngines entry for one tool: a single object, or a
// list of acceptable ones. Anything else is ignored rather than allowed to
// fail the whole manifest.
type devEngines []devEngine

func (d *devEngines) UnmarshalJSON(b []byte) error {
	var one devEngine
	if json.Unmarshal(b, &one) == nil {
		*d = devEngines{one}
		return nil
	}
	var many []devEngine
	if json.Unmarshal(b, &many) == nil {
		*d = many
	}
	return nil
}

func (s *scanner) packageJSON() *packageJSON {
	if s.pkgOnce {
		return s.pkg
	}
	s.pkgOnce = true
	name := s.has("package.json", "package.json5")
	if name == "" {
		return nil
	}
	var p packageJSON
	if err := json.Unmarshal([]byte(relaxJSON(s.read(name))), &p); err != nil {
		// Still proves this is a Node app, so return the shell.
		s.pkg = &packageJSON{}
		return s.pkg
	}
	s.pkg = &p
	return s.pkg
}

// relaxJSON strips comments and trailing commas, because the files worth
// reading are exactly the ones that have them (tsconfig, deno.jsonc,
// package.json5). String-aware, since a naive strip mangles every "https://".
func relaxJSON(src string) string {
	var b strings.Builder
	b.Grow(len(src))
	var inString, escaped bool
	for i := 0; i < len(src); i++ {
		c := src[i]
		if inString {
			b.WriteByte(c)
			switch {
			case escaped:
				escaped = false
			case c == '\\':
				escaped = true
			case c == '"':
				inString = false
			}
			continue
		}
		switch {
		case c == '"':
			inString = true
			b.WriteByte(c)
		case c == '/' && i+1 < len(src) && src[i+1] == '/':
			for i < len(src) && src[i] != '\n' {
				i++
			}
			b.WriteByte('\n')
		case c == '/' && i+1 < len(src) && src[i+1] == '*':
			i += 2
			for i+1 < len(src) && !(src[i] == '*' && src[i+1] == '/') {
				i++
			}
			i++
		case c == ',':
			// A trailing comma is one followed only by whitespace and a
			// closing brace or bracket.
			j := i + 1
			for j < len(src) && (src[j] == ' ' || src[j] == '\t' || src[j] == '\n' || src[j] == '\r') {
				j++
			}
			if j < len(src) && (src[j] == '}' || src[j] == ']') {
				continue
			}
			b.WriteByte(c)
		default:
			b.WriteByte(c)
		}
	}
	return b.String()
}

// hasDep checks the root manifest's direct dependencies.
func (p *packageJSON) hasDep(name string) bool {
	if p == nil {
		return false
	}
	_, ok := p.Dependencies[name]
	if ok {
		return true
	}
	_, ok = p.DevDependencies[name]
	return ok
}

func (p *packageJSON) script(name string) string {
	if p == nil {
		return ""
	}
	return p.Scripts[name]
}

// pythonText is every dependency declaration concatenated: the checks below
// ask "is this used", not "which version".
func (s *scanner) pythonText() string {
	if s.pyOnce {
		return s.pyText
	}
	s.pyOnce = true
	var b strings.Builder
	for _, f := range []string{"requirements.txt", "pyproject.toml", "Pipfile", "setup.py", "requirements.in"} {
		b.WriteString(s.read(f))
		b.WriteString("\n")
	}
	// Split requirement files are the norm the moment an app has a dev set.
	for _, f := range s.glob("requirements*.txt") {
		b.WriteString(s.read(f))
		b.WriteString("\n")
	}
	s.pyText = strings.ToLower(b.String())
	return s.pyText
}

func (s *scanner) runtimes(out *Signals) {
	add := func(value, evidence string) {
		if evidence != "" {
			out.Runtimes = append(out.Runtimes, Signal{Value: value, Evidence: evidence})
		}
	}
	add("Node.js", s.has("package.json", "package.json5"))
	add("Deno", s.has("deno.json", "deno.jsonc"))
	add("Python", s.has("requirements.txt", "pyproject.toml", "Pipfile", "setup.py", "manage.py"))
	add("Go", s.has("go.mod"))
	add("Ruby", s.has("Gemfile"))
	add("PHP", s.has("composer.json"))
	add("Rust", s.has("Cargo.toml"))
	add("Elixir", s.has("mix.exs"))
	add("Gleam", s.has("gleam.toml"))
	add("Java (Maven)", s.has("pom.xml"))
	add("Java (Gradle)", s.has("build.gradle", "build.gradle.kts",
		"settings.gradle", "settings.gradle.kts", "gradlew"))
	add("C/C++", s.has("CMakeLists.txt", "meson.build"))
	for _, pat := range []string{"*.csproj", "*.fsproj", "*.sln"} {
		if hits := s.glob(pat); len(hits) > 0 {
			add(".NET", hits[0])
			break
		}
	}

	// Nothing at the root does not mean nothing: Gradle keeps module manifests
	// a level down, and plenty of repos put the deployable in services/<name>/.
	// "Not at the root" is itself the fact that matters most.
	if len(out.Runtimes) == 0 {
		nested := []struct{ pattern, name string }{
			{"package.json", "Node.js"}, {"go.mod", "Go"},
			{"requirements.txt", "Python"}, {"pyproject.toml", "Python"},
			{"build.gradle*", "Java (Gradle)"}, {"pom.xml", "Java (Maven)"},
			{"Gemfile", "Ruby"}, {"composer.json", "PHP"},
			{"Cargo.toml", "Rust"}, {"mix.exs", "Elixir"},
		}
		for _, n := range nested {
			if hits := s.glob(n.pattern); len(hits) > 0 {
				add(n.name+" (NOT at the repository root)", hits[0])
			}
		}
	}

	// An app can be a handful of source files with no manifest at all;
	// refusing to name a runtime would send the model in blind.
	if len(out.Runtimes) == 0 {
		bare := []struct{ pattern, name string }{
			{"*.php", "PHP (loose scripts, no composer.json)"},
			{"*.py", "Python (loose scripts, no dependency manifest)"},
			{"*.sh", "a shell script"},
			{"*.rb", "Ruby (loose scripts, no Gemfile)"},
		}
		for _, b := range bare {
			if hits := s.glob(b.pattern); len(hits) > 0 {
				add(b.name, hits[0])
			}
		}
	}

	// Saying so keeps the model from inventing a toolchain.
	if len(out.Runtimes) == 0 {
		if f := s.has("index.html", "public/index.html", "Staticfile"); f != "" {
			add("Static files (no build step found)", f)
		}
	}
}

func (s *scanner) packageManager(out *Signals) {
	add := func(value, evidence string) {
		if evidence != "" {
			out.PackageManager = append(out.PackageManager, Signal{Value: value, Evidence: evidence})
		}
	}
	// Manifest declaration, then lockfile, then engines. They can disagree, so
	// all are reported and the order carries the answer.
	if pkg := s.packageJSON(); pkg != nil {
		if pkg.PackageManager != "" {
			add("Node: "+pkg.PackageManager+" (declared, wins over lockfiles)", "package.json > packageManager")
		}
		for _, e := range pkg.DevEngines.PackageManager {
			if e.Name != "" {
				add("Node: "+e.String()+" (declared in devEngines, wins over lockfiles)", "package.json > devEngines.packageManager")
			}
		}
		switch {
		case s.has("pnpm-lock.yaml") != "":
			add("Node: pnpm", "pnpm-lock.yaml")
		case s.has("bun.lockb", "bun.lock") != "":
			add("Node: bun", s.has("bun.lockb", "bun.lock"))
		case s.has(".yarnrc.yml", ".yarnrc.yaml") != "":
			add("Node: yarn (berry, v2+)", s.has(".yarnrc.yml", ".yarnrc.yaml"))
		case s.has("yarn.lock") != "":
			add("Node: yarn 1", "yarn.lock")
		case s.has("package-lock.json") != "":
			if missing := s.npmLockMissing(pkg); len(missing) > 0 {
				add("Node: npm, but package-lock.json is out of sync with package.json (it does not record "+
					strings.Join(missing, ", ")+") — `npm ci` fails on it, use `npm install`", "package-lock.json")
			} else {
				add("Node: npm", "package-lock.json")
			}
		default:
			add("Node: npm (no lockfile found — prefer `npm install` over `npm ci`)", "package.json")
		}
		if pkg.Engines != nil {
			for _, tool := range []string{"pnpm", "yarn", "bun", "npm"} {
				if v := strings.TrimSpace(pkg.Engines[tool]); v != "" {
					add("Node: engines."+tool+" = "+v, "package.json > engines")
				}
			}
		}
	}
	// Python, in the tools' own precedence order.
	switch {
	case s.has("uv.lock") != "":
		add("Python: uv", "uv.lock")
	case s.has("poetry.lock") != "":
		add("Python: poetry", "poetry.lock")
	case s.has("pdm.lock") != "":
		add("Python: pdm", "pdm.lock")
	case s.has("Pipfile.lock", "Pipfile") != "":
		add("Python: pipenv", s.has("Pipfile.lock", "Pipfile"))
	case s.has("requirements.txt") != "":
		add("Python: pip", "requirements.txt")
	}
	if py := s.read("pyproject.toml"); py != "" {
		if strings.Contains(py, "[tool.poetry") {
			add("Python: poetry", "pyproject.toml > [tool.poetry]")
		}
		if strings.Contains(py, "[tool.uv") {
			add("Python: uv", "pyproject.toml > [tool.uv]")
		}
		if strings.Contains(py, "[tool.pdm") {
			add("Python: pdm", "pyproject.toml > [tool.pdm]")
		}
	}
	add("Ruby: bundler", s.has("Gemfile.lock", "Gemfile"))
	add("PHP: composer", s.has("composer.lock", "composer.json"))
	add("Go: go mod download", s.has("go.mod", "go.work"))
	add("Rust: cargo", s.has("Cargo.toml"))
	add("Elixir: mix deps.get", s.has("mix.exs"))
	add("Gleam: gleam deps download", s.has("gleam.toml"))
	add("Deno: deno install (or no install step at all)", s.has("deno.json", "deno.jsonc"))
	add("Java: the Maven wrapper", s.has("mvnw"))
	add("Java: the Gradle wrapper", s.has("gradlew"))
	for _, pat := range []string{"*.csproj", "*.fsproj", "*.sln"} {
		if hits := s.glob(pat); len(hits) > 0 {
			add(".NET: dotnet restore", hits[0])
			break
		}
	}
}

// npmLockMissing names the dependencies package.json declares that
// package-lock.json does not record — what a manifest edited by hand, or by an
// assistant, looks like. `npm ci` refuses such a lockfile outright. Only the
// root entry of a v2/v3 lockfile is read; a v1 lockfile says nothing here.
func (s *scanner) npmLockMissing(pkg *packageJSON) []string {
	f, err := os.Open(filepath.Join(s.dir, "package-lock.json"))
	if err != nil {
		return nil
	}
	defer f.Close()
	root := npmLockRoot(json.NewDecoder(io.LimitReader(f, 64<<20)))
	if root == nil {
		return nil
	}
	var missing []string
	for _, deps := range []map[string]string{pkg.Dependencies, pkg.DevDependencies, pkg.OptionalDependencies} {
		for name := range deps {
			if !root.records(name) {
				missing = append(missing, name)
			}
		}
	}
	sort.Strings(missing)
	return slices.Compact(missing)
}

// npmLockEntry is a lockfile's packages[""]: the project's own manifest as of
// the last install.
type npmLockEntry struct {
	Dependencies         map[string]string `json:"dependencies"`
	DevDependencies      map[string]string `json:"devDependencies"`
	OptionalDependencies map[string]string `json:"optionalDependencies"`
	PeerDependencies     map[string]string `json:"peerDependencies"`
}

func (e *npmLockEntry) records(name string) bool {
	for _, deps := range []map[string]string{e.Dependencies, e.DevDependencies, e.OptionalDependencies, e.PeerDependencies} {
		if _, ok := deps[name]; ok {
			return true
		}
	}
	return false
}

// npmLockRoot decodes packages[""] and stops there. A lockfile runs to
// megabytes, and npm writes the root entry first.
func npmLockRoot(dec *json.Decoder) *npmLockEntry {
	if t, err := dec.Token(); err != nil || t != json.Delim('{') {
		return nil
	}
	for dec.More() {
		key, err := dec.Token()
		if err != nil {
			return nil
		}
		if key != "packages" {
			var skip json.RawMessage
			if dec.Decode(&skip) != nil {
				return nil
			}
			continue
		}
		if t, err := dec.Token(); err != nil || t != json.Delim('{') {
			return nil
		}
		for dec.More() {
			name, err := dec.Token()
			if err != nil {
				return nil
			}
			if name == "" {
				var root npmLockEntry
				if dec.Decode(&root) != nil {
					return nil
				}
				return &root
			}
			var skip json.RawMessage
			if dec.Decode(&skip) != nil {
				return nil
			}
		}
		return nil
	}
	return nil
}

// versionFiles are where a toolchain version gets pinned, and what it pins.
var versionFiles = []struct{ file, tool string }{
	{".nvmrc", "node"},
	{".node-version", "node"},
	{".python-version", "python"},
	{"runtime.txt", "python"},
	{".ruby-version", "ruby"},
	{".go-version", "go"},
	{"rust-toolchain", "rust"},
	{".terraform-version", "terraform"},
}

var (
	goDirectiveRe   = regexp.MustCompile(`(?m)^go\s+([0-9]+\.[0-9]+(?:\.[0-9]+)?)`)
	pipfilePyRe     = regexp.MustCompile(`python_(?:version|full_version)\s*=\s*"([^"]+)"`)
	requiresPyRe    = regexp.MustCompile(`requires-python\s*=\s*"([^"]+)"`)
	rubyGemfileRe   = regexp.MustCompile(`(?m)^\s*ruby\s+["']([^"']+)["']`)
	rustToolchainRe = regexp.MustCompile(`channel\s*=\s*"([^"]+)"`)
	elixirRe        = regexp.MustCompile(`elixir:\s*"([^"]+)"`)
	phpRe           = regexp.MustCompile(`"php"\s*:\s*"([^"]+)"`)
	javaRe          = regexp.MustCompile(`(?:sourceCompatibility|maven\.compiler\.source|java\.version)[^0-9]{0,20}([0-9]+)`)
	dotnetTFMRe     = regexp.MustCompile(`<TargetFramework>([^<]+)</TargetFramework>`)
)

func (s *scanner) versions(out *Signals) {
	add := func(tool, version, evidence string) {
		version = strings.TrimSpace(version)
		if version == "" || evidence == "" {
			return
		}
		out.Versions = append(out.Versions, Signal{Value: tool + " " + version, Evidence: evidence})
	}
	for _, vf := range versionFiles {
		if s.has(vf.file) == "" {
			continue
		}
		body := strings.TrimSpace(s.read(vf.file))
		if vf.file == "runtime.txt" {
			body = strings.TrimPrefix(strings.TrimSpace(body), "python-")
		}
		if i := strings.IndexAny(body, "\r\n"); i >= 0 {
			body = body[:i]
		}
		add(vf.tool, body, vf.file)
	}
	// These pin several tools at once and beat the single-tool files.
	if f := s.has(".tool-versions"); f != "" {
		for line := range strings.SplitSeq(s.read(f), "\n") {
			if fields := strings.Fields(line); len(fields) >= 2 && !strings.HasPrefix(fields[0], "#") {
				add(fields[0], fields[1], f)
			}
		}
	}
	if f := s.has("mise.toml", ".mise.toml"); f != "" {
		out.Versions = append(out.Versions, Signal{Value: "mise config present — read it for pinned tool versions", Evidence: f})
	}
	if pkg := s.packageJSON(); pkg != nil {
		if pkg.Engines != nil {
			add("node", pkg.Engines["node"], "package.json > engines.node")
		}
		for _, e := range pkg.DevEngines.Runtime {
			if e.Name != "" {
				add(e.Name, e.Version, "package.json > devEngines.runtime")
			}
		}
	}
	if m := goDirectiveRe.FindStringSubmatch(s.read("go.mod")); m != nil {
		add("go", m[1], "go.mod")
	}
	if m := pipfilePyRe.FindStringSubmatch(s.read("Pipfile")); m != nil {
		add("python", m[1], "Pipfile")
	}
	if m := requiresPyRe.FindStringSubmatch(s.read("pyproject.toml")); m != nil {
		add("python", m[1], "pyproject.toml > requires-python")
	}
	if m := rubyGemfileRe.FindStringSubmatch(s.read("Gemfile")); m != nil {
		add("ruby", m[1], "Gemfile")
	}
	if f := s.has("rust-toolchain.toml"); f != "" {
		if m := rustToolchainRe.FindStringSubmatch(s.read(f)); m != nil {
			add("rust", m[1], f)
		}
	}
	if m := elixirRe.FindStringSubmatch(s.read("mix.exs")); m != nil {
		add("elixir", m[1], "mix.exs")
	}
	if m := phpRe.FindStringSubmatch(s.read("composer.json")); m != nil {
		add("php", m[1], "composer.json > require.php")
	}
	for _, f := range []string{"pom.xml", "build.gradle", "build.gradle.kts"} {
		if m := javaRe.FindStringSubmatch(s.read(f)); m != nil {
			add("java", m[1], f)
			break
		}
	}
	if hits := s.glob("*.csproj"); len(hits) > 0 {
		if m := dotnetTFMRe.FindStringSubmatch(s.read(hits[0])); m != nil {
			add("dotnet", m[1], hits[0])
		}
	}
}

// Presence of the dependency is the whole test: these packages are not used
// for anything else.
var nodeFrameworks = []struct{ dep, name string }{
	{"next", "Next.js"},
	{"nuxt", "Nuxt"},
	{"@remix-run/node", "Remix"},
	{"@tanstack/react-start", "TanStack Start"},
	{"@react-router/dev", "React Router (framework mode)"},
	{"@sveltejs/kit", "SvelteKit"},
	{"astro", "Astro"},
	{"@angular/core", "Angular"},
	{"react-scripts", "Create React App"},
	{"expo", "Expo"},
	{"@nestjs/core", "NestJS"},
	{"vite", "Vite"},
	{"@medusajs/medusa", "Medusa"},
	{"payload", "Payload CMS"},
	{"strapi", "Strapi"},
	{"@prisma/client", "Prisma (needs `prisma generate` during the build)"},
	{"drizzle-orm", "Drizzle"},
	{"puppeteer", "Puppeteer (needs a headless Chrome and its system libraries)"},
	{"playwright", "Playwright (needs browsers installed in the image)"},
}

// Matched against the lowercased dependency declarations.
var pythonFrameworks = []struct{ needle, name string }{
	{"django", "Django"},
	{"flask", "Flask"},
	{"fastapi", "FastAPI"},
	{"streamlit", "Streamlit"},
	{"gradio", "Gradio"},
	{"litestar", "Litestar"},
	{"quart", "Quart"},
	{"sanic", "Sanic"},
	{"tornado", "Tornado"},
	{"gunicorn", "gunicorn (a production WSGI server is already a dependency)"},
	{"uvicorn", "uvicorn (a production ASGI server is already a dependency)"},
	{"celery", "Celery (a worker — check whether this app is the worker or the web process)"},
}

func (s *scanner) frameworks(out *Signals) {
	add := func(value, evidence string) {
		out.Frameworks = append(out.Frameworks, Signal{Value: value, Evidence: evidence})
	}
	if pkg := s.packageJSON(); pkg != nil {
		for _, fw := range nodeFrameworks {
			if pkg.hasDep(fw.dep) {
				add(fw.name, "package.json depends on "+fw.dep)
			}
		}
	}
	py := s.pythonText()
	for _, fw := range pythonFrameworks {
		if strings.Contains(py, fw.needle) {
			add(fw.name, "a Python dependency file names "+fw.needle)
		}
	}
	if f := s.has("manage.py"); f != "" {
		add("Django (manage.py present — collectstatic and migrations are usually needed)", f)
	}
	for _, m := range []struct{ file, name string }{
		{"artisan", "Laravel"},
		{"config/routes.rb", "Ruby on Rails"},
		{"Rakefile", "Rake tasks present"},
		{"config.ru", "Rack app"},
	} {
		if f := s.has(m.file); f != "" {
			add(m.name, f)
		}
	}
	if strings.Contains(s.read("pom.xml")+s.read("build.gradle")+s.read("build.gradle.kts"), "spring-boot") {
		add("Spring Boot", "the Java build file names spring-boot")
	}
}

// spaVerdict is why the ladder below is a ladder. Every Node meta-framework
// depends on Vite directly, so "depends on vite" is true for SvelteKit, React
// Router, Astro and Nuxt alike. A rule that recognizes its framework and
// concludes *server* must stop the search, or the generic Vite rule below
// flattens a server application into a folder of files.
type spaVerdict int

const (
	// spaNoMatch: this framework is not in the project at all.
	spaNoMatch spaVerdict = iota
	// spaStatic: this framework builds the app, to files.
	spaStatic
	// spaServer: this framework builds the app, and it emits a server. Nothing
	// further down the ladder gets a say.
	spaServer
)

// spaRule is one rung of that ladder.
type spaRule struct {
	name string
	// detect reports whether this framework builds the app, and where to.
	detect func(s *scanner, pkg *packageJSON) (spaVerdict, string, string)
	// defaultStart is the framework's own preview/serve script. A start script
	// equal to it is not a custom server, so it does not disqualify the app.
	defaultStart []string
}

var (
	viteOutDirRe    = regexp.MustCompile(`outDir\s*:\s*["'` + "`" + `]([^"'` + "`" + `]+)`)
	angularOutRe    = regexp.MustCompile(`"outputPath"\s*:\s*"([^"]+)"`)
	nextExportRe    = regexp.MustCompile(`output\s*:\s*["']export["']`)
	reactRouterSSR  = regexp.MustCompile(`ssr\s*:\s*false`)
	svelteStaticRe  = regexp.MustCompile(`adapter-static`)
	astroOutputRe   = regexp.MustCompile(`output\s*:\s*["'](server|hybrid)["']`)
	astroOutDirRe   = regexp.MustCompile(`outDir\s*:\s*["'` + "`" + `]([^"'` + "`" + `]+)`)
	configExtension = []string{".ts", ".js", ".mjs", ".mts", ".cjs"}
)

// configFile finds a framework config by stem, trying every extension the
// ecosystem uses.
func (s *scanner) configFile(stem string) string {
	for _, ext := range configExtension {
		if f := s.has(stem + ext); f != "" {
			return f
		}
	}
	return ""
}

var leadingMajorRe = regexp.MustCompile(`(\d+)`)

// majorVersion pulls the major out of an npm range; 0 means "assume older".
func majorVersion(spec string) int {
	m := leadingMajorRe.FindStringSubmatch(spec)
	if m == nil {
		return 0
	}
	n := 0
	for _, c := range m[1] {
		n = n*10 + int(c-'0')
		if n > 1000 {
			return 0
		}
	}
	return n
}

func (s *scanner) staticSite(out *Signals) {
	// A Staticfile naming a root says outright which folder to serve. One that
	// only sets index_fallback configures *how*, not *where* — the framework
	// below still decides, or an Astro site gets served from its source.
	if f := s.has("Staticfile"); f != "" {
		if root := staticfileRootOrEmpty(s.read(f)); root != "" {
			out.Static = &StaticSite{
				Framework: "a Staticfile", OutputDir: root, Evidence: f,
			}
			return
		}
	}

	// A Node front end on top of a backend is that backend's asset pipeline,
	// not a static site. Laravel with Vite is the case: serving the build
	// output would ship the CSS and throw away the application.
	if backend := s.has("artisan", "composer.json", "manage.py", "config.ru",
		"Gemfile", "pom.xml", "mix.exs"); backend != "" {
		return
	}

	pkg := s.packageJSON()
	if pkg == nil {
		// A repo of HTML with no manifest is still a static site, and the
		// simplest one there is.
		if f := s.has("index.html"); f != "" {
			out.Static = &StaticSite{Framework: "plain HTML", OutputDir: ".", Evidence: f}
		}
		return
	}

	rules := []spaRule{
		{
			name: "React Router (SPA mode)",
			detect: func(s *scanner, pkg *packageJSON) (spaVerdict, string, string) {
				if !pkg.hasDep("@react-router/dev") {
					return spaNoMatch, "", ""
				}
				cfg := s.configFile("react-router.config")
				if cfg == "" || !reactRouterSSR.MatchString(s.read(cfg)) {
					// ssr defaults to true: this is a server app, not an SPA.
					return spaServer, "", ""
				}
				return spaStatic, "build/client", cfg + " sets ssr: false"
			},
			defaultStart: []string{"react-router-serve ./build/server/index.js"},
		},
		{
			name: "Next.js (static export)",
			detect: func(s *scanner, pkg *packageJSON) (spaVerdict, string, string) {
				if !pkg.hasDep("next") {
					return spaNoMatch, "", ""
				}
				cfg := s.configFile("next.config")
				if cfg == "" || !nextExportRe.MatchString(s.read(cfg)) {
					return spaServer, "", ""
				}
				return spaStatic, "out", cfg + " sets output: 'export'"
			},
			defaultStart: []string{"next start"},
		},
		{
			name: "Astro (static)",
			detect: func(s *scanner, pkg *packageJSON) (spaVerdict, string, string) {
				if !pkg.hasDep("astro") {
					return spaNoMatch, "", ""
				}
				cfg := s.configFile("astro.config")
				body := s.read(cfg)
				if astroOutputRe.MatchString(body) {
					// output: 'server' or 'hybrid' means Astro emits a Node
					// server; flattening it to files would drop the app.
					return spaServer, "", ""
				}
				dir := "dist"
				if m := astroOutDirRe.FindStringSubmatch(body); m != nil {
					dir = strings.TrimPrefix(m[1], "./")
				}
				return spaStatic, dir, "astro is a dependency and no server output is configured"
			},
			defaultStart: []string{"astro preview"},
		},
		{
			name: "SvelteKit (static adapter)",
			detect: func(s *scanner, pkg *packageJSON) (spaVerdict, string, string) {
				if !pkg.hasDep("@sveltejs/kit") {
					return spaNoMatch, "", ""
				}
				cfg := s.configFile("svelte.config")
				if cfg == "" || !svelteStaticRe.MatchString(s.read(cfg)) {
					// Any other adapter — node, vercel, cloudflare — produces
					// something that has to be run, not served.
					return spaServer, "", ""
				}
				return spaStatic, "build", cfg + " uses adapter-static"
			},
		},
		{
			name: "Angular",
			detect: func(s *scanner, pkg *packageJSON) (spaVerdict, string, string) {
				if !pkg.hasDep("@angular/core") {
					return spaNoMatch, "", ""
				}
				dir := ""
				if m := angularOutRe.FindStringSubmatch(s.read("angular.json")); m != nil {
					dir = strings.TrimPrefix(m[1], "./")
					// Angular 17 moved the browser bundle into a browser/
					// subdirectory and put the SSR server beside it. Serving
					// the parent gives a visitor a directory listing.
					if majorVersion(pkg.Dependencies["@angular/core"]) >= 17 {
						dir = path.Join(dir, "browser")
					}
				}
				return spaStatic, dir, "angular.json"
			},
			defaultStart: []string{"ng serve"},
		},
		{
			name: "Create React App",
			detect: func(s *scanner, pkg *packageJSON) (spaVerdict, string, string) {
				if !pkg.hasDep("react-scripts") {
					return spaNoMatch, "", ""
				}
				return spaStatic, "build", "package.json depends on react-scripts"
			},
			defaultStart: []string{"react-scripts start", "serve -s build"},
		},
		{
			name: "Expo (web)",
			detect: func(s *scanner, pkg *packageJSON) (spaVerdict, string, string) {
				if !pkg.hasDep("expo") {
					return spaNoMatch, "", ""
				}
				return spaStatic, "dist", "package.json depends on expo"
			},
			defaultStart: []string{"expo start"},
		},
		{
			name: "Vite",
			detect: func(s *scanner, pkg *packageJSON) (spaVerdict, string, string) {
				if !pkg.hasDep("vite") {
					return spaNoMatch, "", ""
				}
				// Reaching here means nothing above claimed the app.
				cfg := s.configFile("vite.config")
				dir := "dist"
				if m := viteOutDirRe.FindStringSubmatch(s.read(cfg)); m != nil {
					dir = strings.TrimPrefix(m[1], "./")
				}
				ev := "package.json depends on vite"
				if cfg != "" {
					ev = cfg
				}
				return spaStatic, dir, ev
			},
			defaultStart: []string{"vite preview", "vite"},
		},
	}

	// Frameworks that always produce a server disqualify the app outright,
	// whatever else is in the dependency list. TanStack Start counts as a
	// devDependency too: railpack reads that as a static site, but the build
	// still writes dist/client and dist/server, with no index.html for a web
	// server to serve.
	alwaysServer := []string{"nuxt", "@remix-run/node", "@tanstack/react-start", "@nestjs/core"}
	if slices.ContainsFunc(alwaysServer, pkg.hasDep) {
		return
	}

	for _, r := range rules {
		verdict, dir, evidence := r.detect(s, pkg)
		if verdict == spaServer {
			// Recognized, and it needs a running process. Stop: every rule
			// below this one is more generic and would say yes.
			return
		}
		if verdict == spaNoMatch {
			continue
		}
		site := &StaticSite{Framework: r.name, OutputDir: dir, Evidence: evidence}
		// An author's own start script means they want a server running.
		if start := strings.TrimSpace(pkg.script("start")); start != "" && !slices.Contains(r.defaultStart, start) {
			site.CustomStart = start
		}
		out.Static = site
		return
	}
}

var (
	procfileWebRe  = regexp.MustCompile(`(?m)^\s*web\s*:\s*(.+)$`)
	startCmdJSONRe = regexp.MustCompile(`"startCommand"\s*:\s*"((?:[^"\\]|\\.)*)"`)
	startCmdTOMLRe = regexp.MustCompile(`(?m)^\s*(?:start|cmd)\s*=\s*"((?:[^"\\]|\\.)*)"`)
	projectScript  = regexp.MustCompile(`(?ms)\[project\.scripts\](.*?)(?:\n\[|\z)`)
	scriptEntryRe  = regexp.MustCompile(`(?m)^\s*([A-Za-z0-9_.-]+)\s*=`)
)

func (s *scanner) startCommands(out *Signals) {
	add := func(value, evidence string) {
		out.StartCommands = append(out.StartCommands, Signal{Value: value, Evidence: evidence})
	}
	// A previous builder's config is a deliberate answer for this app. It
	// outranks anything inferred, including the often-stale Procfile.
	for _, cfg := range []struct {
		file string
		re   *regexp.Regexp
	}{
		{"railpack.json", startCmdJSONRe},
		{"nixpacks.toml", startCmdTOMLRe},
	} {
		if f := s.has(cfg.file); f != "" {
			if m := cfg.re.FindStringSubmatch(s.read(f)); m != nil {
				add(m[1], f+" (an explicit start command from a previous builder)")
			}
		}
	}
	// A Procfile is the author saying it outright, so it goes next.
	if f := s.has("Procfile"); f != "" {
		if m := procfileWebRe.FindStringSubmatch(s.read(f)); m != nil {
			add(strings.TrimSpace(m[1]), f+" > web")
		}
	}
	if pkg := s.packageJSON(); pkg != nil {
		if v := pkg.script("start"); v != "" {
			add("the `start` script: "+v, "package.json > scripts.start")
		}
		if pkg.Main != "" {
			add("node "+pkg.Main, "package.json > main")
		}
		if f := s.has("index.js", "index.mjs", "index.ts", "server.js", "server.ts", "src/index.js", "src/index.ts"); f != "" {
			add("node "+f, f)
		}
		// Meta-frameworks put their entry point where no convention predicts.
		if pkg.hasDep("nuxt") {
			add("node .output/server/index.mjs (Nuxt's built server)", "package.json depends on nuxt")
		}
		if pkg.hasDep("@sveltejs/kit") {
			add("node build (with adapter-node; check svelte.config.js for the adapter)", "package.json depends on @sveltejs/kit")
		}
		if pkg.hasDep("next") {
			add("next start", "package.json depends on next")
		}
		if pkg.hasDep("@remix-run/node") {
			add("remix-serve ./build/server/index.js", "package.json depends on @remix-run/node")
		}
		if pkg.hasDep("@nestjs/core") {
			add("node dist/main.js", "package.json depends on @nestjs/core")
		}
		// TanStack Start builds its server into dist/server and the browser
		// bundle into dist/client; srvx comes with it and serves both. With
		// Nitro, the build is one self-contained server in .output instead.
		if pkg.hasDep("@tanstack/react-start") {
			cmd := "npx srvx --prod -s ../client dist/server/server.js (TanStack Start's built server; -s is relative to dist/server)"
			if pkg.hasDep("nitro") {
				cmd = "node .output/server/index.mjs (TanStack Start built with Nitro)"
			} else if _, prod := pkg.Dependencies["@tanstack/react-start"]; !prod {
				cmd += " — @tanstack/react-start is only a devDependency, and srvx arrives with it, so the runtime image must keep dev dependencies"
			}
			add(cmd, "package.json depends on @tanstack/react-start")
		}
	}
	if f := s.has("manage.py"); f != "" {
		add("a WSGI/ASGI server pointed at the Django project (manage.py runserver is a development server and must not be used)", f)
	}
	for _, m := range []struct{ file, cmd string }{
		{"main.py", "python main.py, or an ASGI/WSGI server pointed at the app object in it"},
		{"app.py", "python app.py, or an ASGI/WSGI server pointed at the app object in it"},
		{"wsgi.py", "gunicorn pointed at wsgi:application"},
		{"asgi.py", "uvicorn pointed at asgi:application"},
		{"config.ru", "a Rack server (puma) pointed at config.ru"},
		{"artisan", "php-fpm behind a web server (`php artisan serve` is a development server)"},
		{"gleam.toml", "the gleam entry point (`gleam run`, or the built Erlang release)"},
		{"deno.json", "the task named in deno.json, or `deno run` against the entry module"},
		{"deno.jsonc", "the task named in deno.jsonc, or `deno run` against the entry module"},
	} {
		if f := s.has(m.file); f != "" {
			add(m.cmd, f)
		}
	}
	// Compiled languages: what to look for, not a literal command.
	if f := s.has("go.mod", "go.work"); f != "" {
		cmd := "the compiled Go binary"
		// `cmd/<name>` is the convention for a repo with more than one binary,
		// and picking the wrong one produces an image that starts the wrong
		// program.
		if mains := s.glob("main.go"); len(mains) > 1 {
			cmd += " — several main.go files exist, so the package to build has to be chosen deliberately"
		}
		add(cmd, f)
	}
	if f := s.has("Cargo.toml"); f != "" {
		cmd := "the release binary from `cargo build --release` (target/release/<name>)"
		if strings.Contains(s.read(f), "[[bin]]") {
			cmd += " — Cargo.toml declares explicit [[bin]] targets, so name the one to run"
		}
		add(cmd, f)
	}
	if f := s.has("pom.xml"); f != "" {
		add("java -jar on the jar Maven produces in target/", f)
	}
	if f := s.has("build.gradle", "build.gradle.kts", "settings.gradle", "settings.gradle.kts"); f != "" {
		add("java -jar on the jar Gradle produces in build/libs/ (or the start script from the application plugin)", f)
	}
	if f := s.has("mix.exs"); f != "" {
		cmd := "the Mix release (`mix release`, then bin/<app> start)"
		if s.dirs["assets"] || strings.Contains(s.read(f), "phoenix") {
			cmd = "the Phoenix release — assets have to be compiled and digested during the build"
		}
		add(cmd, f)
	}
	for _, pat := range []string{"*.csproj", "*.fsproj"} {
		if hits := s.glob(pat); len(hits) > 0 {
			add("dotnet on the DLL that `dotnet publish` produces", hits[0])
			break
		}
	}
	if f := s.has("CMakeLists.txt", "meson.build"); f != "" {
		add("the binary the build produces", f)
	}
	// A packaged project's [project.scripts] name lands on PATH.
	if body := s.read("pyproject.toml"); body != "" {
		if block := projectScript.FindStringSubmatch(body); block != nil {
			if m := scriptEntryRe.FindStringSubmatch(block[1]); m != nil {
				add(m[1]+" (the console script this project installs)", "pyproject.toml > [project.scripts]")
			}
		}
	}
	// A repository whose only executable thing is a script says so plainly.
	if f := s.has("start.sh", "run.sh", "entrypoint.sh", "boot.sh"); f != "" {
		add("the script itself (bash "+f+")", f)
	}

	// An app can still be one loose source file, and naming it beats
	// reporting no candidate at all.
	if len(out.StartCommands) == 0 {
		for _, loose := range []struct{ pattern, cmd string }{
			{"*.py", "python %s"},
			{"*.rb", "ruby %s"},
			{"*.php", "php -S 0.0.0.0:$PORT (or php-fpm behind a web server) serving %s"},
			{"*.sh", "bash %s"},
			{"*.js", "node %s"},
		} {
			hits := s.glob(loose.pattern)
			if len(hits) == 0 {
				continue
			}
			add(fmt.Sprintf(loose.cmd, hits[0])+" — the only entry point donkey could find", hits[0])
			break
		}
	}
	// A Staticfile names the folder to serve, which is the whole answer.
	if f := s.has("Staticfile"); f != "" {
		add("a web server serving "+staticfileRoot(s.read(f))+" (a Staticfile names the folder)", f)
	}
}

var staticfileRootRe = regexp.MustCompile(`(?m)^\s*root\s*:\s*(\S+)`)

// staticfileRootOrEmpty reads a Cloud Foundry Staticfile's `root:` line.
func staticfileRootOrEmpty(body string) string {
	if m := staticfileRootRe.FindStringSubmatch(body); m != nil {
		return strings.Trim(m[1], "/")
	}
	return ""
}

// staticfileRoot defaults to the repository root.
func staticfileRoot(body string) string {
	if root := staticfileRootOrEmpty(body); root != "" {
		return root
	}
	return "."
}

func (s *scanner) workspace(out *Signals) {
	add := func(value, evidence string) {
		if evidence != "" {
			out.Workspace = append(out.Workspace, Signal{Value: value, Evidence: evidence})
		}
	}
	add("pnpm workspace", s.has("pnpm-workspace.yaml"))
	add("Nx monorepo", s.has("nx.json"))
	add("Turborepo", s.has("turbo.json"))
	add("Lerna monorepo", s.has("lerna.json"))
	add("Go workspace", s.has("go.work"))
	add("Rush monorepo", s.has("rush.json"))
	if pkg := s.packageJSON(); pkg != nil && len(pkg.Workspaces) > 0 && string(pkg.Workspaces) != "null" {
		add("npm/yarn workspaces: "+string(pkg.Workspaces), "package.json > workspaces")
	}
	if strings.Contains(s.read("Cargo.toml"), "[workspace]") {
		add("Cargo workspace", "Cargo.toml")
	}
	// Several manifests of a kind means the root is not the app.
	if hits := s.glob("package.json"); len(hits) > 3 {
		add("several package.json files — the app to build may not be at the root", hits[0])
	}
}

// systemDeps is knowledge that costs a failed build to rediscover. Matched
// case-insensitively against the app's dependency declarations.
var systemDeps = []struct {
	match string
	// node restricts the rule to Node deps; otherwise Python.
	node    bool
	build   []string
	runtime []string
}{
	// Python — database drivers are the single most common build failure.
	{match: "psycopg2", build: []string{"libpq-dev", "gcc"}, runtime: []string{"libpq5"}},
	{match: "psycopg[", build: []string{"libpq-dev", "gcc"}, runtime: []string{"libpq5"}},
	{match: "mysqlclient", build: []string{"default-libmysqlclient-dev", "pkg-config", "gcc"}, runtime: []string{"default-mysql-client"}},
	{match: "pyodbc", build: []string{"unixodbc-dev", "gcc"}, runtime: []string{"unixodbc"}},
	{match: "pycairo", build: []string{"libcairo2-dev", "pkg-config"}, runtime: []string{"libcairo2"}},
	{match: "cairosvg", runtime: []string{"libcairo2"}},
	{match: "weasyprint", runtime: []string{"libpango-1.0-0", "libpangoft2-1.0-0", "libharfbuzz0b", "libffi8"}},
	{match: "pdf2image", runtime: []string{"poppler-utils"}},
	{match: "pytesseract", runtime: []string{"tesseract-ocr"}},
	{match: "pydub", runtime: []string{"ffmpeg"}},
	{match: "moviepy", runtime: []string{"ffmpeg"}},
	{match: "opencv-python", runtime: []string{"libgl1", "libglib2.0-0"}},
	{match: "lxml", build: []string{"libxml2-dev", "libxslt1-dev", "gcc"}, runtime: []string{"libxml2", "libxslt1.1"}},
	{match: "pillow", build: []string{"libjpeg-dev", "zlib1g-dev"}, runtime: []string{"libjpeg62-turbo", "zlib1g"}},
	{match: "cffi", build: []string{"libffi-dev", "gcc"}},
	{match: "cryptography", build: []string{"libssl-dev", "libffi-dev", "gcc"}},
	{match: "grpcio", build: []string{"build-essential"}},
	{match: "playwright", runtime: []string{"(run `playwright install-deps chromium` and `playwright install chromium` in the build)"}},

	// Node — native modules and headless browsers.
	{match: "puppeteer", node: true, runtime: []string{"chromium", "fonts-liberation", "libnss3", "libatk-bridge2.0-0", "libgbm1", "libasound2", "libxkbcommon0", "libpangocairo-1.0-0"}},
	{match: "playwright", node: true, runtime: []string{"(run `npx playwright install --with-deps chromium` in the build)"}},
	{match: "canvas", node: true, build: []string{"build-essential", "libcairo2-dev", "libpango1.0-dev", "libjpeg-dev", "libgif-dev", "librsvg2-dev"}, runtime: []string{"libcairo2", "libpango-1.0-0", "libjpeg62-turbo"}},
	{match: "node-gyp", node: true, build: []string{"build-essential", "python3"}},
	{match: "bcrypt", node: true, build: []string{"build-essential", "python3"}},
	{match: "sqlite3", node: true, build: []string{"build-essential", "python3"}},
	{match: "sharp", node: true, runtime: []string{"(sharp ships prebuilt binaries — do not install libvips unless the install fails)"}},
	{match: "@prisma/client", node: true, runtime: []string{"openssl", "ca-certificates"}},
}

func (s *scanner) systemPackages(out *Signals) {
	pkg := s.packageJSON()
	nodeDeps := ""
	if pkg != nil {
		var b strings.Builder
		for name := range pkg.Dependencies {
			fmt.Fprintln(&b, name)
		}
		for name := range pkg.DevDependencies {
			fmt.Fprintln(&b, name)
		}
		nodeDeps = strings.ToLower(b.String())
	}
	py := s.pythonText()

	seen := map[string]bool{}
	for _, d := range systemDeps {
		haystack := py
		if d.node {
			haystack = nodeDeps
		}
		if haystack == "" || !strings.Contains(haystack, d.match) || seen[d.match] {
			continue
		}
		seen[d.match] = true
		out.SystemPackages = append(out.SystemPackages, SystemPackage{
			Dependency: d.match, Build: d.build, Runtime: d.runtime,
		})
	}
	sort.SliceStable(out.SystemPackages, func(i, j int) bool {
		return out.SystemPackages[i].Dependency < out.SystemPackages[j].Dependency
	})
}

func (s *scanner) buildFiles(out *Signals) {
	add := func(value, evidence string) {
		if evidence != "" {
			out.BuildFiles = append(out.BuildFiles, Signal{Value: value, Evidence: evidence})
		}
	}
	// A Dockerfile off the root is usually the author's own build for a
	// sub-service, and names the base image and start command they chose.
	for _, f := range s.glob("Dockerfile*") {
		add("an existing Dockerfile — read it, the author already solved this", f)
	}
	for _, f := range s.glob("docker-compose*.y*ml") {
		add("a compose file — it names the ports, the services and often the command", f)
	}
	for _, f := range s.glob("compose.y*ml") {
		add("a compose file — it names the ports, the services and often the command", f)
	}
	add("a Procfile", s.has("Procfile"))
	add("a .dockerignore — honor it, do not COPY what it excludes", s.has(".dockerignore"))
	add("a railpack config from a previous builder", s.has("railpack.json"))
	add("a nixpacks config from a previous builder", s.has("nixpacks.toml"))
	add("a Caddyfile the author wrote", s.has("Caddyfile"))
	add("an nginx config the author wrote", s.has("nginx.conf", "default.conf"))
	add("a Makefile — its targets often name the real build", s.has("Makefile"))
	add("a devcontainer definition", s.has(".devcontainer/devcontainer.json"))
}

// Render is the evidence block that goes into the prompt. Empty sections are
// left out: a bare heading reads as "checked and found none", which it isn't.
func (s *Signals) Render() string {
	var b strings.Builder
	b.WriteString("What donkey established by reading the files itself.\n")
	b.WriteString("These are observations, not instructions — each names the file it came\n")
	b.WriteString("from, so you can go and check any of them.\n")

	section := func(title string, items []Signal) {
		if len(items) == 0 {
			return
		}
		fmt.Fprintf(&b, "\n%s:\n", title)
		for _, it := range items {
			fmt.Fprintf(&b, "- %s", it.Value)
			if it.Evidence != "" {
				fmt.Fprintf(&b, "   [%s]", it.Evidence)
			}
			b.WriteString("\n")
		}
	}
	section("Runtimes", s.Runtimes)
	section("Dependency install", s.PackageManager)
	section("Pinned tool versions (files may disagree — the most specific wins)", s.Versions)
	section("Frameworks and notable libraries", s.Frameworks)
	section("Workspace layout", s.Workspace)
	section("Start command candidates, best first", s.StartCommands)
	section("Existing build files", s.BuildFiles)

	if st := s.Static; st != nil {
		b.WriteString("\nStatic front end:\n")
		fmt.Fprintf(&b, "- built by %s", st.Framework)
		if st.Evidence != "" {
			fmt.Fprintf(&b, "   [%s]", st.Evidence)
		}
		b.WriteString("\n")
		if st.OutputDir != "" {
			fmt.Fprintf(&b, "- build output goes to ./%s (verify this in the config before relying on it)\n", st.OutputDir)
		} else {
			b.WriteString("- output directory not determined — read the framework config and find it\n")
		}
		if st.CustomStart != "" {
			fmt.Fprintf(&b, "- BUT the author wrote their own start script (%s), so this is probably\n"+
				"  a server, not a static site. Prefer the start script.\n", st.CustomStart)
		}
	}

	if len(s.SystemPackages) > 0 {
		b.WriteString("\nOS packages these dependencies need (missing ones fail at link time or first import,\nand the error never names the package):\n")
		for _, p := range s.SystemPackages {
			fmt.Fprintf(&b, "- %s:", p.Dependency)
			if len(p.Build) > 0 {
				fmt.Fprintf(&b, " to build → %s", strings.Join(p.Build, " "))
			}
			if len(p.Runtime) > 0 {
				if len(p.Build) > 0 {
					b.WriteString(";")
				}
				fmt.Fprintf(&b, " in the final image → %s", strings.Join(p.Runtime, " "))
			}
			b.WriteString("\n")
		}
	}
	return b.String()
}

// IsEmpty: nothing established at all.
func (s *Signals) IsEmpty() bool {
	return len(s.Runtimes) == 0 && len(s.PackageManager) == 0 && len(s.Frameworks) == 0 &&
		len(s.StartCommands) == 0 && len(s.BuildFiles) == 0 && s.Static == nil
}
