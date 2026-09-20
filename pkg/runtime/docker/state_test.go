package docker

import (
	"os"
	"path/filepath"
	"reflect"
	"testing"
)

// The space's own binding is <name>.json; an app's copy is <app>.<name>.json.
// Older state filed the space's under a pseudo-app "shared", which moves.
func TestBindingLayoutAndMigration(t *testing.T) {
	s := state{dir: t.TempDir()}
	dir := s.path("bindings", "team")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	for name, body := range map[string]string{
		"shared.postgres.json":     `{"DATABASE_URL":"old","OPENAI_API_KEY":"k1"}`,
		"shared.redis.json":        `{"REDIS_URL":"legacy"}`,
		"redis.json":               `{"REDIS_URL":"current"}`,
		"api.postgres.json":        `{"DATABASE_URL":"copy"}`,
		"shared.postgres.json.bak": `{}`,
	} {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	if err := s.migrateSharedBindings("team"); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(dir, "shared.postgres.json")); !os.IsNotExist(err) {
		t.Fatal("legacy shared binding was not moved")
	}
	got, err := s.binding("team", sharedOwner, "postgres")
	if err != nil || got["DATABASE_URL"] != "old" {
		t.Fatalf("migrated binding = %v, %v", got, err)
	}
	// An existing file at the new name wins; the legacy one is dropped.
	got, err = s.binding("team", sharedOwner, "redis")
	if err != nil || got["REDIS_URL"] != "current" {
		t.Fatalf("existing binding was overwritten: %v, %v", got, err)
	}
	if _, err := os.Stat(filepath.Join(dir, "shared.redis.json")); !os.IsNotExist(err) {
		t.Fatal("superseded legacy binding was kept")
	}
	if _, err := os.Stat(filepath.Join(dir, "shared.postgres.json.bak")); err != nil {
		t.Fatal("an unrelated file was touched")
	}

	if names, err := s.listBindings("team", sharedOwner); err != nil || !reflect.DeepEqual(names, []string{"postgres", "redis"}) {
		t.Fatalf("space bindings = %v, %v", names, err)
	}
	if names, err := s.listBindings("team", "api"); err != nil || !reflect.DeepEqual(names, []string{"postgres"}) {
		t.Fatalf("app bindings = %v, %v", names, err)
	}
	if apps, err := s.attachments("team", "postgres"); err != nil || !reflect.DeepEqual(apps, []string{"api"}) {
		t.Fatalf("attachments = %v, %v", apps, err)
	}
	if values, err := s.bindingValues("OPENAI_API_KEY", "DATABASE_URL", "old"); err != nil || !reflect.DeepEqual(values, []string{"k1"}) {
		t.Fatalf("shared values = %v, %v", values, err)
	}
	// An app may now be called "shared": its copy sits under its own name.
	if err := s.setBinding("team", "shared", "postgres", map[string]string{"DATABASE_URL": "copy"}); err != nil {
		t.Fatal(err)
	}
	if apps, err := s.attachments("team", "postgres"); err != nil || !reflect.DeepEqual(apps, []string{"api", "shared"}) {
		t.Fatalf("attachments with an app named shared = %v, %v", apps, err)
	}
	if got, err := s.binding("team", sharedOwner, "postgres"); err != nil || got["DATABASE_URL"] != "old" {
		t.Fatalf("the space's binding was confused with the app's: %v, %v", got, err)
	}
}
