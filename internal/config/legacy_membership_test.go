package config

import (
	"reflect"
	"strings"
	"testing"
)

func TestRemoveLegacyRolesOnlyFromMembership(t *testing.T) {
	raw := []byte("admins: {users: [owner@example.com], roles: [old-owner]}\ndevelopers: {groups: [builders], roles: []}\nviewers: {roles: [old-reader]}\nofferings: {service: {config: {roles: [keep]}}}\n")
	clean, removed, err := RemoveLegacyRoles(raw, "admins", "developers", "viewers")
	if err != nil || !reflect.DeepEqual(removed, []string{"admins.roles:old-owner", "viewers.roles:old-reader"}) {
		t.Fatalf("removed = %v, %v", removed, err)
	}
	var policy SpaceConfig
	if err := DecodeYAML(clean, &policy); err != nil || !reflect.DeepEqual(policy.Admins.Users, []string{"owner@example.com"}) || !reflect.DeepEqual(policy.Developers.Groups, []string{"builders"}) || !policy.Viewers.Empty() || !strings.Contains(string(clean), "keep") {
		t.Fatalf("changed other settings or granted access: %s, %v", clean, err)
	}
	if again, roles, err := RemoveLegacyRoles(clean, "admins", "developers", "viewers"); err != nil || again != nil || len(roles) != 0 {
		t.Fatalf("not idempotent: %s %v %v", again, roles, err)
	}
	for _, malformed := range []string{"roles: readers", "roles: [123]", "roles: [a]\nroles: [b]", "roles: [a]\n---\nroles: [b]"} {
		if _, _, err := RemoveLegacyRoles([]byte(malformed), ""); err == nil {
			t.Fatalf("discarded malformed grants: %s", malformed)
		}
	}
	clean, _, err = RemoveLegacyRoles([]byte("roles: [old]\nusres: [typo@example.com]"), "")
	var access AccessConfig
	if err != nil || DecodeYAML(clean, &access) == nil {
		t.Fatal("migration hid an unknown membership field")
	}
}
