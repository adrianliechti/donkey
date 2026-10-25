package kubernetes

import (
	"context"
	"reflect"
	"testing"

	"github.com/adrianliechti/donkey/pkg/runtime/kubernetes/manifests"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes/fake"
)

func TestUpdateSecurityDoesNotRollPods(t *testing.T) {
	labels := manifests.Labels("team", "app", manifests.RoleWorkload)
	labels[manifests.LabelInstallation] = "donkey"
	dep := &appsv1.Deployment{ObjectMeta: metav1.ObjectMeta{Name: "app", Namespace: "team", Labels: labels, Annotations: map[string]string{manifests.AnnotationImage: "registry/app:1"}}, Spec: appsv1.DeploymentSpec{Template: corev1.PodTemplateSpec{Spec: corev1.PodSpec{Containers: []corev1.Container{{Name: "app", Image: "registry/app:1"}}}}}}
	cs := fake.NewClientset(dep)
	c := (&Client{Clientset: cs}).WithInstallation("donkey")
	ctx := context.Background()
	if err := c.UpdateSecurity(ctx, "team", "app", "team", "app", "registry/app:2", "wrong"); err == nil {
		t.Fatal("updated a different image")
	}
	if err := c.UpdateSecurity(ctx, "team", "app", "team", "app", "registry/app:1", "new"); err != nil {
		t.Fatal(err)
	}
	got, err := cs.AppsV1().Deployments("team").Get(ctx, "app", metav1.GetOptions{})
	if err != nil || got.Annotations[manifests.AnnotationSecurity] != "new" || !reflect.DeepEqual(got.Spec, dep.Spec) {
		t.Fatalf("scan changed deployment spec: %+v %v", got, err)
	}
}
