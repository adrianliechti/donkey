package config

import (
	"github.com/adrianliechti/donkey/internal/identity"
	"strings"
	"testing"
	"time"
)

func TestSpaceRoles(t *testing.T) {
	settings := SpaceConfig{
		Admins:     AccessConfig{Users: []string{"Ada@example.com"}, Groups: []string{"owners"}},
		Developers: AccessConfig{Groups: []string{"engineering"}},
	}
	for _, tc := range []struct {
		name, email string
		groups      []string
		want        string
	}{
		{name: "user", email: "ada@EXAMPLE.com", want: SpaceAdmin},
		{name: "admin takes precedence", groups: []string{"owners", "engineering"}, want: SpaceAdmin},
		{name: "developer group", groups: []string{"engineering"}, want: SpaceDeveloper},
		{name: "unassigned", email: "other@example.com"},
		{name: "group case matters", groups: []string{"Engineering"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := settings.Role(&identity.Identity{Email: tc.email, Groups: tc.groups}); got != tc.want {
				t.Fatalf("role = %q, want %q", got, tc.want)
			}
		})
	}
	if got := (SpaceConfig{}).Role(&identity.Identity{Email: "ada@example.com"}); got != "" {
		t.Fatalf("empty membership granted %q", got)
	}
}

func TestSpaceDomains(t *testing.T) {
	settings := DefaultSpaceConfig("team", "example.com")
	settings.Domains = append(settings.Domains, "other.example.net")
	for _, host := range []string{"team.example.com", "api.team.example.com", "deep.api.team.example.com", "web.other.example.net"} {
		if err := settings.CheckHost(host); err != nil {
			t.Fatal(err)
		}
	}
	for _, host := range []string{"example.com", "api.other.example.com", "evilteam.example.com", "team.example.com.evil.net"} {
		if err := settings.CheckHost(host); err == nil {
			t.Errorf("allowed %s", host)
		}
	}
	for _, invalid := range []string{"*", "*.example.com", "https://example.com", "example.com:443", "example.com/path", "UPPER.example.com", ".example.com"} {
		if err := (SpaceConfig{Domains: []string{invalid}}).Validate(); err == nil {
			t.Errorf("accepted %q", invalid)
		}
	}
	unrestricted := SpaceConfig{Domains: []string{}}
	if err := unrestricted.CheckAppHost(&AppConfig{Name: "api"}, "team", "example.com"); err == nil {
		t.Fatal("unrestricted space accepted an implicit hostname")
	}
	if err := unrestricted.CheckAppHost(&AppConfig{Name: "api", Host: "any.example.org"}, "team", "example.com"); err != nil {
		t.Fatal(err)
	}
	if err := unrestricted.CheckAppHost(&AppConfig{Name: "api", Internal: true}, "team", "example.com"); err != nil {
		t.Fatal(err)
	}
}

func TestFirstPlatformAdminWithoutEmailUsesSubject(t *testing.T) {
	id := &identity.Identity{Subject: "User-123"}
	settings := FirstPlatformAdmin(id)
	if err := settings.Validate(); err != nil {
		t.Fatal(err)
	}
	if !settings.IsAdmin(id) {
		t.Fatal("first sign-in without email lost its admin role")
	}
	if settings.IsAdmin(&identity.Identity{Subject: "user-123"}) || settings.IsAdmin(&identity.Identity{Email: "sub:User-123"}) {
		t.Fatal("subject matched a different subject or an email")
	}
}

// A waiver names one app and one finding, carries a reason, and expires:
// a date is valid through that day, an RFC 3339 time to the second.
func TestSecurityWaivers(t *testing.T) {
	for name, bad := range map[string]SecurityWaiver{
		"app":     {App: "Shop", ID: "CVE-1", Reason: "x"},
		"id":      {App: "shop", ID: " ", Reason: "x"},
		"reason":  {App: "shop", ID: "CVE-1"},
		"expires": {App: "shop", ID: "CVE-1", Reason: "x", Expires: "next week"},
	} {
		if err := (SpaceConfig{SecurityWaivers: []SecurityWaiver{bad}}).Validate(); err == nil {
			t.Errorf("%s: %+v validated", name, bad)
		}
	}
	now := time.Date(2026, 11, 24, 12, 0, 0, 0, time.UTC)
	settings := SpaceConfig{SecurityWaivers: []SecurityWaiver{
		{App: "shop", ID: "CVE-1", Reason: "today, still valid", Expires: "2026-11-24"},
		{App: "shop", ID: "CVE-2", Reason: "yesterday", Expires: "2026-11-23"},
		{App: "shop", ID: "CVE-3", Reason: "an hour ago", Expires: "2026-11-24T11:00:00Z"},
		{App: "shop", ID: "CVE-4", Reason: "never expires"},
		{App: "api", ID: "CVE-1", Reason: "another app"},
	}}
	if err := settings.Validate(); err != nil {
		t.Fatal(err)
	}
	got := []string{}
	for _, w := range settings.WaiversFor("shop", now) {
		got = append(got, w.ID)
	}
	if strings.Join(got, ",") != "CVE-1,CVE-4" {
		t.Errorf("active waivers = %v", got)
	}
}
