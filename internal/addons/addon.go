// Package addons is donkey's attached-resource model: an addon is a canonical
// env contract (DATABASE_URL and friends) injected into the app. Provisioned
// or remote, the app sees the same contract, so the two are swappable.
// Runtime-neutral: an addon describes a Workload, each provider materializes
// it its own way.
//
// Every addon, built in or served by a plugin, presents itself through the
// same descriptor (addonv1.DescribeResponse): title, protocol, the JSON
// Schema of its parameters and its environment contract. The catalog, the
// console form and the CLI are driven by that descriptor alone.
package addons

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"slices"
	"sort"
	"strings"
	"sync"

	addonv1 "github.com/adrianliechti/donkey/pkg/addons/v1"
)

// Addon provisions an instance of a service type.
type Addon interface {
	// Type is the lowercase slug the API and CLI speak (postgres, rustfs…).
	Type() string
	// Describe is the catalog descriptor. Built-in addons answer statically;
	// a plugin asks its service and may fail while that service is down.
	Describe(ctx context.Context) (*addonv1.DescribeResponse, error)
	Binding(in Input) map[string]string
	// ExternalBinding derives the same contract from a connection URL, so a
	// remote service binds exactly like a provisioned one.
	ExternalBinding(raw string) (map[string]string, error)
	// Workload is nil (no error) for a binding-only addon — nothing runs.
	Workload(in Input) (*Workload, error)
	// CredentialKey is the binding entry the instance was initialized with.
	// Re-adds reuse it: rotating would lock the service out of its own data.
	CredentialKey() string
}

// Well-known capability values of a descriptor.
const (
	// CapabilityKeepData means Deprovision honours keep_data.
	CapabilityKeepData = "keep-data"
	// CapabilityRuntimePrefix marks the runtimes a plugin serves, e.g.
	// "runtime:kubernetes". A descriptor without any means every runtime.
	CapabilityRuntimePrefix = "runtime:"
)

// HasCapability reports whether a descriptor declares a capability.
func HasCapability(desc *addonv1.DescribeResponse, capability string) bool {
	return desc != nil && slices.Contains(desc.Capabilities, capability)
}

// SupportsRuntime reports whether a descriptor serves the named runtime: it
// does unless it lists runtimes and this one is missing.
func SupportsRuntime(desc *addonv1.DescribeResponse, runtime string) bool {
	if desc == nil {
		return false
	}
	listed := false
	for _, c := range desc.Capabilities {
		if name, ok := strings.CutPrefix(c, CapabilityRuntimePrefix); ok {
			listed = true
			if name == runtime {
				return true
			}
		}
	}
	return !listed
}

// EnvNames lists the declared environment keys, sorted.
func EnvNames(desc *addonv1.DescribeResponse) []string {
	if desc == nil {
		return nil
	}
	names := make([]string, 0, len(desc.Env))
	for _, v := range desc.Env {
		names = append(names, v.Name)
	}
	sort.Strings(names)
	return names
}

// sampleInput yields a throwaway binding: the contract's variable names,
// never real values.
func sampleInput(typ string) Input {
	return Input{
		Space: "space", Namespace: "ns", Name: typ,
		Version: "sample", Storage: "1Gi", Password: "x", AIProxyURL: "http://ai.example/v1",
	}
}

// description builds a built-in catalog entry. The env contract is derived
// from a sample binding; the named keys are marked secret.
func description(a Addon, title, summary, protocol string, schema map[string]any, secrets ...string) *addonv1.DescribeResponse {
	binding := a.Binding(sampleInput(a.Type()))
	var env []*addonv1.EnvVar
	for _, key := range slices.Sorted(func(yield func(string) bool) {
		for k := range binding {
			if !yield(k) {
				return
			}
		}
	}) {
		env = append(env, &addonv1.EnvVar{Name: key, Secret: slices.Contains(secrets, key)})
	}
	return &addonv1.DescribeResponse{
		Type: a.Type(), Title: title, Description: summary, Protocol: protocol,
		Parameters: MustStruct(schema), Env: env,
		Capabilities: []string{CapabilityKeepData},
	}
}

// Workload is one provisioned service instance, free of runtime vocabulary.
// The security posture (unprivileged user, no escalation, group-owned data)
// is the materializer's job, the same for every addon.
type Workload struct {
	Image    string
	Port     int32
	PortName string
	Command  []string // optional entrypoint; credentials are read from SecretEnv
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
	Space     string
	Namespace string
	// Name distinguishes multiple addons of one type (defaults to the type).
	Name string
	// ObjectName is what the instance's own objects are called, and the name
	// apps dial it by. It differs from Name only where a whole installation
	// shares one namespace and every object carries its space — "shop-
	// postgres" rather than "postgres". Empty means the two are the same.
	ObjectName string
	// Parameters are the resolved values of the descriptor's schema. Version
	// and Storage are the two every built-in reads, lifted out for brevity.
	Parameters Parameters
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

// WithParameters lifts the well-known parameters into their fields.
func (in Input) WithParameters(p Parameters) Input {
	in.Parameters = p
	in.Version = p.String(ParamVersion)
	in.Storage = p.String(ParamStorage)
	return in
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

// registry holds the addon types donkey knows.
var registry = map[string]Addon{
	"postgres": postgres{},
	"redis":    redis{},
	"rustfs":   rustfs{},
	"openai":   openai{},
}

var registryMu sync.RWMutex

// Types lists the registered addon types, sorted.
func Types() []Addon {
	registryMu.RLock()
	defer registryMu.RUnlock()
	names := make([]string, 0, len(registry))
	for t := range registry {
		names = append(names, t)
	}
	sort.Strings(names)
	out := make([]Addon, 0, len(names))
	for _, t := range names {
		out = append(out, registry[t])
	}
	return out
}

func Get(typ string) (Addon, error) {
	registryMu.RLock()
	defer registryMu.RUnlock()
	a, ok := registry[typ]
	if !ok {
		known := make([]string, 0, len(registry))
		for t := range registry {
			known = append(known, t)
		}
		sort.Strings(known)
		return nil, fmt.Errorf("donkey doesn't know the addon type %q (available: %s)", typ, strings.Join(known, ", "))
	}
	return a, nil
}

// SharedOwner is the owner value that asks for a space's own service binding
// rather than an app's copy. It is empty on purpose: no app can be called
// that, so nothing needs reserving.
const SharedOwner = ""

// NewPassword generates a credential for a provisioned addon.
func NewPassword() (string, error) {
	b := make([]byte, 24)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return hex.EncodeToString(b), nil
}
