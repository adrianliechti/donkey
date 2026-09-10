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
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes/fake"
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
	cm, err := client.Clientset.CoreV1().ConfigMaps("main").Get(ctx, "donkey.platform", metav1.GetOptions{})
	if err != nil || cm.Data[spaceConfigKey] == "" {
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
	cm.Data[spaceConfigKey] = "{}"
	_, _ = client.Clientset.CoreV1().ConfigMaps("main").Update(ctx, cm, metav1.UpdateOptions{})
	if _, err := restarted.InitPlatformConfig(ctx, first); err == nil {
		t.Fatal("empty persisted settings triggered a new bootstrap")
	}
}
