package server

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/adrianliechti/donkey/internal/config"
	"github.com/adrianliechti/donkey/internal/telemetry"
	"github.com/adrianliechti/donkey/pkg/runtime"
)

type releaseRuntime struct {
	planRuntime
	status *runtime.AppStatus
	meta   map[string]string
	env    map[string]string
}

func (releaseRuntime) ValidateSpace(string) error { return nil }
func (r releaseRuntime) Status(context.Context, string, string) (*runtime.AppStatus, error) {
	return r.status, nil
}
func (releaseRuntime) HasData(context.Context, string, string) bool { return false }
func (r releaseRuntime) GetEnv(context.Context, string, string) (map[string]string, error) {
	return r.env, nil
}
func (releaseRuntime) ListServices(context.Context, string, string) ([]runtime.ServiceBindingInfo, error) {
	return nil, nil
}
func (r releaseRuntime) AppMeta(_ context.Context, _, _, key string) (string, error) {
	return r.meta[key], nil
}
func (r releaseRuntime) SetAppMeta(_ context.Context, _, _, key string, value *string) error {
	if value == nil {
		delete(r.meta, key)
	} else {
		r.meta[key] = *value
	}
	return nil
}

func releaseFixture() AppRelease {
	return AppRelease{
		StartedAt:  "2026-08-28T12:00:00Z",
		FinishedAt: "2026-08-28T12:01:00Z",
		Plan: &Plan{
			App:      PlanApp{Name: "web", Space: "acme", Port: 8080, Exposure: "protected"},
			Build:    PlanBuild{Summary: "Node image", Strategy: "ai"},
			Findings: []Finding{}, Volumes: []PlanVolume{}, Services: []PlanService{}, Ready: true,
		},
		Update: DeployUpdate{Stage: "releasing", BaseImages: []string{"node:22-slim"}},
		Lines:  []string{"✓ Image ready"},
		Result: DeployResult{OK: true, App: "web", Space: "acme", ImageRef: "registry/web:1"},
	}
}

// fakeBuildStore stands in for donkey-telemetry: it records what the server
// stores and serves it back, over the same HTTP surface the real client uses.
type fakeBuildStore struct {
	stored  map[string]json.RawMessage // keyed space/app
	lastPut []byte
}

func (f *fakeBuildStore) handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("PUT /v1/builds", func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		f.lastPut = body
		var envelope struct {
			Space  string          `json:"space"`
			App    string          `json:"app"`
			Record json.RawMessage `json:"record"`
		}
		_ = json.Unmarshal(body, &envelope)
		f.stored[envelope.Space+"/"+envelope.App] = envelope.Record
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"sequence":1}`))
	})
	mux.HandleFunc("GET /v1/builds/record", func(w http.ResponseWriter, r *http.Request) {
		record, ok := f.stored[r.URL.Query().Get("space")+"/"+r.URL.Query().Get("app")]
		if !ok {
			http.Error(w, "no matching build record", http.StatusNotFound)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write(record)
	})
	return mux
}

func testBuildStore(t *testing.T) (*fakeBuildStore, *telemetry.Client) {
	t.Helper()
	store := &fakeBuildStore{stored: map[string]json.RawMessage{}}
	server := httptest.NewServer(store.handler())
	t.Cleanup(server.Close)
	return store, telemetry.NewClient(server.URL, bytes.Repeat([]byte{7}, 32))
}

func TestPersistReleaseRedactsAndStoresVersionedBuild(t *testing.T) {
	const secret = "this-is-a-real-secret"
	rt := releaseRuntime{
		status: &runtime.AppStatus{Found: true, UpToDate: true, WantReplica: 1, ReadyReplica: 1, Port: 8080},
		meta:   map[string]string{},
		env:    map[string]string{"OPENAI_API_KEY": secret},
	}
	store, client := testBuildStore(t)
	s := &Server{cfg: &Config{Domain: "example.com", SessionKey: testSessionKey}, rt: rt, telemetry: client}
	release := releaseFixture()
	release.Lines = append(release.Lines, "OPENAI_API_KEY="+secret)
	release.BuildLog = "Authorization: Bearer " + secret
	release.Update.Recipe = ".donkey/Dockerfile"

	s.persistRelease(context.Background(), "acme", "web", release)
	raw := store.stored["acme/web"]
	if len(raw) == 0 {
		t.Fatal("the release was not stored")
	}
	if strings.Contains(string(store.lastPut), secret) {
		t.Fatal("the stored release leaked a known app secret")
	}
	var stored AppRelease
	if err := json.Unmarshal(raw, &stored); err != nil {
		t.Fatal(err)
	}
	// The recipe itself is in the build's commit; the record says where.
	if stored.Update.Recipe != ".donkey/Dockerfile" {
		t.Fatalf("the record should name the recipe the image was built from, got %q", stored.Update.Recipe)
	}
	var envelope struct {
		Image string `json:"image"`
		OK    bool   `json:"ok"`
	}
	if err := json.Unmarshal(store.lastPut, &envelope); err != nil {
		t.Fatal(err)
	}
	if envelope.Image != "registry/web:1" || !envelope.OK {
		t.Fatalf("record envelope should carry the image and outcome, got %+v", envelope)
	}
}

func TestReleaseRecorderKeepsCompleteOutput(t *testing.T) {
	plan := releaseFixture().Plan
	plan.ID = "expiring-plan"
	plan.ExpiresAt = time.Now().Add(time.Hour).Format(time.RFC3339)
	plan.Config = "name: web\n"
	plan.Findings = nil // Go's nil slice must still become [] for the web view.
	recorder := newReleaseRecorder(time.Now(), plan)
	for i := 0; i < 3000; i++ {
		recorder.line(strings.Repeat("line", 20))
	}
	recorder.progress(strings.Repeat("build-output\n", 20_000))
	recorder.result(DeployResult{OK: true, App: "web", Space: "acme"})

	release := recorder.finish(nil, context.Background(), &config.AppConfig{Name: "web"})
	if release.Truncated || len(release.Lines) != 3000 || release.BuildLog != strings.Repeat("build-output\n", 20_000) {
		t.Fatal("release output was truncated")
	}
	if release.Plan.ID != "" || release.Plan.ExpiresAt != "" || release.Plan.Config != "" {
		t.Fatalf("ephemeral/source plan fields should not be stored: %+v", release.Plan)
	}
	if release.Plan.Findings == nil {
		t.Fatal("stored plan arrays must be normalized to [] rather than null")
	}
	raw, err := marshalRelease(release)
	if err != nil {
		t.Fatal(err)
	}
	var stored AppRelease
	if err := json.Unmarshal(raw, &stored); err != nil || stored.BuildLog != release.BuildLog {
		t.Fatalf("stored log differs: %v", err)
	}
}

func TestHandleBuildReturnsStoredRecord(t *testing.T) {
	want := releaseFixture()
	want.Plan.Findings = nil // Older records may have encoded this as null.
	raw, _ := json.Marshal(want)
	store, client := testBuildStore(t)
	store.stored["acme/web"] = raw
	s := &Server{rt: releaseRuntime{}, telemetry: client}
	req := httptest.NewRequest(http.MethodGet, "/v1/spaces/acme/apps/web/builds/1", nil)
	req.SetPathValue("space", "acme")
	req.SetPathValue("app", "web")
	req.SetPathValue("sequence", "1")
	rec := httptest.NewRecorder()

	s.handleBuild(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("want 200, got %d: %s", rec.Code, rec.Body.String())
	}
	var got AppRelease
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatal(err)
	}
	if got.Result.ImageRef != want.Result.ImageRef || got.Plan.Build.Summary != want.Plan.Build.Summary {
		t.Fatalf("stored release changed: %+v", got)
	}
	if got.Plan.Findings == nil {
		t.Fatal("legacy null arrays should be normalized when read")
	}
}
