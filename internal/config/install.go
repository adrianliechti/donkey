package config

import (
	"fmt"
	"maps"
	"os"
	"strings"

	"github.com/distribution/reference"
	"sigs.k8s.io/yaml"
)

// InstallFile is the values file for `donkey server install -f server.yaml`:
// everything an admin configures about a donkey installation. Flags override
// individual fields; secrets can come from env instead
// (DONKEY_REGISTRY_PASSWORD, DONKEY_OIDC_CLIENT_SECRET, OPENAI_API_KEY).
type InstallFile struct {
	// Runtime is where donkey runs apps: "kubernetes" (the default) or
	// "docker" — straight on a Docker engine, no cluster.
	Runtime string `json:"runtime,omitempty"`
	// Image is the donkey-server image, pullable by the cluster.
	Image string `json:"image,omitempty"`
	// BuildkitImage is the build engine's image (default
	// DefaultBuildkitImage) — an air-gapped installation mirrors it.
	BuildkitImage string          `json:"buildkitImage,omitempty"`
	Registry      InstallRegistry `json:"registry,omitempty"`
	// AddonImages names mirrored repositories for the managed services the
	// runtime pulls itself. Build registry mirrors do not cover these images:
	// they are not part of an app build.
	AddonImages InstallAddonImages `json:"addonImages,omitempty"`
	// AddonImagePullSecrets names pre-existing kubernetes.io/dockerconfigjson
	// secrets used for managed addon images. Each secret must exist in every
	// workload namespace where an addon can run.
	AddonImagePullSecrets []string `json:"addonImagePullSecrets,omitempty"`
	// Domain: apps become <app>.<space>.<domain>.
	Domain string `json:"domain,omitempty"`
	// ServerHost is where the API and console live (default: the domain
	// itself). It must be the domain or a name under it — the central OAuth
	// callback always stays on the domain, and the login cookie's domain has
	// to cover the console too.
	ServerHost string         `json:"serverHost,omitempty"`
	Gateway    InstallGateway `json:"gateway,omitempty"`
	TLS        InstallTLS     `json:"tls,omitempty"`
	// Kubernetes is where donkey's objects go. Only meaningful for the
	// kubernetes runtime.
	Kubernetes InstallKubernetes `json:"kubernetes,omitempty"`
	// DefaultPlatform is the build platform of the cluster's nodes
	// (default linux/amd64).
	DefaultPlatform string `json:"defaultPlatform,omitempty"`

	Build InstallBuild `json:"build,omitempty"`

	Auth InstallAuth `json:"auth,omitempty"`
	AI   InstallAI   `json:"ai,omitempty"`
}

// InstallAddonImages overrides the image repository of each managed addon.
// Versions remain part of the service configuration and are appended by
// donkey, so these values deliberately contain no tag or digest.
type InstallAddonImages struct {
	Postgres string `json:"postgres,omitempty"`
	RustFS   string `json:"rustfs,omitempty"`
}

// Repository returns the configured repository for one addon type.
func (i InstallAddonImages) Repository(typ string) string {
	switch typ {
	case "postgres":
		return i.Postgres
	case "rustfs":
		return i.RustFS
	default:
		return ""
	}
}

// Validate catches values that cannot have an addon version appended to
// them. A registry port is fine; an image tag or digest is not.
func (i InstallAddonImages) Validate() error {
	for _, image := range []struct {
		name       string
		repository string
	}{
		{name: "postgres", repository: i.Postgres},
		{name: "rustfs", repository: i.RustFS},
	} {
		name, repository := image.name, image.repository
		if repository == "" {
			continue
		}
		named, err := reference.ParseNormalizedNamed(repository)
		if err != nil {
			return fmt.Errorf("addonImages.%s %q is not an image repository: %w", name, repository, err)
		}
		if _, tagged := named.(reference.Tagged); tagged {
			return fmt.Errorf("addonImages.%s %q must not include a tag — configure the addon version separately", name, repository)
		}
		if _, digested := named.(reference.Canonical); digested {
			return fmt.Errorf("addonImages.%s %q must not include a digest — configure the addon version separately", name, repository)
		}
	}
	return nil
}

// ValidateAddonImagePullSecrets checks names before the installer writes
// them into pod specs. Secret references are Kubernetes DNS subdomains.
func ValidateAddonImagePullSecrets(names []string) error {
	for _, name := range names {
		if name == "" || len(name) > 253 {
			return fmt.Errorf("addonImagePullSecrets contains invalid secret name %q", name)
		}
		for _, label := range strings.Split(name, ".") {
			if !namespaceRE.MatchString(label) {
				return fmt.Errorf("addonImagePullSecrets contains invalid secret name %q — use lowercase letters, digits, dashes and dots", name)
			}
		}
	}
	return nil
}

// InstallBuild is how apps are built and how a build reaches the outside
// world. BuildSettings travels to donkey-server as environment; the fields
// below are consumed where the installation is written — mirrors into the
// build engine's config, the proxy into both pods.
type InstallBuild struct {
	BuildSettings `json:",inline"`

	// RegistryMirrors maps an upstream registry host to its mirror. Written
	// into the build engine's config, so it covers every FROM a build pulls.
	RegistryMirrors map[string]string `json:"registryMirrors,omitempty"`

	// Proxy is a forward proxy for donkey-server and the build engine
	// themselves — not for the app's build steps, which read Env.
	Proxy InstallProxy `json:"proxy,omitempty"`
}

// BuildSettings is the build configuration donkey-server acts on. There is
// one builder, so its settings sit directly under build:.
type BuildSettings struct {
	// Env is injected into every build, under the app's own build.env.
	// BuildKit hands it to every RUN, which is what puts a package manager's
	// mirror in reach of build steps donkey never sees.
	Env map[string]string `json:"env,omitempty"`
	// Instructions extend the Dockerfile-writing assistant's built-in
	// instructions with installation-specific constraints and hints, such as
	// the image prefixes available on an enterprise registry. They are sent
	// to the configured model, so credentials do not belong here.
	Instructions string `json:"instructions,omitempty"`

	// Verify is a pointer so unset ≠ off; unset means on.
	Verify        *bool `json:"verify,omitempty"`
	VerifySeconds int   `json:"verifySeconds,omitempty"`
	// Attempts is the first Dockerfile plus one revision per failure shown.
	Attempts int `json:"attempts,omitempty"`
}

// InstallProxy is the forward proxy donkey's own processes use.
type InstallProxy struct {
	HTTP    string `json:"http,omitempty"`
	HTTPS   string `json:"https,omitempty"`
	NoProxy string `json:"noProxy,omitempty"`
}

const (
	// Long enough for a JVM or a cold interpreter, short enough that a broken
	// image fails quickly.
	DefaultVerifySeconds = 45
	// The first Dockerfile plus two revisions. Beyond that a model that has
	// seen the failure twice is not converging.
	DefaultAttempts = 3
	// Enough for detailed registry and toolchain guidance without allowing an
	// install setting to dominate every builder prompt accidentally.
	MaxBuilderInstructionsBytes = 16 * 1024
)

// Validate checks what can be wrong without consulting the runtime.
func (b InstallBuild) Validate() error {
	if b.Attempts < 0 {
		return fmt.Errorf("build.attempts cannot be negative")
	}
	if b.VerifySeconds < 0 {
		return fmt.Errorf("build.verifySeconds cannot be negative")
	}
	if len(b.Instructions) > MaxBuilderInstructionsBytes {
		return fmt.Errorf("build.instructions is too long (maximum %d bytes)", MaxBuilderInstructionsBytes)
	}
	for host, mirror := range b.RegistryMirrors {
		if host == "" {
			return fmt.Errorf("build.registryMirrors has an entry with no registry host")
		}
		if mirror == "" {
			return fmt.Errorf("build.registryMirrors[%q] has no mirror address", host)
		}
	}
	return nil
}

// VerifyStartup is on unless an installation turns it off.
func (b BuildSettings) VerifyStartup() bool {
	return b.Verify == nil || *b.Verify
}

// VerifyWindow is how long the app is given to come up.
func (b BuildSettings) VerifyWindow() int {
	if b.VerifySeconds <= 0 {
		return DefaultVerifySeconds
	}
	return b.VerifySeconds
}

// BuildAttempts is how many Dockerfiles the assistant may write.
func (b BuildSettings) BuildAttempts() int {
	if b.Attempts <= 0 {
		return DefaultAttempts
	}
	return b.Attempts
}

// MergeEnv layers an app's build.env over the installation's; the app wins.
func (b BuildSettings) MergeEnv(appEnv map[string]string) map[string]string {
	if len(b.Env) == 0 {
		return appEnv
	}
	merged := make(map[string]string, len(b.Env)+len(appEnv))
	maps.Copy(merged, b.Env)
	maps.Copy(merged, appEnv)
	return merged
}

// InstallRegistry describes the registry app images are pushed to. CASecret
// covers the common in-house case: a registry whose certificate comes from
// the cluster's own CA, which the server and the build engine must trust.
type InstallRegistry struct {
	Registry
	// CASecret is a secret in donkey-system holding ca.crt.
	CASecret string `json:"caSecret,omitempty"`
}

// InstallAuth configures the login gateway — the one way in: apps, console
// and CLI all authenticate against this issuer.
type InstallAuth struct {
	Issuer       string `json:"issuer,omitempty"`
	ClientID     string `json:"clientId,omitempty"`
	ClientSecret string `json:"clientSecret,omitempty"`
	// Admins narrows who may use the console and CLI (users/groups/roles,
	// matched like an app's `access` rules). Empty means everyone the IdP
	// admits may administer.
	Admins *AccessConfig `json:"admins,omitempty"`
}

// InstallKubernetes says which namespaces donkey may use, and by omission
// which ones it may create.
//
// Both fields follow one rule: name a namespace and it is yours — donkey
// expects it to exist, applies its objects into it, and never creates or
// deletes it. Leave a field empty and donkey makes and manages the namespace
// itself, which is what a cluster you own wants.
type InstallKubernetes struct {
	System   InstallNamespace `json:"system,omitempty"`
	Workload InstallNamespace `json:"workload,omitempty"`
}

// InstallNamespace names a namespace donkey was given.
type InstallNamespace struct {
	// Namespace is the one to use.
	//
	// Under system: where the control plane lives. It is also the
	// installation's identity, so two installations on one cluster must not
	// share it — and a second one has to set this.
	//
	// Under workload: the single namespace every app of every space lives in.
	// Setting it is what turns a cluster where donkey may not create
	// namespaces into one it can run on, and it is the only arrangement that
	// needs no cluster-scoped permission at all. Apps are then named
	// <space>--<app> on the cluster, which is what lets one namespace hold two
	// spaces that both have an "api".
	//
	// Leaving workload empty gives every space a namespace of its own,
	// <namespacePrefix><space>, created and deleted by donkey.
	Namespace string `json:"namespace,omitempty"`
	// NamespacePrefix maps a space onto its namespace; only read under
	// workload, and only when Namespace is empty (default "donkey-"). Two
	// installations that both give every space a namespace need different
	// prefixes, or they would fight over donkey-<space>.
	NamespacePrefix string `json:"namespacePrefix,omitempty"`
}

// SystemNamespace is where the control plane goes.
func (k InstallKubernetes) SystemNamespace() string {
	if k.System.Namespace != "" {
		return k.System.Namespace
	}
	return DefaultSystemNamespace
}

// NamespacePrefix maps a space onto its own namespace.
func (k InstallKubernetes) NamespacePrefix() string {
	if k.Workload.NamespacePrefix != "" {
		return k.Workload.NamespacePrefix
	}
	return DefaultNamespacePrefix
}

// Validate checks what can be wrong without consulting the cluster.
func (k InstallKubernetes) Validate() error {
	for field, ns := range map[string]string{
		"kubernetes.system.namespace":   k.System.Namespace,
		"kubernetes.workload.namespace": k.Workload.Namespace,
	} {
		if ns != "" && !ValidNamespace(ns) {
			return fmt.Errorf("%s %q is not a valid namespace name — lowercase letters, digits and dashes (max 63 chars)", field, ns)
		}
	}
	// Apps next to the control plane would share a namespace with a
	// privileged build engine and the policy that guards it, and donkey's own
	// objects would be inside the blast radius of every `donkey destroy`.
	if k.Workload.Namespace != "" && k.Workload.Namespace == k.SystemNamespace() {
		return fmt.Errorf("kubernetes.workload.namespace must differ from kubernetes.system.namespace (%s) — apps do not belong next to the control plane", k.SystemNamespace())
	}
	if k.Workload.Namespace != "" && k.Workload.NamespacePrefix != "" {
		return fmt.Errorf("kubernetes.workload.namespacePrefix has no meaning together with kubernetes.workload.namespace — with one namespace for every app there is no per-space namespace to prefix")
	}
	// The two blocks share a shape, but the prefix only means something for
	// app namespaces — accepting it under system: would quietly do nothing.
	if k.System.NamespacePrefix != "" {
		return fmt.Errorf("kubernetes.system.namespacePrefix is not a setting — the prefix belongs under kubernetes.workload")
	}
	return nil
}

// InstallGateway tunes the traffic entry point.
type InstallGateway struct {
	// ServiceType is the type of the service traffic enters the cluster
	// through: LoadBalancer (the default), NodePort or ClusterIP. Clusters
	// without a load-balancer controller (kind, local demos) need NodePort
	// or ClusterIP — a LoadBalancer service never gets an address there,
	// which would leave every donkey hostname unroutable.
	ServiceType string `json:"serviceType,omitempty"`
	// HTTPNodePort/HTTPSNodePort pin the node ports of a NodePort service,
	// so port mappings (kind) or firewall rules can rely on them.
	HTTPNodePort  int32 `json:"httpNodePort,omitempty"`
	HTTPSNodePort int32 `json:"httpsNodePort,omitempty"`
	// HTTPRoute attaches donkey's edge to a Gateway API gateway the cluster
	// already runs (one HTTPRoute for the domain and everything under it),
	// instead of exposing the edge Service directly. That gateway terminates
	// TLS before routes match, so this pairs with tls.mode "none".
	HTTPRoute InstallHTTPRoute `json:"httpRoute,omitempty"`
}

// InstallHTTPRoute names the Gateway API gateway to attach to.
type InstallHTTPRoute struct {
	Name string `json:"name,omitempty"`
	// Namespace is where that gateway lives; empty means donkey's own system
	// namespace (the Gateway API reading of an unqualified parentRef).
	Namespace string `json:"namespace,omitempty"`
	// SectionName narrows the attachment to one listener of the gateway.
	SectionName string `json:"sectionName,omitempty"`
}

// Enabled reports whether the installation attaches to a cluster gateway.
func (r InstallHTTPRoute) Enabled() bool { return r.Name != "" }

// Validate checks what can be wrong without consulting the cluster.
func (r InstallHTTPRoute) Validate() error {
	if r.Name == "" && (r.Namespace != "" || r.SectionName != "") {
		return fmt.Errorf("gateway.httpRoute needs `name` — the Gateway API gateway to attach to")
	}
	if r.Namespace != "" && !ValidNamespace(r.Namespace) {
		return fmt.Errorf("gateway.httpRoute.namespace %q is not a valid namespace name", r.Namespace)
	}
	return nil
}

// InstallTLS is how the gateway gets its certificates. donkey always serves
// apps over https — the login gateway's cookies require it — and the
// gateway issues the certificates itself: no cert-manager, no ingress
// vendor.
type InstallTLS struct {
	// Mode is "acme" (one certificate per hostname from Let's Encrypt, the
	// default — the gateway must be reachable on ports 80/443), "ca" (a
	// self-signed CA minting per-hostname certificates — local and internal
	// installs; trust its one CA certificate and every app is covered),
	// or "none" (no TLS at donkey's edge — only sensible behind
	// gateway.httpRoute, where the cluster's gateway terminates TLS with
	// certificates the platform provides).
	Mode string `json:"mode,omitempty"`
	// Email is the ACME account contact (expiry warnings); optional.
	Email string `json:"email,omitempty"`
	// Directory overrides the ACME endpoint — Let's Encrypt staging, or an
	// internal CA speaking ACME.
	Directory string `json:"directory,omitempty"`
}

// Validate checks the TLS block without consulting the cluster.
func (t InstallTLS) Validate() error {
	switch t.Mode {
	case "", "acme", "ca", "none":
	// "off" is what people try first — and bare, YAML 1.1 reads it as a
	// boolean and hands us "false". Both spellings deserve the real name.
	case "off", "false":
		return fmt.Errorf(`tls.mode %q is not a mode — "none" turns TLS off at donkey's edge`, t.Mode)
	default:
		return fmt.Errorf("tls.mode must be acme, ca or none, not %q", t.Mode)
	}
	return nil
}

// DefaultBuildkitImage is the build engine donkey installs. It is pinned to a
// digest as well as a tag: a tag can be re-pushed, and the build engine runs
// privileged — the one place where "whatever the registry serves today" is
// not good enough. The digest is the multi-platform index of v0.31.2.
const DefaultBuildkitImage = "moby/buildkit:v0.31.2@sha256:2f5adac4ecd194d9f8c10b7b5d7bceb5186853db1b26e5abd3a657af0b7e26ec"

type InstallAI struct {
	BaseURL string `json:"baseUrl,omitempty"`
	APIKey  string `json:"apiKey,omitempty"`
	Model   string `json:"model,omitempty"`
}

func LoadInstallFile(path string) (*InstallFile, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("couldn't read the settings file: %w", err)
	}
	var f InstallFile
	if err := yaml.UnmarshalStrict(b, &f); err != nil {
		return nil, fmt.Errorf("%s is not valid: %w", path, err)
	}
	return &f, nil
}
