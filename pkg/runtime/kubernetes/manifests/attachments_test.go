package manifests

import (
	"strings"
	"testing"

	"k8s.io/apimachinery/pkg/api/meta"
	"k8s.io/apimachinery/pkg/runtime"
)

func kindsOf(objs []runtime.Object) map[string]int {
	out := map[string]int{}
	for _, obj := range objs {
		out[obj.GetObjectKind().GroupVersionKind().Kind]++
	}
	return out
}

// A plain installation names no cluster machinery, so it must render none —
// staying installable on a cluster with no CRDs at all is the default's job.
func TestNoAttachmentsByDefault(t *testing.T) {
	kinds := kindsOf(SystemObjects(systemInput()))
	if kinds["HTTPRoute"] != 0 || kinds["Certificate"] != 0 {
		t.Fatalf("a default installation must not touch Gateway API or cert-manager, got %v", kinds)
	}
}

// gateway.httpRoute sends the whole domain through the cluster's own gateway:
// one route, both hostname shapes, backend on the plain-http port (the
// cluster gateway has terminated TLS by the time a route matches).
func TestHTTPRouteAttachment(t *testing.T) {
	in := systemInput()
	in.HTTPRouteGateway = GatewayRef{Name: "shared-gateway", Namespace: "infra", SectionName: "https"}
	objs := SystemObjects(in)
	if kindsOf(objs)["HTTPRoute"] != 1 {
		t.Fatal("naming a gateway should render exactly one HTTPRoute")
	}
	rendered, err := Render(objs)
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{
		"kind: HTTPRoute",
		"gateway.networking.k8s.io/v1",
		"name: shared-gateway",
		"namespace: infra",
		"sectionName: https",
		"- apps.example.com",     // the apex — a wildcard never covers it
		`- '*.apps.example.com'`, // and everything under it, any depth
		"port: 80",
	} {
		if !strings.Contains(rendered, want) {
			t.Errorf("the HTTPRoute should carry %q", want)
		}
	}
	// The route lives with the installation, and the backend is the edge.
	for _, obj := range objs {
		if obj.GetObjectKind().GroupVersionKind().Kind != "HTTPRoute" {
			continue
		}
		m, _ := meta.Accessor(obj)
		if m.GetNamespace() != DefaultSystemNamespace {
			t.Errorf("the HTTPRoute belongs in the system namespace, got %q", m.GetNamespace())
		}
	}
}
