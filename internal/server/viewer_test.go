package server

import (
	"context"
	"encoding/json"
	"errors"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/adrianliechti/donkey/internal/config"
	"github.com/adrianliechti/donkey/pkg/runtime"
)

type changedViewerPolicyRuntime struct {
	*spaceTestRuntime
	reads int
	err   error
}

func (r *changedViewerPolicyRuntime) SpaceConfig(ctx context.Context, space string) (config.SpaceConfig, error) {
	r.reads++
	if r.reads > 1 {
		return config.SpaceConfig{}, r.err
	}
	return r.spaceTestRuntime.SpaceConfig(ctx, space)
}

func TestViewerStatusFailsClosedWhenPolicyChangesDuringRequest(t *testing.T) {
	for _, tc := range []struct {
		name   string
		err    error
		status int
	}{
		{"unavailable", errors.New("policy storage unavailable"), 502},
		{"revoked", nil, 403},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s, rt := spaceTestServer()
			rt.settings["team"] = config.SpaceConfig{Viewers: config.AccessConfig{Users: []string{"viewer@example.com"}}}
			rt.running = &runtime.AppStatus{Found: true, Image: "private-image", Instances: []string{"internal-pod"}}
			s.rt = &changedViewerPolicyRuntime{spaceTestRuntime: rt, err: tc.err}
			rec := callAs(t, s, "viewer@example.com", "GET", "/v1/spaces/team/apps/web/status", nil)
			if rec.Code != tc.status || strings.Contains(rec.Body.String(), "private-image") || strings.Contains(rec.Body.String(), "internal-pod") {
				t.Fatalf("status: %d %s", rec.Code, rec.Body)
			}
		})
	}
}

func TestViewerOnlySeesInformativeConsoleData(t *testing.T) {
	s, rt := spaceTestServer()
	policy := rt.settings["team"]
	policy.Developers = config.AccessConfig{}
	policy.Viewers = config.AccessConfig{Users: []string{"viewer@example.com"}}
	rt.settings["team"] = policy
	rt.ref = []runtime.AppRef{{Space: "team", App: "web"}, {Space: "other", App: "hidden"}}
	rt.running = &runtime.AppStatus{Found: true, Image: "private-registry/internal-image", Port: 8000, Instances: []string{"internal-pod"}, PublicHost: "web.team.example.com", Protected: true}
	for _, path := range []string{"/v1/fleet", "/v1/apps", "/v1/spaces", "/v1/spaces/team/apps/web/status"} {
		rec := callAs(t, s, "viewer@example.com", "GET", path, nil)
		if rec.Code != 200 {
			t.Fatalf("%s: %d %s", path, rec.Code, rec.Body)
		}
		for _, secret := range []string{"private-registry", "internal-pod", "hidden"} {
			if strings.Contains(rec.Body.String(), secret) {
				t.Fatalf("%s exposed %s: %s", path, secret, rec.Body)
			}
		}
	}
	for _, tc := range []struct{ method, path string }{
		{"GET", "/v1/spaces/team/apps/web/logs"},
		{"GET", "/v1/spaces/team/apps/web/builds"},
		{"GET", "/v1/spaces/team/apps/web/builds/1"},
		{"GET", "/v1/spaces/team/apps/web/security"},
		{"GET", "/v1/spaces/team/apps/web/env"},
		{"GET", "/v1/spaces/team/apps/web/files"},
		{"GET", "/v1/spaces/team/apps/web/files/download"},
		{"GET", "/v1/spaces/team/apps/web/network"},
		{"GET", "/v1/spaces/team/services"},
		{"GET", "/v1/spaces/team/services/db/binding"},
		{"GET", "/v1/spaces/team/services/db/tunnel"},
		{"GET", "/v1/spaces/team/services/db/databases"},
		{"GET", "/v1/spaces/team/settings"},
		{"GET", "/v1/spaces/other/apps/hidden/status"},
		{"POST", "/v1/spaces/team/apps/web/restart"},
		{"POST", "/v1/spaces/team/apps/web/diagnose"},
		{"PUT", "/v1/spaces/team/apps/web/network"},
		{"PUT", "/v1/spaces/team/settings"},
		{"DELETE", "/v1/spaces/team/apps/web"},
		{"GET", "/v1/platform/settings"},
	} {
		rec := callAs(t, s, "viewer@example.com", tc.method, tc.path, []byte(`{}`))
		if rec.Code != 403 {
			t.Errorf("%s %s: %d %s", tc.method, tc.path, rec.Code, rec.Body)
		}
	}
}

func TestSpacePublicPolicyBlocksPlansAndNetworkChanges(t *testing.T) {
	s, rt := spaceTestServer()
	sp := planned()
	sp.app.Space, sp.app.Host, sp.app.Public = "team", "web.team.example.com", true
	plan := s.renderPlan(httptest.NewRequest("GET", "/", nil).Context(), "test", sp)
	if plan.Ready || !strings.Contains(mustJSON(t, plan.Findings), "Public access is disabled") {
		t.Fatalf("public plan was not blocked: %+v", plan.Findings)
	}
	rt.running = &runtime.AppStatus{Found: true, Port: 8000}
	request := []byte(`{"mode":"web","exposure":"public","host":"web.team.example.com"}`)
	response := callAs(t, s, "owner@example.com", "PUT", "/v1/spaces/team/apps/web/network", request)
	if response.Code != 400 || rt.networkWrites != 0 || !strings.Contains(response.Body.String(), "public access is disabled") {
		t.Fatalf("network update: %d %s, writes=%d", response.Code, response.Body, rt.networkWrites)
	}
	policy := rt.settings["team"]
	policy.AllowPublic = true
	rt.settings["team"] = policy
	plan = s.renderPlan(httptest.NewRequest("GET", "/", nil).Context(), "test", sp)
	if strings.Contains(mustJSON(t, plan.Findings), "Public access is disabled") {
		t.Fatal("space opt-in did not allow public plan")
	}
}

func TestSpaceSettingsClosePublicRoutesWithoutReopeningThem(t *testing.T) {
	s, rt := spaceTestServer()
	policy := rt.settings["team"]
	policy.AllowPublic = true
	rt.settings["team"] = policy
	rt.ref = []runtime.AppRef{{Space: "team", App: "web"}, {Space: "other", App: "untouched"}}
	rt.network = runtime.NetworkState{RouteFound: true, Host: "web.team.example.com",
		Routes: []runtime.PathRouteState{{Host: "web.team.example.com", Path: "/api"}},
	}
	for _, allowPublic := range []bool{false, true} {
		policy.AllowPublic = allowPublic
		rec := callAs(t, s, "owner@example.com", "PUT", "/v1/spaces/team/settings", []byte(mustJSON(t, policy)))
		if rec.Code != 200 {
			t.Fatalf("save: %d %s", rec.Code, rec.Body)
		}
		if rt.settings["team"].AllowPublic != allowPublic || !rt.network.Protected || !rt.network.Routes[0].Protected {
			t.Fatalf("policy or protection incorrect: %+v %+v", rt.settings["team"], rt.network)
		}
		if rt.routeWrites != 1 {
			t.Fatalf("unexpected reconciliation outside the target space or after enabling public: %d", rt.routeWrites)
		}
	}
}

func TestSpaceSettingsRejectUnknownMembershipFields(t *testing.T) {
	for _, field := range []string{"roles", "usres"} {
		s, rt := spaceTestServer()
		rec := callAs(t, s, "owner@example.com", "PUT", "/v1/spaces/team/settings",
			[]byte(`{"admins":{"users":["owner@example.com"]},"viewers":{"`+field+`":["reader@example.com"]}}`))
		if rec.Code != 400 || rt.writes != 0 {
			t.Fatalf("unknown field %q accepted: %d %s", field, rec.Code, rec.Body)
		}
	}
}

func mustJSON(t *testing.T, value any) string {
	t.Helper()
	raw, err := json.Marshal(value)
	if err != nil {
		t.Fatal(err)
	}
	return string(raw)
}
