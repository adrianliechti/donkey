package config

import (
	"reflect"
	"strings"
	"testing"
)

func TestStoredYAMLConfiguration(t *testing.T) {
	want := SpaceConfig{Admins: AccessConfig{Users: []string{"ada@example.com"}}, Developers: AccessConfig{Groups: []string{"engineering"}}, Domains: []string{"team.example.com"}}
	data, err := EncodeYAML(want)
	if err != nil || strings.HasPrefix(string(data), "{") {
		t.Fatalf("configuration was not encoded as YAML: %v", err)
	}
	var got SpaceConfig
	if err := DecodeYAML(data, &got); err != nil || !reflect.DeepEqual(got, want) {
		t.Fatalf("YAML lost settings: %+v, %v", got, err)
	}
	for _, raw := range []string{
		"domains: []\ndomains: [evil.example.com]\n",
		"admins: {usres: [ada@example.com]}\n",
		"viewers: {roles: [readers]}\n",
		"domains: []\n---\ndomains: [evil.example.com]\n",
		"domains: [unterminated\n",
	} {
		var settings SpaceConfig
		if err := DecodeYAML([]byte(raw), &settings); err == nil {
			t.Errorf("ambiguous or malformed settings accepted: %q", raw)
		}
	}
}

func TestSpaceAndAppNameRules(t *testing.T) {
	for _, space := range []string{"-team", "team-", "donkey", "platform", "system", "Team", ""} {
		if err := ValidateSpaceName(space); err == nil {
			t.Errorf("invalid or reserved space %q accepted", space)
		}
	}
	for _, space := range []string{"team", "team2", "my-team", "default", "shared"} {
		if err := ValidateSpaceName(space); err != nil {
			t.Errorf("valid space %q rejected: %v", space, err)
		}
	}
	app := &AppConfig{Name: "my-api", Space: "my-team", Port: 8080}
	app.ApplyDefaults()
	if err := app.Validate(); err != nil {
		t.Fatalf("hyphenated names rejected: %v", err)
	}
	for _, source := range []string{"donkey", "platform", "system", "My Project!", ""} {
		if got := SanitizeSpaceName(source); !ValidSpaceName(got) {
			t.Errorf("folder %q produced invalid space %q", source, got)
		}
	}
	if got := SanitizeSpaceName("My Project!"); got != "my-project" {
		t.Errorf("folder names keep their hyphens: got %q", got)
	}
}

// Object names are derived from logical names and never parsed back, so no
// segment of a name is off limits and no app name is reserved for bindings.
func TestNoNameSegmentIsReserved(t *testing.T) {
	for _, name := range []string{"shared", "binding", "binding-api", "api-binding", "api-binding-db", "api-service-db", "my-service"} {
		if err := ValidateAppName(name); err != nil {
			t.Errorf("valid app name rejected: %v", err)
		}
		if err := ValidateServiceName(name); err != nil {
			t.Errorf("valid service name rejected: %v", err)
		}
	}
	for _, name := range []string{"-api", "api-", "Api", "a.b", ""} {
		if ValidateAppName(name) == nil || ValidateServiceName(name) == nil {
			t.Errorf("invalid name accepted: %q", name)
		}
	}
}
