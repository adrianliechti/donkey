package kubernetes

import (
	"context"
	"encoding/json"
	"fmt"
	"reflect"
	"testing"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	dynamicfake "k8s.io/client-go/dynamic/fake"
	"k8s.io/client-go/kubernetes/fake"
	k8stesting "k8s.io/client-go/testing"

	"github.com/adrianliechti/donkey/internal/config"
	"github.com/adrianliechti/donkey/pkg/runtime/kubernetes/manifests"
)

func trafficMigrationFixture(t *testing.T, shared, legacySecret bool) (*Runtime, *dynamicfake.FakeDynamicClient, *corev1.Secret) {
	t.Helper()
	in := manifests.Input{
		App: &config.AppConfig{Name: "my-api"}, Space: "team", SystemNamespace: "main",
		Domain: "api.example.test", TLS: config.InstallTLS{Mode: "cert-manager", IssuerName: "issuer"},
		ExternalDNS: config.InstallExternalDNS{Targets: []string{"192.0.2.1"}},
	}
	objects := manifests.AppTrafficObjects(in)
	secretName := "team-my-api-tls"
	if legacySecret {
		secretName = "donkey-app-team.my-api-tls"
	}
	for _, object := range objects {
		u := object.(*unstructured.Unstructured)
		u.SetName(legacyAppTrafficName(u.GetKind(), in.Space, in.App.Name))
		u.SetUID(types.UID("old-" + u.GetKind()))
		u.SetAnnotations(map[string]string{"example.com/note": "preserve"})
		if err := manifests.StampInstallation(u, "main"); err != nil {
			t.Fatal(err)
		}
		if u.GetKind() == "Certificate" {
			if err := unstructured.SetNestedField(u.Object, secretName, "spec", "secretName"); err != nil {
				t.Fatal(err)
			}
		}
	}
	identity := manifests.Labels(in.Space, in.App.Name, manifests.RoleEdge)
	identity[manifests.LabelInstallation] = "main"
	secret := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{
			Name: secretName, Namespace: "main", UID: "issued-secret", Labels: identity,
			Annotations: map[string]string{"cert-manager.io/certificate-name": "donkey-app-team.my-api"},
			OwnerReferences: []metav1.OwnerReference{{
				APIVersion: "cert-manager.io/v1", Kind: "Certificate", Name: "donkey-app-team.my-api", UID: "old-Certificate",
			}},
		},
		Type: corev1.SecretTypeTLS,
		Data: map[string][]byte{"tls.crt": []byte("existing-certificate"), "tls.key": []byte("existing-private-key")},
	}
	dyn := dynamicfake.NewSimpleDynamicClientWithCustomListKinds(runtime.NewScheme(), map[schema.GroupVersionResource]string{
		gvrFor["Certificate"]: "CertificateList", gvrFor["DNSEndpoint"]: "DNSEndpointList",
	}, objects...)
	// The dynamic fake does not implement server-side apply creation. Store
	// successful applies so subsequent migration reads see real API behavior.
	dyn.PrependReactor("patch", "*", func(action k8stesting.Action) (bool, runtime.Object, error) {
		patch := action.(k8stesting.PatchAction)
		if patch.GetPatchType() != types.ApplyPatchType {
			return false, nil, nil
		}
		var object unstructured.Unstructured
		if err := json.Unmarshal(patch.GetPatch(), &object); err != nil {
			return true, nil, err
		}
		existing, err := dyn.Tracker().Get(patch.GetResource(), patch.GetNamespace(), patch.GetName())
		if apierrors.IsNotFound(err) {
			object.SetUID(types.UID("new-" + object.GetKind()))
			err = dyn.Tracker().Create(patch.GetResource(), &object, patch.GetNamespace())
		} else if err == nil {
			object.SetUID(existing.(*unstructured.Unstructured).GetUID())
			err = dyn.Tracker().Update(patch.GetResource(), &object, patch.GetNamespace())
		}
		return true, &object, err
	})
	placement := manifests.Placement{NamespacePrefix: "donkey-"}
	if shared {
		placement.SharedNamespace = "workloads"
	}
	r := NewRuntime(&Client{Clientset: fake.NewClientset(secret), Dynamic: dyn}, Options{
		SystemNamespace: "main", Placement: placement, TLS: in.TLS, ExternalDNS: in.ExternalDNS,
	})
	return r, dyn, secret
}

func TestTrafficMigrationPreservesIssuedTLSAndDNS(t *testing.T) {
	for _, shared := range []bool{false, true} {
		for _, legacySecret := range []bool{false, true} {
			t.Run(fmt.Sprintf("shared=%v/legacy-secret=%v", shared, legacySecret), func(t *testing.T) {
				ctx := context.Background()
				r, dyn, before := trafficMigrationFixture(t, shared, legacySecret)
				for range 2 {
					if err := r.EnsureSpaceConfigs(ctx); err != nil {
						t.Fatal(err)
					}
					if err := r.FinishConfigMigration(ctx); err != nil {
						t.Fatal(err)
					}
				}
				for _, kind := range []string{"Certificate", "DNSEndpoint"} {
					resource := dyn.Resource(gvrFor[kind]).Namespace("main")
					if _, err := resource.Get(ctx, legacyAppTrafficName(kind, "team", "my-api"), metav1.GetOptions{}); !apierrors.IsNotFound(err) {
						t.Fatalf("legacy %s remains: %v", kind, err)
					}
					name := "team-my-api"
					if kind == "DNSEndpoint" {
						name += "-dns"
					}
					after, err := resource.Get(ctx, name, metav1.GetOptions{})
					if err != nil {
						t.Fatal(err)
					}
					if after.GetAnnotations()["example.com/note"] != "preserve" || after.GetLabels()[manifests.LabelApp] != "my-api" {
						t.Fatal("traffic metadata changed")
					}
					if kind == "Certificate" {
						names, _, _ := unstructured.NestedStringSlice(after.Object, "spec", "dnsNames")
						secretName, _, _ := unstructured.NestedString(after.Object, "spec", "secretName")
						if !reflect.DeepEqual(names, []string{"api.example.test"}) || secretName != "team-my-api-tls" {
							t.Fatal("certificate hostname or Secret reference changed")
						}
					} else if err := expectedDNSEndpoint(after.Object, "api.example.test", "A", []string{"192.0.2.1"}); err != nil {
						t.Fatal(err)
					}
				}
				after, err := r.c.Clientset.CoreV1().Secrets("main").Get(ctx, "team-my-api-tls", metav1.GetOptions{})
				if err != nil || after.Type != before.Type || !reflect.DeepEqual(after.Data, before.Data) {
					t.Fatal("issued TLS certificate or private key changed")
				}
				if !legacySecret && after.UID != before.UID {
					t.Fatal("the current TLS Secret was replaced")
				}
				if after.Annotations["cert-manager.io/certificate-name"] != "team-my-api" {
					t.Fatal("TLS Secret still points to the removed Certificate")
				}
				if legacySecret {
					if _, err := r.c.Clientset.CoreV1().Secrets("main").Get(ctx, before.Name, metav1.GetOptions{}); !apierrors.IsNotFound(err) {
						t.Fatal("legacy TLS Secret remains")
					}
				}
				for _, action := range dyn.Actions() {
					deleted, ok := action.(k8stesting.DeleteAction)
					if !ok || action.GetResource().Resource != "certificates" {
						continue
					}
					opts := deleted.GetDeleteOptions()
					if opts.PropagationPolicy == nil || *opts.PropagationPolicy != metav1.DeletePropagationOrphan ||
						opts.Preconditions == nil || opts.Preconditions.UID == nil || *opts.Preconditions.UID != "old-Certificate" {
						t.Fatal("legacy Certificate deletion could garbage-collect its TLS Secret")
					}
				}
			})
		}
	}
}

func TestTrafficMigrationRetainsOldObjectsOnApplyFailure(t *testing.T) {
	ctx := context.Background()
	r, dyn, _ := trafficMigrationFixture(t, false, false)
	dyn.PrependReactor("patch", "certificates", func(action k8stesting.Action) (bool, runtime.Object, error) {
		return true, nil, apierrors.NewForbidden(gvrFor["Certificate"].GroupResource(), "team-my-api", fmt.Errorf("rejected"))
	})
	if err := r.migrateAppTrafficNames(ctx); err == nil {
		t.Fatal("migration succeeded after a rejected apply")
	}
	for _, kind := range []string{"Certificate", "DNSEndpoint"} {
		if _, err := dyn.Resource(gvrFor[kind]).Namespace("main").Get(ctx, legacyAppTrafficName(kind, "team", "my-api"), metav1.GetOptions{}); err != nil {
			t.Fatalf("old %s lost after apply failure: %v", kind, err)
		}
	}
}

func TestTrafficMigrationRefusesForeignDestination(t *testing.T) {
	ctx := context.Background()
	r, dyn, _ := trafficMigrationFixture(t, true, false)
	certs := dyn.Resource(gvrFor["Certificate"]).Namespace("main")
	foreign, err := certs.Get(ctx, "donkey-app-team.my-api", metav1.GetOptions{})
	if err != nil {
		t.Fatal(err)
	}
	foreign.SetName("team-my-api")
	foreign.SetLabels(map[string]string{manifests.LabelInstallation: "another"})
	if _, err := certs.Create(ctx, foreign, metav1.CreateOptions{}); err != nil {
		t.Fatal(err)
	}
	if err := r.migrateAppTrafficNames(ctx); err == nil {
		t.Fatal("migration adopted a foreign Certificate")
	}
	if _, err := certs.Get(ctx, "donkey-app-team.my-api", metav1.GetOptions{}); err != nil {
		t.Fatal("old Certificate was removed despite the destination conflict")
	}
}

func TestTrafficMigrationKeepsCurrentSpecAfterPartialUpgrade(t *testing.T) {
	ctx := context.Background()
	r, dyn, _ := trafficMigrationFixture(t, false, false)
	certs := dyn.Resource(gvrFor["Certificate"]).Namespace("main")
	current, err := certs.Get(ctx, "donkey-app-team.my-api", metav1.GetOptions{})
	if err != nil {
		t.Fatal(err)
	}
	current.SetName("team-my-api")
	if err := unstructured.SetNestedStringSlice(current.Object, []string{"updated.example.test"}, "spec", "dnsNames"); err != nil {
		t.Fatal(err)
	}
	if _, err := certs.Create(ctx, current, metav1.CreateOptions{}); err != nil {
		t.Fatal(err)
	}
	if err := r.migrateAppTrafficNames(ctx); err != nil {
		t.Fatal(err)
	}
	current, err = certs.Get(ctx, "team-my-api", metav1.GetOptions{})
	if err != nil {
		t.Fatal(err)
	}
	names, _, _ := unstructured.NestedStringSlice(current.Object, "spec", "dnsNames")
	if !reflect.DeepEqual(names, []string{"updated.example.test"}) {
		t.Fatal("migration restored the old hostname over a newer Certificate")
	}
}
