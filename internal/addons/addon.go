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
	"sort"
	"strings"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"

	"github.com/adrianliechti/donkey/internal/k8s/manifests"
)

// Addon provisions an in-cluster instance of a service type.
type Addon interface {
	Type() string
	// Description is the catalog's one-liner: what a person gets, not how
	// it is wired.
	Description() string
	// Binding returns the env contract for a provisioned instance.
	Binding(in Input) map[string]string
	// ExternalBinding derives the same env contract from a user-supplied
	// connection URL, so a service running elsewhere binds exactly like a
	// provisioned one.
	ExternalBinding(raw string) (map[string]string, error)
	// Objects returns the Kubernetes objects for a provisioned instance
	// (excluding the binding secret, which donkey manages).
	Objects(in Input) ([]runtime.Object, error)
	// CredentialKey names the binding entry that holds the credential the
	// provisioned instance was initialized with — re-adds reuse it, because
	// rotating it would lock the service out of its own kept data.
	CredentialKey() string
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
	"postgres":   postgres{},
	"pocketbase": pocketbase{},
}

// Types lists the registered addon types, sorted.
func Types() []Addon {
	names := make([]string, 0, len(Registry))
	for t := range Registry {
		names = append(names, t)
	}
	sort.Strings(names)
	out := make([]Addon, 0, len(names))
	for _, t := range names {
		out = append(out, Registry[t])
	}
	return out
}

func Get(typ string) (Addon, error) {
	a, ok := Registry[typ]
	if !ok {
		known := make([]string, 0, len(Registry))
		for t := range Registry {
			known = append(known, t)
		}
		sort.Strings(known)
		return nil, fmt.Errorf("donkey doesn't know the addon type %q (available: %s)", typ, strings.Join(known, ", "))
	}
	return a, nil
}

// SharedOwner is the pseudo-app owning a space's shared addons. Everything
// app-scoped — instance names, binding secrets, labels, deletion — works
// verbatim with this as the app, which is the whole trick. The name is
// reserved: an actual app called "shared" would be deleted together with the
// space's services.
const SharedOwner = "shared"

func SecretName(app, addon string) string { return app + "-addon-" + addon }
func Component(addon string) string       { return "addon-" + addon }

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

// AttachmentSecret is an app's copy of a space-shared addon's binding — the
// same contract, pointing at the shared instance, marked so that removing
// the shared addon can find its copies (and only them).
func AttachmentSecret(space, app, namespace, name string, data map[string]string) *corev1.Secret {
	sec := BindingSecret(space, app, namespace, name, data)
	sec.Labels[manifests.LabelShared] = "true"
	return sec
}

// NewPassword generates a credential for a provisioned addon.
func NewPassword() (string, error) {
	b := make([]byte, 24)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return hex.EncodeToString(b), nil
}
