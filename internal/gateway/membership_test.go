package gateway

import (
	"context"
	"errors"
	"github.com/adrianliechti/donkey/internal/config"
	"github.com/adrianliechti/donkey/pkg/runtime"
	"testing"
)

type unavailablePolicySource struct{}

func (unavailablePolicySource) list(context.Context) ([]route, error) {
	return nil, errors.New("policy store unavailable")
}
func TestPolicyReadFailureRevokesCachedAppRoutes(t *testing.T) {
	s := testServer()
	s.source = unavailablePolicySource{}
	s.routes = map[string][]route{"app.example.com": {{Space: "team", App: "web"}}, "console.example.com": {{Host: "console.example.com"}}}
	if err := s.refreshRoutes(context.Background()); err == nil {
		t.Fatal("missing policy error")
	}
	if len(s.routes["app.example.com"]) != 0 || len(s.routes["console.example.com"]) != 1 {
		t.Fatalf("unsafe cached routes: %+v", s.routes)
	}
}

type snapshotSource struct{ snapshot runtime.GatewaySnapshot }

func (s *snapshotSource) GatewaySnapshot(context.Context) (runtime.GatewaySnapshot, error) {
	return s.snapshot, nil
}

func TestGatewayUsesCurrentSpacePolicy(t *testing.T) {
	ctx := context.Background()
	source := &snapshotSource{snapshot: runtime.GatewaySnapshot{
		Routes:   []config.GatewayRoute{{Space: "team", App: "web", Host: "web.example.com", Path: "/", Backend: "http://web:8000"}},
		Spaces:   map[string]config.SpaceConfig{"team": {Viewers: config.AccessConfig{Groups: []string{"readers"}}, Admins: config.AccessConfig{Users: []string{"owner@example.com"}}, Developers: config.AccessConfig{Users: []string{"dev@example.com"}}}},
		Platform: config.PlatformConfig{Admins: config.AccessConfig{Users: []string{"root@example.com"}}},
	}}
	s := testServer()
	s.source = runtimeRoutes{source: source}
	if err := s.refreshRoutes(ctx); err != nil {
		t.Fatal(err)
	}
	rt := s.routes["web.example.com"][0]
	if !rt.Login || !rt.allows("u", "viewer@example.com", []string{"readers"}) {
		t.Fatalf("default policy not enforced: %+v", rt)
	}
	for _, email := range []string{"owner@example.com", "dev@example.com", "root@example.com"} {
		if !rt.allows("u", email, nil) {
			t.Errorf("member %s denied", email)
		}
	}
	if rt.allows("u", "stranger@example.com", nil) {
		t.Fatal("unassigned sign-in admitted")
	}
	source.snapshot.Spaces["team"] = config.SpaceConfig{AllowPublic: true}
	if err := s.refreshRoutes(ctx); err != nil {
		t.Fatal(err)
	}
	rt = s.routes["web.example.com"][0]
	if rt.Login || rt.allows("u", "viewer@example.com", []string{"readers"}) {
		t.Fatal("membership removal or public opt-in was not observed")
	}
	source.snapshot.Spaces["team"] = config.SpaceConfig{}
	if err := s.refreshRoutes(ctx); err != nil {
		t.Fatal(err)
	}
	if !s.routes["web.example.com"][0].Login {
		t.Fatal("stale public route bypassed the space policy")
	}
}
