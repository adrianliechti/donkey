package kubernetes

import (
	"context"
	"fmt"
	"testing"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes/fake"

	"github.com/adrianliechti/donkey/pkg/runtime/kubernetes/manifests"
)

func TestDetachServiceRemovesAliasesBeforeDeletingSecret(t *testing.T) {
	for _, withEnvFrom := range []bool{true, false} {
		t.Run(fmt.Sprint(withEnvFrom), func(t *testing.T) {
			ctx := context.Background()
			labels := manifests.Labels("team", "api", manifests.RoleWorkload)
			labels[manifests.LabelInstallation] = "donkey-system"
			ref := func(env, secret string) corev1.EnvVar {
				return corev1.EnvVar{Name: env, ValueFrom: &corev1.EnvVarSource{SecretKeyRef: &corev1.SecretKeySelector{
					LocalObjectReference: corev1.LocalObjectReference{Name: secret}, Key: "URL",
				}}}
			}
			container := corev1.Container{Name: "app", Env: []corev1.EnvVar{
				ref("DB_URL", "db-binding"), ref("DATABASE_URL", "db-binding"), ref("AI_URL", "ai-binding"), {Name: "MODE", Value: "production"},
			}}
			if withEnvFrom {
				container.EnvFrom = []corev1.EnvFromSource{{SecretRef: &corev1.SecretEnvSource{LocalObjectReference: corev1.LocalObjectReference{Name: "db-binding"}}}}
			}
			dep := &appsv1.Deployment{ObjectMeta: metav1.ObjectMeta{Name: "api", Namespace: "team", Labels: labels}, Spec: appsv1.DeploymentSpec{
				Template: corev1.PodTemplateSpec{Spec: corev1.PodSpec{Containers: []corev1.Container{container}}},
			}}
			cs := fake.NewClientset(dep)
			client := (&Client{Clientset: cs}).WithInstallation("donkey-system")
			for range 2 {
				if found, err := client.SetAddonEnvFrom(ctx, "team", "api", "team", "api", "db-binding", false); err != nil || !found {
					t.Fatalf("detach: %v %v", found, err)
				}
			}
			got, err := cs.AppsV1().Deployments("team").Get(ctx, "api", metav1.GetOptions{})
			if err != nil {
				t.Fatal(err)
			}
			app := got.Spec.Template.Spec.Containers[0]
			if len(app.EnvFrom) != 0 || len(app.Env) != 2 || app.Env[0].Name != "AI_URL" || app.Env[1].Name != "MODE" {
				t.Fatalf("detach left stale aliases or removed unrelated env: %+v", app)
			}
			updates := 0
			for _, action := range cs.Actions() {
				if action.GetVerb() == "update" {
					updates++
				}
			}
			if updates != 1 {
				t.Fatalf("repeating the detach caused another rollout: %d updates", updates)
			}
		})
	}
}
