package server

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"net/http"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/adrianliechti/donkey/internal/ai"
	"github.com/adrianliechti/donkey/internal/config"
	"github.com/adrianliechti/donkey/internal/telemetry"
)

// AppRelease is one deploy as the release screen rendered it live. Records
// are versioned in the telemetry store — one per build, success or failure,
// newest-first with bounded retention — so the console can show history, not
// just the latest attempt.
type AppRelease struct {
	StartedAt  string       `json:"startedAt"`
	FinishedAt string       `json:"finishedAt"`
	Plan       *Plan        `json:"plan"`
	Update     DeployUpdate `json:"update"`
	Lines      []string     `json:"lines"`
	BuildLog   string       `json:"buildLog,omitempty"`
	// Dockerfile is the exact file the image was built from — the durable
	// recipe, whether the app brought it or donkey's assistant wrote it.
	Dockerfile   string            `json:"dockerfile,omitempty"`
	Dockerignore *string           `json:"dockerignore,omitempty"`
	Changes      []ai.SourceChange `json:"changes,omitempty"`
	Result       DeployResult      `json:"result"`
	// Decisions is what this deploy settled (storage, health route), kept
	// with the image it was verified against: a `--no-build` rollback to
	// this record's tag adopts these, not whatever a newer analysis found.
	Decisions json.RawMessage `json:"decisions,omitempty"`
	// Truncated is retained for reading older records that omitted output.
	Truncated bool `json:"truncated,omitempty"`
}

// releaseRecorder observes the same structured events and text sent to the
// live client. Build progress and source analysis can arrive concurrently,
// so it owns a small lock even though the response stream itself is ordered.
type releaseRecorder struct {
	mu sync.Mutex

	release  AppRelease
	buildLog strings.Builder

	analysis *ai.Analysis
	build    PlanBuild
}

func newReleaseRecorder(started time.Time, plan *Plan) *releaseRecorder {
	r := &releaseRecorder{release: AppRelease{
		StartedAt: started.UTC().Format(time.RFC3339),
		Lines:     []string{},
		Update:    DeployUpdate{Stage: "detecting"},
	}}
	if plan != nil {
		r.release.Plan = releasePlanCopy(plan)
		r.build = plan.Build
		r.release.Update.BaseImages = append([]string{}, plan.Build.BaseImages...)
	}
	return r
}

func releasePlanCopy(in *Plan) *Plan {
	if in == nil {
		return nil
	}
	b, err := json.Marshal(in)
	if err != nil {
		return nil
	}
	var out Plan
	if json.Unmarshal(b, &out) != nil {
		return nil
	}
	// A saved release cannot be deployed again, and app source/config should
	// not be copied into a Kubernetes annotation merely to re-render a view
	// that does not use it.
	out.ID = ""
	out.ExpiresAt = ""
	out.Config = ""
	normalizeReleasePlan(&out)
	return &out
}

func normalizeReleasePlan(plan *Plan) {
	if plan == nil {
		return
	}
	if plan.Findings == nil {
		plan.Findings = []Finding{}
	}
	if plan.Volumes == nil {
		plan.Volumes = []PlanVolume{}
	}
	if plan.Services == nil {
		plan.Services = []PlanService{}
	}
	if plan.App.Storage == nil {
		plan.App.Storage = []string{}
	}
}

func (r *releaseRecorder) line(line string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.release.Lines = append(r.release.Lines, line)
}

func (r *releaseRecorder) progress(progress string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.buildLog.WriteString(progress)
}

func (r *releaseRecorder) update(update DeployUpdate) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if update.Stage != "" {
		r.release.Update.Stage = update.Stage
	}
	if update.BaseImages != nil {
		r.release.Update.BaseImages = append([]string{}, update.BaseImages...)
	}
	if update.Dockerfile != "" {
		r.release.Dockerfile = update.Dockerfile
	}
	if update.Dockerignore != nil {
		value := *update.Dockerignore
		r.release.Dockerignore = &value
	}
	for _, change := range update.Changes {
		change.Diff = ai.Redact(change.Diff)
		r.release.Changes = append(r.release.Changes, change)
	}
}

func (r *releaseRecorder) inspection(build PlanBuild, analysis *ai.Analysis) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.build = build
	if analysis != nil {
		r.analysis = analysis
	}
}

func (r *releaseRecorder) result(result DeployResult) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.release.Result = result
}

// finish takes the immutable copy persisted after the final stream event.
// A reviewed deploy already carries its exact plan; older/automation clients
// get an equivalent best-effort snapshot from the analysis used by deploy.
func (r *releaseRecorder) finish(s *Server, ctx context.Context, app *config.AppConfig) AppRelease {
	r.mu.Lock()
	r.release.FinishedAt = time.Now().UTC().Format(time.RFC3339)
	release := r.release
	release.BuildLog = r.buildLog.String()
	release.Lines = append([]string{}, r.release.Lines...)
	release.Update.BaseImages = append([]string{}, r.release.Update.BaseImages...)
	analysis := r.analysis
	build := r.build
	r.mu.Unlock()

	if release.Plan == nil {
		release.Plan = s.recordedDeployPlan(ctx, app, analysis, build)
	}
	normalizeReleasePlan(release.Plan)
	if release.Result.OK && app != nil {
		release.Decisions = json.RawMessage(decisionsJSON(app))
	}
	return release
}

// recordedDeployPlan supplies the structured half of the release view for a
// deploy that did not arrive with a reviewed plan (for example, a redeploy
// from the CLI). It describes what was actually configured; an inferred but
// undeclared service stays visible as not included.
func (s *Server) recordedDeployPlan(ctx context.Context, app *config.AppConfig, analysis *ai.Analysis, build PlanBuild) *Plan {
	if app == nil {
		return &Plan{Build: build, Findings: []Finding{}, Volumes: []PlanVolume{}, Services: []PlanService{}}
	}
	copy := cloneReleaseApp(app)
	if analysis == nil {
		analysis = &ai.Analysis{}
	}
	if build.Summary == "" {
		build = PlanBuild{Summary: "Use the selected image without rebuilding it", Strategy: "existing-image"}
	}

	services := []PlanService{}
	seenTypes := map[string]bool{}
	for _, service := range copy.Addons {
		services = append(services, PlanService{
			Type: service.Type, Name: service.Name, Reason: "from donkey.yaml",
			Known: knownAddon(service.Type), Selected: true,
			Exists: s.spaceHasService(ctx, copy.Space, service.Name),
		})
		seenTypes[service.Type] = true
	}
	for _, service := range analysis.Services {
		if seenTypes[service.Type] {
			continue
		}
		services = append(services, PlanService{
			Type: service.Type, Name: service.Type, Reason: service.Reason,
			Known: knownAddon(service.Type), Selected: false,
			Exists: s.spaceHasService(ctx, copy.Space, service.Type),
		})
		seenTypes[service.Type] = true
	}

	plan := &Plan{
		HasConfig: true,
		AppType:   analysis.AppType,
		Notes:     analysis.Notes,
		Build:     build,
		Findings:  analysisWarnings(copy, analysis),
		Volumes:   planVolumes(copy, analysis),
		Services:  services,
		Ready:     true,
		App: PlanApp{
			Name:        copy.Name,
			Space:       copy.Space,
			Port:        copy.Port,
			PortSource:  PortFromConfig,
			PortReason:  analysis.PortReason,
			Healthcheck: copy.Healthcheck.Path,
			Storage:     []string{},
			Exposure:    exposureOfConfig(copy),
			URL:         s.cfg.AppURL(copy.Space, copy),
		},
	}
	if copy.Storage != nil {
		plan.App.Storage = append(plan.App.Storage, copy.Storage.Paths...)
		plan.App.StorageSize = copy.Storage.Size
	}
	plan.Ready = !Blocking(plan.Findings)
	return plan
}

func cloneReleaseApp(in *config.AppConfig) *config.AppConfig {
	b, err := json.Marshal(in)
	if err != nil {
		return in
	}
	var out config.AppConfig
	if json.Unmarshal(b, &out) != nil {
		return in
	}
	out.ApplyDefaults()
	return &out
}

// persistRelease stores only records attached to a real app. A first build
// that fails before anything exists has no app page to attach one to; a failed
// update does, and its failure is exactly the record worth keeping. Storage
// is best-effort by design: the deploy already happened, and a telemetry
// hiccup must not turn it into a failure after the fact.
func (s *Server) persistRelease(ctx context.Context, space, app string, release AppRelease) {
	if space == "" || app == "" {
		return
	}
	ctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 15*time.Second)
	defer cancel()
	status, err := s.rt.Status(ctx, space, app)
	if err != nil || status == nil || !status.Found {
		return
	}
	scrubRelease(&release, s.knownSecrets(ctx, space, app))
	raw, err := marshalRelease(release)
	if err != nil {
		log.Printf("store release app=%s space=%s: %v", app, space, err)
		return
	}
	if _, err := s.telemetry.RecordBuild(ctx, space, app, release.Result.ImageRef, release.Result.OK, raw); err != nil {
		log.Printf("store release app=%s space=%s: %v", app, space, err)
	}
}

func marshalRelease(release AppRelease) ([]byte, error) {
	return json.Marshal(release)
}

func scrubRelease(release *AppRelease, known []string) {
	usable := known[:0]
	for _, value := range known {
		if len(value) >= 8 {
			usable = append(usable, value)
		}
	}
	sort.Slice(usable, func(i, j int) bool { return len(usable[i]) > len(usable[j]) })
	scrub := func(text string) string {
		for _, value := range usable {
			text = strings.ReplaceAll(text, value, "***")
		}
		return ai.Redact(text)
	}
	for i := range release.Lines {
		release.Lines[i] = scrub(release.Lines[i])
	}
	release.BuildLog = scrub(release.BuildLog)
	release.Result.Error = scrub(release.Result.Error)
	release.Result.Diagnosis = scrub(release.Result.Diagnosis)
	release.Dockerfile = scrub(release.Dockerfile)
	if release.Dockerignore != nil {
		value := scrub(*release.Dockerignore)
		release.Dockerignore = &value
	}
	for i := range release.Changes {
		release.Changes[i].Diff = scrub(release.Changes[i].Diff)
	}
	for i := range release.Result.Proposals {
		proposal := &release.Result.Proposals[i]
		proposal.Explanation = scrub(proposal.Explanation)
		// A proposal's content is a Dockerfile or config file worth keeping —
		// scrubbed like everything else, never blanked.
		proposal.NewContent = scrub(proposal.NewContent)
		proposal.Diff = scrub(proposal.Diff)
		proposal.Value = scrub(proposal.Value)
	}
	if release.Plan == nil {
		return
	}
	release.Plan.Config = ""
	release.Plan.AppType = scrub(release.Plan.AppType)
	release.Plan.Notes = scrub(release.Plan.Notes)
	release.Plan.Build.Summary = scrub(release.Plan.Build.Summary)
	for i := range release.Plan.Findings {
		finding := &release.Plan.Findings[i]
		finding.Title = scrub(finding.Title)
		finding.Detail = scrub(finding.Detail)
		finding.Fix = scrub(finding.Fix)
	}
	for i := range release.Plan.Volumes {
		release.Plan.Volumes[i].Reason = scrub(release.Plan.Volumes[i].Reason)
	}
	for i := range release.Plan.Services {
		release.Plan.Services[i].Reason = scrub(release.Plan.Services[i].Reason)
	}
}

// handleBuilds: GET /v1/spaces/{space}/apps/{app}/builds — the retained
// history, newest first. An app that predates the build store simply has an
// empty history.
func (s *Server) handleBuilds(w http.ResponseWriter, r *http.Request) {
	space, app, err := s.spaceApp(r)
	if err != nil {
		writeErr(w, http.StatusBadRequest, err)
		return
	}
	builds, err := s.telemetry.Builds(r.Context(), space, app)
	if err != nil {
		writeErr(w, http.StatusBadGateway, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"builds": builds})
}

// handleBuild: GET /v1/spaces/{space}/apps/{app}/builds/{sequence}
func (s *Server) handleBuild(w http.ResponseWriter, r *http.Request) {
	space, app, err := s.spaceApp(r)
	if err != nil {
		writeErr(w, http.StatusBadRequest, err)
		return
	}
	sequence, err := strconv.ParseInt(r.PathValue("sequence"), 10, 64)
	if err != nil {
		writeErr(w, http.StatusBadRequest, fmt.Errorf("the build number must be a number"))
		return
	}
	raw, err := s.telemetry.Build(r.Context(), space, app, sequence)
	if errors.Is(err, telemetry.ErrNoBuild) {
		writeErr(w, http.StatusNotFound, fmt.Errorf("no stored build %d is available for this app", sequence))
		return
	}
	if err != nil {
		writeErr(w, http.StatusBadGateway, err)
		return
	}
	var release AppRelease
	if err := json.Unmarshal(raw, &release); err != nil {
		writeErr(w, http.StatusBadGateway, fmt.Errorf("the stored build record is invalid: %w", err))
		return
	}
	normalizeReleasePlan(release.Plan)
	writeJSON(w, http.StatusOK, release)
}
