// Package addons is donkey's attached-resource model: an addon is a canonical
// env contract (DATABASE_URL and friends) injected into the app. Provisioned
// or remote, the app sees the same contract, so the two are swappable.
// Runtime-neutral: an addon describes a Workload, each provider materializes
// it its own way.
package addons

import (
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"sort"
	"strings"
)

// Addon provisions an instance of a service type.
type Addon interface {
	Type() string
	// Description is the catalog's one-liner: what a person gets.
	Description() string
	Binding(in Input) map[string]string
	// ExternalBinding derives the same contract from a connection URL, so a
	// remote service binds exactly like a provisioned one.
	ExternalBinding(raw string) (map[string]string, error)
	// Workload is nil (no error) for a binding-only addon — nothing runs.
	Workload(in Input) (*Workload, error)
	// CredentialKey is the binding entry the instance was initialized with.
	// Re-adds reuse it: rotating would lock the service out of its own data.
	CredentialKey() string
	DefaultVersion() string
	DefaultStorage() string
}

// Workload is one provisioned service instance, free of runtime vocabulary.
// The security posture (unprivileged user, no escalation, group-owned data)
// is the materializer's job, the same for every addon.
type Workload struct {
	Image    string
	Port     int32
	PortName string
	// SecretEnv maps env var names onto binding keys, so a runtime may
	// reference credentials (secretKeyRef) instead of inlining them.
	Env       map[string]string
	SecretEnv map[string]string
	// DataPath is where the instance's data volume mounts; the size comes
	// from Input.Storage.
	DataPath string
	// RunAsUser is the unprivileged identity the image expects; the data
	// volume is group-owned accordingly.
	RunAsUser int64
	// Readiness: an HTTP path on Port, or a command to run — exactly one.
	ReadyHTTPPath string
	ReadyCommand  []string
}

type Input struct {
	App       string
	Space     string
	Namespace string
	// Name distinguishes multiple addons of one type (defaults to the type).
	Name string
	// ObjectName is what the instance's own objects are called, and the name
	// apps dial it by. It differs from Name only where a whole installation
	// shares one namespace and every object carries its space — "shop--
	// postgres" rather than "postgres". Empty means the two are the same.
	ObjectName string
	Version    string
	Storage    string
	Password   string
	// ImageRepository overrides the addon's public image repository. The
	// selected service version is still appended by ImageRef.
	ImageRepository string
	// AIProxyURL is where donkey-server's AI proxy is reachable from apps —
	// the runtime provider fills it in (its address is runtime vocabulary).
	AIProxyURL string
}

// ImageRef combines the installation's optional mirror path with the
// addon's selected version.
func (in Input) ImageRef(defaultRepository string) string {
	repository := in.ImageRepository
	if repository == "" {
		repository = defaultRepository
	}
	return repository + ":" + in.Version
}

// HostName is what the instance is called on the network — its object name
// where that differs from the name the user gave it.
func (in Input) HostName() string {
	if in.ObjectName != "" {
		return in.ObjectName
	}
	return in.Name
}

// Registry holds the addon types donkey knows.
var Registry = map[string]Addon{
	"postgres": postgres{},
	"rustfs":   rustfs{},
	"openai":   openai{},
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

// NewPassword generates a credential for a provisioned addon.
func NewPassword() (string, error) {
	b := make([]byte, 24)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return hex.EncodeToString(b), nil
}
