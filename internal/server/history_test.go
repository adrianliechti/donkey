package server

import (
	"archive/zip"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/adrianliechti/donkey/internal/config"
	"github.com/adrianliechti/donkey/internal/history"
	"github.com/adrianliechti/donkey/internal/identity"
	"github.com/adrianliechti/donkey/pkg/runtime"
)

func writeFiles(t *testing.T, dir string, files map[string]string) {
	t.Helper()
	for name, content := range files {
		full := filepath.Join(dir, filepath.FromSlash(name))
		if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(full, []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}
}

// buildCopy makes the workspace a deploy builds from: a copy of the upload,
// then whatever the build did to it.
func buildCopy(t *testing.T, upload string, edit func(dir string)) string {
	t.Helper()
	workspace := t.TempDir()
	if err := os.CopyFS(workspace, os.DirFS(upload)); err != nil {
		t.Fatal(err)
	}
	edit(workspace)
	return workspace
}

func TestBuildChangesFindsWhatTheBuildChanged(t *testing.T) {
	upload := t.TempDir()
	writeFiles(t, upload, map[string]string{
		"main.py": "print('hi')\n", "util.py": "a = 1\n", "old.txt": "bye\n", "run.sh": "#!/bin/sh\n", "tool.sh": "#!/bin/sh\n",
	})
	if err := os.Chmod(filepath.Join(upload, "tool.sh"), 0o755); err != nil {
		t.Fatal(err)
	}
	workspace := buildCopy(t, upload, func(dir string) {
		writeFiles(t, dir, map[string]string{".donkey/Dockerfile": "FROM python\n", "util.py": "a = 2\n"})
		_ = os.Remove(filepath.Join(dir, "old.txt"))
		_ = os.Chmod(filepath.Join(dir, "run.sh"), 0o755)
		// A stricter umask on the copy changes bits, not executability.
		_ = os.Chmod(filepath.Join(dir, "tool.sh"), 0o700)
	})
	changes, err := buildChanges(upload, workspace)
	if err != nil {
		t.Fatal(err)
	}
	got := map[string]history.FileChange{}
	for _, c := range changes {
		got[c.Path] = c
	}
	if len(got) != 4 {
		t.Fatalf("changes = %+v", changes)
	}
	if string(got[".donkey/Dockerfile"].Content) != "FROM python\n" || string(got["util.py"].Content) != "a = 2\n" {
		t.Fatalf("written and changed files should carry their new content: %+v", got)
	}
	if !got["old.txt"].Deleted {
		t.Fatal("a file the build removed should be a deletion")
	}
	if !got["run.sh"].Executable {
		t.Fatal("a mode change is a change")
	}
	if _, ok := got["main.py"]; ok {
		t.Fatal("an untouched file is not a change")
	}
}

func TestPlatformMessageSaysWhatTheBuildDid(t *testing.T) {
	msg := platformMessage(12, []history.FileChange{
		{Path: ".donkey/Dockerfile"}, {Path: ".donkey/Dockerfile.dockerignore"}, {Path: "package.json"}, {Path: "tmp.txt", Deleted: true},
	})
	want := "Build #12 wrote the build recipe and repaired 2 files\n\n" +
		"- Wrote .donkey/Dockerfile\n- Wrote .donkey/Dockerfile.dockerignore\n- Repaired package.json\n- Removed tmp.txt"
	if msg != want {
		t.Fatalf("message =\n%s\nwant\n%s", msg, want)
	}
	if got := platformMessage(0, []history.FileChange{{Path: "app.js"}}); !strings.HasPrefix(got, "The build repaired 1 file\n") {
		t.Fatalf("without a build number: %q", got)
	}
	// In a monorepo the paths are the build context's: the recipe is in the
	// app's folder.
	if got := platformMessage(3, []history.FileChange{{Path: "web/.donkey/Dockerfile"}}); !strings.HasPrefix(got, "Build #3 wrote the build recipe\n") {
		t.Fatalf("monorepo recipe: %q", got)
	}
}

// destroyRuntime removes nothing for real; it only has to say the runtime
// part of a removal went through.
type destroyRuntime struct {
	releaseRuntime
}

func (destroyRuntime) Destroy(context.Context, string, string, bool) error { return nil }

// The history is written in the orchestrator, where deploys run; the API has
// no volume of its own and forwards every read and removal there, so one
// process owns the repositories.
func TestHistoryIsServedWhereItIsWritten(t *testing.T) {
	orchestrator := historyServer(t, "registry/web:1")
	rt := destroyRuntime{releaseRuntime{
		planRuntime: planRuntime{platform: &config.PlatformConfig{Admins: config.AccessConfig{Users: []string{"u1@example.com"}}}},
		status:      &runtime.AppStatus{Found: true, UpToDate: true, WantReplica: 1, ReadyReplica: 1, Image: "registry/web:1"},
		meta:        map[string]string{},
	}}
	orchestrator.rt = rt
	upload := t.TempDir()
	writeFiles(t, upload, map[string]string{"main.py": "print('hi')\n"})
	release := releaseFixture()
	release.source = captureSource(upload, buildCopy(t, upload, func(string) {}), ".", "")
	orchestrator.persistRelease(asPerson(context.Background()), "acme", "web", release)
	orchestratorHTTP := httptest.NewServer(orchestrator.orchestratorRoutes())
	defer orchestratorHTTP.Close()

	api := apiServer(orchestratorHTTP.URL)
	api.rt = rt
	send := func(method, path string) *httptest.ResponseRecorder {
		req := httptest.NewRequest(method, "https://donkey.example.com"+path, nil)
		asAdmin(t, req)
		rec := httptest.NewRecorder()
		api.routes().ServeHTTP(rec, req)
		return rec
	}

	rec := send(http.MethodGet, "/v1/spaces/acme/apps/web/commits")
	var listed struct {
		Commits []SourceCommit `json:"commits"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &listed); err != nil || len(listed.Commits) != 1 {
		t.Fatalf("history through the API: %d %s", rec.Code, rec.Body.String())
	}
	rec = send(http.MethodGet, "/v1/spaces/acme/apps/web/commits/"+listed.Commits[0].ID+"/archive")
	if rec.Code != http.StatusOK || !strings.Contains(rec.Header().Get("Content-Disposition"), ".zip") {
		t.Fatalf("archive through the API: %d %v", rec.Code, rec.Header())
	}
	if rec := send(http.MethodDelete, "/v1/spaces/acme/apps/web"); rec.Code != http.StatusOK {
		t.Fatalf("remove through the API: %d %s", rec.Code, rec.Body.String())
	}
	if commits, err := orchestrator.history.Log("acme", "web", 10); err != nil || len(commits) != 0 {
		t.Fatalf("removing the app through the API removes the orchestrator's history: %d commits, %v", len(commits), err)
	}
}

func TestRemovingAnAppRemovesItsHistory(t *testing.T) {
	for _, keepData := range []bool{false, true} {
		s := historyServer(t, "registry/web:1")
		upload := t.TempDir()
		writeFiles(t, upload, map[string]string{"main.py": "print('hi')\n"})
		release := releaseFixture()
		release.source = captureSource(upload, buildCopy(t, upload, func(string) {}), ".", "")
		s.persistRelease(asPerson(context.Background()), "acme", "web", release)
		s.rt = destroyRuntime{releaseRuntime: s.rt.(releaseRuntime)}

		req := httptest.NewRequest(http.MethodDelete, fmt.Sprintf("/v1/spaces/acme/apps/web?keepData=%t", keepData), nil)
		req.SetPathValue("space", "acme")
		req.SetPathValue("app", "web")
		rec := httptest.NewRecorder()
		s.handleDestroy(rec, req)
		if rec.Code != http.StatusOK {
			t.Fatalf("destroy: %d %s", rec.Code, rec.Body.String())
		}

		commits, err := s.history.Log("acme", "web", 10)
		if err != nil {
			t.Fatal(err)
		}
		builds, err := s.deploymentStore().Builds(context.Background(), "acme", "web")
		if err != nil {
			t.Fatal(err)
		}
		kept := len(commits) == 1 && len(builds) == 1
		gone := len(commits) == 0 && len(builds) == 0
		if keepData && !kept {
			t.Fatalf("keeping the data keeps the history for a redeploy: %d commits, %d builds", len(commits), len(builds))
		}
		if !keepData && !gone {
			t.Fatalf("removing the app removes its history: %d commits, %d builds", len(commits), len(builds))
		}
	}
}

func historyServer(t *testing.T, image string) *Server {
	t.Helper()
	store, err := history.New(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	return &Server{
		cfg: &Config{DataDir: t.TempDir(), Domain: "example.com", SessionKey: testSessionKey},
		rt: releaseRuntime{
			status: &runtime.AppStatus{Found: true, UpToDate: true, WantReplica: 1, ReadyReplica: 1, Image: image},
			meta:   map[string]string{},
		},
		history: store,
	}
}

func asPerson(ctx context.Context) context.Context {
	return context.WithValue(ctx, identityContextKey{}, &identity.Identity{
		Subject: "u1", Name: "Adrian Liechti", Email: "adrian@example.com",
	})
}

func historyGet(t *testing.T, handler http.HandlerFunc, target string, values map[string]string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodGet, target, nil)
	req.SetPathValue("space", "acme")
	req.SetPathValue("app", "web")
	for k, v := range values {
		req.SetPathValue(k, v)
	}
	rec := httptest.NewRecorder()
	handler(rec, req)
	return rec
}

func TestBuildsRecordTheSettingsTheyRanWith(t *testing.T) {
	s := historyServer(t, "registry/web:1")
	upload := t.TempDir()
	writeFiles(t, upload, map[string]string{"Dockerfile": "FROM nginx\n", "donkey.yaml": "name: web\nport: 8080\n"})

	// A build of the app's own Dockerfile changes nothing in its copy, yet
	// its commit still says what it ran with — without values that are not
	// the history's to keep.
	release := releaseFixture()
	release.source = captureSource(upload, buildCopy(t, upload, func(string) {}), ".", "")
	release.settings = &config.AppConfig{
		Name: "web", Space: "acme", Port: 8080, Healthcheck: config.Healthcheck{Path: "/healthz"}, Storage: &config.StorageConfig{},
		Env:    map[string]string{"API_TOKEN": "s3cret-token-value"},
		Addons: []config.AddonConfig{{Type: "postgres", Parameters: map[string]any{"password": "p4ssw0rd-value"}}},
	}
	s.persistRelease(asPerson(context.Background()), "acme", "web", release)

	commits, err := s.history.Log("acme", "web", 10)
	if err != nil || len(commits) != 2 {
		t.Fatalf("commits = %+v, %v", commits, err)
	}
	built := commits[0]
	if built.Kind != history.KindPlatform || !strings.HasPrefix(built.Message, "Build #1 recorded its settings") {
		t.Fatalf("the build's commit = %+v", built)
	}
	body, err := s.history.ReadFile("acme", "web", built.ID, settingsFile)
	if err != nil {
		t.Fatal(err)
	}
	settings := string(body)
	for _, want := range []string{"# What build #1 ran with", "# image: registry/web:1", "port: 8080", "path: /healthz", "type: postgres"} {
		if !strings.Contains(settings, want) {
			t.Errorf("deploy.yaml is missing %q:\n%s", want, settings)
		}
	}
	// An app without a disk says nothing about storage.
	for _, secret := range []string{"s3cret-token-value", "API_TOKEN", "p4ssw0rd-value", "storage"} {
		if strings.Contains(settings, secret) {
			t.Errorf("deploy.yaml keeps %q:\n%s", secret, settings)
		}
	}
}

func TestDeploysRecordUploadsAndPlatformChanges(t *testing.T) {
	s := historyServer(t, "registry/web:2")
	ctx := asPerson(context.Background())
	upload := t.TempDir()
	writeFiles(t, upload, map[string]string{"main.py": "print('hi')\n", "util.py": "a = 1\n"})
	repair := func(dir string) {
		writeFiles(t, dir, map[string]string{".donkey/Dockerfile": "FROM python:3.12\nCMD python main.py\n", "util.py": "a = 2\n"})
	}

	// Build 1: the upload, plus what the build wrote and repaired.
	release := releaseFixture()
	release.Result.ImageRef = "registry/web:1"
	release.source = captureSource(upload, buildCopy(t, upload, repair), ".", "4f2a9c1b03de")
	s.persistRelease(ctx, "acme", "web", release)

	// Build 2: the same upload built the same way — no new commits, only a
	// second build on the platform commit.
	release = releaseFixture()
	release.Result.ImageRef = "registry/web:2"
	release.source = captureSource(upload, buildCopy(t, upload, repair), ".", "4f2a9c1b03de")
	s.persistRelease(ctx, "acme", "web", release)

	// Build 3: a no-build redeploy of the same image (an addon change) —
	// a build record without a commit, which must not take "live" away.
	release = releaseFixture()
	release.Result.ImageRef = "registry/web:2"
	s.persistRelease(ctx, "acme", "web", release)

	rec := historyGet(t, s.handleCommits, "/v1/spaces/acme/apps/web/commits", nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("list: %d %s", rec.Code, rec.Body.String())
	}
	var listed struct {
		Commits []SourceCommit `json:"commits"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &listed); err != nil {
		t.Fatal(err)
	}
	if len(listed.Commits) != 2 {
		t.Fatalf("history = %+v", listed)
	}
	built, uploaded := listed.Commits[0], listed.Commits[1]
	if built.Kind != history.KindPlatform || built.Author.Name != "Donkey Platform" {
		t.Fatalf("newest should be the platform's commit: %+v", built)
	}
	if !strings.HasPrefix(built.Message, "Build #1 wrote the build recipe and repaired 1 file") {
		t.Fatalf("platform message = %q", built.Message)
	}
	if built.Stats != (history.Stats{Files: 2, Additions: 3, Deletions: 1}) {
		t.Fatalf("platform stats = %+v", built.Stats)
	}
	if len(built.Builds) != 2 || built.Builds[0].Sequence != 2 || built.Builds[0].OK == nil || !*built.Builds[0].OK {
		t.Fatalf("both builds used the platform commit: %+v", built.Builds)
	}
	if !built.Live || uploaded.Live {
		t.Fatal("the commit build 2 released is what runs")
	}
	if uploaded.Kind != history.KindUpload || uploaded.Author != (history.Author{Name: "Adrian Liechti", Email: "adrian@example.com"}) {
		t.Fatalf("the upload is the person's: %+v", uploaded)
	}
	if uploaded.Message != "Deploy web at 4f2a9c1b03de" || uploaded.Stats.Additions != 2 {
		t.Fatalf("upload = %+v", uploaded)
	}

	rec = historyGet(t, s.handleCommit, "/", map[string]string{"commit": built.ID})
	var detail SourceCommitDetail
	if err := json.Unmarshal(rec.Body.Bytes(), &detail); err != nil || rec.Code != http.StatusOK {
		t.Fatalf("detail: %d %s", rec.Code, rec.Body.String())
	}
	if len(detail.Changes) != 2 || detail.Changes[1].Path != "util.py" || detail.Changes[1].Status != "modified" {
		t.Fatalf("changes = %+v", detail.Changes)
	}

	rec = historyGet(t, s.handleCommitDiff, "/?path=util.py", map[string]string{"commit": built.ID})
	var diff history.FileDiff
	if err := json.Unmarshal(rec.Body.Bytes(), &diff); err != nil || !strings.Contains(diff.Diff, "+a = 2") {
		t.Fatalf("diff: %d %s", rec.Code, rec.Body.String())
	}

	rec = historyGet(t, s.handleCommitTree, "/", map[string]string{"commit": uploaded.ID})
	var tree struct {
		Files []history.Entry `json:"files"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &tree); err != nil || len(tree.Files) != 2 {
		t.Fatalf("the upload's tree has no recipe: %s", rec.Body.String())
	}

	rec = historyGet(t, s.handleCommitFile, "/?path=util.py", map[string]string{"commit": uploaded.ID})
	if rec.Body.String() != "a = 1\n" || rec.Header().Get("Content-Type") != "text/plain; charset=utf-8" ||
		rec.Header().Get("X-Content-Type-Options") != "nosniff" || rec.Header().Get("X-Donkey-Binary") != "" {
		t.Fatalf("file: %v %q", rec.Header(), rec.Body.String())
	}
	rec = historyGet(t, s.handleCommitFile, "/?path=util.py&download=true", map[string]string{"commit": uploaded.ID})
	if !strings.HasPrefix(rec.Header().Get("Content-Disposition"), "attachment;") {
		t.Fatalf("download disposition = %q", rec.Header().Get("Content-Disposition"))
	}

	rec = historyGet(t, s.handleCommitArchive, "/", map[string]string{"commit": built.ID})
	if rec.Code != http.StatusOK || !strings.Contains(rec.Header().Get("Content-Disposition"), "web-"+built.ID[:12]+".zip") {
		t.Fatalf("archive: %d %v", rec.Code, rec.Header())
	}
	zr, err := zip.NewReader(bytes.NewReader(rec.Body.Bytes()), int64(rec.Body.Len()))
	if err != nil {
		t.Fatal(err)
	}
	names := map[string]string{}
	for _, f := range zr.File {
		r, _ := f.Open()
		b, _ := io.ReadAll(r)
		r.Close()
		names[f.Name] = string(b)
	}
	prefix := "web-" + built.ID[:12] + "/"
	if len(names) != 3 || names[prefix+"util.py"] != "a = 2\n" || names[prefix+".donkey/Dockerfile"] == "" {
		t.Fatalf("archive holds what was built: %v", names)
	}
}

func TestHistoryRequestsFailPlainly(t *testing.T) {
	s := historyServer(t, "")
	unknown := strings.Repeat("a", 40)
	if rec := historyGet(t, s.handleCommit, "/", map[string]string{"commit": unknown}); rec.Code != http.StatusNotFound {
		t.Fatalf("unknown commit: %d", rec.Code)
	}
	if rec := historyGet(t, s.handleCommit, "/", map[string]string{"commit": "main"}); rec.Code != http.StatusBadRequest {
		t.Fatalf("a branch name is not a commit id: %d", rec.Code)
	}
	if rec := historyGet(t, s.handleCommitArchive, "/", map[string]string{"commit": unknown}); rec.Code != http.StatusNotFound ||
		rec.Header().Get("Content-Disposition") != "" {
		t.Fatalf("a missing archive is an error, not an empty download: %d %v", rec.Code, rec.Header())
	}
}
