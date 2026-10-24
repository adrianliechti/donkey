package kubernetes

import (
	"context"
	"maps"
	"reflect"
	"strings"
	"testing"

	rt "github.com/adrianliechti/donkey/pkg/runtime"
	"github.com/adrianliechti/donkey/pkg/runtime/kubernetes/manifests"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	k8sruntime "k8s.io/apimachinery/pkg/runtime"
	"k8s.io/client-go/kubernetes/fake"
)

func TestConfigMigrationPreservesSettingsAndCredentials(t *testing.T) {
	for _, previous := range []struct {
		shared    bool
		separator string
	}{{false, "-addon-"}, {true, "-addon-"}, {false, "-service-"}, {true, "-service-"}, {false, "-binding-"}, {true, "-binding-"}} {
		shared := previous.shared
		t.Run(map[bool]string{false: "namespace per space", true: "shared namespace"}[shared]+previous.separator, func(t *testing.T) {
			ctx := context.Background()
			client := &Client{Clientset: fake.NewClientset(), Installation: "main"}
			placement := manifests.Placement{NamespacePrefix: "donkey-"}
			oldApp, oldOwner := "api", "shared"
			if shared {
				placement.SharedNamespace = "apps"
				oldApp, oldOwner = "team--api", "team--shared"
			}
			r := NewRuntime(client, Options{SystemNamespace: "main", Placement: placement})
			ns := placement.Namespace("team")
			space := manifests.SpaceMarker("team", ns)
			space.Name = "donkey.space.team"
			space.Labels = client.expectedObjectIdentity(space.Labels)
			space.Data = map[string]string{"config.json": `{"admins":{"users":["owner@example.com"]},"developers":{"groups":["dev"]},"domains":["team.example.com"]}`, "extra": "keep"}
			_, _ = client.Clientset.CoreV1().ConfigMaps(ns).Create(ctx, space, metav1.CreateOptions{})
			if !shared {
				home := manifests.Namespace("team", ns)
				home.Labels = client.expectedObjectIdentity(home.Labels)
				_, _ = client.Clientset.CoreV1().Namespaces().Create(ctx, home, metav1.CreateOptions{})
			}
			route := &corev1.ConfigMap{ObjectMeta: metav1.ObjectMeta{Name: oldApp, Namespace: ns,
				Labels:      client.expectedObjectIdentity(manifests.Labels("team", "api", manifests.RoleRoute)),
				Annotations: map[string]string{manifests.AnnotationHost: "api.team.example.com", manifests.AnnotationBackend: "http://" + oldApp + "." + ns + ".svc:80", manifests.AnnotationLogin: manifests.LoginRequired, manifests.AnnotationAccess: `{"users":["visitor@example.com"]}`}},
			}
			_, _ = client.Clientset.CoreV1().ConfigMaps(ns).Create(ctx, route, metav1.CreateOptions{})
			oldSecret := &corev1.Secret{ObjectMeta: metav1.ObjectMeta{Name: oldOwner + previous.separator + "db", Namespace: ns,
				Labels: client.expectedObjectIdentity(manifests.AddonLabels("team", "db", manifests.RoleBinding))},
				Data: map[string][]byte{"PASSWORD": []byte("existing-password"), "DATABASE_URL": []byte("postgres://user:existing-password@db:5432/db")},
			}
			_, _ = client.Clientset.CoreV1().Secrets(ns).Create(ctx, oldSecret, metav1.CreateOptions{})
			dep := &appsv1.Deployment{ObjectMeta: metav1.ObjectMeta{Name: oldApp, Namespace: ns, Labels: client.expectedObjectIdentity(manifests.Labels("team", "api", manifests.RoleWorkload))},
				Spec: appsv1.DeploymentSpec{Replicas: new(int32(1)), Selector: &metav1.LabelSelector{MatchLabels: manifests.AppSelector("team", "api", manifests.RoleWorkload)},
					Template: corev1.PodTemplateSpec{Spec: corev1.PodSpec{Containers: []corev1.Container{{Name: "app", Image: "test", EnvFrom: []corev1.EnvFromSource{{SecretRef: &corev1.SecretEnvSource{LocalObjectReference: corev1.LocalObjectReference{Name: oldSecret.Name}}}}}}}}},
			}
			_, _ = client.Clientset.AppsV1().Deployments(ns).Create(ctx, dep, metav1.CreateOptions{})
			if err := r.EnsureSpaceConfigs(ctx); err != nil {
				t.Fatal(err)
			}
			settings, err := r.SpaceConfig(ctx, "team")
			if err != nil || !reflect.DeepEqual(settings.Admins.Users, []string{"owner@example.com"}) || !reflect.DeepEqual(settings.Domains, []string{"team.example.com"}) {
				t.Fatalf("space settings changed: %+v, %v", settings, err)
			}
			cm, _ := client.Clientset.CoreV1().ConfigMaps(ns).Get(ctx, "team-space", metav1.GetOptions{})
			if cm.Data["config.json"] != "" || cm.Data["config.yaml"] == "" || cm.Data["extra"] != "keep" {
				t.Fatalf("unexpected migrated config keys")
			}
			cm, err = client.Clientset.CoreV1().ConfigMaps(ns).Get(ctx, placement.Name("team", "api")+"-route", metav1.GetOptions{})
			if err != nil {
				t.Fatal(err)
			}
			gotRoute, err := manifests.ReadRoute(cm)
			if err != nil || !gotRoute.Login || gotRoute.Access == nil || !reflect.DeepEqual(gotRoute.Access.Users, []string{"visitor@example.com"}) || strings.Contains(gotRoute.Backend, "--") {
				t.Fatalf("route/access changed: %+v, %v", gotRoute, err)
			}
			newSecret := manifests.SpaceBindingSecretName(placement.Name("team", "db"))
			secret, err := client.Clientset.CoreV1().Secrets(ns).Get(ctx, newSecret, metav1.GetOptions{})
			if err != nil || !reflect.DeepEqual(secret.Data, oldSecret.Data) {
				t.Fatal("service credentials changed")
			}
			current, err := client.Clientset.AppsV1().Deployments(ns).Get(ctx, placement.Name("team", "api"), metav1.GetOptions{})
			if err != nil || current.Spec.Template.Spec.Containers[0].EnvFrom[0].SecretRef.Name != newSecret || *current.Spec.Replicas != 1 {
				t.Fatalf("workload migration: %v", err)
			}
			if err := r.EnsureSpaceConfigs(ctx); err != nil {
				t.Fatal(err)
			}
			for range 2 {
				if err := rt.ReconcileAccess(ctx, r, ""); err != nil {
					t.Fatal(err)
				}
			}
			settings.Viewers.Users = []string{"visitor@example.com"}
			migrated, err := r.SpaceConfig(ctx, "team")
			if err != nil || !reflect.DeepEqual(migrated, settings) {
				t.Fatalf("access migration lost settings or duplicated users: %+v, %v", migrated, err)
			}
			network, err := r.Network(ctx, "team", "api")
			if err != nil || network.Access != nil || !network.Protected {
				t.Fatalf("legacy route access was not migrated: %+v, %v", network, err)
			}
			// A live consumer keeps the old Secret until it drains.
			pod := &corev1.Pod{ObjectMeta: metav1.ObjectMeta{Name: "old-consumer", Namespace: ns}, Spec: *dep.Spec.Template.Spec.DeepCopy()}
			_, _ = client.Clientset.CoreV1().Pods(ns).Create(ctx, pod, metav1.CreateOptions{})
			if err := r.FinishConfigMigration(ctx); err != nil {
				t.Fatal(err)
			}
			if _, err := client.Clientset.CoreV1().Secrets(ns).Get(ctx, oldSecret.Name, metav1.GetOptions{}); err != nil {
				t.Fatal("deleted Secret while a Pod still used it")
			}
			_ = client.Clientset.CoreV1().Pods(ns).Delete(ctx, pod.Name, metav1.DeleteOptions{})
			if err := r.FinishConfigMigration(ctx); err != nil {
				t.Fatal(err)
			}
			if _, err := client.Clientset.CoreV1().Secrets(ns).Get(ctx, oldSecret.Name, metav1.GetOptions{}); !apierrors.IsNotFound(err) {
				t.Fatal("old Secret remains after consumers drained")
			}
		})
	}
}

func TestSecretMigrationRefusesForeignDestination(t *testing.T) {
	client := &Client{Clientset: fake.NewClientset(), Installation: "main"}
	old := &corev1.Secret{ObjectMeta: metav1.ObjectMeta{Name: "old", Namespace: "main", Labels: client.expectedObjectIdentity(manifests.SystemLabels(manifests.GatewayAppName))}, Data: map[string][]byte{"account": []byte("keep")}}
	foreign := old.DeepCopy()
	foreign.Name = "new"
	foreign.Labels = maps.Clone(old.Labels)
	foreign.Labels[manifests.LabelInstallation] = "another"
	_, _ = client.Clientset.CoreV1().Secrets("main").Create(context.Background(), foreign, metav1.CreateOptions{})
	if err := client.copySecret(context.Background(), old, "new"); err == nil {
		t.Fatal("foreign Secret was adopted")
	}
}

func TestPreserveSharedVolumeAfterNamingMigration(t *testing.T) {
	client := &Client{Clientset: fake.NewClientset(), Installation: "main"}
	labels := client.expectedObjectIdentity(manifests.Labels("team", "my-api", manifests.RoleStorage))
	old := &corev1.PersistentVolumeClaim{ObjectMeta: metav1.ObjectMeta{Name: "team--my-api-data", Namespace: "apps", Labels: labels}, Spec: corev1.PersistentVolumeClaimSpec{VolumeName: "existing-disk"}}
	_, _ = client.Clientset.CoreV1().PersistentVolumeClaims("apps").Create(context.Background(), old, metav1.CreateOptions{})
	pvc := old.DeepCopy()
	pvc.Name = "team-my-api-data"
	dep := &appsv1.Deployment{ObjectMeta: metav1.ObjectMeta{Namespace: "apps"}, Spec: appsv1.DeploymentSpec{Template: corev1.PodTemplateSpec{Spec: corev1.PodSpec{Volumes: []corev1.Volume{{VolumeSource: corev1.VolumeSource{PersistentVolumeClaim: &corev1.PersistentVolumeClaimVolumeSource{ClaimName: pvc.Name}}}}}}}}
	if err := client.preserveDataClaims(context.Background(), []k8sruntime.Object{pvc, dep}); err != nil {
		t.Fatal(err)
	}
	if pvc.Name != old.Name || dep.Spec.Template.Spec.Volumes[0].PersistentVolumeClaim.ClaimName != old.Name {
		t.Fatal("rename would allocate an empty replacement disk")
	}
	if !client.HasDataClaim(context.Background(), "apps", "team-my-api", "team", "my-api") {
		t.Fatal("kept data disappeared from the app")
	}
}
