package manifests

import (
	"strings"
	"testing"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
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
