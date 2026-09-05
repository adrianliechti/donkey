package kubernetes

import (
	"context"
	"net/url"
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
				namespace, name, owner := "donkey-"+space, "postgres", addons.SharedOwner
				if shared {
					namespace, name, owner = "workloads", space+"--postgres", space+"--shared"
				}
				expected[space] = name + "." + namespace + ".svc"
				addon, _ := addons.Get("postgres")
				binding := addon.Binding(addons.Input{Name: "postgres", ObjectName: name, Namespace: namespace, Password: space})
				data := map[string][]byte{}
				for key, value := range binding {
					data[key] = []byte(value)
				}
				labels := manifests.AddonLabels(space, "postgres", manifests.RoleBinding)
				labels[manifests.LabelInstallation] = manifests.DefaultSystemNamespace
				objects = append(objects, &corev1.Secret{ObjectMeta: metav1.ObjectMeta{Name: addons.SecretName(owner, "postgres"), Namespace: namespace, Labels: labels}, Data: data})
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
