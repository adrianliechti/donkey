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
	"k8s.io/apimachinery/pkg/api/meta"
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
	}
}

func TestProtectionRendering(t *testing.T) {
	// Every app is an entry in the donkey gateway's routing table; a
	// protected one is marked for login.
	objs, err := AppObjects(testInput(t))
	if err != nil {
		t.Fatal(err)
	}
	out, err := Render(objs)
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{
		AnnotationHost + ": shop.acme.apps.example.com",
		AnnotationLogin + ": " + LoginRequired,
		AnnotationBackend + ": http://shop.donkey-acme.svc:80",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("protected app should render %s", want)
		}
	}
	// Routing, login and TLS live in donkey-gateway, not in ingress-vendor
	// CRDs.
	for _, forbidden := range []string{"kind: Ingress", "kind: SecurityPolicy", "kind: HTTPRoute", "kind: ReferenceGrant", "kind: Certificate", "shop-oidc"} {
		if strings.Contains(out, forbidden) {
			t.Errorf("%s must not be rendered anymore", forbidden)
		}
	}

	// public: true drops the login mark but keeps riding the gateway.
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
	if strings.Contains(out, AnnotationLogin+": "+LoginRequired) {
		t.Error("public app must not demand a login")
	}
	for _, want := range []string{AnnotationHost + ":", AnnotationBackend + ":"} {
		if !strings.Contains(out, want) {
			t.Errorf("public app still needs %s", want)
		}
	}
}

// Access rules ride on the route for the gateway to enforce — and only on
// routes that demand a login.
func TestAccessAnnotation(t *testing.T) {
	in := testInput(t)
	in.App.Access = &config.AccessConfig{Users: []string{"ada@example.com"}, Groups: []string{"staff"}}
	out, err := Render(mustObjects(t, in))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out, `donkey.local/access: '{"users":["ada@example.com"],"groups":["staff"]}'`) {
		t.Errorf("access rules should ride on the route, got:\n%s", out)
	}

	in.App.Access = nil
	out, err = Render(mustObjects(t, in))
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(out, AnnotationAccess) {
		t.Error("no rules, no annotation")
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
	goldenPath := filepath.Join("..", "..", "..", "..", "testdata", "golden", "app-objects.yaml")
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
		t.Fatalf("golden file missing — run: go test ./pkg/runtime/kubernetes/manifests -update (%v)", err)
	}
	if got != string(want) {
		t.Errorf("rendered manifests differ from golden file.\nRun `go test ./pkg/runtime/kubernetes/manifests -update` if the change is intended.\n--- got ---\n%s", got)
	}
}

func TestAppTelemetryDefaultsAreScopedAndOverridable(t *testing.T) {
	in := testInput(t)
	in.TelemetryEndpoint = "http://donkey-telemetry.donkey-system.svc:4318"
	in.TelemetryToken = "scoped-token"
	objects, err := AppObjects(in)
	if err != nil {
		t.Fatal(err)
	}
	rendered, err := Render(objects)
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{
		"name: shop-telemetry",
		"OTEL_SERVICE_NAME: shop",
		"OTEL_RESOURCE_ATTRIBUTES: service.namespace=acme",
		"OTEL_EXPORTER_OTLP_PROTOCOL: http/protobuf",
		"OTEL_METRICS_EXPORTER: otlp",
	} {
		if !strings.Contains(rendered, want) {
			t.Errorf("telemetry defaults should contain %q", want)
		}
	}
	_, envFrom := appEnvironment(in)
	if len(envFrom) < 2 || envFrom[0].SecretRef == nil || envFrom[0].SecretRef.Name != TelemetrySecretName(in.Name()) ||
		envFrom[1].SecretRef == nil || envFrom[1].SecretRef.Name != EnvSecretName(in.Name()) {
		t.Fatalf("telemetry must load before the user env Secret so user values win: %+v", envFrom)
	}
}

func TestAppSecurityContextBalancesCompatibilityAndContainment(t *testing.T) {
	dep, err := deployment(testInput(t))
	if err != nil {
		t.Fatal(err)
	}
	spec := dep.Spec.Template.Spec
	if spec.AutomountServiceAccountToken == nil || *spec.AutomountServiceAccountToken {
		t.Fatal("app pods must not receive a Kubernetes service-account token")
	}
	security := spec.Containers[0].SecurityContext
	if security == nil || security.AllowPrivilegeEscalation == nil || *security.AllowPrivilegeEscalation {
		t.Fatal("app containers must not gain privileges")
	}
	if security.SeccompProfile == nil || security.SeccompProfile.Type != corev1.SeccompProfileTypeRuntimeDefault {
		t.Fatal("app containers must use the runtime-default seccomp profile")
	}
	wantDrop := []corev1.Capability{"NET_RAW"}
	if !slices.Equal(security.Capabilities.Drop, wantDrop) || len(security.Capabilities.Add) != 0 {
		t.Fatalf("app capabilities = %+v, want only NET_RAW dropped", security.Capabilities)
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
	if !strings.Contains(out, "donkey.local/space: acme") {
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

// By default certificates need no objects at all: donkey's own gateway issues
// one per hostname itself the moment the route appears.
func TestNoCertificateObjects(t *testing.T) {
	objs, err := AppObjects(testInput(t))
	if err != nil {
		t.Fatal(err)
	}
	out, err := Render(objs)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(out, "kind: Certificate") {
		t.Error("apps must not render cert-manager Certificates anymore")
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
	for _, forbidden := range []string{AnnotationHost + ":", AnnotationBackend + ":", "kind: Certificate", "kind: SecurityPolicy", "client-secret"} {
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
		AnnotationHost + ": dashboard.acme.apps.example.com",   // the other app's hostname
		AnnotationPath + ": /api",                              // …under this prefix
		AnnotationBackend + ": http://shop.donkey-acme.svc:80", // …reaching this app
	} {
		if !strings.Contains(rendered, want) {
			t.Errorf("a shared-address route should contain %q", want)
		}
	}
	// A public app's shared route demands no login, so a machine client can
	// use it even when the app it shares with is private.
	in.App.Public = true
	open, err := Render(mustObjects(t, in))
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(open, AnnotationLogin+": "+LoginRequired) {
		t.Error("a public app must not demand a login on its shared route")
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
// tools that never heard of donkey: the full recommended set — name,
// instance, component, part-of, managed-by (and version where an image tag
// supplies one) — on apps and platform alike.
func TestStandardLabels(t *testing.T) {
	l := Labels("demo", "backend", ComponentWeb)
	for k, want := range map[string]string{
		LabelName:      "backend",
		LabelInstance:  "demo" + NameSeparator + "backend",
		LabelPartOf:    "demo",
		LabelComponent: ComponentWeb,
		LabelManagedBy: ManagedByValue,
		LabelApp:       "backend",
		LabelSpace:     "demo",
	} {
		if l[k] != want {
			t.Errorf("%s = %q, want %q", k, l[k], want)
		}
	}
	// A caller with no space must not claim the app is part of one, and has
	// no instance to name.
	noSpace := Labels("", "backend", ComponentWeb)
	if _, ok := noSpace[LabelPartOf]; ok {
		t.Error("part-of must be absent when the space is unknown")
	}
	if _, ok := noSpace[LabelInstance]; ok {
		t.Error("instance must be absent when the space is unknown")
	}
}

// Platform workloads are part of the application called donkey; their
// instance is the installation, stamped at the apply/render boundary.
func TestSystemStandardLabels(t *testing.T) {
	l := SystemLabels(ServerName)
	for k, want := range map[string]string{
		LabelName:      ServerName,
		LabelPartOf:    ManagedByValue,
		LabelComponent: ServerName,
		LabelManagedBy: ManagedByValue,
	} {
		if l[k] != want {
			t.Errorf("%s = %q, want %q", k, l[k], want)
		}
	}
	if _, ok := l[LabelInstance]; ok {
		t.Error("instance is the installation's to stamp, not SystemLabels'")
	}
	// The space itself is the unit, not a qualified copy of one.
	if got := SpaceLabels("demo")[LabelInstance]; got != "demo" {
		t.Errorf("space instance = %q, want the space itself", got)
	}
}

func TestEveryAppAndSystemObjectUsesCanonicalMetadata(t *testing.T) {
	appObjects, err := AppObjects(testInput(t))
	if err != nil {
		t.Fatal(err)
	}
	for group, objects := range map[string][]runtime.Object{
		"app":    appObjects,
		"system": SystemObjects(systemInput()),
	} {
		for _, object := range objects {
			accessor, err := meta.Accessor(object)
			if err != nil {
				t.Fatalf("%s object metadata: %v", group, err)
			}
			labels := accessor.GetLabels()
			kind := object.GetObjectKind().GroupVersionKind().Kind
			if labels[LabelManagedBy] != ManagedByValue || labels[LabelComponent] == "" ||
				labels["app.kubernetes.io/name"] == "" {
				t.Errorf("%s %s/%s has incomplete canonical labels: %v", group, kind, accessor.GetName(), labels)
			}
			if _, duplicate := labels["donkey.local/component"]; duplicate {
				t.Errorf("%s %s/%s carries the removed duplicate component label", group, kind, accessor.GetName())
			}
			if group == "app" && labels[LabelSpace] != "acme" {
				t.Errorf("app %s/%s has space label %q", kind, accessor.GetName(), labels[LabelSpace])
			}
		}
	}
}

// A selector cannot be changed after the Deployment exists, so it stays at
// the minimum immutable identity. Descriptive name, part-of and version
// labels must never leak into it.
func TestDeploymentSelectorIsUnaffectedByLabels(t *testing.T) {
	in := testInput(t)
	dep, err := deployment(in)
	if err != nil {
		t.Fatal(err)
	}
	sel := dep.Spec.Selector.MatchLabels
	if len(sel) != 3 || sel[LabelSpace] != in.Space || sel[LabelApp] != in.App.Name || sel[LabelComponent] != ComponentWeb {
		t.Fatalf("the selector must be logical space + app + component, got %v", sel)
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
