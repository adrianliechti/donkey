package manifests

import (
	"slices"
	"strings"
	"testing"

	rbacv1 "k8s.io/api/rbac/v1"
	"k8s.io/apimachinery/pkg/api/meta"
	"k8s.io/apimachinery/pkg/runtime"
)

// clusterScoped names the objects of an install that live outside any
// namespace — the ones two installations on a cluster could collide on.
func clusterScoped(objs []runtime.Object) []string {
	var out []string
	for _, obj := range objs {
		m, err := meta.Accessor(obj)
		if err != nil {
			continue
		}
		if m.GetNamespace() == "" {
			out = append(out, obj.GetObjectKind().GroupVersionKind().Kind+":"+m.GetName())
		}
	}
	return out
}

func has(list []string, want string) bool { return slices.Contains(list, want) }

// The whole point of giving donkey its namespaces: it then needs nothing
// above a namespace, which is both what a restricted cluster can grant and
// what makes two installations safe on one cluster.
func TestSharedNamespaceNeedsNothingClusterScoped(t *testing.T) {
	in := systemInput()
	in.WorkloadNamespace = "donkey-apps"
	in.SkipNamespace = true

	if left := clusterScoped(SystemObjects(in)); len(left) > 0 {
		t.Errorf("an installation confined to its namespaces must apply no cluster-scoped object, got %v", left)
	}
	rendered := componentNames(in)
	if strings.Contains(rendered, "cluster-admin") {
		t.Error("cluster-admin has no place in an installation confined to two namespaces")
	}
	// The Roles have to reach both namespaces: the control plane's, and the
	// one the apps live in.
	for _, ns := range []string{DefaultSystemNamespace, "donkey-apps"} {
		if !strings.Contains(rendered, "namespace: "+ns) {
			t.Errorf("the installation should grant itself access in %s", ns)
		}
	}
	if !strings.Contains(rendered, "pods/exec") {
		t.Error("the file browser needs pods/exec — leaving it out ships a Role that cannot do the job")
	}
}

func TestManagedTrafficPermissionsStayInSystemNamespace(t *testing.T) {
	in := systemInput()
	in.WorkloadNamespace = "donkey-apps"
	in.SkipNamespace = true

	foundSystemEdge := false
	for _, obj := range SystemObjects(in) {
		role, ok := obj.(*rbacv1.Role)
		if !ok || role.Name != ServerName {
			continue
		}
		hasEdge := false
		for _, rule := range role.Rules {
			if slices.Contains(rule.Resources, "certificates") || slices.Contains(rule.Resources, "dnsendpoints") {
				hasEdge = true
			}
		}
		switch role.Namespace {
		case in.Namespace:
			foundSystemEdge = hasEdge
		case in.WorkloadNamespace:
			if hasEdge {
				t.Error("the workload Role must not grant Certificate or DNSEndpoint writes")
			}
		}
	}
	if !foundSystemEdge {
		t.Error("the system Role must let donkey-server manage Certificates and DNSEndpoints")
	}
}

// A namespace per space means creating and deleting namespaces, which no Role
// can express — so that arrangement keeps its cluster binding, and says so.
func TestPerSpaceStillBindsClusterAdmin(t *testing.T) {
	in := systemInput()
	objs := SystemObjects(in)
	if !has(clusterScoped(objs), "ClusterRoleBinding:"+ServerName) {
		t.Error("a namespace-per-space installation still needs its ClusterRoleBinding")
	}
	if !strings.Contains(componentNames(in), "cluster-admin") {
		t.Error("creating namespaces is what cluster-admin is still there for")
	}
}

// Installing a second donkey must not disturb the first. Server-side apply
// forces the subject list, so a ClusterRoleBinding of the same name would
// take the first installation's ServiceAccount straight out of it.
func TestSecondInstallationDoesNotCollide(t *testing.T) {
	first := clusterScoped(SystemObjects(systemInput()))

	second := systemInput()
	second.Namespace = "bank-donkey-system"
	secondNames := clusterScoped(SystemObjects(second))

	if len(secondNames) == 0 {
		t.Fatal("expected the per-space arrangement to have cluster-scoped objects")
	}
	for _, name := range secondNames {
		if has(first, name) {
			t.Errorf("%s is applied by both installations — the second would take the first's permissions away", name)
		}
	}
	// And it must still bind its own ServiceAccount, in its own namespace.
	for _, obj := range SystemObjects(second) {
		crb, ok := obj.(*rbacv1.ClusterRoleBinding)
		if !ok {
			continue
		}
		for _, s := range crb.Subjects {
			if s.Namespace != second.Namespace {
				t.Errorf("%s binds a subject in %q, want %q", crb.Name, s.Namespace, second.Namespace)
			}
		}
	}
}

// The control plane's namespace is the installation's identity, and every
// component has to agree on where that is.
func TestSystemNamespaceReachesEveryComponent(t *testing.T) {
	in := systemInput()
	in.Namespace = "bank-donkey-system"
	rendered := componentNames(in)

	if strings.Contains(rendered, "namespace: "+DefaultSystemNamespace) {
		t.Error("nothing should still be pinned to the default namespace")
	}
	// Both pods are told where they are: the server to name its own objects,
	// the gateway to find its certificates.
	if n := strings.Count(rendered, "value: bank-donkey-system"); n < 2 {
		t.Errorf("both donkey-server and donkey-gateway need DONKEY_SYSTEM_NAMESPACE, found %d", n)
	}
	if !strings.Contains(rendered, "tcp://donkey-buildkitd.bank-donkey-system.svc:1234") {
		t.Error("the server must reach the build engine in its own namespace")
	}
}

// donkey never adopts a namespace somebody else made: applying it would take
// ownership of its labels and stamp the annotation that makes it deletable.
func TestSkipNamespaceLeavesItAlone(t *testing.T) {
	in := systemInput()
	in.SkipNamespace = true
	for _, obj := range SystemObjects(in) {
		if obj.GetObjectKind().GroupVersionKind().Kind == "Namespace" {
			t.Fatal("a namespace that already exists must not be applied")
		}
	}
	// Everything else still installs.
	if !strings.Contains(componentNames(in), "donkey-server") {
		t.Error("skipping the namespace must not skip the control plane")
	}
}

// sharedInput is the same app, rendered for an installation that shares one
// namespace it was given.
func sharedInput(t *testing.T) Input {
	t.Helper()
	in := testInput(t)
	p := Placement{SharedNamespace: "donkey-apps"}
	in.Namespace = p.Namespace(in.Space)
	in.SharedNamespace = true
	in.ObjectName = p.Name(in.Space, in.App.Name)
	return in
}

// Every object of the app carries its space, so a second space with an app of
// the same name lands beside it instead of on top of it.
func TestSharedNamespaceQualifiesEveryName(t *testing.T) {
	objs, err := AppObjects(sharedInput(t))
	if err != nil {
		t.Fatal(err)
	}
	want := map[string]string{
		"ConfigMap":             "acme-shop-route",
		"Secret":                "acme-shop-registry",
		"Service":               "acme-shop",
		"Deployment":            "acme-shop",
		"PersistentVolumeClaim": "acme-shop-data",
	}
	seen := map[string]bool{}
	for _, obj := range objs {
		kind := obj.GetObjectKind().GroupVersionKind().Kind
		m, err := meta.Accessor(obj)
		if err != nil {
			t.Fatal(err)
		}
		if kind == "Namespace" {
			t.Fatal("a namespace donkey was given must never be applied by a deploy")
		}
		if m.GetNamespace() != "donkey-apps" {
			t.Errorf("%s %s is in %q, want donkey-apps", kind, m.GetName(), m.GetNamespace())
		}
		// The space marker is the one object named after the space itself.
		if m.GetLabels()[LabelRole] == RoleSpace {
			seen["marker"] = true
			continue
		}
		if expect, ok := want[kind]; ok && m.GetName() == expect {
			seen[kind] = true
		}
		if !strings.HasPrefix(m.GetName(), "acme--") {
			t.Errorf("%s %s does not carry its space — a second space's app of the same name would overwrite it", kind, m.GetName())
		}
	}
	if !seen["marker"] {
		t.Error("the deploy should record the space, or a space with no namespace has nothing to be found by")
	}
	for kind, name := range want {
		if !seen[kind] && kind != "PersistentVolumeClaim" {
			t.Errorf("expected a %s called %s", kind, name)
		}
	}
}

// Object names are qualified; labels remain logical and selectors include
// the space explicitly.
func TestSharedNamespaceUsesLogicalSelectors(t *testing.T) {
	in := sharedInput(t)
	dep, err := deployment(in)
	if err != nil {
		t.Fatal(err)
	}
	if got := dep.Spec.Selector.MatchLabels[LabelApp]; got != "shop" {
		t.Errorf("selector %s = %q, want logical app shop", LabelApp, got)
	}
	if got := dep.Spec.Selector.MatchLabels[LabelSpace]; got != "acme" {
		t.Errorf("selector %s = %q, want acme", LabelSpace, got)
	}
	if got := dep.Labels[LabelSpace]; got != "acme" {
		t.Errorf("the space label stays the plain space, got %q", got)
	}
	if got := service(in).Spec.Selector[LabelApp]; got != "shop" || service(in).Spec.Selector[LabelSpace] != "acme" {
		t.Errorf("the Service selector must use logical app + space: %v", service(in).Spec.Selector)
	}
}

// The gateway forwards to the Service by name, so the backend has to follow
// the qualification too — otherwise it points at a Service that isn't there.
func TestSharedNamespaceBackendFollows(t *testing.T) {
	in := sharedInput(t)
	cm := appRoute(in)
	route, err := ReadRoute(cm)
	if err != nil {
		t.Fatal(err)
	}
	want := "http://acme-shop.donkey-apps.svc:80"
	if got := route.Backend; got != want {
		t.Errorf("backend = %q, want %q", got, want)
	}
	if cm.Name != "acme-shop-route" {
		t.Errorf("the route object is named %q, want acme-shop-route", cm.Name)
	}
	// The hostname a visitor types is unchanged: it is built from the plain
	// app and space names, and nothing about placement should touch it.
	if got := route.Host; got != "shop.acme.apps.example.com" {
		t.Errorf("host = %q — placement must not change the address people use", got)
	}
}
