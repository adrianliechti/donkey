package manifests

import (
	"flag"
	"os"
	"path/filepath"
	"strings"
	"testing"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"

	"monobox.ch/donkey/internal/config"
)

var update = flag.Bool("update", false, "rewrite golden files")

func testInput(t *testing.T) Input {
	t.Helper()
	replicas := int32(2)
	app := &config.AppConfig{
		Name:     "shop",
		Space:    "acme",
		Port:     8000,
		Replicas: &replicas,
		Env:      map[string]string{"LOG_LEVEL": "info", "APP_MODE": "prod"},
		Healthcheck: config.Healthcheck{
			Path: "/healthz",
		},
		Addons: []config.AddonConfig{{Name: "db", Type: "postgres"}},
	}
	app.ApplyDefaults()
	// ApplyDefaults keeps the explicit replicas/healthcheck values.
	return Input{
		App:              app,
		Space:            "acme",
		Namespace:        "donkey-acme",
		ImageRef:         "reg.example.com/donkey/shop:abc123-20260101000000",
		RegistryHost:     "reg.example.com",
		RegistryUsername: "bob",
		RegistryPassword: "hunter2",
		AddonSecretNames: []string{"shop-addon-db"},
		Domain:           "shop.acme.apps.example.com",
		TLS:              TLSConfig{Issuer: "letsencrypt", Kind: "ClusterIssuer"},
		GatewayName:      "donkey",
		GatewayNamespace: "donkey-system",
		OIDC: OIDCConfig{
			Issuer:       "https://id.example.com",
			ClientID:     "donkey-apps",
			ClientSecret: "oidc-secret",
		},
	}
}

func TestProtectionRendering(t *testing.T) {
	// Protected app: HTTPRoute + SecurityPolicy + oidc secret.
	objs, err := AppObjects(testInput(t))
	if err != nil {
		t.Fatal(err)
	}
	out, err := Render(objs)
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"kind: HTTPRoute", "kind: SecurityPolicy", "oauth2/callback", "shop-oidc"} {
		if !strings.Contains(out, want) {
			t.Errorf("protected app should render %s", want)
		}
	}
	if strings.Contains(out, "kind: Ingress") {
		t.Error("Ingress must not be rendered anymore")
	}

	// public: true opts out of the SecurityPolicy but keeps the route.
	in := testInput(t)
	in.App.Public = true
	objs, err = AppObjects(in)
	if err != nil {
		t.Fatal(err)
	}
	out, err = Render(objs)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(out, "SecurityPolicy") {
		t.Error("public app must not get a SecurityPolicy")
	}
	if !strings.Contains(out, "kind: HTTPRoute") {
		t.Error("public app still needs its route")
	}
}

func TestAppObjectsGolden(t *testing.T) {
	objs, err := AppObjects(testInput(t))
	if err != nil {
		t.Fatal(err)
	}
	got, err := Render(objs)
	if err != nil {
		t.Fatal(err)
	}
	goldenPath := filepath.Join("..", "..", "..", "testdata", "golden", "app-objects.yaml")
	if *update {
		if err := os.MkdirAll(filepath.Dir(goldenPath), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(goldenPath, []byte(got), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	want, err := os.ReadFile(goldenPath)
	if err != nil {
		t.Fatalf("golden file missing — run: go test ./internal/k8s/manifests -update (%v)", err)
	}
	if got != string(want) {
		t.Errorf("rendered manifests differ from golden file.\nRun `go test ./internal/k8s/manifests -update` if the change is intended.\n--- got ---\n%s", got)
	}
}

func TestRenderPrunesNoise(t *testing.T) {
	objs, err := AppObjects(testInput(t))
	if err != nil {
		t.Fatal(err)
	}
	out, err := Render(objs)
	if err != nil {
		t.Fatal(err)
	}
	for _, forbidden := range []string{"creationTimestamp", "status: {}"} {
		if strings.Contains(out, forbidden) {
			t.Errorf("rendered output should not contain %q", forbidden)
		}
	}
}

func TestPullSecretSkippedWithoutCreds(t *testing.T) {
	in := testInput(t)
	in.RegistryUsername = ""
	in.RegistryPassword = ""
	objs, err := AppObjects(in)
	if err != nil {
		t.Fatal(err)
	}
	out, err := Render(objs)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(out, "dockerconfigjson") {
		t.Error("anonymous registry should not produce a pull secret")
	}
	if strings.Contains(out, "imagePullSecrets") {
		t.Error("anonymous registry should not reference a pull secret")
	}
}

// Storage is one claim with a folder per path — and an app that has one can
// only run as a single copy.
func TestStorageRendering(t *testing.T) {
	in := testInput(t)
	in.App.Replicas = nil
	in.App.Storage = &config.StorageConfig{Size: "5Gi", Paths: []string{"/app/data", "/app/uploads"}}
	in.App.ApplyDefaults()
	objs, err := AppObjects(in)
	if err != nil {
		t.Fatal(err)
	}
	out, err := Render(objs)
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{
		"kind: PersistentVolumeClaim",
		"name: shop-data",
		"storage: 5Gi",
		"mountPath: /app/data",
		"subPath: app-data",
		"mountPath: /app/uploads",
		"subPath: app-uploads",
		"claimName: shop-data",
		"type: Recreate",
		"fsGroup: 2000",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("an app with storage should render %s", want)
		}
	}
	if strings.Count(out, "claimName: shop-data") != 1 {
		t.Error("both paths must share the single claim")
	}
	var dep *appsv1.Deployment
	for _, o := range objs {
		if d, ok := o.(*appsv1.Deployment); ok {
			dep = d
		}
	}
	if dep == nil || *dep.Spec.Replicas != 1 {
		t.Error("storage pins the app to one copy")
	}
}

// Without storage nothing changes: no claim, no volume, normal rollout.
func TestNoStorageNoClaim(t *testing.T) {
	objs, err := AppObjects(testInput(t))
	if err != nil {
		t.Fatal(err)
	}
	out, err := Render(objs)
	if err != nil {
		t.Fatal(err)
	}
	for _, forbidden := range []string{"PersistentVolumeClaim", "volumeMounts", "type: Recreate"} {
		if strings.Contains(out, forbidden) {
			t.Errorf("an app without storage must not render %s", forbidden)
		}
	}
}

// Every app object says which space it belongs to, and the namespace is the
// space's — not the app's.
func TestSpaceLabels(t *testing.T) {
	objs, err := AppObjects(testInput(t))
	if err != nil {
		t.Fatal(err)
	}
	out, err := Render(objs)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out, "donkey.dev/space: acme") {
		t.Error("objects should carry the space label")
	}
	if !strings.Contains(out, "namespace: donkey-acme") {
		t.Error("objects belong to the space's namespace")
	}
	ns := objs[0].(*corev1.Namespace)
	if ns.Labels[LabelApp] != "" {
		t.Error("the namespace belongs to the space, so it must not be labelled with one app")
	}
}

// Each app hostname gets its own certificate, next to the gateway — that is
// what keeps donkey compatible with every cert-manager solver.
func TestAppCertificate(t *testing.T) {
	objs, err := AppObjects(testInput(t))
	if err != nil {
		t.Fatal(err)
	}
	out, err := Render(objs)
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{
		"kind: Certificate",
		"name: donkey-tls-acme-shop",
		"secretName: donkey-tls-acme-shop",
		"- shop.acme.apps.example.com",
		"name: letsencrypt",
		"namespace: donkey-system",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("the app's certificate should render %s", want)
		}
	}
}

func TestBadResourceQuantity(t *testing.T) {
	in := testInput(t)
	in.App.Resources = &config.Resources{Requests: map[string]string{"cpu": "lots"}}
	if _, err := AppObjects(in); err == nil {
		t.Fatal("invalid quantity should error")
	}
}
