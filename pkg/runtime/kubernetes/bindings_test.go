package kubernetes

import (
	"context"
	"net/url"
	"reflect"
	"testing"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	k8sruntime "k8s.io/apimachinery/pkg/runtime"
	"k8s.io/client-go/kubernetes/fake"

	"github.com/adrianliechti/donkey/internal/addons"
	"github.com/adrianliechti/donkey/pkg/runtime/kubernetes/manifests"
)

func TestServiceBindingsSeparateSpacesInBothPlacements(t *testing.T) {
	for _, shared := range []bool{false, true} {
		label := "separate namespaces"
		placement := manifests.Placement{NamespacePrefix: "donkey-"}
		if shared {
			label = "shared namespace"
			placement = manifests.Placement{SharedNamespace: "workloads"}
		}
		t.Run(label, func(t *testing.T) {
			var objects []k8sruntime.Object
			expected := map[string]string{}
			for _, space := range []string{"one", "two"} {
				namespace, name := "donkey-"+space, "postgres"
				if shared {
					namespace, name = "workloads", space+"-postgres"
				}
				expected[space] = name + "." + namespace + ".svc"
				entry, _ := addons.Get(context.Background(), "postgres")
				addon := entry.(addons.Builtin)
				binding := addon.Binding(addons.Input{Name: "postgres", ObjectName: name, Namespace: namespace, Password: space})
				data := map[string][]byte{}
				for key, value := range binding {
					data[key] = []byte(value)
				}
				labels := manifests.AddonLabels(space, "postgres", manifests.RoleBinding)
				labels[manifests.LabelInstallation] = manifests.DefaultSystemNamespace
				objects = append(objects, &corev1.Secret{ObjectMeta: metav1.ObjectMeta{Name: manifests.SpaceBindingSecretName(name), Namespace: namespace, Labels: labels}, Data: data})
				labels = manifests.AddonLabels(space, "postgres", manifests.RoleAddon)
				labels[manifests.LabelInstallation] = manifests.DefaultSystemNamespace
				objects = append(objects, &corev1.Service{ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: namespace, Labels: labels}, Spec: corev1.ServiceSpec{Ports: []corev1.ServicePort{{Port: 5432}}}})
			}
			r := NewRuntime(&Client{Clientset: fake.NewClientset(objects...)}, Options{Placement: placement})
			for _, space := range []string{"one", "two"} {
				binding, err := r.ServiceBinding(context.Background(), space, addons.SharedOwner, "postgres")
				if err != nil {
					t.Fatal(err)
				}
				u, _ := url.Parse(binding["DATABASE_URL"])
				password, _ := u.User.Password()
				if u.Hostname() != expected[space] || password != space {
					t.Fatalf("%s binding crossed spaces: %+v", space, binding)
				}
				host, port, err := r.ServiceEndpoint(context.Background(), space, "postgres")
				if err != nil || host != expected[space] || port != 5432 {
					t.Fatalf("%s endpoint: %s:%d %v", space, host, port, err)
				}
			}
		})
	}
}

func TestServiceConnectionReadsOnlyItsOwnedBinding(t *testing.T) {
	secret := spaceBindingSecret("team", "team-db", "workloads", "db", map[string]string{"DATABASE_URL": "postgres://db/app"}, addons.Metadata{Type: "vendor", Protocol: "custom"})
	secret.Labels[manifests.LabelInstallation] = manifests.DefaultSystemNamespace
	cs := fake.NewClientset(secret)
	r := NewRuntime(&Client{Clientset: cs}, Options{Placement: manifests.Placement{SharedNamespace: "workloads"}})
	got, err := r.ServiceConnection(context.Background(), "team", "db")
	if err != nil || got.Type != "vendor" || got.Protocol != "custom" || got.Binding["DATABASE_URL"] == "" {
		t.Fatalf("connection metadata: %+v %v", got, err)
	}
	if actions := cs.Actions(); len(actions) != 1 || actions[0].GetVerb() != "get" {
		t.Fatal("connection lookup listed other services")
	}
	secret.Labels[manifests.LabelInstallation] = "another-installation"
	if _, err := cs.CoreV1().Secrets("workloads").Update(context.Background(), secret, metav1.UpdateOptions{}); err != nil {
		t.Fatal(err)
	}
	if _, err := r.ServiceConnection(context.Background(), "team", "db"); err == nil {
		t.Fatal("read another installation's credentials")
	}
}

func TestAIProxyTokensExcludeOtherProvidersAndAppCopies(t *testing.T) {
	r := NewRuntime(&Client{Clientset: fake.NewClientset()}, Options{Placement: manifests.Placement{SharedNamespace: "workloads"}})
	ctx := context.Background()
	for _, fixture := range []struct{ name, endpoint, key, app string }{
		{"platform", r.aiProxyURL(), "platform-token", ""},
		{"vendor", "https://vendor.example/v1", "vendor-secret", ""},
		{"copy", r.aiProxyURL(), "app-token", "api"},
	} {
		secret := spaceBindingSecret("team", "team-"+fixture.name, "workloads", fixture.name, map[string]string{"OPENAI_BASE_URL": fixture.endpoint, "OPENAI_API_KEY": fixture.key}, addons.Metadata{})
		secret.Labels[manifests.LabelInstallation] = manifests.DefaultSystemNamespace
		if fixture.app != "" {
			secret.Labels[manifests.LabelApp] = fixture.app
		}
		if _, err := r.c.Clientset.CoreV1().Secrets("workloads").Create(ctx, secret, metav1.CreateOptions{}); err != nil {
			t.Fatal(err)
		}
	}
	got, err := r.AIProxyTokens(ctx)
	if err != nil || !reflect.DeepEqual(got, []string{"platform-token"}) {
		t.Fatalf("wrong proxy tokens: %v", err)
	}
}
