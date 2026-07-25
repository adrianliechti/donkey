// Package addons implements donkey's attached-resource model: an addon is a
// binding Secret with a canonical env contract (e.g. DATABASE_URL), injected
// into the app via envFrom. Whether donkey provisioned the service in-cluster
// or the user attached a remote one, the app sees the exact same contract —
// the two are freely swappable.
package addons

import (
	"crypto/rand"
	"encoding/hex"
	"fmt"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"

	"monobox.ch/donkey/internal/k8s/manifests"
)

// Addon provisions an in-cluster instance of a service type.
type Addon interface {
	Type() string
	// Binding returns the env contract for a provisioned instance.
	Binding(in Input) map[string]string
	// Objects returns the Kubernetes objects for a provisioned instance
	// (excluding the binding secret, which donkey manages).
	Objects(in Input) ([]runtime.Object, error)
	// DefaultVersion and DefaultStorage document the defaults per type.
	DefaultVersion() string
	DefaultStorage() string
}

type Input struct {
	App       string
	Space     string
	Namespace string
	// Name distinguishes multiple addons of one type (defaults to the type).
	Name     string
	Version  string
	Storage  string
	Password string
}

// Registry holds the addon types donkey knows.
var Registry = map[string]Addon{
	"postgres": postgres{},
}

func Get(typ string) (Addon, error) {
	a, ok := Registry[typ]
	if !ok {
		known := ""
		for t := range Registry {
			if known != "" {
				known += ", "
			}
			known += t
		}
		return nil, fmt.Errorf("donkey doesn't know the addon type %q (available: %s)", typ, known)
	}
	return a, nil
}

func SecretName(app, addon string) string { return app + "-addon-" + addon }
func Component(addon string) string       { return "addon-" + addon }

// InstanceName is the object name for a provisioned addon's workload.
func InstanceName(app, addon string) string { return app + "-" + addon }

// BindingSecret wraps the env contract in the Secret the app's envFrom picks
// up. Identical shape for provisioned and external addons.
func BindingSecret(space, app, namespace, name string, data map[string]string) *corev1.Secret {
	bytes := map[string][]byte{}
	for k, v := range data {
		bytes[k] = []byte(v)
	}
	return &corev1.Secret{
		TypeMeta: metav1.TypeMeta{APIVersion: "v1", Kind: "Secret"},
		ObjectMeta: metav1.ObjectMeta{
			Name:      SecretName(app, name),
			Namespace: namespace,
			Labels:    manifests.Labels(space, app, Component(name)),
		},
		Data: bytes,
	}
}

// NewPassword generates a credential for a provisioned addon.
func NewPassword() (string, error) {
	b := make([]byte, 24)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return hex.EncodeToString(b), nil
}
