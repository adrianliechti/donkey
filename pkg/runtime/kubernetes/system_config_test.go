package kubernetes

import (
	"context"
	"errors"
	"testing"

	"github.com/adrianliechti/donkey/pkg/runtime/kubernetes/manifests"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/client-go/kubernetes/fake"
	k8stesting "k8s.io/client-go/testing"
)

func TestSystemSecretDataPreservesUpgradeCredentials(t *testing.T) {
	for _, tt := range []struct {
		name, current, legacy, want string
		foreign, readFailure        bool
	}{
		{name: "first install"},
		{name: "upgrade", legacy: "existing-key", want: "existing-key"},
		{name: "new name wins", current: "new-key", legacy: "old-key", want: "new-key"},
		{name: "foreign new secret", current: "foreign-key", legacy: "old-key", foreign: true},
		{name: "foreign legacy secret", legacy: "foreign-key", foreign: true},
		{name: "read error must not bootstrap", legacy: "existing-key", readFailure: true},
	} {
		t.Run(tt.name, func(t *testing.T) {
			clientset := fake.NewClientset()
			client := &Client{Clientset: clientset, Installation: "main"}
			for name, key := range map[string]string{manifests.SystemConfigSecretName: tt.current, "donkey-system-config": tt.legacy} {
				if key == "" {
					continue
				}
				labels := client.expectedObjectIdentity(manifests.SystemLabels(manifests.ServerName))
				if tt.foreign {
					labels[manifests.LabelInstallation] = "another-installation"
				}
				_, err := clientset.CoreV1().Secrets("main").Create(context.Background(), &corev1.Secret{
					ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: "main", Labels: labels},
					Data:       map[string][]byte{"session-key": []byte(key)},
				}, metav1.CreateOptions{})
				if err != nil {
					t.Fatal(err)
				}
			}
			if tt.readFailure {
				clientset.PrependReactor("get", "secrets", func(k8stesting.Action) (bool, runtime.Object, error) {
					return true, nil, errors.New("API unavailable")
				})
			}
			data, err := client.SystemSecretData(context.Background(), "main")
			if tt.foreign || tt.readFailure {
				if err == nil {
					t.Fatal("unsafe credential fallback succeeded")
				}
				return
			}
			if err != nil || data["DONKEY_SESSION_KEY"] != tt.want {
				t.Fatalf("credentials were not preserved: %v", err)
			}
		})
	}
}
