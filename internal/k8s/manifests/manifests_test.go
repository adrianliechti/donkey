package manifests

import (
	"flag"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/runtime"

	"github.com/adrianliechti/donkey/internal/config"
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

// An internal app is reachable only by its siblings: it gets a Deployment and
// a Service and nothing that puts it on the internet.
func TestInternalAppIsNotExposed(t *testing.T) {
	in := testInput(t)
	in.App.Internal = true
	in.Domain = "" // the server settles this for an internal app
	objs, err := AppObjects(in)
	if err != nil {
		t.Fatal(err)
	}
	rendered, err := Render(objs)
	if err != nil {
		t.Fatal(err)
	}
	for _, forbidden := range []string{"kind: HTTPRoute", "kind: Certificate", "kind: SecurityPolicy", "client-secret"} {
		if strings.Contains(rendered, forbidden) {
			t.Errorf("an internal app must not render %q", forbidden)
		}
	}
	// …but it is still a running app its siblings can call by name.
	for _, want := range []string{"kind: Deployment", "kind: Service"} {
		if !strings.Contains(rendered, want) {
			t.Errorf("an internal app still needs %q", want)
		}
	}
}

// A pre-deploy step must see exactly what the app sees — its env, its addon
// bindings and its disk — or a migration cannot reach the database.
func TestPreDeployJobMatchesTheApp(t *testing.T) {
	in := testInput(t)
	// Each entry is a command *line*, exactly as it appears in donkey.yaml.
	in.App.PreDeploy = []string{"python manage.py migrate --noinput", "python manage.py collectstatic --noinput"}
	in.App.Storage = &config.StorageConfig{Size: "1Gi", Paths: []string{"/app/data"}}
	in.AddonSecretNames = []string{"shop-addon-db"}

	job := PreDeployJob(in)
	spec := job.Spec.Template.Spec
	c := spec.Containers[0]

	if c.Image != in.ImageRef {
		t.Errorf("job image %q, want the app's %q", c.Image, in.ImageRef)
	}
	// It has to reach a shell. Handed to Command directly, Kubernetes treats
	// the whole line as the name of an executable and the container dies in
	// exec() before it can print anything that would explain why.
	if len(c.Command) != 3 || c.Command[0] != "sh" || c.Command[1] != "-c" {
		t.Fatalf("the steps must run through a shell, got %q", c.Command)
	}
	want := "python manage.py migrate --noinput && python manage.py collectstatic --noinput"
	if c.Command[2] != want {
		t.Errorf("steps should run in order and stop at the first failure:\n got %q\nwant %q", c.Command[2], want)
	}
	if spec.RestartPolicy != corev1.RestartPolicyNever {
		t.Error("a half-applied migration must not be retried automatically")
	}
	var envFrom []string
	for _, f := range c.EnvFrom {
		envFrom = append(envFrom, f.SecretRef.Name)
	}
	for _, want := range []string{EnvSecretName(in.App.Name), "shop-addon-db"} {
		if !slices.Contains(envFrom, want) {
			t.Errorf("the job must receive %s", want)
		}
	}
	if len(c.VolumeMounts) == 0 {
		t.Error("the job must mount the app's disk — a migration often creates the database file")
	}
}

// Sharing an address is what lets a frontend and its API live at one
// hostname, and login attaches per route so the two can differ.
func TestPathRoutesShareAnAddress(t *testing.T) {
	in := testInput(t)
	in.RouteTargets = []RouteTarget{{App: "dashboard", Host: "dashboard.acme.apps.example.com", Path: "/api"}}
	objs, err := AppObjects(in)
	if err != nil {
		t.Fatal(err)
	}
	rendered, err := Render(objs)
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{
		"dashboard.acme.apps.example.com", // the other app's hostname
		"value: /api",                     // …under this prefix
		"name: shop",                      // …reaching this app's service
	} {
		if !strings.Contains(rendered, want) {
			t.Errorf("a shared-address route should contain %q", want)
		}
	}
	// A public app's shared route carries no login policy, so a machine
	// client can use it even when the app it shares with is private.
	in.App.Public = true
	open, err := Render(mustObjects(t, in))
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(open, "kind: SecurityPolicy") {
		t.Error("a public app must not get login protection on its shared route")
	}
}

func mustObjects(t *testing.T, in Input) []runtime.Object {
	t.Helper()
	objs, err := AppObjects(in)
	if err != nil {
		t.Fatal(err)
	}
	return objs
}

// The app.kubernetes.io/* labels exist so donkey's grouping is readable by
// tools that never heard of donkey. `part-of` is the one that matters: it is
// the standard spelling of the thing a space is.
func TestStandardLabels(t *testing.T) {
	l := Labels("demo", "backend", ComponentWeb)
	for k, want := range map[string]string{
		"app.kubernetes.io/name":       "backend",
		"app.kubernetes.io/part-of":    "demo",
		"app.kubernetes.io/component":  ComponentWeb,
		"app.kubernetes.io/managed-by": ManagedByValue,
		LabelApp:                       "backend",
		LabelSpace:                     "demo",
	} {
		if l[k] != want {
			t.Errorf("%s = %q, want %q", k, l[k], want)
		}
	}
	// A caller with no space must not claim the app is part of one.
	if _, ok := Labels("", "backend", ComponentWeb)["app.kubernetes.io/part-of"]; ok {
		t.Error("part-of must be absent when the space is unknown")
	}
}

// A selector cannot be changed after the Deployment exists, so the labels the
// pods are selected by must stay exactly what they always were — adding
// metadata labels must never reach them.
func TestDeploymentSelectorIsUnaffectedByLabels(t *testing.T) {
	in := testInput(t)
	dep, err := deployment(in)
	if err != nil {
		t.Fatal(err)
	}
	sel := dep.Spec.Selector.MatchLabels
	if len(sel) != 2 || sel[LabelApp] != in.App.Name || sel[LabelComponent] != ComponentWeb {
		t.Fatalf("the selector must stay donkey.dev/app + donkey.dev/component only, got %v", sel)
	}
	// The pod template still has to satisfy it.
	for k, v := range sel {
		if dep.Spec.Template.Labels[k] != v {
			t.Errorf("pod template is missing selector label %s=%s", k, v)
		}
	}
}

func TestVersionLabelFromImageTag(t *testing.T) {
	for image, want := range map[string]string{
		"registry.example.com/donkey/shop:4f2a9c-20260724143022": "4f2a9c-20260724143022",
		"registry.example.com:5000/shop:v1.2.3":                  "v1.2.3",
		// No tag, and a digest: neither is a version to publish.
		"registry.example.com/donkey/shop":        "",
		"registry.example.com/shop@sha256:abc123": "",
		// Rejected rather than risked: an invalid value would fail the apply.
		"registry.example.com/shop:-nope":                      "",
		"registry.example.com/shop:" + strings.Repeat("x", 64): "",
	} {
		got := withVersion(map[string]string{}, image)["app.kubernetes.io/version"]
		if got != want {
			t.Errorf("withVersion(%q) = %q, want %q", image, got, want)
		}
	}
}
