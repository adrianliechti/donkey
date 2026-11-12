package docker

import (
	"bytes"
	"log"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/adrianliechti/donkey/internal/config"
)

func TestLegacyRoleMigrationPreservesUsersAndGroups(t *testing.T) {
	s := state{dir: t.TempDir()}
	files := map[string]string{
		"routes/team/web.json":    `{"host":"web.example.com","backend":"http://web:80","access":{"users":["reader@example.com"],"roles":["old-reader"]}}`,
		"spaces/team/config.yaml": "admins: {users: [owner@example.com], roles: [old-owner]}\ndevelopers: {groups: [builders]}\nviewers: {roles: [old-viewer]}\n",
		"platform/config.yaml":    "admins: {users: [root@example.com], roles: [old-root]}\n",
	}
	for path, data := range files {
		if err := os.MkdirAll(filepath.Dir(s.path(path)), 0700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(s.path(path), []byte(data), 0600); err != nil {
			t.Fatal(err)
		}
	}
	var warnings bytes.Buffer
	previous := log.Writer()
	log.SetOutput(&warnings)
	t.Cleanup(func() { log.SetOutput(previous) })
	for range 2 {
		if err := s.migrateLegacyRoles(); err != nil {
			t.Fatal(err)
		}
	}
	for _, role := range []string{"old-reader", "old-owner", "old-viewer", "old-root"} {
		if strings.Count(warnings.String(), role) != 1 {
			t.Fatalf("missing or repeated warning for %s: %s", role, &warnings)
		}
	}
	route, err := s.route("team", "web")
	if err != nil || route.Backend != "http://web:80" || !reflect.DeepEqual(route.Access.Users, []string{"reader@example.com"}) || len(route.Access.Groups) != 0 {
		t.Fatalf("route = %+v, %v", route, err)
	}
	var space config.SpaceConfig
	if _, err := s.readRecord(&space, "spaces", "team", "config"); err != nil {
		t.Fatal(err)
	}
	if !space.Viewers.Empty() || !reflect.DeepEqual(space.Admins.Users, []string{"owner@example.com"}) || !reflect.DeepEqual(space.Developers.Groups, []string{"builders"}) {
		t.Fatalf("grants changed: %+v", space)
	}
	var platform config.PlatformConfig
	if _, err := s.readRecord(&platform, "platform", "config"); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(platform.Admins.Users, []string{"root@example.com"}) || len(platform.Admins.Groups) != 0 {
		t.Fatalf("platform = %+v", platform)
	}
	for _, path := range []string{"routes/team/web.yaml", "spaces/team/config.yaml", "platform/config.yaml"} {
		raw, err := os.ReadFile(s.path(path))
		if err != nil || strings.Contains(string(raw), "roles:") {
			t.Fatalf("legacy roles remain: %s, %v", raw, err)
		}
	}
}
