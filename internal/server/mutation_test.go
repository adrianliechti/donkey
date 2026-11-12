package server

import (
	"bytes"
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/adrianliechti/donkey/internal/ai"
	"github.com/adrianliechti/donkey/internal/config"
	"github.com/adrianliechti/donkey/internal/history"
	"github.com/adrianliechti/donkey/pkg/runtime"
)

type recordingRuntime struct {
	releaseRuntime
	deployed, deleted bool
	recording, resume chan struct{}
}

func (r *recordingRuntime) Deploy(context.Context, runtime.DeploySpec, runtime.Progress) error {
	r.deployed = true
	return nil
}

func (r *recordingRuntime) Status(ctx context.Context, space, app string) (*runtime.AppStatus, error) {
	if r.deployed && r.recording != nil {
		close(r.recording)
		r.recording = nil
		<-r.resume
	}
	return r.releaseRuntime.Status(ctx, space, app)
}

func (r *recordingRuntime) Destroy(context.Context, string, string, bool) error {
	r.deleted = true
	return nil
}

func (r *recordingRuntime) DeleteSpace(context.Context, string) error {
	r.deleted = true
	return nil
}

func TestDeletionWaitsForTheDeployReleaseRecord(t *testing.T) {
	for _, target := range []string{"/v1/spaces/team/apps/web", "/v1/spaces/team"} {
		t.Run(target, func(t *testing.T) {
			recording := make(chan struct{})
			rt := &recordingRuntime{
				releaseRuntime: releaseRuntime{status: &runtime.AppStatus{Found: true}, meta: map[string]string{}},
				recording:      recording, resume: make(chan struct{}),
			}
			s := planServer(rt, &ai.Analysis{})
			s.cfg.DataDir = t.TempDir()
			var err error
			s.history, err = history.New(t.TempDir())
			if err != nil {
				t.Fatal(err)
			}
			s.cfg.Registry = config.Registry{URL: "registry.example.com"}
			// Resolve the lock from donkey.yaml, without a space query parameter.
			upload := appUpload(t, map[string]string{"donkey.yaml": "name: web\nspace: team\nport: 4000\n"})
			deploy := httptest.NewRequest(http.MethodPost, "/v1/deploys?noBuild=true&tag=registry.example.com/web:v1", bytes.NewReader(upload))
			asForwarded(t, deploy)
			handler := s.orchestratorRoutes()
			result := httptest.NewRecorder()
			deployDone := make(chan struct{})
			go func() {
				defer close(deployDone)
				handler.ServeHTTP(result, deploy)
			}()
			waitMutation(t, recording)
			remove := httptest.NewRequest(http.MethodDelete, target, nil)
			asForwarded(t, remove)
			deleteDone := make(chan struct{})
			go func() {
				defer close(deleteDone)
				handler.ServeHTTP(httptest.NewRecorder(), remove)
			}()
			expectMutationBlocked(t, deleteDone)
			close(rt.resume)
			waitMutation(t, deployDone)
			waitMutation(t, deleteDone)
			if !rt.deleted || !strings.Contains(result.Body.String(), `"ok":true`) {
				t.Fatalf("deploy/delete did not finish: deleted=%v, deploy=%s", rt.deleted, result.Body.String())
			}
			if got := s.board.list(time.Now(), func(string) bool { return true }); len(got) != 0 {
				t.Fatalf("deletion left a deployment placeholder: %+v", got)
			}
		})
	}
}

func TestOrchestratedMutationsRespectAppAndSpaceBoundaries(t *testing.T) {
	for _, tc := range []struct {
		name, first, second string
		blocked             bool
	}{
		{"same app", "PUT /v1/spaces/team/apps/web/network", "DELETE /v1/spaces/team/apps/web", true},
		{"app environment", "PUT /v1/spaces/team/apps/web/env", "POST /v1/spaces/team/apps/web/restart", true},
		{"settings and app", "PUT /v1/spaces/team/settings", "PUT /v1/spaces/team/apps/web/network", true},
		{"app and settings", "PUT /v1/spaces/team/apps/web/network", "PUT /v1/spaces/team/settings", true},
		{"deletion and history", "DELETE /v1/spaces/team", "GET /v1/spaces/team/apps/web/commits", true},
		{"different apps", "PUT /v1/spaces/team/apps/web/network", "POST /v1/spaces/team/apps/api/restart", false},
		{"different spaces", "DELETE /v1/spaces/team", "PUT /v1/spaces/other/settings", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s := &Server{}
			mux := http.NewServeMux()
			release := make(chan struct{})
			entered := make(chan struct{})
			for _, route := range s.orchestrated() {
				mux.HandleFunc(route.pattern, s.serializeMutation(func(w http.ResponseWriter, r *http.Request) {
					if r.Method+" "+r.URL.Path == tc.first {
						close(entered)
						<-release
					}
				}))
			}
			call := func(request string) <-chan struct{} {
				done := make(chan struct{})
				go func() {
					defer close(done)
					method, path, _ := strings.Cut(request, " ")
					mux.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest(method, path, nil))
				}()
				return done
			}
			first := call(tc.first)
			waitMutation(t, entered)
			second := call(tc.second)
			if tc.blocked {
				expectMutationBlocked(t, second)
			} else {
				waitMutation(t, second)
			}
			close(release)
			waitMutation(t, first)
			waitMutation(t, second)
		})
	}
}

func waitMutation(t *testing.T, done <-chan struct{}) {
	t.Helper()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("request did not reach the expected stage")
	}
}

func expectMutationBlocked(t *testing.T, done <-chan struct{}) {
	t.Helper()
	select {
	case <-done:
		t.Error("request overtook an unfinished mutation")
	case <-time.After(50 * time.Millisecond):
	}
}
