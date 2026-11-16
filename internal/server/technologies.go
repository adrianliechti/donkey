package server

import (
	"context"
	"net/http"
	"net/url"
	"regexp"
	"slices"
	"sort"
	"strings"
	"time"

	"github.com/adrianliechti/donkey/internal/config"
	"github.com/adrianliechti/donkey/internal/detect"
)

// The technology report is what the fleet is made of: the languages, the
// frameworks, the base images and operating systems, and how apps install
// their dependencies — each with the apps that use it, the most used first.
// Nothing is detected for it. Every build already records what donkey read
// off the app (the plan's signals, the Dockerfile's base images) and every
// scan names the operating system; this is those records, side by side.

// Technology categories, in the order a report lists them.
const (
	TechnologyLanguage       = "language"
	TechnologyFramework      = "framework"
	TechnologyBaseImage      = "base-image"
	TechnologyOS             = "os"
	TechnologyPackageManager = "package-manager"
)

var technologyCategories = []string{
	TechnologyLanguage, TechnologyFramework, TechnologyBaseImage, TechnologyOS, TechnologyPackageManager,
}

// TechnologyReport is what the running apps are built with.
type TechnologyReport struct {
	GeneratedAt string `json:"generatedAt"`
	// Apps is every running app, whether or not anything is known about it.
	Apps       []ReportApp          `json:"apps"`
	Categories []TechnologyCategory `json:"categories"`
	// Spaces names who answers for each space with an app; PlatformAdmins
	// who runs the installation.
	Spaces         map[string]SpaceContacts `json:"spaces"`
	PlatformAdmins config.AccessConfig      `json:"platformAdmins"`
}

// TechnologyCategory is one kind of technology, most used first.
type TechnologyCategory struct {
	ID           string       `json:"id"`
	Technologies []Technology `json:"technologies"`
}

// Technology is one thing apps are built with, and the apps that are.
type Technology struct {
	Name string `json:"name"`
	// Key is the language's short name where the console has a mark for it
	// (python, node, go, …), the same word an app's stack is.
	Key string `json:"key,omitempty"`
	// EndOfLife marks an operating system release its vendor stopped
	// patching, as the scan of an app on it said.
	EndOfLife bool            `json:"endOfLife,omitempty"`
	Apps      []TechnologyApp `json:"apps"`
}

// TechnologyApp is one app using a technology.
type TechnologyApp struct {
	Space string `json:"space"`
	App   string `json:"app"`
	// Version is the one this app is on, where its records say; Evidence
	// the file or image the detection rests on.
	Version  string `json:"version,omitempty"`
	Evidence string `json:"evidence,omitempty"`
}

// languages are the names donkey has for what an app is written in: the key
// an app's stack carries (detect.Stack), and the word for people.
var languages = []struct{ key, name string }{
	{"python", "Python"}, {"node", "Node.js"}, {"bun", "Bun"}, {"deno", "Deno"}, {"go", "Go"},
	{"java", "Java"}, {"ruby", "Ruby"}, {"php", "PHP"}, {"rust", "Rust"}, {"dotnet", ".NET"},
	{"elixir", "Elixir"}, {"gleam", "Gleam"}, {"c", "C/C++"}, {"shell", "Shell"}, {detect.StackStatic, "Static site"},
}

// languageNames maps what a detection signal or a version file calls a
// language to its key.
var languageNames = map[string]string{
	"python": "python", "node.js": "node", "node": "node", "nodejs": "node", "bun": "bun", "deno": "deno",
	"go": "go", "golang": "go", "java": "java", "ruby": "ruby", "php": "php", "rust": "rust",
	".net": "dotnet", "dotnet": "dotnet", "elixir": "elixir", "gleam": "gleam", "c/c++": "c",
	"a shell script": "shell", "static files": detect.StackStatic,
}

func languageName(key string) string {
	for _, language := range languages {
		if language.key == key {
			return language.name
		}
	}
	return key
}

// signalName is a detection signal as a name. The signals are written for
// whoever builds the app and carry advice after the name — "Prisma (needs
// `prisma generate` during the build)", "Vite → ./dist" — which a list of
// what is in use has no place for.
func signalName(value string) string {
	for _, cut := range []string{" (", " → ", " — "} {
		if i := strings.Index(value, cut); i >= 0 {
			value = value[:i]
		}
	}
	return strings.TrimSpace(value)
}

var leadingVersion = regexp.MustCompile(`^v?(\d+(?:\.\d+)?)`)

// imageVersion reads a version off an image's tag: python:3.12-slim is 3.12,
// node:20.11.1-alpine is 20.11. A tag that names no number (lts, latest) has
// none.
func imageVersion(image string) string {
	if m := leadingVersion.FindStringSubmatch(imageTag(image)); m != nil {
		return m[1]
	}
	return ""
}

// baseImageName is a base image the way two Dockerfiles would both write it:
// without a digest, and without Docker Hub's implied prefixes.
func baseImageName(image string) string {
	image = strings.TrimSpace(image)
	if at := strings.Index(image, "@"); at >= 0 {
		image = image[:at]
	}
	for _, prefix := range []string{"docker.io/", "index.docker.io/", "registry-1.docker.io/"} {
		image = strings.TrimPrefix(image, prefix)
	}
	return strings.TrimPrefix(image, "library/")
}

// osRelease is an operating system by the release that gets patched as one:
// debian 12.5 and 12.7 are both Debian 12, alpine 3.20.3 is Alpine 3.20.
func osRelease(os string) string {
	family, version, _ := strings.Cut(strings.TrimSpace(os), " ")
	parts := strings.Split(version, ".")
	keep := 2
	if family == "debian" {
		keep = 1
	}
	if len(parts) > keep {
		parts = parts[:keep]
	}
	return strings.TrimSpace(family + " " + strings.Join(parts, "."))
}

// packageManager is the tool behind a "dependency install" signal — "Node:
// pnpm@9.1.0 (declared, wins over lockfiles)" is pnpm 9.1.0 — or "" for a
// signal that names a constraint rather than a tool.
func packageManager(value string) (name, version string) {
	_, tool, found := strings.Cut(value, ": ")
	if !found || strings.HasPrefix(tool, "engines.") {
		return "", ""
	}
	for _, cut := range []string{" (", ",", " — ", " = "} {
		if i := strings.Index(tool, cut); i >= 0 {
			tool = tool[:i]
		}
	}
	tool, version, _ = strings.Cut(strings.TrimSpace(tool), "@")
	return strings.TrimPrefix(tool, "the "), version
}

// technologyBuild collects one category's technologies by name.
type technologyBuild struct {
	byName map[string]*Technology
}

// use records that an app uses a technology — once, however many signals
// said so. The first to say it keeps its version and evidence.
func (b *technologyBuild) use(name, key string, app inventoryApp, version, evidence string) *Technology {
	if name == "" {
		return nil
	}
	if b.byName == nil {
		b.byName = map[string]*Technology{}
	}
	id := strings.ToLower(name)
	technology := b.byName[id]
	if technology == nil {
		technology = &Technology{Name: name, Key: key}
		b.byName[id] = technology
	}
	for i := range technology.Apps {
		if have := &technology.Apps[i]; have.Space == app.Space && have.App == app.App {
			if have.Version == "" {
				have.Version = version
			}
			return technology
		}
	}
	technology.Apps = append(technology.Apps, TechnologyApp{Space: app.Space, App: app.App, Version: version, Evidence: evidence})
	return technology
}

// list is the category most used first; equally used ones by name.
func (b *technologyBuild) list() []Technology {
	technologies := make([]Technology, 0, len(b.byName))
	for _, technology := range b.byName {
		technologies = append(technologies, *technology)
	}
	sort.Slice(technologies, func(i, j int) bool {
		if len(technologies[i].Apps) != len(technologies[j].Apps) {
			return len(technologies[i].Apps) > len(technologies[j].Apps)
		}
		return strings.ToLower(technologies[i].Name) < strings.ToLower(technologies[j].Name)
	})
	return technologies
}

// technologyReport reads the inventory's build and scan records as what the
// apps are made of.
func (s *Server) technologyReport(ctx context.Context, inventory *fleetInventory) *TechnologyReport {
	report := &TechnologyReport{
		GeneratedAt:    inventory.At.UTC().Format(time.RFC3339),
		Apps:           s.reportApps(inventory),
		Spaces:         reportContacts(inventory),
		PlatformAdmins: platformAdmins(ctx),
	}
	categories := map[string]*technologyBuild{}
	for _, id := range technologyCategories {
		categories[id] = &technologyBuild{}
	}

	for _, app := range inventory.Apps {
		var found PlanBuild
		if app.Facts != nil && app.Facts.build != nil {
			found = *app.Facts.build
		}

		// A language is known from the image an app is built on (its stack)
		// and from the manifests in its source. The image also says which
		// version actually runs; a version file only says what was asked for.
		versions := map[string]string{}
		evidence := map[string]string{}
		for _, image := range found.BaseImages {
			key := detect.ImageStack(image)
			if key == "" || evidence[key] != "" {
				continue
			}
			evidence[key] = baseImageName(image)
			// A site served as files has no language to be a version of: the
			// tag is its web server's.
			if key != detect.StackStatic {
				versions[key] = imageVersion(image)
			}
		}
		for _, signal := range found.Versions {
			tool, version, _ := strings.Cut(signalName(signal.Value), " ")
			if key := languageNames[strings.ToLower(tool)]; key != "" && versions[key] == "" {
				versions[key] = strings.TrimSpace(version)
			}
		}
		if key := app.Status.Stack; key != "" {
			categories[TechnologyLanguage].use(languageName(key), key, app, versions[key], evidence[key])
		}
		for _, signal := range found.Runtimes {
			name := signalName(signal.Value)
			if key := languageNames[strings.ToLower(name)]; key != "" {
				categories[TechnologyLanguage].use(languageName(key), key, app, versions[key], signal.Evidence)
			} else {
				categories[TechnologyLanguage].use(name, "", app, "", signal.Evidence)
			}
		}

		for _, signal := range found.Frameworks {
			categories[TechnologyFramework].use(signalName(signal.Value), "", app, "", signal.Evidence)
		}
		for _, image := range found.BaseImages {
			if name := baseImageName(image); name != "" && name != "scratch" {
				categories[TechnologyBaseImage].use(name, "", app, "", "")
			}
		}
		for _, signal := range found.PackageManagers {
			name, version := packageManager(signal.Value)
			categories[TechnologyPackageManager].use(name, "", app, version, signal.Evidence)
		}
		if app.Facts != nil && app.Facts.scan != nil && app.Facts.scan.OS != "" {
			scan := app.Facts.scan
			if os := categories[TechnologyOS].use(osRelease(scan.OS), "", app, scan.OS, ""); os != nil && scan.EndOfLife {
				os.EndOfLife = true
			}
		}
	}

	for _, id := range technologyCategories {
		report.Categories = append(report.Categories, TechnologyCategory{ID: id, Technologies: categories[id].list()})
	}
	return report
}

// technologyFilter narrows a report the way the console's list does. The
// console applies the same rules in technologies.ts — change both together.
type technologyFilter struct {
	// categories empty means every category.
	categories []string
	query      string
}

func parseTechnologyFilter(values url.Values) technologyFilter {
	filter := technologyFilter{query: strings.ToLower(strings.TrimSpace(values.Get("q")))}
	for _, raw := range values["category"] {
		for category := range strings.SplitSeq(raw, ",") {
			if category = strings.TrimSpace(category); category != "" {
				filter.categories = append(filter.categories, category)
			}
		}
	}
	return filter
}

// apply keeps what matches: a query that names the technology keeps it
// whole, one that names a space or an app keeps it with only those apps.
func (f technologyFilter) apply(categories []TechnologyCategory) []TechnologyCategory {
	kept := []TechnologyCategory{}
	for _, category := range categories {
		if len(f.categories) > 0 && !slices.Contains(f.categories, category.ID) {
			continue
		}
		technologies := []Technology{}
		for _, technology := range category.Technologies {
			if f.query == "" || strings.Contains(strings.ToLower(technology.Name), f.query) {
				technologies = append(technologies, technology)
				continue
			}
			var apps []TechnologyApp
			for _, app := range technology.Apps {
				if strings.Contains(strings.ToLower(app.Space+"/"+app.App), f.query) {
					apps = append(apps, app)
				}
			}
			if len(apps) > 0 {
				technology.Apps = apps
				technologies = append(technologies, technology)
			}
		}
		category.Technologies = technologies
		kept = append(kept, category)
	}
	return kept
}

var technologiesCSVHeader = []string{
	"category", "technology", "version", "end_of_life", "space", "app", "exposure", "url", "evidence",
	"space_admins", "space_developers",
}

// technologyRows is the report as one row per technology and app.
func technologyRows(report *TechnologyReport) [][]string {
	apps := make(map[[2]string]ReportApp, len(report.Apps))
	for _, app := range report.Apps {
		apps[[2]string{app.Space, app.App}] = app
	}
	var rows [][]string
	for _, category := range report.Categories {
		for _, technology := range category.Technologies {
			endOfLife := ""
			if technology.EndOfLife {
				endOfLife = "yes"
			}
			for _, use := range technology.Apps {
				app := apps[[2]string{use.Space, use.App}]
				contacts := report.Spaces[use.Space]
				rows = append(rows, []string{
					category.ID, technology.Name, use.Version, endOfLife, use.Space, use.App, app.Exposure, app.URL, use.Evidence,
					contactList(contacts.Admins), contactList(contacts.Developers),
				})
			}
		}
	}
	return rows
}

// handleTechnologies: GET /v1/technologies — on donkey-orchestrator, which
// holds the build records. ?category= and ?q= narrow it; ?format=csv
// answers with a download.
func (s *Server) handleTechnologies(w http.ResponseWriter, r *http.Request) {
	asCSV, err := reportFormat(r)
	if err != nil {
		writeErr(w, http.StatusBadRequest, err)
		return
	}
	inventory, err := s.currentInventory(r.Context())
	if err != nil {
		writeErr(w, http.StatusBadGateway, err)
		return
	}
	report := s.technologyReport(r.Context(), inventory)
	report.Categories = parseTechnologyFilter(r.URL.Query()).apply(report.Categories)
	if asCSV {
		writeCSV(w, r, "technologies", technologiesCSVHeader, technologyRows(report))
		return
	}
	writeReport(w, r, report)
}
