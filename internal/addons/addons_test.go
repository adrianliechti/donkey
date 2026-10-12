package addons

import (
	"context"
	"strings"
	"testing"
)

// defaultParameters resolves a type's schema with no input: its defaults.
func defaultParameters(t *testing.T, a Builtin) Parameters {
	t.Helper()
	desc, err := a.Describe(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	params, err := ResolveParameters(desc.Parameters, nil)
	if err != nil {
		t.Fatal(err)
	}
	return params
}

var input = Input{
	Space: "prod", Namespace: "donkey-prod",
	Name: "db", Version: "", Storage: "1Gi", Password: "s3cret",
}

// Every registered type has to satisfy the whole contract with its own
// defaults — a type whose default storage doesn't parse would fail its very
// first provisioning.
func TestRegistryDefaultsProvision(t *testing.T) {
	for _, entry := range Types(context.Background()) {
		a := entry.(Builtin)
		in := input
		in.Name = a.Type()
		in = in.WithParameters(defaultParameters(t, a))
		binding := a.Binding(in)
		if len(binding) == 0 {
			t.Fatalf("%s: empty binding", a.Type())
		}
		if _, ok := binding[a.CredentialKey()]; !ok {
			t.Fatalf("%s: CredentialKey %s missing from its own binding", a.Type(), a.CredentialKey())
		}
		w, err := a.Workload(in)
		if err != nil {
			t.Fatalf("%s: %v", a.Type(), err)
		}
		if w == nil {
			continue // a binding-only addon runs nothing in the space
		}
		if w.Image == "" || w.Port == 0 || w.PortName == "" || w.DataPath == "" {
			t.Fatalf("%s: incomplete workload: %+v", a.Type(), w)
		}
		if w.RunAsUser == 0 {
			t.Fatalf("%s: a provisioned service must not run as root", a.Type())
		}
		if w.ReadyHTTPPath == "" && len(w.ReadyCommand) == 0 {
			t.Fatalf("%s: a provisioned service needs a readiness check", a.Type())
		}
	}
}

func TestManagedAddonDefaultVersions(t *testing.T) {
	for typ, want := range map[string]string{
		"postgres": "18.6",
		"redis":    "7.4.11",
		"s3":       "1.0.0-rc.3",
	} {
		if got := defaultParameters(t, builtins[typ]).String(ParamVersion); got != want {
			t.Errorf("%s default version = %q, want %q", typ, got, want)
		}
	}
}

func TestPostgresBindingContract(t *testing.T) {
	a := builtins["postgres"]
	got := a.Binding(input)
	// The host is the service's plain name — reachable the way siblings
	// reach each other.
	if want := "postgres://donkey:s3cret@db.donkey-prod.svc:5432/app?sslmode=disable"; got["DATABASE_URL"] != want {
		t.Fatalf("DATABASE_URL = %q, want %q", got["DATABASE_URL"], want)
	}
	if !strings.HasPrefix(got["JDBC_DATABASE_URL"], "jdbc:postgresql://db.donkey-prod.svc:5432/app?") {
		t.Fatalf("JDBC_DATABASE_URL = %q", got["JDBC_DATABASE_URL"])
	}
	for _, k := range []string{"PGHOST", "PGPORT", "PGUSER", "PGPASSWORD", "PGDATABASE", "JDBC_DATABASE_USERNAME", "JDBC_DATABASE_PASSWORD"} {
		if got[k] == "" {
			t.Fatalf("%s missing from binding", k)
		}
	}
}

func TestManagedAddonImageRepositoryOverride(t *testing.T) {
	for typ, repository := range map[string]string{
		"postgres": "registry.example.com/mirror/library/postgres",
		"redis":    "registry.example.com/mirror/library/redis",
		"s3":       "registry.example.com/mirror/rustfs/rustfs",
	} {
		a := builtins[typ]
		in := input
		in.Version = defaultParameters(t, a).String(ParamVersion)
		in.ImageRepository = repository
		w, err := a.Workload(in)
		if err != nil {
			t.Fatal(err)
		}
		if want := repository + ":" + in.Version; w.Image != want {
			t.Errorf("%s image = %q, want %q", typ, w.Image, want)
		}
	}
}

// External and provisioned bindings are freely swappable, so both must
// speak the same set of keys.
func TestPostgresExternalMatchesProvisioned(t *testing.T) {
	a := builtins["postgres"]
	ext, err := a.ExternalBinding("postgres://u:p%40ss@db.example.com/mydb")
	if err != nil {
		t.Fatal(err)
	}
	for k := range a.Binding(input) {
		if _, ok := ext[k]; !ok {
			t.Fatalf("external binding lacks %s", k)
		}
	}
	if ext["PGPORT"] != "5432" {
		t.Fatalf("default port not filled in: %q", ext["PGPORT"])
	}
	if ext["PGPASSWORD"] != "p@ss" {
		t.Fatalf("password not decoded: %q", ext["PGPASSWORD"])
	}
	if !strings.Contains(ext["JDBC_DATABASE_URL"], "password=p%40ss") {
		t.Fatalf("JDBC password not escaped: %q", ext["JDBC_DATABASE_URL"])
	}

	for _, bad := range []string{"mysql://u:p@h/db", "postgres://u:p@/db", "postgres://u:p@host:5432/"} {
		if _, err := a.ExternalBinding(bad); err == nil {
			t.Fatalf("%q should not pass as a postgres URL", bad)
		}
	}
}

// The openai addon is the platform standing in for a service: the binding
// points at donkey-server's proxy and carries a stand-in key, never the
// real one.
func TestOpenAIBinding(t *testing.T) {
	a := builtins["openai"]
	in := input
	in.Name = "openai"
	in.AIProxyURL = "http://donkey-server.donkey-system.svc/ai/v1"
	got := a.Binding(in)
	if want := "http://donkey-server.donkey-system.svc/ai/v1"; got["OPENAI_BASE_URL"] != want {
		t.Fatalf("OPENAI_BASE_URL = %q, want %q", got["OPENAI_BASE_URL"], want)
	}
	if got["OPENAI_API_KEY"] != "s3cret" {
		t.Fatalf("the binding must carry the per-space stand-in key, got %v", got)
	}
	if w, err := a.Workload(in); err != nil || w != nil {
		t.Fatalf("openai must run nothing in the space, got %+v (%v)", w, err)
	}
	ext, err := a.ExternalBinding("https://api.openai.com/v1/")
	if err != nil {
		t.Fatal(err)
	}
	if ext["OPENAI_BASE_URL"] != "https://api.openai.com/v1" {
		t.Fatalf("external OPENAI_BASE_URL = %q", ext["OPENAI_BASE_URL"])
	}
	if _, err := a.ExternalBinding("api.openai.com"); err == nil {
		t.Fatal("a schemeless address should not pass")
	}
}

func TestGetUnknownListsTypes(t *testing.T) {
	_, err := Get(context.Background(), "mariadb")
	if err == nil {
		t.Fatal("expected an error")
	}
	for _, want := range []string{"s3", "postgres"} {
		if !strings.Contains(err.Error(), want) {
			t.Fatalf("error should offer %s: %v", want, err)
		}
	}
}
