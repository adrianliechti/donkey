package kubernetes

import (
	"context"
	"errors"
	"fmt"
	"reflect"
	"sync"
	"testing"

	"github.com/adrianliechti/donkey/internal/config"
	rt "github.com/adrianliechti/donkey/pkg/runtime"
	"github.com/adrianliechti/donkey/pkg/runtime/kubernetes/manifests"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/client-go/kubernetes/fake"
	k8stesting "k8s.io/client-go/testing"
)

func TestPlatformConfigInitializationAndPersistence(t *testing.T) {
	ctx := context.Background()
	client := &Client{Clientset: fake.NewClientset()}
	opts := Options{SystemNamespace: "main"}
	r := NewRuntime(client, opts)
	if _, err := r.PlatformConfig(ctx); !errors.Is(err, rt.ErrPlatformNotFound) {
		t.Fatalf("missing settings: %v", err)
	}
	var wg sync.WaitGroup
	observed := make([]config.PlatformConfig, 16)
	for i := range 16 {
		wg.Go(func() {
			seed := config.PlatformConfig{Admins: config.AccessConfig{Users: []string{fmt.Sprintf("user%d@example.com", i)}}}
			settings, err := r.InitPlatformConfig(ctx, seed)
			if err != nil {
				t.Error(err)
			}
			observed[i] = settings
		})
	}
	wg.Wait()
	first, err := r.PlatformConfig(ctx)
	for _, settings := range observed {
		if !reflect.DeepEqual(settings, first) {
			t.Fatalf("concurrent sign-ins received different admins: %+v and %+v", settings, first)
		}
	}
	if err != nil || len(first.Admins.Users) != 1 {
		t.Fatalf("first admin: %+v, %v", first, err)
	}
	cm, err := client.Clientset.CoreV1().ConfigMaps("main").Get(ctx, "donkey", metav1.GetOptions{})
	if err != nil || cm.Data[platformConfigKey] == "" {
		t.Fatalf("missing ConfigMap in main: %v", err)
	}
	cm.Data["unrelated"] = "keep"
	if _, err := client.Clientset.CoreV1().ConfigMaps("main").Update(ctx, cm, metav1.UpdateOptions{}); err != nil {
		t.Fatal(err)
	}
	updated := config.PlatformConfig{Admins: config.AccessConfig{Users: append(first.Admins.Users, "added@example.com")}}
	if err := r.SetPlatformConfig(ctx, updated); err != nil {
		t.Fatal(err)
	}
	restarted := NewRuntime(client, opts)
	got, err := restarted.InitPlatformConfig(ctx, first)
	if err != nil || !reflect.DeepEqual(got, updated) {
		t.Fatalf("restart reset admins: %+v, %v", got, err)
	}
	cm, _ = client.Clientset.CoreV1().ConfigMaps("main").Get(ctx, cm.Name, metav1.GetOptions{})
	if cm.Data["unrelated"] != "keep" {
		t.Fatal("overwrote another config key")
	}
	cm.Data[platformConfigKey] = "{}"
	_, _ = client.Clientset.CoreV1().ConfigMaps("main").Update(ctx, cm, metav1.UpdateOptions{})
	if _, err := restarted.InitPlatformConfig(ctx, first); err == nil {
		t.Fatal("empty persisted settings triggered a new bootstrap")
	}
}

func TestPlatformConfigInitializesExistingConsoleRoute(t *testing.T) {
	for _, conflict := range []bool{false, true} {
		t.Run(fmt.Sprintf("concurrent sign-in=%t", conflict), func(t *testing.T) {
			ctx := context.Background()
			clientset := fake.NewClientset()
			client := &Client{Clientset: clientset, Installation: "main"}
			cm := &corev1.ConfigMap{
				ObjectMeta: metav1.ObjectMeta{Name: manifests.SystemConfigMapName, Namespace: "main",
					Labels:      client.expectedObjectIdentity(manifests.SystemLabels(manifests.ServerName)),
					Annotations: map[string]string{manifests.AnnotationBackend: "http://donkey-server.main.svc:80"}},
			}
			if _, err := clientset.CoreV1().ConfigMaps("main").Create(ctx, cm, metav1.CreateOptions{}); err != nil {
				t.Fatal(err)
			}
			want := "first@example.com"
			if conflict {
				want = "other@example.com"
				clientset.PrependReactor("update", "configmaps", func(k8stesting.Action) (bool, runtime.Object, error) {
					concurrent := cm.DeepCopy()
					concurrent.Data = map[string]string{platformConfigKey: `{"admins":{"users":["other@example.com"]}}`}
					if err := clientset.Tracker().Update(corev1.SchemeGroupVersion.WithResource("configmaps"), concurrent, "main"); err != nil {
						t.Fatal(err)
					}
					return true, nil, apierrors.NewConflict(schema.GroupResource{Resource: "configmaps"}, cm.Name, errors.New("concurrent first sign-in"))
				})
			}
			r := NewRuntime(client, Options{SystemNamespace: "main"})
			settings, err := r.InitPlatformConfig(ctx, config.PlatformConfig{Admins: config.AccessConfig{Users: []string{"first@example.com"}}})
			if err != nil || !reflect.DeepEqual(settings.Admins.Users, []string{want}) {
				t.Fatalf("first sign-in: %+v, %v", settings, err)
			}
			settings.Admins.Users = append(settings.Admins.Users, "added@example.com")
			if err := r.SetPlatformConfig(ctx, settings); err != nil {
				t.Fatal(err)
			}
			got, err := clientset.CoreV1().ConfigMaps("main").Get(ctx, cm.Name, metav1.GetOptions{})
			if err != nil || !reflect.DeepEqual(got.Annotations, cm.Annotations) {
				t.Fatalf("platform settings overwrote the console route: %v", err)
			}
		})
	}
}

func TestPlatformConfigMigratesLegacyAdmins(t *testing.T) {
	ctx := context.Background()
	clientset := fake.NewClientset()
	client := &Client{Clientset: clientset, Installation: "main"}
	legacy := &corev1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{Name: "donkey.platform", Namespace: "main",
			Labels: client.expectedObjectIdentity(manifests.SystemLabels("donkey-platform"))},
		Data: map[string]string{"config.json": `{"admins":{"users":["owner@example.com"],"groups":["operators"]}}`},
	}
	if _, err := clientset.CoreV1().ConfigMaps("main").Create(ctx, legacy, metav1.CreateOptions{}); err != nil {
		t.Fatal(err)
	}
	r := NewRuntime(client, Options{SystemNamespace: "main"})
	want := config.PlatformConfig{Admins: config.AccessConfig{Users: []string{"owner@example.com"}, Groups: []string{"operators"}}}
	got, err := r.PlatformConfig(ctx)
	if err != nil || !reflect.DeepEqual(got, want) {
		t.Fatalf("legacy admins were not migrated: %+v, %v", got, err)
	}
	got.Admins.Users = append(got.Admins.Users, "added@example.com")
	if err := r.SetPlatformConfig(ctx, got); err != nil {
		t.Fatal(err)
	}
	again, err := r.PlatformConfig(ctx)
	if err != nil || !reflect.DeepEqual(again, got) {
		t.Fatalf("legacy settings overwrote new settings: %+v, %v", again, err)
	}
}

func TestPlatformJSONEntryMigratesToYAMLWithoutReset(t *testing.T) {
	ctx := context.Background()
	client := &Client{Clientset: fake.NewClientset(), Installation: "main"}
	cm := &corev1.ConfigMap{ObjectMeta: metav1.ObjectMeta{Name: "donkey", Namespace: "main", Labels: client.expectedObjectIdentity(manifests.SystemLabels(manifests.ServerName))}, Data: map[string]string{
		"platform.json": `{"admins":{"users":["owner@example.com"]}}`, "buildkitd.toml": "existing build config", "route.yaml": "existing route config",
	}}
	_, _ = client.Clientset.CoreV1().ConfigMaps("main").Create(ctx, cm, metav1.CreateOptions{})
	r := NewRuntime(client, Options{SystemNamespace: "main"})
	settings, err := r.InitPlatformConfig(ctx, config.PlatformConfig{Admins: config.AccessConfig{Users: []string{"new@example.com"}}})
	if err != nil || !reflect.DeepEqual(settings.Admins.Users, []string{"owner@example.com"}) {
		t.Fatalf("upgrade reset admins: %+v, %v", settings, err)
	}
	stored, _ := client.Clientset.CoreV1().ConfigMaps("main").Get(ctx, "donkey", metav1.GetOptions{})
	if stored.Data["platform.json"] != "" || stored.Data["platform.yaml"] == "" || stored.Data["buildkitd.toml"] != cm.Data["buildkitd.toml"] || stored.Data["route.yaml"] != cm.Data["route.yaml"] {
		t.Fatal("migration changed other configuration entries")
	}
}
