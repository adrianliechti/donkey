package docker

import (
	"os"
	"path/filepath"
	"reflect"
	"testing"
)

// The space's own binding is record <name>; an app's copy is <app>.<name>.
// Older state filed the space's under a pseudo-app "shared", which moves.
// The fixtures are legacy JSON files, which read as records do.
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
	// The pseudo-app layout was JSON only; the YAML copy of an app called
	// "shared" is not migrated away at the next startup.
	if err := s.migrateSharedBindings("team"); err != nil {
		t.Fatal(err)
	}
	if got, err := s.binding("team", "shared", "postgres"); err != nil || got["DATABASE_URL"] != "copy" {
		t.Fatalf("the copy of an app named shared was migrated: %v, %v", got, err)
	}
}

// Records are written as YAML; JSON written by older versions is read until
// the next write replaces it, and a remove takes both.
func TestRecordsAreYAMLWithLegacyJSONFallback(t *testing.T) {
	s := state{dir: t.TempDir()}
	write := func(body string, parts ...string) {
		t.Helper()
		p := s.path(parts...)
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	exists := func(parts ...string) bool {
		_, err := os.Stat(s.path(parts...))
		return err == nil
	}

	write(`{"MODE":"legacy","KEEP":"me"}`, "env", "team", "api.json")
	if env, err := s.env("team", "api"); err != nil || env["MODE"] != "legacy" {
		t.Fatalf("legacy env = %v, %v", env, err)
	}
	env, _ := s.env("team", "api")
	env["MODE"] = "current"
	if err := s.setEnv("team", "api", env); err != nil {
		t.Fatal(err)
	}
	if exists("env", "team", "api.json") {
		t.Fatal("the legacy JSON survived its replacement")
	}
	data, err := os.ReadFile(s.path("env", "team", "api.yaml"))
	if err != nil || string(data) != "KEEP: me\nMODE: current\n" {
		t.Fatalf("env record = %q, %v", data, err)
	}
	if info, err := os.Stat(s.path("env", "team", "api.yaml")); err != nil || info.Mode().Perm() != 0o600 {
		t.Fatalf("env record is not private: %v", err)
	}

	// An interrupted rewrite leaves both: one record, read from its YAML.
	write(`{"host":"old.example.com","path":"/","backend":"http://old:80"}`, "routes", "team", "web.json")
	if err := s.setRoute("team", "web", Route{Space: "team", App: "web", Host: "new.example.com", Path: "/", Backend: "http://new:80"}); err != nil {
		t.Fatal(err)
	}
	write(`{"host":"old.example.com","path":"/","backend":"http://old:80"}`, "routes", "team", "web.json")
	write(`{"host":"legacy.example.com","path":"/","backend":"http://legacy:80"}`, "routes", "team", "legacy.json")
	write(`{}`, "routes", "team", ".web.yaml-123")
	if names, err := s.listRoutes("team"); err != nil || !reflect.DeepEqual(names, []string{"legacy", "web"}) {
		t.Fatalf("routes = %v, %v", names, err)
	}
	routes, err := ReadRoutes(s.dir)
	if err != nil || len(routes) != 2 {
		t.Fatalf("gateway routes = %+v, %v", routes, err)
	}
	hosts := map[string]bool{}
	for _, r := range routes {
		hosts[r.Host] = true
	}
	if !hosts["legacy.example.com"] || !hosts["new.example.com"] || hosts["old.example.com"] {
		t.Fatalf("gateway read the wrong copies: %+v", routes)
	}
	if web, err := s.route("team", "web"); err != nil || web.Space != "team" || web.App != "web" {
		t.Fatalf("route lost the fields Kubernetes keeps out of its YAML: %+v, %v", web, err)
	}
	if err := s.removeRoute("team", "web"); err != nil {
		t.Fatal(err)
	}
	if exists("routes", "team", "web.yaml") || exists("routes", "team", "web.json") {
		t.Fatal("remove left a copy of the record")
	}

	// Create-only treats a legacy record as existing.
	write(`{"admins":{"users":["owner@example.com"]}}`, "platform", "config.json")
	if err := s.storeRecord(map[string]string{"admins": "intruder"}, false, "platform", "config"); err != nil {
		t.Fatal(err)
	}
	if exists("platform", "config.yaml") {
		t.Fatal("create-only wrote next to a legacy record")
	}

	// Folder listings take both formats.
	write(`{"DATABASE_URL":"space"}`, "bindings", "team", "postgres.json")
	if err := s.setBinding("team", "api", "postgres", map[string]string{"DATABASE_URL": "copy"}); err != nil {
		t.Fatal(err)
	}
	write(`{"REDIS_URL":"legacy-copy"}`, "bindings", "team", "web.redis.json")
	if names, err := s.listBindings("team", sharedOwner); err != nil || !reflect.DeepEqual(names, []string{"postgres"}) {
		t.Fatalf("space bindings = %v, %v", names, err)
	}
	if apps, err := s.attachments("team", "postgres"); err != nil || !reflect.DeepEqual(apps, []string{"api"}) {
		t.Fatalf("postgres attachments = %v, %v", apps, err)
	}
	if apps, err := s.attachments("team", "redis"); err != nil || !reflect.DeepEqual(apps, []string{"web"}) {
		t.Fatalf("redis attachments = %v, %v", apps, err)
	}

	// Readers outside the provider name the file the way they were
	// configured; the YAML still wins.
	write(`[{"name":"legacy","targetUrl":"http://legacy"}]`, "proxy", "services.json")
	if data, err := ReadRecordFile(s.path("proxy", "services.json")); err != nil || string(data) != `[{"name":"legacy","targetUrl":"http://legacy"}]` {
		t.Fatalf("legacy proxy services = %q, %v", data, err)
	}
	if err := s.writeRecord([]map[string]string{{"name": "erp", "targetUrl": "http://erp"}}, "proxy", "services"); err != nil {
		t.Fatal(err)
	}
	for _, configured := range []string{"services.json", "services.yaml"} {
		data, err := ReadRecordFile(s.path("proxy", configured))
		if err != nil || string(data) != "- name: erp\n  targetUrl: http://erp\n" {
			t.Fatalf("%s read %q, %v", configured, data, err)
		}
	}
}

// Values YAML would read as something else stay the strings they were.
func TestRecordValuesRoundTrip(t *testing.T) {
	s := state{dir: t.TempDir()}
	env := map[string]string{
		"BOOL": "yes", "TRUE": "true", "NULL": "null", "TILDE": "~", "EMPTY": "",
		"OCTAL": "0123", "FLOAT": "1e3", "VERSION": "11.4", "TIME": "2026-10-24T10:00:00Z",
		"MULTILINE": "-----BEGIN KEY-----\nabc\n-----END KEY-----\n", "SPACES": "  padded  ",
		"REF": "${db.DATABASE_URL}", "COLON": "a: b", "HASH": "#not a comment", "QUOTES": `"'`,
		"LONG":    "a very long value with spaces that goes well past eighty columns, so a YAML emitter might fold it",
		"UNICODE": "grüezi ✓", "STAR": "*alias", "DASH": "- item",
	}
	if err := s.setEnv("team", "api", env); err != nil {
		t.Fatal(err)
	}
	got, err := s.env("team", "api")
	if err != nil || !reflect.DeepEqual(got, env) {
		t.Fatalf("round trip changed values:\n got %q\nwant %q\n%v", got, env, err)
	}
	// Hand-edited YAML is read as leniently as it is written: an emptied file
	// is an empty record, an unquoted number is still a string.
	if err := os.WriteFile(s.path("env", "team", "api.yaml"), nil, 0o600); err != nil {
		t.Fatal(err)
	}
	if got, err := s.env("team", "api"); err != nil || got == nil || len(got) != 0 {
		t.Fatalf("emptied env = %v, %v", got, err)
	}
	if err := os.WriteFile(s.path("env", "team", "api.yaml"), []byte("WORKERS: 4\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if got, err := s.env("team", "api"); err != nil || got["WORKERS"] != "4" {
		t.Fatalf("hand-edited env = %v, %v", got, err)
	}
}
