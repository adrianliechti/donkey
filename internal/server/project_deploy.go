package server

import (
	"context"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"time"

	"github.com/adrianliechti/donkey/internal/ai"
	"github.com/adrianliechti/donkey/internal/config"
)

// buildContext allows workspace parents only inside the uploaded repository.
func buildContext(root, dir, setting string) (string, error) {
	if root == "" {
		root = dir
	}
	contextDir := dir
	if setting != "" {
		if filepath.IsAbs(setting) || strings.Contains(setting, `\`) {
			return "", fmt.Errorf("build.context must be relative to the app folder")
		}
		contextDir = filepath.Join(dir, setting)
	}
	rel, err := filepath.Rel(root, contextDir)
	if err != nil || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
		return "", fmt.Errorf("build.context points outside the uploaded project")
	}
	appPath, err := filepath.Rel(contextDir, dir)
	if err != nil || appPath == ".." || strings.HasPrefix(appPath, ".."+string(filepath.Separator)) {
		return "", fmt.Errorf("build.context must contain the app folder")
	}
	if info, err := os.Stat(contextDir); err != nil || !info.IsDir() {
		return "", fmt.Errorf("build.context folder does not exist")
	}
	return contextDir, nil
}

func buildScope(root, dir string) string {
	rel, _ := filepath.Rel(root, dir)
	return fmt.Sprintf("\nBuild and start ONLY the app in %q. Docker COPY paths are relative to the build context root, which is the repository you can read. Preserve workspace installs and shared packages needed by this app. Other runnable apps must not be started in its container.\n", filepath.ToSlash(rel))
}

func applyProjectPatches(root string, patches []projectPatch) error {
	// Validate every precondition before writing any file.
	for _, patch := range patches {
		full, _, err := projectPath(root, patch.file)
		if err != nil {
			return err
		}
		data, err := os.ReadFile(full)
		if err != nil || string(data) != patch.before {
			return fmt.Errorf("%s changed since review — upload the project again", patch.file)
		}
	}
	for _, patch := range patches {
		full := filepath.Join(root, filepath.FromSlash(patch.file))
		if err := os.WriteFile(full, []byte(patch.after), 0600); err != nil {
			return err
		}
	}
	return nil
}

func (s *Server) handleProjectDeploy(w http.ResponseWriter, r *http.Request) {
	if s.draining.Load() {
		writeErr(w, http.StatusServiceUnavailable, fmt.Errorf("the server is restarting — try again in a moment"))
		return
	}
	s.deploying.Add(1)
	defer s.deploying.Done()
	id := r.URL.Query().Get("plan")
	stored, ok := s.plans.get(id)
	if !ok || stored.project == nil {
		writeErr(w, http.StatusGone, fmt.Errorf("this project plan expired — upload the project again"))
		return
	}
	dir, err := os.MkdirTemp("", "donkey-project-deploy-*")
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err)
		return
	}
	defer os.RemoveAll(dir)
	if _, err := extractContext(r.Body, dir); err != nil {
		writeErr(w, http.StatusBadRequest, err)
		return
	}
	digest, err := contextDigest(dir)
	if err != nil {
		writeErr(w, http.StatusBadRequest, err)
		return
	}
	stored.mu.Lock()
	for _, app := range stored.project.apps {
		if !s.requireSpace(w, r, app.app.Space, false) {
			stored.mu.Unlock()
			return
		}
	}
	if digest != stored.project.digest {
		stored.mu.Unlock()
		writeErr(w, http.StatusConflict, fmt.Errorf("the project changed since review — upload it again"))
		return
	}
	plan, apps, patches := s.resolveProject(r.Context(), id, stored.project)
	connections := append([]ai.ProjectConnection(nil), stored.project.reading.Connections...)
	stored.mu.Unlock()
	if !plan.Ready {
		writeErr(w, http.StatusConflict, fmt.Errorf("this project still has blocking findings — review it before deploying"))
		return
	}
	// Check every build boundary before creating any app or service.
	for _, sp := range apps {
		if _, err := buildContext(dir, filepath.Join(dir, sp.root), sp.app.Build.Context); err != nil {
			writeErr(w, http.StatusBadRequest, err)
			return
		}
	}
	// The reviewed fixes are donkey's, accepted by the person deploying: the
	// source history keeps the upload as sent, and the fixes show up as the
	// platform's change beside whatever the build itself does.
	var pristine string
	if len(patches) > 0 && s.history != nil && r.URL.Query().Get("dryRun") != "true" {
		if pristine, err = os.MkdirTemp("", "donkey-project-upload-*"); err == nil {
			defer os.RemoveAll(pristine)
			err = os.CopyFS(pristine, os.DirFS(dir))
		}
		if err != nil {
			writeErr(w, http.StatusInternalServerError, err)
			return
		}
	}
	if err := applyProjectPatches(dir, patches); err != nil {
		writeErr(w, http.StatusConflict, err)
		return
	}
	out := newLineStreamer(w, nil)
	ctx := context.WithoutCancel(r.Context())
	// Hold the whole project, including deferred waits for cyclic peers.
	// Stable ordering lets overlapping project uploads wait without deadlock.
	spaceMu := s.spaceLock(apps[0].app.Space)
	spaceMu.RLock()
	defer spaceMu.RUnlock()
	unlock := s.lockProject(apps)
	defer unlock()
	dryRun := r.URL.Query().Get("dryRun") == "true"
	hosts := map[string]string{}
	connected := map[string]bool{}
	for _, c := range connections {
		if c.Kind == "server" || c.Kind == "address" || c.Kind == "service" {
			connected[c.App] = true
		}
	}
	for _, app := range apps {
		hosts[app.app.Name] = config.AppHost(app.app, app.app.Space, s.cfg.Domain)
	}
	result := DeployResult{OK: true, Space: apps[0].app.Space, Apps: []DeployResult{}}
	// Dependencies are released first. Cycles are applied without waiting so
	// a caller's readiness check cannot prevent its peer from being created.
	order, cyclic := projectOrder(apps, connections)
	records := map[string]*releaseRecorder{}
	configs := map[string]*config.AppConfig{}
	finish := func(res DeployResult) {
		recorder := records[res.App]
		recorder.result(res)
		if !dryRun {
			s.persistRelease(ctx, res.Space, res.App, recorder.finish(s, ctx, configs[res.App]))
		}
		out.Update(DeployUpdate{App: res.App, Result: &res})
	}
	for _, index := range order {
		sp := apps[index]
		out.Update(DeployUpdate{App: sp.app.Name, Stage: "detecting", BaseImages: sp.build.BaseImages})
		out.Line("Deploying %s (%s)…", sp.app.Name, sp.root)
		recorder := newReleaseRecorder(time.Now(), plan.Apps[index])
		records[sp.app.Name], configs[sp.app.Name] = recorder, sp.app
		out.release = recorder
		res := s.deployApp(ctx, out, filepath.Join(dir, sp.root), sp.app, deployOpts{Root: dir, Pristine: pristine, Analysis: sp.analysis, NoWait: cyclic, Hosts: hosts, Reviewed: true, DryRun: dryRun, NoVerify: connected[sp.root]})
		out.release = nil
		if !cyclic || dryRun {
			finish(res)
		}
		result.Apps = append(result.Apps, res)
	}
	if cyclic && !dryRun {
		out.Line("Waiting for the connected apps to become healthy…")
		waitCtx, cancel := context.WithTimeout(ctx, 2*time.Minute)
		s.waitProject(waitCtx, result.Apps)
		cancel()
		for _, res := range result.Apps {
			finish(res)
		}
	}
	for _, res := range result.Apps {
		if !res.OK {
			result.OK = false
		}
	}
	if !result.OK {
		result.Error = "Some apps could not be deployed. See each app's result."
	}
	out.Result(result)
}

func (s *Server) waitProject(ctx context.Context, results []DeployResult) {
	pending := map[int]bool{}
	for i, res := range results {
		if res.OK {
			pending[i] = true
		}
	}
	for len(pending) > 0 {
		for i := range pending {
			res := &results[i]
			if status, err := s.rt.Status(ctx, res.Space, res.App); err == nil && status != nil && status.Healthy() {
				delete(pending, i)
			}
		}
		if len(pending) == 0 {
			return
		}
		select {
		case <-ctx.Done():
			for i := range pending {
				results[i].OK = false
				results[i].Error = "The app did not become healthy after its dependencies were deployed."
			}
			return
		case <-time.After(2 * time.Second):
		}
	}
}

func (s *Server) lockProject(apps []*storedPlan) func() {
	names := make([]string, 0, len(apps))
	for _, sp := range apps {
		names = append(names, sp.app.Name)
	}
	slices.Sort(names)
	var unlocks []func()
	for _, name := range slices.Compact(names) {
		unlocks = append(unlocks, s.lockApp(apps[0].app.Space, name))
	}
	return func() {
		for _, unlock := range slices.Backward(unlocks) {
			unlock()
		}
	}
}

func projectOrder(apps []*storedPlan, connections []ai.ProjectConnection) ([]int, bool) {
	byPath := map[string]int{}
	for i, app := range apps {
		byPath[app.root] = i
	}
	deps := make([]map[int]bool, len(apps))
	for i := range deps {
		deps[i] = map[int]bool{}
	}
	for _, c := range connections {
		from, ok := byPath[c.App]
		to, found := byPath[c.Target]
		if ok && found && from != to && (c.Kind == "server" || c.Kind == "address" || c.Kind == "browser") {
			deps[from][to] = true
		}
	}
	var order []int
	done := map[int]bool{}
	for len(order) < len(apps) {
		progress := false
		for i := range apps {
			if done[i] {
				continue
			}
			ready := true
			for dep := range deps[i] {
				if !done[dep] {
					ready = false
				}
			}
			if ready {
				order = append(order, i)
				done[i] = true
				progress = true
			}
		}
		if !progress {
			for i := range apps {
				if !done[i] {
					order = append(order, i)
				}
			}
			return order, true
		}
	}
	return order, false
}
