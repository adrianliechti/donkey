package config

import (
	"testing"

	"github.com/adrianliechti/donkey/internal/identity"
)

func TestSpaceViewerMembership(t *testing.T) {
	var policy SpaceConfig
	if err := DecodeYAML([]byte(`admins:
  users: [owner@example.com]
developers:
  groups: [builders]
viewers:
  users: [viewer@example.com, "sub:service-id"]
  groups: [readers]
`), &policy); err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		id   identity.Identity
		want string
	}{
		{identity.Identity{Email: "VIEWER@example.com"}, SpaceViewer},
		{identity.Identity{Subject: "service-id"}, SpaceViewer},
		{identity.Identity{Groups: []string{"readers"}}, SpaceViewer},
		{identity.Identity{Groups: []string{"readers", "builders"}}, SpaceDeveloper},
		{identity.Identity{Email: "owner@example.com", Groups: []string{"builders"}}, SpaceAdmin},
		{identity.Identity{Email: "stranger@example.com"}, ""},
	} {
		if got := policy.Role(&tc.id); got != tc.want {
			t.Errorf("%+v: role %q, want %q", tc.id, got, tc.want)
		}
	}
	if policy.AllowPublic {
		t.Fatal("public apps must be disabled by default")
	}
}
