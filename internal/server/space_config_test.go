package server

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/adrianliechti/donkey/internal/ai"
	"github.com/adrianliechti/donkey/internal/config"
	"github.com/adrianliechti/donkey/internal/identity"
	"github.com/adrianliechti/donkey/pkg/runtime"
)

type spaceTestRuntime struct {
	planRuntime
	settings                                              map[string]config.SpaceConfig
	readErr                                               error
	ref                                                   []runtime.AppRef
	network                                               runtime.NetworkState
	restarts, creates, writes, networkWrites, routeWrites int
}

func (r *spaceTestRuntime) SpaceConfig(_ context.Context, space string) (config.SpaceConfig, error) {
	if r.readErr != nil {
		return config.SpaceConfig{}, r.readErr
	}
	settings, ok := r.settings[space]
	if !ok {
		return settings, runtime.ErrSpaceNotFound
	}
	return settings, nil
}
func (r *spaceTestRuntime) SetSpaceConfig(_ context.Context, space string, settings config.SpaceConfig) error {
	r.writes++
	r.settings[space] = settings
	return nil
}
func (r *spaceTestRuntime) CreateSpace(_ context.Context, space string) error {
	r.creates++
	r.settings[space] = config.SpaceConfig{}
	return nil
}
func (r *spaceTestRuntime) ListSpaces(context.Context) ([]string, error) {
	var spaces []string
	for space := range r.settings {
		spaces = append(spaces, space)
	}
	return spaces, nil
}
func (r *spaceTestRuntime) ListApps(context.Context) ([]runtime.AppRef, error) { return r.ref, nil }
func (r *spaceTestRuntime) GetEnv(context.Context, string, string) (map[string]string, error) {
	return map[string]string{"TOKEN": "space-secret"}, nil
}
func (r *spaceTestRuntime) Restart(context.Context, string, string) (bool, error) {
	r.restarts++
	return true, nil
}
func (r *spaceTestRuntime) Network(context.Context, string, string) (*runtime.NetworkState, error) {
	return &r.network, nil
}
func (r *spaceTestRuntime) ApplyNetwork(context.Context, *config.AppConfig) (string, error) {
	r.networkWrites++
	return "", nil
}

func (r *spaceTestRuntime) ReconcileAppRoutes(_ context.Context, _, _ string, protect bool) error {
	r.routeWrites++
	r.network.Access = nil
	if protect {
		r.network.Protected = true
	}
	for i := range r.network.Routes {
		r.network.Routes[i].Access = nil
		if protect {
			r.network.Routes[i].Protected = true
		}
	}
	return nil
}

func spaceTestServer() (*Server, *spaceTestRuntime) {
	r := &spaceTestRuntime{settings: map[string]config.SpaceConfig{
		"team":  {Admins: config.AccessConfig{Users: []string{"owner@example.com"}}, Developers: config.AccessConfig{Groups: []string{"team-dev"}}, Domains: []string{"team.example.com"}},
		"other": {Admins: config.AccessConfig{Users: []string{"other@example.com"}}, Domains: []string{"other.example.com"}},
	}}
	s := planServer(r, &ai.Analysis{Port: 4000})
	r.platform = &config.PlatformConfig{Admins: config.AccessConfig{Users: []string{"root@example.com"}}}
	return s, r
}

// callAs drives the whole path a request takes: donkey-server's routes and,
// for what they forward, donkey-orchestrator's. The same Server plays both,
// as the one binary does in production, so authorization is checked on each
// hop exactly as it would be there.
func callAs(t *testing.T, s *Server, email, method, path string, body []byte) *httptest.ResponseRecorder {
	t.Helper()
	orchestrator := httptest.NewServer(s.orchestratorRoutes())
	defer orchestrator.Close()
	s.cfg.OrchestratorURL = orchestrator.URL
	req := httptest.NewRequest(method, "https://donkey.example.com"+path, bytes.NewReader(body))
	token, err := identity.SealToken(testSessionKey, identity.Identity{Subject: email, Email: email,
		Groups: []string{"team-dev"}, Expires: time.Now().Add(time.Hour).Unix()})
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Authorization", "Bearer "+token)
	rec := httptest.NewRecorder()
	s.routes().ServeHTTP(rec, req)
	return rec
}

func TestSpaceRBACProtectsReadWriteAndMembership(t *testing.T) {
	s, r := spaceTestServer()
	for _, tc := range []struct {
		method, path string
		status       int
	}{
		{"GET", "/v1/spaces/team/apps/api/env", 200},
		{"GET", "/v1/spaces/team/settings", 200},
		{"POST", "/v1/spaces/team/apps/api/restart", 200},
		{"GET", "/v1/spaces/other/apps/api/env", 403},
		{"GET", "/v1/spaces/other/settings", 403},
		{"GET", "/v1/spaces/other/apps/api/logs", 403},
		{"GET", "/v1/spaces/other/apps/api/files", 403},
		{"GET", "/v1/spaces/other/services/db/tunnel", 403},
		{"DELETE", "/v1/spaces/team", 403},
		{"PUT", "/v1/spaces/team/settings", 403},
		{"POST", "/v1/spaces", 403},
		{"GET", "/v1/doctor", 403},
	} {
		t.Run(tc.method+tc.path, func(t *testing.T) {
			rec := callAs(t, s, "dev@example.com", tc.method, tc.path, []byte(`{}`))
			if rec.Code != tc.status {
				t.Fatalf("got %d: %s", rec.Code, rec.Body.String())
			}
		})
	}
	if r.restarts != 1 || r.writes != 0 || r.creates != 0 {
		t.Fatalf("unexpected mutations: %+v", r)
	}
	for _, path := range []string{"/v1/spaces", "/v1/apps", "/v1/fleet"} {
		r.ref = []runtime.AppRef{{Space: "other", App: "private"}}
		rec := callAs(t, s, "dev@example.com", "GET", path, nil)
		if rec.Code != 200 || strings.Contains(rec.Body.String(), "other") || strings.Contains(rec.Body.String(), "private") {
			t.Fatalf("fleet leaked another space: %d %s", rec.Code, rec.Body.String())
		}
	}
	r.readErr = errors.New("storage unavailable")
	for _, path := range []string{"/v1/spaces/team/apps/api/env", "/v1/spaces/team/settings"} {
		if rec := callAs(t, s, "dev@example.com", "GET", path, nil); rec.Code != 502 || strings.Contains(rec.Body.String(), "space-secret") {
			t.Fatalf("storage failure granted access: %d %s", rec.Code, rec.Body.String())
		}
	}
}

func TestSpaceSettingsReadPermissions(t *testing.T) {
	for _, tc := range []struct {
		email  string
		status int
	}{
		{"dev@example.com", 200},
		{"owner@example.com", 200},
		{"root@example.com", 200},
		{"viewer@example.com", 403},
		{"outsider@example.com", 403},
	} {
		t.Run(tc.email, func(t *testing.T) {
			s, rt := spaceTestServer()
			policy := rt.settings["team"]
			policy.Developers = config.AccessConfig{Users: []string{"dev@example.com"}, Groups: []string{"builders"}}
			policy.Viewers = config.AccessConfig{Users: []string{"viewer@example.com"}, Groups: []string{"readers"}}
			policy.Offerings = map[string]config.SpaceOffering{"postgres": {Config: map[string]any{"password": "space-secret"}}}
			rt.settings["team"] = policy
			rec := callAs(t, s, tc.email, "GET", "/v1/spaces/team/settings", nil)
			if rec.Code != tc.status {
				t.Fatalf("settings: %d %s", rec.Code, rec.Body)
			}
			if tc.status != 200 {
				return
			}
			var settings config.SpaceConfig
			if err := json.Unmarshal(rec.Body.Bytes(), &settings); err != nil {
				t.Fatal(err)
			}
			if !reflect.DeepEqual(settings, policy) {
				t.Fatalf("expected membership, domains and service offerings, got %s", rec.Body)
			}
			if tc.email == "dev@example.com" {
				rec = callAs(t, s, tc.email, "PUT", "/v1/spaces/team/settings", []byte(mustJSON(t, policy)))
				if rec.Code != 403 || rt.writes != 0 {
					t.Fatalf("developer changed space settings: %d %s", rec.Code, rec.Body)
				}
			}
		})
	}
}

func TestSpaceAdminCanDelegateAndRevocationIsImmediate(t *testing.T) {
	s, r := spaceTestServer()
	body := []byte(`{"admins":{"users":["owner@example.com"]},"developers":{},"domains":["team.example.com"]}`)
	if rec := callAs(t, s, "owner@example.com", "PUT", "/v1/spaces/team/settings", body); rec.Code != 200 {
		t.Fatalf("save: %d %s", rec.Code, rec.Body.String())
	}
	if rec := callAs(t, s, "dev@example.com", "POST", "/v1/spaces/team/apps/api/restart", nil); rec.Code != 403 {
		t.Fatalf("revocation: %d", rec.Code)
	}
	if rec := callAs(t, s, "dev@example.com", "GET", "/v1/spaces/team/settings", nil); rec.Code != 403 {
		t.Fatalf("revoked member can still read settings: %d", rec.Code)
	}
	if r.restarts != 0 || r.writes != 1 {
		t.Fatal("revoked developer mutated the app")
	}
}

func TestNewSpaceDomainsDefaultAndCanBeCleared(t *testing.T) {
	for _, tc := range []struct {
		body  string
		count int
	}{
		{`{"name":"new"}`, 1},
		{`{"name":"new","domains":[]}`, 0},
		{`{"name":"new","domains":["custom.example.org"]}`, 1},
	} {
		s, r := spaceTestServer()
		rec := callAs(t, s, "root@example.com", "POST", "/v1/spaces", []byte(tc.body))
		if rec.Code != 200 {
			t.Fatalf("create: %d %s", rec.Code, rec.Body.String())
		}
		if got := r.settings["new"].Domains; len(got) != tc.count {
			t.Fatalf("domains = %v", got)
		}
		if r.settings["new"].AllowPublic {
			t.Fatal("new spaces must default to public access disabled")
		}
		if tc.body == `{"name":"new"}` && r.settings["new"].Domains[0] != "new.example.com" {
			t.Fatal("wrong default suffix")
		}
		if rec := callAs(t, s, "root@example.com", "POST", "/v1/spaces", []byte(tc.body)); rec.Code != 409 {
			t.Fatal("creating twice must not reset space settings")
		}
	}
}

func TestPlanMoveCannotBypassSpaceRBAC(t *testing.T) {
	s, _ := spaceTestServer()
	app := &config.AppConfig{Name: "api", Space: "team", Port: 4000}
	app.ApplyDefaults()
	stored := &storedPlan{app: app, analysis: &ai.Analysis{}, created: time.Now()}
	id := s.plans.put(stored)
	edit := PlanEdit{Name: "api", Space: "other", Port: 4000, Exposure: "protected"}
	body, _ := json.Marshal(edit)
	if rec := callAs(t, s, "dev@example.com", "PUT", "/v1/plans/"+id, body); rec.Code != 403 {
		t.Fatalf("plan move: %d %s", rec.Code, rec.Body.String())
	}
	if stored.app.Space != "team" {
		t.Fatal("denied plan edit changed its target")
	}
	stored.app.Space = "other"
	edit.Space = "team"
	body, _ = json.Marshal(edit)
	if rec := callAs(t, s, "dev@example.com", "PUT", "/v1/plans/"+id, body); rec.Code != 403 {
		t.Fatal("stole a plan from another space")
	}
}

func TestNetworkAndUploadCannotEscapeSpaceDomains(t *testing.T) {
	s, r := spaceTestServer()
	r.running = &runtime.AppStatus{Found: true, Port: 4000}
	rec := callAs(t, s, "dev@example.com", "PUT", "/v1/spaces/team/apps/api/network",
		[]byte(`{"mode":"web","exposure":"protected","host":"api.other.example.com"}`))
	if rec.Code != 400 || r.networkWrites != 0 {
		t.Fatalf("network escaped policy: %d %s", rec.Code, rec.Body.String())
	}
	upload := appUpload(t, map[string]string{"donkey.yaml": "name: api\nspace: other\nport: 4000\n"})
	rec = callAs(t, s, "dev@example.com", "POST", "/v1/deploys", upload)
	if rec.Code != 403 {
		t.Fatalf("upload bypassed RBAC: %d %s", rec.Code, rec.Body.String())
	}
}

func TestProjectInheritsAuthorizedSpaceBeforeReadingSettings(t *testing.T) {
	s, _ := spaceTestServer()
	dir := t.TempDir()
	for _, name := range []string{"web", "api"} {
		appDir := filepath.Join(dir, name)
		if err := os.MkdirAll(appDir, 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(appDir, "Dockerfile"), []byte("FROM scratch\n"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.WriteFile(filepath.Join(dir, "api", "donkey.yaml"), []byte("name: api\nspace: team\nport: 4000\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	reading := &ai.Project{Apps: []ai.ProjectApp{{Path: "web"}, {Path: "api"}}}
	ctx := context.WithValue(context.Background(), identityContextKey{}, &identity.Identity{Email: "dev@example.com", Groups: []string{"team-dev"}})
	project, err := s.readProject(ctx, dir, reading, planInput{})
	if err != nil {
		t.Fatal(err)
	}
	for _, app := range project.apps {
		if app.app.Space != "team" || !strings.HasSuffix(app.app.Host, ".team.example.com") {
			t.Fatalf("wrong inherited settings: %+v", app.app)
		}
	}
	if err := os.WriteFile(filepath.Join(dir, "api", "donkey.yaml"), []byte("name: api\nspace: other\nport: 4000\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := s.readProject(ctx, dir, reading, planInput{}); !errors.Is(err, errSpaceForbidden) {
		t.Fatalf("unauthorized inherited space was accepted: %v", err)
	}
}
