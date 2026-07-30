package kubernetes

import (
	"testing"

	"github.com/adrianliechti/donkey/internal/addons"
)

// An attachment is a marked copy: same name and shape as any binding, plus
// the shared label that detach and cascade-remove select on.
func TestAttachmentSecret(t *testing.T) {
	sec := attachmentSecret("prod", "shop", "donkey-prod", "db", map[string]string{"DATABASE_URL": "x"})
	if sec.Name != addons.SecretName("shop", "db") {
		t.Fatalf("name = %q", sec.Name)
	}
	if sec.Labels["donkey.dev/shared"] != "true" {
		t.Fatalf("attachment must carry the shared label, got %v", sec.Labels)
	}
	if plain := bindingSecret("prod", "shop", "donkey-prod", "db", nil); plain.Labels["donkey.dev/shared"] != "" {
		t.Fatalf("a plain binding must not carry the shared label")
	}
}

// The materializer turns one Workload into the same trio every addon used to
// render by hand — with the security posture applied uniformly.
func TestAddonObjects(t *testing.T) {
	in := addons.Input{App: "shared", Space: "prod", Namespace: "donkey-prod", Name: "db", Storage: "1Gi"}
	w := &addons.Workload{
		Image: "postgres:16", Port: 5432, PortName: "postgres",
		Env:       map[string]string{"POSTGRES_USER": "donkey"},
		SecretEnv: map[string]string{"POSTGRES_PASSWORD": "PGPASSWORD"},
		DataPath:  "/var/lib/postgresql/data", RunAsUser: 999,
		ReadyCommand: []string{"pg_isready"},
	}
	objs, err := addonObjects(in, w)
	if err != nil {
		t.Fatal(err)
	}
	if len(objs) != 3 {
		t.Fatalf("expected PVC+Deployment+Service, got %d objects", len(objs))
	}

	bad := *w
	bad.Image = "postgres:16"
	in.Storage = "lots"
	if _, err := addonObjects(in, &bad); err == nil {
		t.Fatal("an unparseable storage size should error")
	}
}
