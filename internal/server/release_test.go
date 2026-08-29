package server

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/adrianliechti/donkey/internal/config"
	"github.com/adrianliechti/donkey/pkg/runtime"
)

type releaseRuntime struct {
	runtime.Provider
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

func TestPersistReleaseRedactsAndStatusOffersIt(t *testing.T) {
	const secret = "this-is-a-real-secret"
	rt := releaseRuntime{
		status: &runtime.AppStatus{Found: true, UpToDate: true, WantReplica: 1, ReadyReplica: 1, Port: 8080},
		meta:   map[string]string{},
		env:    map[string]string{"OPENAI_API_KEY": secret},
	}
	s := &Server{cfg: &Config{Domain: "example.com", SessionKey: testSessionKey}, rt: rt}
	release := releaseFixture()
	release.Lines = append(release.Lines, "OPENAI_API_KEY="+secret)
	release.BuildLog = "Authorization: Bearer " + secret

	s.persistRelease(context.Background(), "acme", "web", release)
	raw := rt.meta[runtime.MetaRelease]
	if raw == "" {
		t.Fatal("the release was not stored")
	}
	if strings.Contains(raw, secret) {
		t.Fatal("the stored release leaked a known app secret")
	}

	req := httptest.NewRequest(http.MethodGet, "/v1/spaces/acme/apps/web/status", nil)
	req.SetPathValue("space", "acme")
	req.SetPathValue("app", "web")
	rec := httptest.NewRecorder()
	s.handleStatus(rec, req)
	var status StatusResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &status); err != nil {
		t.Fatal(err)
	}
	if !status.HasRelease {
		t.Fatal("app status should advertise the stored release")
	}
}

func TestStatusHidesUnavailableOrIncompleteRelease(t *testing.T) {
	for name, raw := range map[string]string{
		"no metadata":       "",
		"malformed":         "not-json",
		"no plan or result": `{}`,
	} {
		t.Run(name, func(t *testing.T) {
			rt := releaseRuntime{
				status: &runtime.AppStatus{Found: true, UpToDate: true},
				meta:   map[string]string{runtime.MetaRelease: raw},
			}
			s := &Server{cfg: &Config{Domain: "example.com", SessionKey: testSessionKey}, rt: rt}
			req := httptest.NewRequest(http.MethodGet, "/v1/spaces/acme/apps/old/status", nil)
			req.SetPathValue("space", "acme")
			req.SetPathValue("app", "old")
			rec := httptest.NewRecorder()
			s.handleStatus(rec, req)

			var status StatusResponse
			if err := json.Unmarshal(rec.Body.Bytes(), &status); err != nil {
				t.Fatal(err)
			}
			if status.HasRelease {
				t.Fatal("an app without a complete deployment record must not advertise the tab")
			}
		})
	}
}

func TestReleaseRecorderKeepsABoundedUsefulTail(t *testing.T) {
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
	if !release.Truncated {
		t.Fatal("oversized output should say it was truncated")
	}
	if len(release.BuildLog) > releaseBuildLogBytes+64 {
		t.Fatalf("build log was not bounded: %d bytes", len(release.BuildLog))
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
	if len(raw) > releaseMetaBytes {
		t.Fatalf("stored release is %d bytes, limit is %d", len(raw), releaseMetaBytes)
	}
}

func TestHandleReleaseReturnsStoredRecord(t *testing.T) {
	want := releaseFixture()
	want.Plan.Findings = nil // Legacy records may have encoded this as null.
	raw, _ := json.Marshal(want)
	rt := releaseRuntime{meta: map[string]string{runtime.MetaRelease: string(raw)}}
	s := &Server{rt: rt}
	req := httptest.NewRequest(http.MethodGet, "/v1/spaces/acme/apps/web/release", nil)
	req.SetPathValue("space", "acme")
	req.SetPathValue("app", "web")
	rec := httptest.NewRecorder()

	s.handleRelease(rec, req)
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

func TestPrefixUTF8KeepsContentAroundInvalidBytes(t *testing.T) {
	// An invalid byte early in a diff (a binary hunk) must not eat the rest
	// of the prefix.
	diff := "keep\xffthis and everything after it 0123456789"
	got := prefixUTF8(diff, 20)
	if !strings.HasPrefix(got, "keep\xffthis") || len(got) < 20 {
		t.Fatalf("prefixUTF8 discarded content after an invalid byte: %q", got)
	}
	// A multi-byte rune straddling the cut is trimmed cleanly, not split.
	got = prefixUTF8(strings.Repeat("a", 19)+"é", 20)
	if want := strings.Repeat("a", 19) + "\n… omitted …"; got != want {
		t.Fatalf("got %q, want %q", got, want)
	}
	if short := prefixUTF8("short", 100); short != "short" {
		t.Fatalf("an under-limit value must pass through, got %q", short)
	}
}
