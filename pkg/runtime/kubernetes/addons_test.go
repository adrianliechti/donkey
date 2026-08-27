package kubernetes

import (
	"testing"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"

	"github.com/adrianliechti/donkey/internal/addons"
	"github.com/adrianliechti/donkey/internal/config"
	"github.com/adrianliechti/donkey/pkg/runtime/kubernetes/manifests"
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

func TestAddonObjectsUseRegistryCredentialsForPrivateMirror(t *testing.T) {
	in := addons.Input{App: "shared", Space: "prod", Namespace: "donkey-prod", Name: "postgres", Storage: "1Gi"}
	w := &addons.Workload{
		Image: "registry.example.com/mirror/library/postgres:16", Port: 5432, PortName: "postgres",
		DataPath: "/var/lib/postgresql/data", RunAsUser: 999, ReadyCommand: []string{"pg_isready"},
	}
	creds := &imagePullCredentials{Host: "registry.example.com", Username: "builder", Password: "s3cret"}
	objs, err := addonObjects(in, w, creds, nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(objs) != 4 {
		t.Fatalf("expected PVC+pull Secret+Deployment+Service, got %d objects", len(objs))
	}
	secret, ok := objs[1].(*corev1.Secret)
	if !ok || secret.Type != corev1.SecretTypeDockerConfigJson {
		t.Fatalf("second object is not a registry pull secret: %T", objs[1])
	}
	dep, ok := objs[2].(*appsv1.Deployment)
	if !ok || len(dep.Spec.Template.Spec.ImagePullSecrets) != 1 ||
		dep.Spec.Template.Spec.ImagePullSecrets[0].Name != manifests.PullSecretName("postgres") {
		t.Fatalf("addon deployment does not reference its pull secret: %T %+v", objs[2], dep)
	}
}

func TestAddonObjectsReferenceExistingPullSecret(t *testing.T) {
	in := addons.Input{App: "shared", Space: "prod", Namespace: "donkey-prod", Name: "postgres", Storage: "1Gi"}
	w := &addons.Workload{
		Image: "mirror.example.com/docker/library/postgres:18.6", Port: 5432, PortName: "postgres",
		DataPath: "/var/lib/postgresql/data", RunAsUser: 999, ReadyCommand: []string{"pg_isready"},
	}
	objs, err := addonObjects(in, w, nil, []string{"oci-mirror-registry-example-com", "", "oci-mirror-registry-example-com"})
	if err != nil {
		t.Fatal(err)
	}
	if len(objs) != 3 {
		t.Fatalf("expected PVC+Deployment+Service without copied credentials, got %d objects", len(objs))
	}
	dep, ok := objs[1].(*appsv1.Deployment)
	if !ok || len(dep.Spec.Template.Spec.ImagePullSecrets) != 1 ||
		dep.Spec.Template.Spec.ImagePullSecrets[0].Name != "oci-mirror-registry-example-com" {
		t.Fatalf("addon deployment does not reference the existing pull secret: %T %+v", objs[1], dep)
	}
}

func TestAddonPullCredentialsAreScopedToRegistryHost(t *testing.T) {
	t.Setenv("DONKEY_REGISTRY_PASSWORD", "s3cret")
	r := &Runtime{opts: Options{Registry: config.Registry{
		URL: "registry.example.com/platform/donkey", Username: "builder",
	}}}
	got, err := r.addonImagePullCredentials("registry.example.com/mirror/library/postgres:16")
	if err != nil || got == nil || got.Password != "s3cret" {
		t.Fatalf("same-host mirror should receive registry credentials: %+v, %v", got, err)
	}
	got, err = r.addonImagePullCredentials("docker.io/library/postgres:16")
	if err != nil || got != nil {
		t.Fatalf("public image should not receive enterprise credentials: %+v, %v", got, err)
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
	objs, err := addonObjects(in, w, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(objs) != 3 {
		t.Fatalf("expected PVC+Deployment+Service, got %d objects", len(objs))
	}

	bad := *w
	bad.Image = "postgres:16"
	in.Storage = "lots"
	if _, err := addonObjects(in, &bad, nil, nil); err == nil {
		t.Fatal("an unparseable storage size should error")
	}
}
