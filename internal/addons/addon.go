// Package addons implements donkey's attached-resource model: an addon is a
// binding with a canonical env contract (e.g. DATABASE_URL), injected into
// the app's environment. Whether donkey provisioned the service or the user
// attached a remote one, the app sees the exact same contract — the two are
// freely swappable.
//
// The package is runtime-neutral on purpose: an addon describes what a
// provisioned instance is (image, port, env, data path — a Workload), and
// each runtime provider materializes that its own way.
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
	// Description is the catalog's one-liner: what a person gets, not how
	// it is wired.
	Description() string
	// Binding returns the env contract for a provisioned instance.
	Binding(in Input) map[string]string
	// ExternalBinding derives the same env contract from a user-supplied
	// connection URL, so a service running elsewhere binds exactly like a
	// provisioned one.
	ExternalBinding(raw string) (map[string]string, error)
	// Workload describes the provisioned instance for the runtime to
	// materialize. Nil (with no error) means the addon is binding-only —
	// nothing runs (e.g. openai, whose service is the platform itself).
	Workload(in Input) (*Workload, error)
	// CredentialKey names the binding entry that holds the credential the
	// provisioned instance was initialized with — re-adds reuse it, because
	// rotating it would lock the service out of its own kept data.
	CredentialKey() string
	// DefaultVersion and DefaultStorage document the defaults per type.
	DefaultVersion() string
	DefaultStorage() string
}

// Workload describes one provisioned service instance, free of any
// runtime's vocabulary. Kubernetes materializes it as a PVC + Deployment +
// Service, Docker as a container + volume; the security posture (an
// unprivileged user, no escalation, the data volume group-owned) is the
// materializer's job, the same for every addon.
type Workload struct {
	// Image is the full reference including tag.
	Image string
	// Port is the one port the service speaks; PortName says what it is
	// ("postgres", "http", "s3").
	Port     int32
	PortName string
	// Env is plain configuration. SecretEnv maps env var names onto keys of
	// the instance's binding, for values that are credentials — a runtime
	// may reference them (Kubernetes: secretKeyRef) instead of inlining.
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
	Name     string
	Version  string
	Storage  string
	Password string
	// AIProxyURL is where donkey-server's AI proxy is reachable from apps —
	// the runtime provider fills it in (its address is runtime vocabulary).
	AIProxyURL string
}

// Registry holds the addon types donkey knows.
var Registry = map[string]Addon{
	"postgres":   postgres{},
	"pocketbase": pocketbase{},
	"rustfs":     rustfs{},
	"openai":     openai{},
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
