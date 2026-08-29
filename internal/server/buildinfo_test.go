package server

import (
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/adrianliechti/donkey/internal/ai"
	"github.com/adrianliechti/donkey/internal/config"
	"github.com/adrianliechti/donkey/internal/detect"
)

func TestDockerfileBaseImages(t *testing.T) {
	source := []byte(`# syntax=docker/dockerfile:1
ARG NODE_VERSION=22
FROM --platform=$BUILDPLATFORM node:${NODE_VERSION}-alpine AS build
FROM build AS tests
FROM gcr.io/distroless/nodejs22-debian12 \
  AS runtime
FROM gcr.io/distroless/nodejs22-debian12 AS duplicate
`)
	want := []string{"node:22-alpine", "gcr.io/distroless/nodejs22-debian12"}
	if got := dockerfileBaseImages(source); !reflect.DeepEqual(got, want) {
		t.Fatalf("base images = %v, want %v", got, want)
	}
}

func signalText(signals []PlanSignal) string {
	var values []string
	for _, signal := range signals {
		values = append(values, signal.Value)
	}
	return strings.Join(values, ", ")
}

// These are the projects people use to judge the browser release flow. Keep
// the right-hand detection panel populated for the representative shapes:
// plain files, Dockerfiles, Node and Python frameworks, and a Go service.
func TestPlanBuildInfoForTestdataApps(t *testing.T) {
	root, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	root = filepath.Join(root, "..", "..", "testdata")
	cases := []struct {
		path        string
		wantStack   string
		wantManager string
		wantBase    string
	}{
		{path: "static-site", wantStack: "plain HTML → ."},
		{path: "fastapi-sample", wantStack: "FastAPI", wantManager: "Python: pip"},
		{path: "dockerfile-sample", wantStack: "Static files", wantBase: "busybox:1.36"},
		{path: "express-api", wantStack: "Node.js", wantManager: "Node: npm"},
		{path: "nextjs-app", wantStack: "Next.js", wantManager: "Node: npm"},
		{path: "My Cool App", wantStack: "plain HTML → ."},
		{path: "odd-port-sample", wantStack: "Node.js", wantManager: "Node: npm"},
		{path: "sqlite-notes", wantStack: "FastAPI", wantManager: "Python: pip"},
		{path: "monorepo-root/services/api", wantStack: "FastAPI", wantManager: "Python: pip"},
		{path: "addons-sample", wantStack: "Node.js", wantManager: "Node: npm"},
		{path: "flask-beginner", wantStack: "Flask", wantManager: "Python: pip"},
		{path: "streamlit-report", wantStack: "Streamlit", wantManager: "Python: pip"},
		{path: "django-blog", wantStack: "Django", wantManager: "Python: pip"},
		{path: "local-habits", wantStack: "Vite → ./dist", wantManager: "Node: npm"},
		{path: "kitchen-sink-sample/api", wantStack: "FastAPI", wantManager: "Python: pip"},
		{path: "kitchen-sink-sample/web", wantStack: "Vite → ./dist", wantManager: "Node: npm"},
		{path: "kitchen-sink-sample/worker", wantStack: "Go", wantManager: "Go: go mod download"},
		{path: "kitchen-sink-sample/gateway", wantBase: "busybox:1.36"},
		// Tcl deliberately has no deterministic detector. Its non-empty AI
		// build summary is the useful fact until the assistant names the app.
		{path: "kitchen-sink-sample/legacy"},
	}

	for _, tc := range cases {
		t.Run(strings.ReplaceAll(tc.path, "/", "_"), func(t *testing.T) {
			dir := filepath.Join(root, filepath.FromSlash(tc.path))
			det, err := detect.Detect(dir, nil)
			if err != nil {
				t.Fatal(err)
			}
			got := describePlanBuild(dir, det, ai.Scan(dir))
			stack := signalText(append(append([]PlanSignal{}, got.Runtimes...), got.Frameworks...))
			managers := signalText(got.PackageManagers)
			t.Logf("stack=%q packages=%q bases=%v system=%+v", stack, managers, got.BaseImages, got.SystemPackages)
			if got.Summary == "" || got.Strategy == "" {
				t.Errorf("build description is incomplete: %+v", got)
			}

			if tc.wantStack != "" && !strings.Contains(stack, tc.wantStack) {
				t.Errorf("stack %q does not contain %q", stack, tc.wantStack)
			}
			if tc.wantManager != "" && !strings.Contains(managers, tc.wantManager) {
				t.Errorf("package managers %q do not contain %q", managers, tc.wantManager)
			}
			if tc.wantBase != "" && !slicesContains(got.BaseImages, tc.wantBase) {
				t.Errorf("base images %v do not contain %q", got.BaseImages, tc.wantBase)
			}
		})
	}
}

func slicesContains(values []string, want string) bool {
	for _, value := range values {
		if value == want {
			return true
		}
	}
	return false
}

func TestDescribePlanBuildCarriesDetectionEvidence(t *testing.T) {
	dir := t.TempDir()
	write := func(name, content string) {
		t.Helper()
		if err := writeGeneratedFile(dir, name, content); err != nil {
			t.Fatal(err)
		}
	}
	write("Dockerfile", "FROM python:3.12-slim\n")

	got := describePlanBuild(dir, &detect.Detection{
		Strategy:       config.BuildDockerfile,
		DockerfilePath: "Dockerfile",
		Summary:        "your Dockerfile",
	}, &ai.Signals{
		Runtimes:       []ai.Signal{{Value: "Python", Evidence: "requirements.txt"}},
		PackageManager: []ai.Signal{{Value: "pip", Evidence: "requirements.txt"}},
		SystemPackages: []ai.SystemPackage{{Dependency: "psycopg2", Build: []string{"libpq-dev"}, Runtime: []string{"libpq5"}}},
	})

	if !reflect.DeepEqual(got.BaseImages, []string{"python:3.12-slim"}) {
		t.Errorf("base images = %v", got.BaseImages)
	}
	if len(got.Runtimes) != 1 || got.Runtimes[0].Evidence != "requirements.txt" {
		t.Errorf("runtime evidence = %+v", got.Runtimes)
	}
	if len(got.PackageManagers) != 1 || got.PackageManagers[0].Value != "pip" {
		t.Errorf("package managers = %+v", got.PackageManagers)
	}
	if len(got.SystemPackages) != 1 || !reflect.DeepEqual(got.SystemPackages[0].Runtime, []string{"libpq5"}) {
		t.Errorf("system packages = %+v", got.SystemPackages)
	}
}
