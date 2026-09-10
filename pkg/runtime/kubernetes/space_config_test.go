package kubernetes

import (
	"context"
	"errors"
	"reflect"
	"testing"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes/fake"

	"github.com/adrianliechti/donkey/internal/config"
	rt "github.com/adrianliechti/donkey/pkg/runtime"
	"github.com/adrianliechti/donkey/pkg/runtime/kubernetes/manifests"
)

func TestSpaceConfigPersistsInConfigMapInBothModes(t *testing.T) {
	for _, shared := range []string{"", "workloads"} {
		t.Run("namespace="+shared, func(t *testing.T) {
			ctx := context.Background()
			client := &Client{Clientset: fake.NewClientset()}
			opts := Options{Placement: manifests.Placement{SharedNamespace: shared, NamespacePrefix: "donkey-"}, Domain: "example.com"}
			r := NewRuntime(client, opts)
			if err := r.ensureSpaceHome(ctx, "team"); err != nil {
				t.Fatal(err)
			}
			settings, err := r.SpaceConfig(ctx, "team")
			if err != nil {
				t.Fatal(err)
			}
			if !reflect.DeepEqual(settings.Domains, []string{"team.example.com"}) {
				t.Fatalf("defaults: %+v", settings)
			}
			cm, err := client.Clientset.CoreV1().ConfigMaps(r.ns("team")).Get(ctx, manifests.SpaceMarkerName("team"), metav1.GetOptions{})
			if err != nil || cm.Data[spaceConfigKey] == "" {
				t.Fatalf("missing config.yaml: %v", err)
			}
			cm.Data["unrelated"] = "keep"
			if _, err := client.Clientset.CoreV1().ConfigMaps(cm.Namespace).Update(ctx, cm, metav1.UpdateOptions{}); err != nil {
				t.Fatal(err)
			}
			settings.Admins.Users = []string{"owner@example.com"}
			settings.Developers.Groups = []string{"team"}
			settings.Domains = []string{}
			if err := r.SetSpaceConfig(ctx, "team", settings); err != nil {
				t.Fatal(err)
			}
			if err := r.ensureSpaceHome(ctx, "team"); err != nil {
				t.Fatal(err)
			}
			got, err := NewRuntime(client, opts).SpaceConfig(ctx, "team")
			if err != nil || !reflect.DeepEqual(got, settings) {
				t.Fatalf("settings changed after re-ensure/restart: %+v, %v", got, err)
			}
			cm, _ = client.Clientset.CoreV1().ConfigMaps(r.ns("team")).Get(ctx, cm.Name, metav1.GetOptions{})
			if cm.Data["unrelated"] != "keep" {
				t.Fatal("overwrote unrelated data")
			}
			cm.Data[spaceConfigKey] = "invalid json"
			_, _ = client.Clientset.CoreV1().ConfigMaps(cm.Namespace).Update(ctx, cm, metav1.UpdateOptions{})
			if _, err := r.SpaceConfig(ctx, "team"); err == nil {
				t.Fatal("corrupt settings must fail closed")
			}
		})
	}
}

func TestSpaceConfigRefusesForeignResourcesAndMissingSpaces(t *testing.T) {
	ctx := context.Background()
	foreign := nsWith("donkey-team", true, "another-installation")
	client := &Client{Clientset: fake.NewClientset(foreign)}
	r := NewRuntime(client, Options{Placement: manifests.Placement{NamespacePrefix: "donkey-"}})
	if err := r.ensureSpaceHome(ctx, "team"); err == nil {
		t.Fatal("adopted a foreign namespace")
	}
	if _, err := r.SpaceConfig(ctx, "missing"); !errors.Is(err, rt.ErrSpaceNotFound) {
		t.Fatalf("missing space: %v", err)
	}
	if err := r.SetSpaceConfig(ctx, "missing", config.SpaceConfig{}); !errors.Is(err, rt.ErrSpaceNotFound) {
		t.Fatalf("created a missing space while saving: %v", err)
	}
	marker := manifests.SpaceMarker("team", "workloads")
	marker.Labels[manifests.LabelInstallation] = "another-installation"
	client = &Client{Clientset: fake.NewClientset(marker, &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: "workloads"}})}
	r = NewRuntime(client, Options{Placement: manifests.Placement{SharedNamespace: "workloads"}})
	if err := r.SetSpaceConfig(ctx, "team", config.SpaceConfig{}); err == nil {
		t.Fatal("rewrote a foreign marker")
	}
}

func TestExistingSpacesMigrateWithoutResettingSettings(t *testing.T) {
	ctx := context.Background()
	for _, shared := range []string{"", "workloads"} {
		t.Run("namespace="+shared, func(t *testing.T) {
			client := &Client{Clientset: fake.NewClientset()}
			r := NewRuntime(client, Options{Placement: manifests.Placement{SharedNamespace: shared, NamespacePrefix: "donkey-"}, Domain: "example.com"})
			if shared == "" {
				ns := manifests.Namespace("old", r.ns("old"))
				ns.Labels = r.c.expectedObjectIdentity(ns.Labels)
				if _, err := client.Clientset.CoreV1().Namespaces().Create(ctx, ns, metav1.CreateOptions{}); err != nil {
					t.Fatal(err)
				}
			} else {
				marker := manifests.SpaceMarker("old", shared)
				marker.Labels = r.c.expectedObjectIdentity(marker.Labels)
				if _, err := client.Clientset.CoreV1().ConfigMaps(shared).Create(ctx, marker, metav1.CreateOptions{}); err != nil {
					t.Fatal(err)
				}
			}
			if err := r.EnsureSpaceConfigs(ctx); err != nil {
				t.Fatal(err)
			}
			settings, err := r.SpaceConfig(ctx, "old")
			if err != nil || settings.Domains == nil || len(settings.Domains) != 0 || !settings.Admins.Empty() || !settings.Developers.Empty() {
				t.Fatalf("migration must preserve routes and grant no new space roles: %+v, %v", settings, err)
			}
			settings.Admins.Users = []string{"owner@example.com"}
			settings.Domains = []string{"custom.example.org"}
			if err := r.SetSpaceConfig(ctx, "old", settings); err != nil {
				t.Fatal(err)
			}
			if err := r.EnsureSpaceConfigs(ctx); err != nil {
				t.Fatal(err)
			}
			got, err := r.SpaceConfig(ctx, "old")
			if err != nil || !reflect.DeepEqual(got, settings) {
				t.Fatalf("migration reset settings: %+v, %v", got, err)
			}
		})
	}
}

func TestEmptyRenderedSpaceMarkerDoesNotHideLegacySettings(t *testing.T) {
	ctx := context.Background()
	client := &Client{Clientset: fake.NewClientset(), Installation: "main"}
	r := NewRuntime(client, Options{SystemNamespace: "main", Placement: manifests.Placement{SharedNamespace: "apps"}})
	for _, name := range []string{"team-space", "donkey.space.team"} {
		cm := manifests.SpaceMarker("team", "apps")
		cm.Name = name
		cm.Labels = client.expectedObjectIdentity(cm.Labels)
		if name == "donkey.space.team" {
			cm.Data = map[string]string{"config.json": `{"admins":{"users":["owner@example.com"]},"domains":["team.example.com"]}`}
		}
		_, _ = client.Clientset.CoreV1().ConfigMaps("apps").Create(ctx, cm, metav1.CreateOptions{})
	}
	if err := r.EnsureSpaceConfigs(ctx); err != nil {
		t.Fatal(err)
	}
	settings, err := r.SpaceConfig(ctx, "team")
	if err != nil || !reflect.DeepEqual(settings.Admins.Users, []string{"owner@example.com"}) || !reflect.DeepEqual(settings.Domains, []string{"team.example.com"}) {
		t.Fatalf("rendered marker reset legacy settings: %+v, %v", settings, err)
	}
}
