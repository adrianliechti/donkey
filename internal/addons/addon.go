// Package addons is donkey's attached-resource model: an addon is a canonical
// env contract (DATABASE_URL and friends) injected into the app. Provisioned
// or remote, the app sees the same contract, so the two are swappable.
//
// The catalog has two kinds of entries. Built-ins describe a runtime-neutral
// Workload that each runtime provider materializes its own way. Offerings
// come from configured providers (independent gRPC services) that provision
// their own resources. Both present themselves through the same descriptor
// (addonv1.Offering): title, protocol, the JSON Schema of the parameters
// and the environment contract. The catalog, the console form and the CLI
// are driven by that descriptor alone.
package addons

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"maps"
	"slices"
	"sort"
	"strings"
	"sync"

	addonv1 "github.com/adrianliechti/donkey/pkg/addons/v1"
)

// Addon is one catalog entry donkey can provision.
type Addon interface {
	// Type is the lowercase id the API and CLI speak (postgres, mariadb…).
	Type() string
	// Describe is the catalog descriptor. Built-ins answer statically; an
	// offering answers from its provider's cached catalog and may fail
	// while the provider has never answered.
	Describe(ctx context.Context) (*addonv1.Offering, error)
}

// Builtin is an addon donkey materializes itself: it renders the binding
// and, unless binding-only, a Workload for the runtime to run.
type Builtin interface {
	Addon
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

// Catalog categories of the built-ins.
const (
	CategoryDatabase = "database"
	CategoryCache    = "cache"
	CategoryStorage  = "storage"
	CategoryAI       = "ai"
)

// EnvNames lists the declared environment keys, sorted.
func EnvNames(desc *addonv1.Offering) []string {
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

// EnvTemplate is a binding with the declared keys and no values: what a
// service will inject, for planning before it exists.
func EnvTemplate(desc *addonv1.Offering) map[string]string {
	values := map[string]string{}
	for _, name := range EnvNames(desc) {
		values[name] = ""
	}
	return values
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
// from a sample binding; the named keys are marked secret. Built-ins keep
// their data on removal and re-apply changed parameters in place.
func description(a Builtin, title, summary, category, protocol string, schema map[string]any, secrets ...string) *addonv1.Offering {
	binding := a.Binding(sampleInput(a.Type()))
	var env []*addonv1.EnvVar
	for _, key := range slices.Sorted(maps.Keys(binding)) {
		env = append(env, &addonv1.EnvVar{Name: key, Secret: slices.Contains(secrets, key)})
	}
	return &addonv1.Offering{
		Id: a.Type(), Title: title, Description: summary, Category: category, Protocol: protocol,
		Parameters: MustStruct(schema), Env: env,
		Capabilities: &addonv1.Capabilities{RetainData: true, Update: true},
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
	// ServiceProxyURL is the installation's HTTP service proxy endpoint.
	ServiceProxyURL string
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

// builtins are the addon types donkey runs itself; their ids are reserved.
var builtins = map[string]Builtin{
	"postgres": postgres{},
	"redis":    redis{},
	"rustfs":   rustfs{},
	"openai":   openai{},
}

var (
	providersMu sync.RWMutex
	providers   []*Provider
)

// IsBuiltin reports whether an id names a built-in type.
func IsBuiltin(typ string) bool {
	_, ok := builtins[typ]
	return ok || proxyBuiltin(typ) != nil
}

func builtinTypes() []Addon {
	local := maps.Clone(builtins)
	maps.Copy(local, proxyBuiltins())
	names := make([]string, 0, len(local))
	for t := range local {
		names = append(names, t)
	}
	sort.Strings(names)
	out := make([]Addon, 0, len(names))
	for _, t := range names {
		out = append(out, local[t])
	}
	return out
}

func allProviders() []*Provider {
	providersMu.RLock()
	defer providersMu.RUnlock()
	return slices.Clone(providers)
}

func providerNamed(name string) *Provider {
	for _, p := range allProviders() {
		if p.name == name {
			return p
		}
	}
	return nil
}

// Types lists the catalog: the built-ins, then every provider's offerings.
// It never blocks on a provider longer than catalogWait: a provider with a
// cached catalog answers from it (refreshing in the background when stale),
// and one that has never answered is listed as a single unavailable entry.
func Types(ctx context.Context) []Addon {
	out := builtinTypes()
	for _, p := range allProviders() {
		out = append(out, p.entries(ctx, catalogWait)...)
	}
	return out
}

// Get resolves an id to its catalog entry. Providers without a cached
// catalog are asked now (bounded by describeTimeout); one that keeps
// failing is not asked again before its backoff elapses.
func Get(ctx context.Context, typ string) (Addon, error) {
	if b, ok := builtins[typ]; ok {
		return b, nil
	}
	if p := proxyBuiltin(typ); p != nil {
		return p, nil
	}
	list := allProviders()
	results := make([]*Plugin, len(list))
	var wg sync.WaitGroup
	for i, p := range list {
		wg.Add(1)
		go func() {
			defer wg.Done()
			results[i] = p.offering(ctx, typ, describeTimeout)
		}()
	}
	wg.Wait()
	for _, found := range results {
		if found != nil {
			return found, nil
		}
	}
	known := make([]string, 0, len(builtins))
	for t := range builtins {
		known = append(known, t)
	}
	for t := range proxyBuiltins() {
		known = append(known, t)
	}
	for _, p := range list {
		known = append(known, p.knownIDs()...)
	}
	sort.Strings(known)
	return nil, fmt.Errorf("donkey doesn't know the addon type %q (available: %s)", typ, strings.Join(known, ", "))
}

// Lookup resolves the entry for a service that may already exist. An
// instance a provider created uses its recorded offering, so its lifecycle
// never depends on a catalog refresh or a change to the provider's schema.
func Lookup(ctx context.Context, typ string, previous *Provisioning) (Addon, error) {
	if previous == nil || previous.Provider == "" {
		return Get(ctx, typ)
	}
	return previous.Plugin()
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
