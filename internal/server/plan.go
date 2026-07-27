package server

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/adrianliechti/donkey/internal/addons"
	"github.com/adrianliechti/donkey/internal/ai"
	"github.com/adrianliechti/donkey/internal/config"
	"github.com/adrianliechti/donkey/internal/detect"
)

// A plan is everything donkey works out about an app before building it, so
// that "deploy this folder" is a decision somebody reviews rather than a form
// somebody fills in. The console and `donkey init` both render one from here,
// so they cannot drift, and the deploy reuses its analysis.

// Plan is what a review screen (or `donkey init`) shows.
type Plan struct {
	// ID names this plan for the deploy that follows it. It expires — losing
	// one costs a re-upload, never a build.
	ID string `json:"id"`
	// App is the config the deploy would use, with everything donkey.yaml
	// left open already filled in.
	App   PlanApp   `json:"app"`
	Build PlanBuild `json:"build"`
	// Findings are what to know before saying yes. A block among them means
	// the deploy would refuse — better heard now than after the build.
	Findings []Finding `json:"findings"`
	// Root is the folder inside the upload that holds the app; empty means
	// the upload itself is the app.
	Root string `json:"root,omitempty"`
	// HasConfig says the app brought its own donkey.yaml. Then the file
	// decides and these fields only report what it says.
	HasConfig bool `json:"hasConfig"`
	// AppType and Notes are the assistant's own words about the app.
	AppType string `json:"appType,omitempty"`
	Notes   string `json:"notes,omitempty"`
	// The full set every time: a choice turned down stays visible and
	// unticked, because a control that vanishes when cleared can never be set
	// again. Both are things donkey *creates* — it never proposes a command to
	// run, because a model-written command executed by default is not a
	// feature worth having.
	Volumes  []PlanVolume  `json:"volumes"`
	Services []PlanService `json:"services"`
	// Skipped is what the upload lost on the way in (secret-shaped files,
	// build artifacts, VCS internals).
	Skipped []string `json:"skipped,omitempty"`
	// Config is the donkey.yaml this plan amounts to. A browser deploy has no
	// folder to write it back to, so it is shown for copying into the project.
	Config string `json:"config"`
	// Ready is false when something blocks the deploy.
	Ready     bool   `json:"ready"`
	ExpiresAt string `json:"expiresAt"`
}

// PlanApp is the app as this plan would deploy it.
type PlanApp struct {
	Name  string `json:"name"`
	Space string `json:"space"`
	Port  int    `json:"port"`
	// PortSource is donkey.yaml, the code, the PORT convention, or a guess —
	// a review screen would rather explain a port than ask for one.
	PortSource  string   `json:"portSource"`
	PortReason  string   `json:"portReason,omitempty"`
	Healthcheck string   `json:"healthcheck,omitempty"`
	Storage     []string `json:"storage"`
	StorageSize string   `json:"storageSize,omitempty"`
	// Exposure is public, protected or internal — how the app will be reached.
	Exposure string `json:"exposure"`
	// URL is the address the app will answer on (empty for internal).
	URL string `json:"url,omitempty"`
}

type PlanBuild struct {
	// Summary is the one-line description of how the app gets built.
	Summary  string `json:"summary"`
	Strategy string `json:"strategy"`
}

// PlanVolume is one folder whose contents have to survive a restart.
type PlanVolume struct {
	Path     string `json:"path"`
	Reason   string `json:"reason,omitempty"`
	Selected bool   `json:"selected"`
}

// PlanService is one backing service the app will have — declared in
// donkey.yaml, or proposed by the assistant.
type PlanService struct {
	Type   string `json:"type"`
	Name   string `json:"name"`
	Reason string `json:"reason,omitempty"`
	// Exists says the space already runs one under this name, so deploying
	// connects to it instead of creating a second.
	Exists bool `json:"exists"`
	// Known is false for a type donkey has no recipe for: it is reported,
	// never offered.
	Known    bool `json:"known"`
	Selected bool `json:"selected"`
}

// PlanEdit is the review screen's answer: what the person deploying decided
// after reading the plan. Every field is authoritative — an omitted storage
// list means "no disk", not "you decide".
type PlanEdit struct {
	Name        string   `json:"name"`
	Space       string   `json:"space"`
	Port        int      `json:"port"`
	Healthcheck string   `json:"healthcheck"`
	Storage     []string `json:"storage"`
	// Services names the services to have, by name.
	Services []string `json:"services"`
	// Exposure is public, protected or internal.
	Exposure string `json:"exposure"`
}

// planTTL bounds how long a plan stays valid. It has to outlast a careful
// read of the review screen and a slow upload, and nothing more.
const planTTL = 2 * time.Hour

// maxPlans caps the store. Plans are small (no build context, no source), so
// this is generous — it exists to bound memory, not to ration reviews.
const maxPlans = 256

// storedPlan is a plan as the server keeps it: the config it settled on, and
// the assistant's reading of the app that produced it.
type storedPlan struct {
	// mu guards the config and the choices, which every edit rewrites. The
	// console can have a debounced re-check in flight when the deploy's final
	// one starts, so two requests really do write this at once.
	mu       sync.Mutex
	app      *config.AppConfig
	analysis *ai.Analysis

	volumes  []PlanVolume
	services []PlanService
	build    PlanBuild

	root       string
	hasConfig  bool
	portSource string
	skipped    []string
	appType    string
	notes      string

	created time.Time
}

// config returns a copy: a deploy settles console choices and stored
// decisions into the config it runs, and must not write those back into a
// plan the user might deploy again.
func (p *storedPlan) appCopy() *config.AppConfig {
	p.mu.Lock()
	defer p.mu.Unlock()
	b, err := json.Marshal(p.app)
	if err != nil {
		return p.app
	}
	var out config.AppConfig
	if json.Unmarshal(b, &out) != nil {
		return p.app
	}
	out.ApplyDefaults()
	return &out
}

// reading is the assistant's answer this plan was built from, nil-safe so a
// deploy without a plan reads as "nobody has asked yet".
func (p *storedPlan) reading() *ai.Analysis {
	if p == nil {
		return nil
	}
	return p.analysis
}

type planStore struct {
	mu    sync.Mutex
	items map[string]*storedPlan
}

func newPlanStore() *planStore {
	return &planStore{items: map[string]*storedPlan{}}
}

func (s *planStore) put(p *storedPlan) string {
	var raw [16]byte
	rand.Read(raw[:])
	id := hex.EncodeToString(raw[:])
	s.mu.Lock()
	defer s.mu.Unlock()
	s.sweep()
	s.items[id] = p
	return id
}

func (s *planStore) get(id string) (*storedPlan, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	p, ok := s.items[id]
	if !ok || time.Since(p.created) > planTTL {
		return nil, false
	}
	return p, true
}

// sweep drops what expired, then the oldest of whatever is left over the cap.
// Called with the lock held.
func (s *planStore) sweep() {
	for id, p := range s.items {
		if time.Since(p.created) > planTTL {
			delete(s.items, id)
		}
	}
	for len(s.items) >= maxPlans {
		oldest, at := "", time.Now()
		for id, p := range s.items {
			if p.created.Before(at) {
				oldest, at = id, p.created
			}
		}
		if oldest == "" {
			return
		}
		delete(s.items, oldest)
	}
}

// handlePlan: POST /v1/plans?space=&path=&name=
// Body: the same upload a deploy takes (tar.gz from the CLI, zip from the
// console). Nothing is built and nothing on the cluster changes — this reads.
func (s *Server) handlePlan(w http.ResponseWriter, r *http.Request) {
	if s.draining.Load() {
		writeErr(w, http.StatusServiceUnavailable,
			fmt.Errorf("the donkey server is restarting — try again in a moment"))
		return
	}
	dir, err := os.MkdirTemp("", "donkey-plan-*")
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
	dir, root, err := contextRoot(dir, r.URL.Query().Get("path"))
	if err != nil {
		writeErr(w, http.StatusBadRequest, err)
		return
	}
	app, err := config.LoadApp(dir)
	if err != nil {
		writeErr(w, http.StatusBadRequest, err)
		return
	}
	// The assistant is the slow part; it gets the same budget a deploy gives
	// it, and no more.
	ctx, cancel := context.WithTimeout(r.Context(), analysisDeadline)
	defer cancel()

	sp, err := s.readApp(ctx, dir, app, planInput{
		Space:    r.URL.Query().Get("space"),
		NameHint: r.URL.Query().Get("name"),
		Root:     root,
		Skipped:  skipped,
	})
	if err != nil {
		writeErr(w, http.StatusBadGateway, err)
		return
	}
	writeJSON(w, http.StatusOK, s.renderPlan(ctx, s.plans.put(sp), sp))
}

// handlePlanUpdate: PUT /v1/plans/{id}
// The review screen's answers come back here so the checks run against what
// the user actually chose, before a second upload. No model call — this is
// the same reading, re-decided.
func (s *Server) handlePlanUpdate(w http.ResponseWriter, r *http.Request) {
	sp, ok := s.plans.get(r.PathValue("id"))
	if !ok {
		writeErr(w, http.StatusGone, fmt.Errorf("this plan has expired — upload the app again"))
		return
	}
	var edit PlanEdit
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<20)).Decode(&edit); err != nil {
		writeErr(w, http.StatusBadRequest, err)
		return
	}
	// The edit and the answer are one operation: a second request must not
	// re-decide the config while this one is describing it.
	sp.mu.Lock()
	defer sp.mu.Unlock()
	applyPlanEdit(sp, edit)
	writeJSON(w, http.StatusOK, s.renderPlan(r.Context(), r.PathValue("id"), sp))
}

// planInput is what the caller knows that the upload itself doesn't say.
type planInput struct {
	// Space the app should belong to; empty lets donkey.yaml (then the
	// default) decide.
	Space string
	// NameHint is the folder or archive the upload came from. A browser
	// upload is rebased to the app's folder, so the name is otherwise lost.
	NameHint string
	Root     string
	Skipped  []string
}

// readApp reads an upload the way a deploy would and settles what donkey.yaml
// left open. Failing here means the app was not understood, which is a reason
// to stop rather than deploy.
func (s *Server) readApp(ctx context.Context, dir string, app *config.AppConfig, in planInput) (*storedPlan, error) {
	det, detErr := detect.Detect(dir, app)
	analysis, err := s.analyzeApp(ctx, dir, det, detErr)
	if err != nil {
		if detErr != nil {
			return nil, fmt.Errorf("%w\n\n%s", detErr, err)
		}
		return nil, err
	}

	sp := &storedPlan{
		analysis:  analysis,
		root:      in.Root,
		hasConfig: app != nil,
		skipped:   in.Skipped,
		appType:   analysis.AppType,
		notes:     analysis.Notes,
		created:   time.Now(),
	}

	// The only way detection fails is a donkey.yaml naming a Dockerfile that
	// is not there — every other app gets one written for it.
	if detErr != nil {
		return nil, detErr
	}
	sp.build.Summary = det.Summary
	sp.build.Strategy = string(det.Strategy)

	if app == nil {
		// No donkey.yaml: the plan is the app's first one. The folder (or the
		// archive) it came from names it, the way `donkey init` would.
		app = &config.AppConfig{Name: config.SanitizeName(in.NameHint)}
		app.ApplyDefaults()
	}
	if app.Name == "" {
		app.Name = config.SanitizeName(in.NameHint)
	}
	switch {
	case in.Space != "":
		app.Space = in.Space
	case app.Space == "":
		app.Space = config.DefaultSpace
	}
	sp.portSource = PortFromConfig
	if app.Port == 0 {
		app.Port, sp.portSource = proposedPort(det, analysis)
	}
	sp.app = app

	// Storage and the health route: what donkey.yaml left open, the assistant
	// fills — the same call a deploy makes, so a reviewed plan and an
	// unreviewed deploy land on the same app.
	applyAnalysis(app, analysis)
	sp.volumes = planVolumes(app, analysis)
	// A deploy only ever reports the services and migrations it noticed. A
	// plan is reviewed before anything happens, so it may propose them —
	// visibly, and every one of them can be unticked.
	sp.services = s.planServices(ctx, app, analysis)
	return sp, nil
}

// planVolumes ticks the folders the config actually keeps. `paths: []` is a
// decision against all of them, shown unticked rather than disappearing.
func planVolumes(app *config.AppConfig, analysis *ai.Analysis) []PlanVolume {
	out := []PlanVolume{}
	kept := map[string]bool{}
	if app.Storage != nil {
		for _, p := range app.Storage.Paths {
			kept[p] = true
		}
	}
	seen := map[string]bool{}
	for _, v := range analysis.Volumes {
		seen[v.Path] = true
		out = append(out, PlanVolume{Path: v.Path, Reason: v.Reason, Selected: kept[v.Path]})
	}
	// A path donkey.yaml asks for that the assistant didn't propose is still
	// the app's storage — it belongs in the list, ticked.
	if app.Storage != nil {
		for _, p := range app.Storage.Paths {
			if !seen[p] {
				out = append(out, PlanVolume{Path: p, Reason: "from donkey.yaml", Selected: true})
			}
		}
	}
	return out
}

// planServices turns "looks like it needs postgres" into a choice the deploy
// acts on. Only types donkey has a recipe for are offered; the rest are
// reported by analysisWarnings.
func (s *Server) planServices(ctx context.Context, app *config.AppConfig, analysis *ai.Analysis) []PlanService {
	out := []PlanService{}
	for _, a := range app.Addons {
		out = append(out, PlanService{
			Type: a.Type, Name: a.Name, Reason: "from donkey.yaml",
			Known: knownAddon(a.Type), Selected: true,
			Exists: s.spaceHasService(ctx, app.Space, a.Name),
		})
	}
	for _, svc := range analysis.Services {
		if hasAddonType(app, svc.Type) || !knownAddon(svc.Type) {
			continue
		}
		out = append(out, PlanService{
			Type: svc.Type, Name: svc.Type, Reason: svc.Reason,
			Known: true, Selected: true,
			Exists: s.spaceHasService(ctx, app.Space, svc.Type),
		})
		app.Addons = append(app.Addons, config.AddonConfig{Type: svc.Type, Name: svc.Type})
	}
	return out
}

// applyPlanEdit settles the review screen's answers into the plan's config.
// Everything the screen shows is authoritative: an empty storage list is the
// user saying no disk, not the user saying nothing.
func applyPlanEdit(sp *storedPlan, edit PlanEdit) {
	app := sp.app
	if name := strings.TrimSpace(edit.Name); name != "" {
		app.Name = name
	}
	if space := strings.TrimSpace(edit.Space); space != "" {
		app.Space = space
	}
	if edit.Port > 0 && edit.Port != app.Port {
		app.Port = edit.Port
		sp.portSource = PortFromConfig
	}
	app.Healthcheck.Path = strings.TrimSpace(edit.Healthcheck)

	size := config.DefaultStorage
	if app.Storage != nil && app.Storage.Size != "" {
		size = app.Storage.Size
	}
	paths := []string{}
	for _, p := range edit.Storage {
		if p = strings.TrimSpace(p); p != "" {
			paths = append(paths, p)
		}
	}
	app.Storage = &config.StorageConfig{Size: size, Paths: paths}
	for i := range sp.volumes {
		sp.volumes[i].Selected = slices.Contains(paths, sp.volumes[i].Path)
	}

	// Services: keep what donkey.yaml said about the ones that stay (its
	// version and size are the user's, not the plan's), and drop the rest.
	declared := map[string]config.AddonConfig{}
	for _, a := range app.Addons {
		declared[a.Name] = a
	}
	app.Addons = nil
	for i := range sp.services {
		svc := &sp.services[i]
		svc.Selected = svc.Known && slices.Contains(edit.Services, svc.Name)
		if !svc.Selected {
			continue
		}
		if a, ok := declared[svc.Name]; ok {
			app.Addons = append(app.Addons, a)
			continue
		}
		app.Addons = append(app.Addons, config.AddonConfig{Type: svc.Type, Name: svc.Name})
	}

	switch edit.Exposure {
	case "public":
		app.Public, app.Internal = true, false
	case "internal":
		app.Public, app.Internal = false, true
	case "protected":
		app.Public, app.Internal = false, false
	}
	app.ApplyDefaults()
}

// renderPlan recomputes findings every time: a name free a minute ago may not
// be, and a plan reporting yesterday's checks is worse than no checks.
func (s *Server) renderPlan(ctx context.Context, id string, sp *storedPlan) *Plan {
	app := sp.app
	p := &Plan{
		ID:        id,
		Root:      sp.root,
		HasConfig: sp.hasConfig,
		AppType:   sp.appType,
		Notes:     sp.notes,
		Build:     sp.build,
		Volumes:   sp.volumes,
		Services:  sp.services,
		Skipped:   sp.skipped,
		ExpiresAt: sp.created.Add(planTTL).UTC().Format(time.RFC3339),
		App: PlanApp{
			Name:        app.Name,
			Space:       app.Space,
			Port:        app.Port,
			PortSource:  sp.portSource,
			PortReason:  sp.analysis.PortReason,
			Healthcheck: app.Healthcheck.Path,
			Storage:     []string{},
			Exposure:    exposureOfConfig(app),
			URL:         s.cfg.AppURL(app.Space, app),
		},
	}
	if app.Storage != nil {
		p.App.Storage = append(p.App.Storage, app.Storage.Paths...)
		p.App.StorageSize = app.Storage.Size
	}
	p.Findings = analysisWarnings(app, sp.analysis)
	p.Findings = append(p.Findings, s.clusterFindings(ctx, app, sp.services)...)
	if !sp.hasConfig {
		p.Findings = append(p.Findings, Finding{
			Level: LevelInfo,
			Code:  "no-config",
			Title: "This upload has no donkey.yaml, so donkey wrote one from what it found",
			Fix:   "Copy it into the project to keep `donkey deploy` doing the same thing.",
		})
	}
	if len(sp.skipped) > 0 {
		p.Findings = append(p.Findings, Finding{
			Level:  LevelInfo,
			Code:   "upload-skipped",
			Title:  "Left out of the upload (secret-shaped or rebuilt anyway)",
			Detail: strings.Join(sp.skipped, ", "),
		})
	}
	// The plan is only worth showing if the deploy would accept it.
	if err := app.Validate(); err != nil {
		p.Findings = append(p.Findings, Finding{
			Level: LevelBlock,
			Code:  "config-invalid",
			Title: err.Error(),
		})
	}
	if b, err := app.Render(); err == nil {
		p.Config = string(b)
	}
	p.Ready = !Blocking(p.Findings)
	return p
}

// clusterFindings checks the plan against what already runs: the questions
// only the cluster can answer, and the ones a deploy would otherwise answer
// minutes later with a failure.
func (s *Server) clusterFindings(ctx context.Context, app *config.AppConfig, services []PlanService) []Finding {
	space := app.Space
	if err := s.rt.ValidateSpace(space); err != nil {
		return []Finding{{Level: LevelBlock, Code: "space-invalid", Title: err.Error()}}
	}
	if err := s.rt.CheckSpaceFree(ctx, space); err != nil {
		return []Finding{{Level: LevelBlock, Code: "space-taken", Title: err.Error()}}
	}
	var out []Finding
	if app.Name == addons.SharedOwner {
		out = append(out, Finding{
			Level: LevelBlock,
			Code:  "name-reserved",
			Title: fmt.Sprintf("The name %q is reserved for a space's shared services", addons.SharedOwner),
			Fix:   "Pick another name.",
		})
	}
	// Services take plain names too ("postgres" is reachable like an app); an
	// app cannot move in over one.
	if data, err := s.rt.ServiceBinding(ctx, space, addons.SharedOwner, app.Name); err == nil && len(data) > 0 {
		out = append(out, Finding{
			Level: LevelBlock,
			Code:  "name-service",
			Title: fmt.Sprintf("A service named %q already runs in this space", app.Name),
			Fix:   "The app needs another name.",
		})
	}
	if spaces, err := s.rt.ListSpaces(ctx); err == nil && !slices.Contains(spaces, space) {
		out = append(out, Finding{
			Level:  LevelInfo,
			Code:   "space-new",
			Title:  fmt.Sprintf("The space %s doesn't exist yet — this deploy creates it", space),
			Detail: "Apps in one space reach each other by name and share an address.",
		})
	}
	if st, err := s.rt.Status(ctx, space, app.Name); err == nil && st.Found {
		state := "running"
		if !st.Healthy() {
			state = "not fully up (" + st.ReadyText() + ")"
		}
		out = append(out, Finding{
			Level:  LevelInfo,
			Code:   "app-update",
			Title:  fmt.Sprintf("An app called %s already runs in this space — this replaces it", app.Name),
			Detail: fmt.Sprintf("Currently %s on %s.", state, st.Image),
		})
	}
	for _, svc := range services {
		if !svc.Selected {
			continue
		}
		if svc.Exists {
			out = append(out, Finding{
				Level: LevelInfo,
				Code:  "service-exists",
				Title: fmt.Sprintf("Connecting to the %s this space already runs", svc.Name),
			})
			continue
		}
		out = append(out, Finding{
			Level:  LevelInfo,
			Code:   "service-create",
			Title:  fmt.Sprintf("Creating %s in this space and connecting the app to it", svc.Name),
			Detail: svc.Reason,
		})
	}
	return out
}

// spaceHasService reports whether the space already runs a service under this
// name — the difference between "creating postgres" and "connecting to the
// postgres that is already there".
func (s *Server) spaceHasService(ctx context.Context, space, name string) bool {
	data, err := s.rt.ServiceBinding(ctx, space, addons.SharedOwner, name)
	return err == nil && len(data) > 0
}

func knownAddon(typ string) bool {
	_, err := addons.Get(typ)
	return err == nil
}

func exposureOfConfig(app *config.AppConfig) string {
	switch {
	case app.Internal:
		return "internal"
	case app.Public:
		return "public"
	default:
		return "protected"
	}
}
