package server

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"sort"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	"github.com/adrianliechti/donkey/internal/ai"
	"github.com/adrianliechti/donkey/internal/config"
	"github.com/adrianliechti/donkey/pkg/runtime"
)

// AppRelease is the last deploy as the release screen rendered it live. It is
// intentionally one bounded record rather than an unbounded history: runtime
// app metadata survives deploys on both Kubernetes and Docker, while still
// being small enough for a Kubernetes annotation.
type AppRelease struct {
	StartedAt  string       `json:"startedAt"`
	FinishedAt string       `json:"finishedAt"`
	Plan       *Plan        `json:"plan"`
	Update     DeployUpdate `json:"update"`
	Lines      []string     `json:"lines"`
	BuildLog   string       `json:"buildLog,omitempty"`
	Result     DeployResult `json:"result"`
	// Truncated says the useful tail was retained but older output was left
	// out to keep the app metadata bounded.
	Truncated bool `json:"truncated,omitempty"`
}

const (
	releaseLinesBytes    = 40 << 10
	releaseBuildLogBytes = 64 << 10
	releaseDiffBytes     = 16 << 10
	// Kubernetes caps all annotations on an object at 256 KiB. Leave room
	// for donkey's decisions, console choices, and platform annotations.
	releaseMetaBytes = 192 << 10
)

// releaseRecorder observes the same structured events and text sent to the
// live client. Build progress and source analysis can arrive concurrently,
// so it owns a small lock even though the response stream itself is ordered.
type releaseRecorder struct {
	mu sync.Mutex

	release      AppRelease
	lineBytes    int
	omittedLine  bool
	omittedBuild bool

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

// hasRenderableRelease keeps the Deployment tab honest for apps that predate
// release recording, as well as for an interrupted or malformed metadata
// write. A real record always has both its structured plan and final result.
func hasRenderableRelease(raw string) bool {
	if raw == "" {
		return false
	}
	var release AppRelease
	if json.Unmarshal([]byte(raw), &release) != nil {
		return false
	}
	return release.Plan != nil && release.FinishedAt != "" &&
		release.Result.App != "" && release.Result.Space != ""
}

func (r *releaseRecorder) line(line string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.release.Lines = append(r.release.Lines, line)
	r.lineBytes += len(line)
	for r.lineBytes > releaseLinesBytes && len(r.release.Lines) > 1 {
		r.lineBytes -= len(r.release.Lines[0])
		r.release.Lines = r.release.Lines[1:]
		r.omittedLine = true
	}
}

func (r *releaseRecorder) progress(progress string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.release.BuildLog += progress
	if len(r.release.BuildLog) > releaseBuildLogBytes {
		r.release.BuildLog = tailUTF8(r.release.BuildLog, releaseBuildLogBytes)
		r.omittedBuild = true
	}
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
	release.Lines = append([]string{}, r.release.Lines...)
	release.Update.BaseImages = append([]string{}, r.release.Update.BaseImages...)
	analysis := r.analysis
	build := r.build
	omittedLine, omittedBuild := r.omittedLine, r.omittedBuild
	r.mu.Unlock()

	if omittedLine {
		release.Lines = append([]string{"· Earlier release log omitted to keep this record small."}, release.Lines...)
		release.Truncated = true
	}
	if omittedBuild {
		release.BuildLog = "… earlier build output omitted …\n" + release.BuildLog
		release.Truncated = true
	}
	if release.Plan == nil {
		release.Plan = s.recordedDeployPlan(ctx, app, analysis, build)
	}
	normalizeReleasePlan(release.Plan)
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
// update does, and its failure is exactly the record worth keeping.
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
	value := string(raw)
	if err := s.rt.SetAppMeta(ctx, space, app, runtime.MetaRelease, &value); err != nil {
		log.Printf("store release app=%s space=%s: %v", app, space, err)
	}
}

func marshalRelease(release AppRelease) ([]byte, error) {
	b, err := json.Marshal(release)
	if err != nil || len(b) <= releaseMetaBytes {
		return b, err
	}
	// JSON escaping and unusually large findings can push a bounded log over
	// the final annotation budget. Shed old output first, then bulky diffs.
	release.Truncated = true
	release.BuildLog = tailUTF8(release.BuildLog, 16<<10)
	release.Lines = tailLines(release.Lines, 16<<10)
	for i := range release.Result.Proposals {
		release.Result.Proposals[i].NewContent = ""
		release.Result.Proposals[i].Diff = prefixUTF8(release.Result.Proposals[i].Diff, 4<<10)
	}
	b, err = json.Marshal(release)
	if err != nil || len(b) <= releaseMetaBytes {
		return b, err
	}
	release.BuildLog = ""
	release.Lines = []string{"· Detailed release output was too large to retain; structured findings and the result remain."}
	release.Result.Proposals = nil
	b, err = json.Marshal(release)
	if err != nil {
		return nil, err
	}
	if len(b) > releaseMetaBytes {
		return nil, fmt.Errorf("release record is %d bytes after trimming (limit %d)", len(b), releaseMetaBytes)
	}
	return b, nil
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
	for i := range release.Result.Proposals {
		proposal := &release.Result.Proposals[i]
		proposal.Explanation = scrub(proposal.Explanation)
		proposal.NewContent = ""
		proposal.Diff = prefixUTF8(scrub(proposal.Diff), releaseDiffBytes)
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

func tailLines(lines []string, limit int) []string {
	bytes := 0
	start := len(lines)
	for start > 0 && bytes+len(lines[start-1]) <= limit {
		start--
		bytes += len(lines[start])
	}
	return append([]string{}, lines[start:]...)
}

func tailUTF8(value string, limit int) string {
	if len(value) <= limit {
		return value
	}
	value = value[len(value)-limit:]
	for len(value) > 0 && !utf8.RuneStart(value[0]) {
		value = value[1:]
	}
	return value
}

func prefixUTF8(value string, limit int) string {
	if len(value) <= limit {
		return value
	}
	// Back the cut up to a rune boundary and no further: an invalid byte
	// earlier in the content (binary hunks in a diff) is the content's
	// business — trimming until the whole prefix validates would discard
	// everything after it.
	cut := limit
	for cut > 0 && limit-cut < utf8.UTFMax && !utf8.RuneStart(value[cut]) {
		cut--
	}
	return value[:cut] + "\n… omitted …"
}

// handleRelease: GET /v1/spaces/{space}/apps/{app}/release
func (s *Server) handleRelease(w http.ResponseWriter, r *http.Request) {
	space, app, err := s.spaceApp(r)
	if err != nil {
		writeErr(w, http.StatusBadRequest, err)
		return
	}
	raw, err := s.rt.AppMeta(r.Context(), space, app, runtime.MetaRelease)
	if err != nil {
		writeErr(w, http.StatusBadGateway, err)
		return
	}
	if raw == "" {
		writeErr(w, http.StatusNotFound, fmt.Errorf("no stored release is available for this app yet"))
		return
	}
	var release AppRelease
	if err := json.Unmarshal([]byte(raw), &release); err != nil {
		writeErr(w, http.StatusBadGateway, fmt.Errorf("the stored release record is invalid: %w", err))
		return
	}
	if !hasRenderableRelease(raw) {
		writeErr(w, http.StatusNotFound, fmt.Errorf("no complete stored release is available for this app yet"))
		return
	}
	normalizeReleasePlan(release.Plan)
	writeJSON(w, http.StatusOK, release)
}
