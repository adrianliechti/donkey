package addons

import (
	"strings"
	"testing"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
)

var input = Input{
	App: "shop", Space: "prod", Namespace: "donkey-prod",
	Name: "db", Version: "", Storage: "1Gi", Password: "s3cret",
}

// Every registered type has to satisfy the whole contract with its own
// defaults — a type whose default storage doesn't parse would fail its very
// first provisioning.
func TestRegistryDefaultsProvision(t *testing.T) {
	for _, a := range Types() {
		in := input
		in.Name = a.Type()
		in.Version = a.DefaultVersion()
		in.Storage = a.DefaultStorage()
		binding := a.Binding(in)
		if len(binding) == 0 {
			t.Fatalf("%s: empty binding", a.Type())
		}
		if _, ok := binding[a.CredentialKey()]; !ok {
			t.Fatalf("%s: CredentialKey %s missing from its own binding", a.Type(), a.CredentialKey())
		}
		objs, err := a.Objects(in)
		if err != nil {
			t.Fatalf("%s: %v", a.Type(), err)
		}
		var dep, svc, pvc bool
		for _, o := range objs {
			switch o.(type) {
			case *appsv1.Deployment:
				dep = true
			case *corev1.Service:
				svc = true
			case *corev1.PersistentVolumeClaim:
				pvc = true
			}
		}
		if !dep || !svc || !pvc {
			t.Fatalf("%s: expected Deployment+Service+PVC, got dep=%v svc=%v pvc=%v", a.Type(), dep, svc, pvc)
		}
	}
}

func TestPostgresBindingContract(t *testing.T) {
	a := Registry["postgres"]
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

// External and provisioned bindings are freely swappable, so both must
// speak the same set of keys.
func TestPostgresExternalMatchesProvisioned(t *testing.T) {
	a := Registry["postgres"]
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

func TestPocketbaseBinding(t *testing.T) {
	a := Registry["pocketbase"]
	in := input
	in.Name = "pocketbase"
	got := a.Binding(in)
	if want := "http://pocketbase.donkey-prod.svc:8090"; got["POCKETBASE_URL"] != want {
		t.Fatalf("POCKETBASE_URL = %q, want %q", got["POCKETBASE_URL"], want)
	}
	if got["POCKETBASE_ADMIN_EMAIL"] == "" || got["POCKETBASE_ADMIN_PASSWORD"] != "s3cret" {
		t.Fatalf("admin credentials wrong: %v", got)
	}
}

func TestPocketbaseExternalBinding(t *testing.T) {
	a := Registry["pocketbase"]
	got, err := a.ExternalBinding("https://pb.example.com/")
	if err != nil {
		t.Fatal(err)
	}
	// The trailing slash has to go: apps join paths like /api/health onto it.
	if got["POCKETBASE_URL"] != "https://pb.example.com" {
		t.Fatalf("POCKETBASE_URL = %q", got["POCKETBASE_URL"])
	}
	for _, bad := range []string{"pb.example.com", "postgres://u:p@h/db"} {
		if _, err := a.ExternalBinding(bad); err == nil {
			t.Fatalf("%q should not pass as a PocketBase address", bad)
		}
	}
}

// An attachment is a marked copy: same name and shape as any binding, plus
// the shared label that detach and cascade-remove select on.
func TestAttachmentSecret(t *testing.T) {
	sec := AttachmentSecret("prod", "shop", "donkey-prod", "db", map[string]string{"DATABASE_URL": "x"})
	if sec.Name != SecretName("shop", "db") {
		t.Fatalf("name = %q", sec.Name)
	}
	if sec.Labels["donkey.dev/shared"] != "true" {
		t.Fatalf("attachment must carry the shared label, got %v", sec.Labels)
	}
	if plain := BindingSecret("prod", "shop", "donkey-prod", "db", nil); plain.Labels["donkey.dev/shared"] != "" {
		t.Fatalf("a plain binding must not carry the shared label")
	}
}

func TestGetUnknownListsTypes(t *testing.T) {
	_, err := Get("mariadb")
	if err == nil {
		t.Fatal("expected an error")
	}
	for _, want := range []string{"pocketbase", "postgres"} {
		if !strings.Contains(err.Error(), want) {
			t.Fatalf("error should offer %s: %v", want, err)
		}
	}
}
