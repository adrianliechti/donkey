package server

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"io/fs"
	"maps"
	"net"
	"net/http"
	"net/url"
	"os"
	"path"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"time"

	"github.com/aymanbagabas/go-udiff"
	"golang.org/x/sync/errgroup"

	"github.com/adrianliechti/donkey/internal/addons"
	"github.com/adrianliechti/donkey/internal/ai"
	"github.com/adrianliechti/donkey/internal/config"
)

// ProjectPlan is one review and one deploy for every app in an upload.
type ProjectPlan struct {
	ID          string           `json:"id"`
	Apps        []*Plan          `json:"apps"`
	Connections []PlanConnection `json:"connections"`
	Changes     []PlanChange     `json:"changes"`
	Findings    []Finding        `json:"findings"`
	Ready       bool             `json:"ready"`
}

type PlanConnection struct {
	App    string `json:"app"`
	Target string `json:"target"`
	Env    string `json:"env,omitempty"`
	Value  string `json:"value,omitempty"`
	Reason string `json:"reason"`
}

type PlanChange struct {
	File string `json:"file"`
	Diff string `json:"diff"`
}

type projectState struct {
	apps    []*storedPlan
	reading *ai.Project
	files   map[string]string // bounded source for validating and reviewing edits
	digest  string
}

type projectPatch struct{ file, before, after string }

const maxProjectApps = 16

// Uploads are bound to their reviewed source. A second upload must not apply
// reviewed edits to different code, or deploy an unreviewed sibling app.
func contextDigest(root string) (string, error) {
	h := sha256.New()
	err := filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			return nil
		}
		rel, err := filepath.Rel(root, path)
		if err != nil {
			return err
		}
		info, err := d.Info()
		if err != nil {
			return err
		}
		fmt.Fprintf(h, "%s\x00%d\x00%d\x00", filepath.ToSlash(rel), info.Mode().Perm(), info.Size())
		f, err := os.Open(path)
		if err != nil {
			return err
		}
		_, err = io.Copy(h, f)
		f.Close()
		return err
	})
	return hex.EncodeToString(h.Sum(nil)), err
}

func projectPath(root, path string) (string, string, error) {
	rel, ok := safeRelPath(path)
	if !ok {
		return "", "", fmt.Errorf("%q is outside the uploaded project", path)
	}
	return filepath.Join(root, rel), filepath.ToSlash(rel), nil
}

func (s *Server) handleProjectPlan(w http.ResponseWriter, r *http.Request) {
	if s.draining.Load() {
		writeErr(w, http.StatusServiceUnavailable, fmt.Errorf("the server is restarting — try again in a moment"))
		return
	}
	reader, ok := s.assistant.(ai.ProjectReader)
	if !ok {
		writeErr(w, http.StatusServiceUnavailable, fmt.Errorf("the assistant cannot read multi-app projects"))
		return
	}
	dir, err := os.MkdirTemp("", "donkey-project-*")
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err)
		return
	}
	defer os.RemoveAll(dir)
	skipped, err := extractContext(r.Body, dir)
	if err != nil {
		writeErr(w, http.StatusBadRequest, err)
		return
	}
	ctx, cancel := context.WithCancel(r.Context())
	defer cancel()
	reading, err := reader.ReadProject(ctx, dir)
	if err != nil {
		writeErr(w, http.StatusBadGateway, err)
		return
	}
	state, err := s.readProject(ctx, dir, reading, planInput{Space: r.URL.Query().Get("space"), NameHint: r.URL.Query().Get("name"), Skipped: skipped})
	if err != nil {
		writeErr(w, http.StatusBadRequest, err)
		return
	}
	stored := &storedPlan{project: state, created: time.Now()}
	id := s.plans.put(stored)
	plan, _, _ := s.resolveProject(ctx, id, state)
	writeJSON(w, http.StatusOK, plan)
}

func (s *Server) readProject(ctx context.Context, dir string, reading *ai.Project, in planInput) (*projectState, error) {
	if reading == nil || len(reading.Apps) == 0 {
		return nil, fmt.Errorf("no runnable apps were found in this project")
	}
	if len(reading.Apps) > maxProjectApps || len(reading.Connections) > 128 {
		return nil, fmt.Errorf("a project upload supports up to %d apps and 128 connections", maxProjectApps)
	}
	state := &projectState{reading: reading, apps: make([]*storedPlan, len(reading.Apps)), files: map[string]string{}}
	var err error
	state.digest, err = contextDigest(dir)
	if err != nil {
		return nil, err
	}
	seen := map[string]bool{}
	// Paths are normalized before reading; duplicates are errors, never a
	// second deployment that quietly overwrites the first app.
	for i := range reading.Apps {
		a := &reading.Apps[i]
		full, rel, err := projectPath(dir, a.Path)
		if err != nil {
			return nil, err
		}
		if seen[rel] {
			return nil, fmt.Errorf("app folder %s was listed twice", rel)
		}
		seen[rel], a.Path = true, rel
		if info, err := os.Stat(full); err != nil || !info.IsDir() {
			return nil, fmt.Errorf("app folder %s is missing", rel)
		}
		if a.Context == "" {
			a.Context = a.Path
		}
	}
	spaces := make([]string, len(reading.Apps))
	group, readCtx := errgroup.WithContext(ctx)
	group.SetLimit(3)
	for i, found := range reading.Apps {
		group.Go(func() error {
			full := filepath.Join(dir, filepath.FromSlash(found.Path))
			app, err := config.LoadApp(full)
			if err != nil {
				return fmt.Errorf("%s: %w", found.Path, err)
			}
			setting := ""
			if app != nil {
				spaces[i], setting = app.Space, app.Build.Context
			}
			if setting == "" {
				contextDir, _, err := projectPath(dir, found.Context)
				if err != nil {
					return fmt.Errorf("%s: %w", found.Path, err)
				}
				setting, _ = filepath.Rel(full, contextDir)
			}
			if _, err := buildContext(dir, full, setting); err != nil {
				return fmt.Errorf("%s: %w", found.Path, err)
			}
			input := in
			input.Root = found.Path
			if len(reading.Apps) > 1 || input.NameHint == "" {
				input.NameHint = found.Name
				if input.NameHint == "" {
					input.NameHint = filepath.Base(full)
				}
			}
			sp, err := s.readApp(readCtx, full, app, input)
			if err != nil {
				return fmt.Errorf("%s: %w", found.Path, err)
			}
			for _, variable := range found.Env {
				if variable.Name == "PORT" {
					continue
				}
				if sp.app.Env == nil {
					sp.app.Env = map[string]string{}
				}
				if _, configured := sp.app.Env[variable.Name]; !configured {
					sp.app.Env[variable.Name] = variable.Value
				}
			}
			if setting != "." {
				sp.app.Build.Context = filepath.ToSlash(setting)
			}
			state.apps[i] = sp
			return nil
		})
	}
	if err := group.Wait(); err != nil {
		return nil, err
	}
	// All siblings share one space; an explicit caller choice wins. Otherwise
	// keep an agreed donkey.yaml space, or ask through a blocking finding.
	space := in.Space
	if space == "" {
		for _, configured := range spaces {
			if configured != "" {
				space = configured
				break
			}
		}
	}
	if space == "" {
		space = config.DefaultSpace
	}
	for i, sp := range state.apps {
		if in.Space != "" || spaces[i] == "" {
			sp.app.Space = space
		}
		s.adoptConsoleChoices(ctx, sp.app.Space, sp.app)
	}
	sourceSize := 0
	for i := range reading.Connections {
		c := &reading.Connections[i]
		_, c.App, err = projectPath(dir, c.App)
		if err != nil {
			return nil, err
		}
		if c.Kind != "service" {
			_, c.Target, err = projectPath(dir, c.Target)
			if err != nil {
				return nil, err
			}
		} else if knownAddon(c.Target) {
			// The repository reading may see a connection the per-app reading
			// missed. Offer it once, before any review choices are made.
			for _, sp := range state.apps {
				if sp.root == c.App && !hasAddonType(sp.app, c.Target) {
					sp.app.Addons = append(sp.app.Addons, config.AddonConfig{Type: c.Target, Name: c.Target})
					sp.services = append(sp.services, PlanService{Type: c.Target, Name: c.Target, Known: true, Selected: true, Reason: c.Reason})
				}
			}
		}
		if c.File == "" {
			continue
		}
		full, rel, err := projectPath(dir, c.File)
		if err != nil {
			return nil, err
		}
		c.File = rel
		if _, loaded := state.files[rel]; loaded {
			continue
		}
		if config.SensitiveUploadFile(filepath.Base(rel)) {
			return nil, fmt.Errorf("cannot repair excluded file %s", rel)
		}
		info, err := os.Stat(full)
		if err != nil || !info.Mode().IsRegular() || info.Size() > 1<<20 {
			return nil, fmt.Errorf("cannot read connection source %s", rel)
		}
		data, err := os.ReadFile(full)
		if err != nil {
			return nil, err
		}
		state.files[rel] = string(data)
		sourceSize += len(data)
		if sourceSize > 2<<20 {
			return nil, fmt.Errorf("connection edits span more than 2 MB of source — split this project into smaller uploads")
		}
	}
	return state, nil
}

func (s *Server) handleProjectUpdate(w http.ResponseWriter, r *http.Request) {
	stored, ok := s.plans.get(r.PathValue("id"))
	if !ok || stored.project == nil {
		writeErr(w, http.StatusGone, fmt.Errorf("this project plan expired — upload the project again"))
		return
	}
	var edits []PlanEdit
	if err := json.NewDecoder(r.Body).Decode(&edits); err != nil {
		writeErr(w, http.StatusBadRequest, err)
		return
	}
	stored.mu.Lock()
	defer stored.mu.Unlock()
	if len(edits) != len(stored.project.apps) {
		writeErr(w, http.StatusBadRequest, fmt.Errorf("send one edit for each app"))
		return
	}
	// Validate the entire answer before changing any app.
	for i, edit := range edits {
		if err := edit.validate(); err != nil {
			writeErr(w, http.StatusBadRequest, fmt.Errorf("%s: %w", stored.project.apps[i].root, err))
			return
		}
	}
	for i, edit := range edits {
		applyPlanEdit(stored.project.apps[i], edit)
	}
	plan, _, _ := s.resolveProject(r.Context(), r.PathValue("id"), stored.project)
	writeJSON(w, http.StatusOK, plan)
}

var projectEnvRE = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]*$`)

func (s *Server) resolveProject(ctx context.Context, id string, state *projectState) (*ProjectPlan, []*storedPlan, []projectPatch) {
	p := &ProjectPlan{ID: id, Apps: []*Plan{}, Connections: []PlanConnection{}, Changes: []PlanChange{}, Findings: []Finding{}}
	apps := make([]*storedPlan, len(state.apps))
	byPath := map[string]*storedPlan{}
	names, hosts := map[string]bool{}, map[string]bool{}
	block := func(text string) {
		p.Findings = append(p.Findings, Finding{Level: LevelBlock, Code: "project-connection", Title: text})
	}
	for i, base := range state.apps {
		// A recheck starts from the user's choices, not last recheck's generated
		// URLs/routes. Renaming or moving a project cannot leave stale links.
		sp := &storedPlan{app: base.appCopy(), analysis: base.analysis, volumes: slices.Clone(base.volumes), services: slices.Clone(base.services), build: base.build, root: base.root, hasConfig: base.hasConfig, portSource: base.portSource, skipped: base.skipped, appType: base.appType, notes: base.notes, created: base.created}
		apps[i], byPath[sp.root] = sp, sp
		if sp.app.Space != state.apps[0].app.Space {
			block("All apps in this project must use the same space.")
		}
		if names[sp.app.Name] {
			block(fmt.Sprintf("Two apps are named %s — give each app a different name.", sp.app.Name))
		}
		names[sp.app.Name] = true
		if host := s.cfg.AppURL(sp.app.Space, sp.app); host != "" {
			if hosts[host] {
				block("Two apps claim the same address: " + host)
			}
			hosts[host] = true
		}
	}
	services := map[string]config.AddonConfig{}
	for _, sp := range apps {
		for _, service := range sp.app.Addons {
			if names[service.Name] {
				block("An app and a service are both named " + service.Name + ". Give them different names.")
			}
			if previous, exists := services[service.Name]; exists {
				if previous.Type != service.Type || previous.Version != "" && service.Version != "" && previous.Version != service.Version || previous.Storage != "" && service.Storage != "" && previous.Storage != service.Storage {
					block("Apps disagree on the settings for shared service " + service.Name + ". Use the same type, version and storage settings.")
				}
				if service.Version == "" {
					service.Version = previous.Version
				}
				if service.Storage == "" {
					service.Storage = previous.Storage
				}
				service.External = service.External || previous.External
			}
			services[service.Name] = service
		}
	}
	for _, sp := range apps {
		for i, service := range sp.app.Addons {
			sp.app.Addons[i] = services[service.Name]
		}
		if err := s.bindEnvironment(ctx, sp.app); err != nil {
			block(sp.app.Name + ": " + err.Error())
		}
	}
	files := maps.Clone(state.files)
	values := map[string]string{}
	for _, c := range state.reading.Connections {
		sp := byPath[c.App]
		if sp == nil {
			block("A connection names an unknown app: " + c.App)
			continue
		}
		app := sp.app
		value, target := "", c.Target
		if c.Env == "" && c.File == "" && c.Kind != "browser" {
			block("A connection for " + app.Name + " needs an environment variable or source edit.")
			continue
		}
		if c.Env != "" && !projectEnvRE.MatchString(c.Env) {
			block("Invalid connection variable: " + c.Env)
			continue
		}
		if c.Kind == "service" {
			recipe, err := addons.Get(c.Target)
			if err != nil {
				block(err.Error())
				continue
			}
			if _, exists := recipe.Binding(addons.Input{})[c.Key]; !exists {
				block("Unknown binding key for " + c.Target + ": " + c.Key)
				continue
			}
			var matches []config.AddonConfig
			for _, a := range app.Addons {
				if a.Type == c.Target {
					matches = append(matches, a)
				}
			}
			if len(matches) != 1 {
				block(fmt.Sprintf("%s needs exactly one selected %s service for this connection.", app.Name, c.Target))
				continue
			}
			target = matches[0].Name
			value = config.EnvRef(target, c.Key)
			if c.Suffix != "" || strings.Contains(c.New, "{{url}}") {
				block("Service connections must read runtime environment variables: " + app.Name)
				continue
			}
			if config.PublicEnv(c.Env) {
				block("Service credentials cannot be included in browser variable " + c.Env)
				continue
			}
		} else {
			to := byPath[c.Target]
			if to == nil || to == sp {
				block("A connection needs a different app in this project: " + c.Target)
				continue
			}
			target = to.app.Name
			if !validURLPath(c.Suffix) {
				block("Invalid URL path for " + target)
				continue
			}
			switch c.Kind {
			case "browser":
				if app.Internal {
					block(app.Name + " needs an address for its browser connections.")
					continue
				}
				if c.Env == "" && c.File == "" {
					// A same-origin caller may already be connected by donkey.yaml.
					// Require a declared route covering the path the caller uses.
					routed := c.Suffix != "" && slices.ContainsFunc(to.app.Routes, func(route config.RouteConfig) bool {
						return route.App == app.Name && (route.Path == c.Suffix || strings.HasPrefix(c.Suffix, strings.TrimRight(route.Path, "/")+"/"))
					})
					if !routed {
						block("A connection for " + app.Name + " needs an existing route, environment variable or source edit.")
						continue
					}
					value = c.Suffix
				} else {
					prefix := "/_donkey/" + target
					value = prefix + c.Suffix
					route := config.RouteConfig{App: app.Name, Path: prefix, StripPrefix: true}
					if !slices.Contains(to.app.Routes, route) {
						to.app.Routes = append(to.app.Routes, route)
					}
				}
				if app.Public && !to.app.Public {
					block(app.Name + " is public, but " + target + " requires login. Match their exposure to allow browser calls.")
				}
			case "server", "address":
				if config.PublicEnv(c.Env) {
					block("Internal server URLs cannot be used in browser variable " + c.Env)
					continue
				}
				value = s.rt.InternalURL(app.Space, target, to.app.Port) + c.Suffix
				if c.Kind == "address" {
					if c.Suffix != "" {
						block("A host:port connection cannot have a URL path: " + target)
						continue
					}
					u, err := url.Parse(value)
					if err != nil || u.Hostname() == "" {
						block("No internal address is available for " + target)
						continue
					}
					port := u.Port()
					if port == "" {
						port = "80"
					}
					value = net.JoinHostPort(u.Hostname(), port)
				}
			case "origin":
				value = s.cfg.AppURL(to.app.Space, to.app) + c.Suffix
				if to.app.Internal {
					block(target + " has no public origin.")
					continue
				}
			default:
				block("Unknown connection kind: " + c.Kind)
				continue
			}
		}
		if c.Env != "" {
			key := c.App + ":" + c.Env
			if prev, ok := values[key]; ok && prev != value {
				block("Conflicting values for " + app.Name + "." + c.Env)
				continue
			}
			values[key] = value
			if app.Env == nil {
				app.Env = map[string]string{}
			}
			app.Env[c.Env] = value
			_, baked := app.Build.Env[c.Env]
			if c.Kind != "service" && (c.Kind == "browser" || config.PublicEnv(c.Env) || baked) {
				if app.Build.Env == nil {
					app.Build.Env = map[string]string{}
				}
				app.Build.Env[c.Env] = value
			}
		}
		p.Connections = append(p.Connections, PlanConnection{App: app.Name, Target: target, Env: c.Env, Value: value, Reason: c.Reason})
		if c.File != "" {
			before, exists := files[c.File]
			if !exists || c.Old == "" || strings.Count(before, c.Old) != 1 {
				block("The connection edit in " + c.File + " does not match exactly once — upload and read the project again.")
				continue
			}
			after := strings.ReplaceAll(c.New, "{{url}}", value)
			files[c.File] = strings.Replace(before, c.Old, after, 1)
		}
	}
	var patches []projectPatch
	for _, file := range slices.Sorted(maps.Keys(files)) {
		after, before := files[file], state.files[file]
		if before == after {
			continue
		}
		patches = append(patches, projectPatch{file: file, before: before, after: after})
		p.Changes = append(p.Changes, PlanChange{File: file, Diff: ai.Redact(udiff.Unified(file, file, before, after))})
	}
	for _, sp := range apps {
		analysis := *sp.analysis
		analysis.LocalURLs = slices.DeleteFunc(slices.Clone(analysis.LocalURLs), func(u ai.LocalURL) bool {
			file := path.Join(sp.root, u.File)
			if _, exists := files[file]; !exists {
				file = u.File
			}
			after, changed := files[file]
			return changed && strings.Contains(state.files[file], u.URL) && !strings.Contains(after, u.URL)
		})
		sp.analysis = &analysis

		for i := range sp.services {
			sp.services[i].Exists = s.spaceHasService(ctx, sp.app.Space, sp.services[i].Name)
		}
		p.Apps = append(p.Apps, s.renderPlan(ctx, sp.root, sp))
	}
	p.Ready = !Blocking(p.Findings)
	for _, app := range p.Apps {
		p.Ready = p.Ready && app.Ready
	}
	return p, apps, patches
}

// Keep the original API path without allowing URL normalization to escape
// the sibling's proxy prefix. Query strings and fragments are not paths.
func validURLPath(raw string) bool {
	if raw == "" {
		return true
	}
	u, err := url.Parse(raw)
	if err != nil || u.RawQuery != "" || u.ForceQuery || u.Fragment != "" || strings.Contains(raw, "#") || u.Host != "" {
		return false
	}
	if !strings.HasPrefix(u.Path, "/") || strings.HasPrefix(u.Path, "//") || strings.ContainsAny(u.Path, "\\\"'`") {
		return false
	}
	for _, r := range u.Path {
		if r < 0x20 || r == 0x7f {
			return false
		}
	}
	for segment := range strings.SplitSeq(u.Path, "/") {
		if segment == "." || segment == ".." {
			return false
		}
	}
	return true
}
