package manifests

import (
	"strings"
	"testing"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
)

func TestRenderForInstallationStampsOfflineObjects(t *testing.T) {
	object := &corev1.ConfigMap{
		TypeMeta:   metav1.TypeMeta{APIVersion: "v1", Kind: "ConfigMap"},
		ObjectMeta: metav1.ObjectMeta{Name: "example", Namespace: "donkey-system"},
	}
	out, err := RenderForInstallation([]runtime.Object{object}, "bank-donkey-system")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out, LabelInstallation+": bank-donkey-system") {
		t.Fatalf("rendered object has no installation label:\n%s", out)
	}
	if !strings.Contains(out, LabelManagedBy+": "+ManagedByValue) {
		t.Fatalf("rendered object has no managed-by label:\n%s", out)
	}
	if object.Labels != nil {
		t.Fatal("rendering should not mutate the source object")
	}
	plain, err := Render([]runtime.Object{object})
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(plain, LabelInstallation) {
		t.Fatal("ordinary dry-run render unexpectedly stamped an installation")
	}
}

// The stamp fills the standard instance label where the labels name none —
// control-plane objects belong to the installation — but never overrides an
// app's own instance and never lets the descriptive label into a selector.
func TestStampInstallationFillsInstance(t *testing.T) {
	system := &appsv1.Deployment{
		TypeMeta:   metav1.TypeMeta{APIVersion: "apps/v1", Kind: "Deployment"},
		ObjectMeta: metav1.ObjectMeta{Name: ServerName, Namespace: "donkey-system", Labels: SystemLabels(ServerName)},
		Spec: appsv1.DeploymentSpec{
			Selector: &metav1.LabelSelector{MatchLabels: SystemSelector(ServerName)},
			Template: corev1.PodTemplateSpec{
				ObjectMeta: metav1.ObjectMeta{Labels: SystemLabels(ServerName)},
			},
		},
	}
	m, err := runtime.DefaultUnstructuredConverter.ToUnstructured(system)
	if err != nil {
		t.Fatal(err)
	}
	u := &unstructured.Unstructured{Object: m}
	if err := StampInstallation(u, "bank-donkey-system"); err != nil {
		t.Fatal(err)
	}
	if got := u.GetLabels()[LabelInstance]; got != "bank-donkey-system" {
		t.Errorf("system object instance = %q, want the installation", got)
	}
	template, _, err := unstructured.NestedStringMap(u.Object, "spec", "template", "metadata", "labels")
	if err != nil {
		t.Fatal(err)
	}
	if got := template[LabelInstance]; got != "bank-donkey-system" {
		t.Errorf("system pod instance = %q, want the installation", got)
	}
	selector, _, err := unstructured.NestedStringMap(u.Object, "spec", "selector", "matchLabels")
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := selector[LabelInstance]; ok {
		t.Error("the descriptive instance label must stay out of selectors")
	}

	app := &corev1.ConfigMap{
		TypeMeta:   metav1.TypeMeta{APIVersion: "v1", Kind: "ConfigMap"},
		ObjectMeta: metav1.ObjectMeta{Name: "shop", Namespace: "donkey-acme", Labels: Labels("acme", "shop", RoleRoute)},
	}
	m, err = runtime.DefaultUnstructuredConverter.ToUnstructured(app)
	if err != nil {
		t.Fatal(err)
	}
	u = &unstructured.Unstructured{Object: m}
	if err := StampInstallation(u, "bank-donkey-system"); err != nil {
		t.Fatal(err)
	}
	if got := u.GetLabels()[LabelInstance]; got != "acme" {
		t.Errorf("app instance = %q, the stamp must not override it", got)
	}
}
