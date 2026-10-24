package config

import (
	"fmt"
	"os"
	"path"
	"path/filepath"
	"regexp"
	"strings"

	"sigs.k8s.io/yaml"
)

// AppFileName is the per-app config file, committed to the repo. No secrets.
const AppFileName = "donkey.yaml"

type AppConfig struct {
	Name string `json:"name"`
	// Space is one namespace (siblings reach each other by plain name) and one
	// level of the address. Empty means "work it out" — see ResolveSpace.
	Space string `json:"space,omitempty"`
	Port  int    `json:"port"`
	// Public opts the app out of the login gateway: anyone can reach it.
	Public bool `json:"public,omitempty"`
	// Mode describes the app's traffic. Empty is web; api enables API-aware
	// gateway behavior when the app also uses protected exposure.
	Mode string `json:"mode,omitempty"`
	// CORS selects the cross-origin policy at the gateway. Empty disables
	// cross-origin access; allowAll answers preflights and emits
	// Access-Control-Allow-Origin: *.
	CORS string `json:"cors,omitempty"`
	// Analytics: false keeps donkey-gateway from adding its web analytics
	// script to the app's HTML pages. Omitted means on — for web apps with an
	// address; API-mode and internal apps never get it.
	Analytics *bool `json:"analytics,omitempty"`
	// Internal keeps the app off the gateway entirely — no hostname, no
	// certificate, no login. What a backend behind a frontend wants.
	Internal bool        `json:"internal,omitempty"`
	Replicas *int32      `json:"replicas,omitempty"`
	Build    BuildConfig `json:"build,omitempty"`
	Command  []string    `json:"command,omitempty"`
	// Routes put this app on another app's hostname under a path prefix, so
	// a frontend and its API can share one address.
	Routes      []RouteConfig     `json:"routes,omitempty"`
	Healthcheck Healthcheck       `json:"healthcheck,omitempty"`
	Env         map[string]string `json:"env,omitempty"`
	Addons      []AddonConfig     `json:"addons,omitempty"`
	Storage     *StorageConfig    `json:"storage,omitempty"`
	// Host is the address this app answers on. See AppHost for the three
	// shapes it takes; omitted keeps the default, <app>.<space>.<domain>.
	Host      string     `json:"host,omitempty"`
	Resources *Resources `json:"resources,omitempty"`
}

// RootHost claims the space's own address, borrowing the zone-file
// convention. Legal as neither an app name nor a hostname, so it can never be
// mistaken for either.
const RootHost = "@"

const (
	AppModeWeb   = "web"
	AppModeAPI   = "api"
	CORSAllowAll = "allowAll"
)

func (a *AppConfig) IsAPI() bool { return a.Mode == AppModeAPI }

// UsesOAuth is true only when an API has an authenticated public address.
// Public APIs proxy directly, and internal APIs have no address of their own.
func (a *AppConfig) UsesOAuth() bool { return a.IsAPI() && !a.Public && !a.Internal }

func (a *AppConfig) GatewayMode() string {
	if a.Mode == "" {
		return AppModeWeb
	}
	return a.Mode
}

func (a *AppConfig) AllowsAllOrigins() bool { return a.CORS == CORSAllowAll }

// AnalyticsEnabled says the app has not opted out of web analytics. Whether
// the gateway actually counts anything also depends on the app being a web
// app with an address of its own.
func (a *AppConfig) AnalyticsEnabled() bool { return a.Analytics == nil || *a.Analytics }

// CountsVisits says donkey-gateway adds its analytics script to this app's
// HTML pages.
func (a *AppConfig) CountsVisits() bool {
	return a.AnalyticsEnabled() && !a.IsAPI() && !a.Internal
}

// AppHost is the address an app is served at, and the one place the rule
// lives — the control plane and every runtime provider ask here, so they
// cannot drift apart:
//
//	internal: true        no address at all (siblings reach it by name)
//	host: "@"             <space>.<domain> — the space's own front door
//	host: shop.acme.com   exactly that hostname (it has a dot)
//	host: shop            shop.<space>.<domain> — a label of its own
//	otherwise             <app>.<space>.<domain>
//
// The dot is exact, not a guess: dots are illegal in app and space names, and
// a hostname always has at least two labels.
func AppHost(app *AppConfig, space, domain string) string {
	switch {
	case app.Internal:
		return ""
	case app.Host == RootHost:
		return space + "." + domain
	case strings.Contains(app.Host, "."):
		return app.Host
	case app.Host != "":
		return app.Host + "." + space + "." + domain
	default:
		return app.Name + "." + space + "." + domain
	}
}

// validateHost checks the three shapes `host` may take. A wildcard is
// refused with the rest: it would hand the app every name underneath it,
// and no certificate donkey issues covers one.
func validateHost(host string) error {
	switch {
	case host == "" || host == RootHost:
		return nil
	case strings.Contains(host, "."):
		if !domainRE.MatchString(host) || len(host) > 253 {
			return fmt.Errorf("host %q must be a plain hostname like %q — lowercase, no wildcard, no https:// and no path", host, "app.example.com")
		}
	case !nameRE.MatchString(host):
		return fmt.Errorf("host %q must be a plain name (lowercase letters, digits and dashes) to sit under the space, %q for the space's own address, or a full hostname like %q", host, RootHost, "app.example.com")
	}
	return nil
}

// StorageConfig asks for disk that survives restarts. donkey creates exactly
// ONE volume per app, each path a subPath on it. Nil means "nobody decided
// yet" (the assistant may propose); an empty list means "no disk".
type StorageConfig struct {
	// Size of the single volume, e.g. "5Gi" (default 1Gi).
	Size string `json:"size,omitempty"`
	// Class is the StorageClass; empty uses the cluster's default.
	Class string `json:"class,omitempty"`
	// Paths are absolute paths inside the container.
	Paths []string `json:"paths"`
}

type BuildStrategy string

const (
	BuildAuto       BuildStrategy = "auto"
	BuildDockerfile BuildStrategy = "dockerfile"
	// BuildAI has the assistant write the Dockerfile, builds it, and checks it
	// starts. The file is kept at .donkey/Dockerfile, so every later deploy is
	// an ordinary repeatable build.
	BuildAI BuildStrategy = "ai"
)

type BuildConfig struct {
	Type BuildStrategy `json:"type,omitempty"`
	// Context is relative to this app's folder; uploads may use a parent
	// workspace directory, but never a directory outside the upload.
	Context string `json:"context,omitempty"`
	// Dockerfile is the path (relative to the app dir) used when Type is "dockerfile".
	Dockerfile   string            `json:"dockerfile,omitempty"`
	Platform     string            `json:"platform,omitempty"`
	Env          map[string]string `json:"env,omitempty"`
	BuildkitAddr string            `json:"buildkitAddr,omitempty"`
}

// Healthcheck configures the app's probes. An empty Path means "check that
// the port answers" (TCP) — the safe zero-config default, since many apps
// (e.g. MCP servers, APIs) have no route on "/".
type Healthcheck struct {
	Path string `json:"path,omitempty"`
}

// RouteConfig sends one path prefix of another app's address to this app.
// Both apps keep their own login setting, so a public API can sit under a
// path of a private frontend — one address, two rules.
type RouteConfig struct {
	// App is the app whose hostname this route joins; it must live in the
	// same space.
	App string `json:"app"`
	// Path is the prefix that reaches this app, e.g. "/api".
	Path        string `json:"path"`
	StripPrefix bool   `json:"stripPrefix,omitempty"`
}

// EnvRef names a live service binding without copying its credential into config.
func EnvRef(service, key string) string { return "${" + service + "." + key + "}" }

func ParseEnvRef(value string) (service, key string, ok bool) {
	m := envRefRE.FindStringSubmatch(value)
	if m == nil {
		return "", "", false
	}
	return m[1], m[2], true
}

var envRefRE = regexp.MustCompile(`^\$\{([a-z0-9][a-z0-9-]*)\.([A-Za-z_][A-Za-z0-9_]*)\}$`)

// NormalizeAddonType maps historical addon types to their current catalog name.
// Instance names are deliberately independent: renaming a type must not move data.
func NormalizeAddonType(typ string) string {
	if typ == "rustfs" {
		return "s3"
	}
	return typ
}

type AddonConfig struct {
	// Name is optional in donkey.yaml (it defaults to the type), so Save
	// must be able to drop it again — omitempty keeps a minimized entry
	// from serializing as `name: ""`.
	Name     string `json:"name,omitempty"`
	Type     string `json:"type"`
	External bool   `json:"external,omitempty"`
	// Version and Storage are shorthand for the parameters of those names.
	Version string `json:"version,omitempty"`
	Storage string `json:"storage,omitempty"`
	// Parameters are the service type's own settings, as its catalog schema
	// describes them (`donkey addon add` records what was chosen).
	Parameters map[string]any `json:"parameters,omitempty"`
}

// ServiceParameters merges the shorthand fields into the parameters; nil
// means "the type's defaults, or what the instance already has".
func (a AddonConfig) ServiceParameters() map[string]any {
	out := map[string]any{}
	for k, v := range a.Parameters {
		out[k] = v
	}
	if a.Version != "" {
		out["version"] = a.Version
	}
	if a.Storage != "" {
		out["storage"] = a.Storage
	}
	if len(out) == 0 {
		return nil
	}
	return out
}

type Resources struct {
	Requests map[string]string `json:"requests,omitempty"`
	Limits   map[string]string `json:"limits,omitempty"`
}

const (
	DefaultReplicas   = int32(1)
	DefaultDockerfile = "Dockerfile"
	DefaultStorage    = "1Gi"
	// DefaultSpace is used when neither the folder nor the user names one.
	DefaultSpace      = "default"
	defaultCPURequest = "100m"
	defaultMemRequest = "128Mi"
)

// nameRE follows RFC 1123 DNS labels: app and space names become part of
// namespace, hostnames and object names.
var nameRE = regexp.MustCompile(`^[a-z0-9]([a-z0-9-]{0,38}[a-z0-9])?$`)

// ValidName reports whether s is a name donkey accepts for apps and
// addons. The server checks URL path values against it — they feed namespace,
// hostname and object names, so they follow donkey.yaml's rules.
func ValidName(s string) bool { return nameRE.MatchString(s) }

func ValidateAppName(s string) error { return validateWorkloadName(s, "app") }

func ValidateServiceName(s string) error { return validateWorkloadName(s, "service") }

func validateWorkloadName(s, kind string) error {
	if !ValidName(s) {
		return fmt.Errorf("%s name %q must use lowercase letters, digits and dashes (max 40 chars)", kind, s)
	}
	return nil
}

// ValidSpaceName reports whether s is a name donkey accepts for a space.
func ValidSpaceName(s string) bool { return ValidateSpaceName(s) == nil }

// ValidateSpaceName applies the same DNS-label rule as apps and services.
// Object names derived from a space are never parsed back, so a space may
// contain hyphens like any other name; identity always comes from labels.
func ValidateSpaceName(s string) error {
	if !ValidName(s) {
		return fmt.Errorf("space %q must use lowercase letters, digits and dashes (max 40 chars)", s)
	}
	switch s {
	case "donkey", "platform", "system":
		return fmt.Errorf("space name %q is reserved for Donkey; choose another name", s)
	}
	return nil
}

// namespaceRE is an RFC 1123 DNS label — what Kubernetes accepts as a
// namespace name. Longer than donkey's own names allow, because the
// namespaces donkey is *given* are named by somebody else.
var namespaceRE = regexp.MustCompile(`^[a-z0-9]([a-z0-9-]{0,61}[a-z0-9])?$`)

// ValidNamespace reports whether s is a name Kubernetes accepts for a
// namespace.
func ValidNamespace(s string) bool { return namespaceRE.MatchString(s) }

// A plain lowercase DNS hostname of at least two labels: it becomes a route
// host, a certificate name and part of the callback URL, none of which accept
// a wildcard, scheme, port or path.
var domainRE = regexp.MustCompile(`^[a-z0-9]([a-z0-9-]{0,61}[a-z0-9])?(\.[a-z0-9]([a-z0-9-]{0,61}[a-z0-9])?)+$`)

// sizeRE accepts the Kubernetes quantity spellings people actually use for
// disks; the cluster does the real parsing.
var sizeRE = regexp.MustCompile(`^[0-9]+(\.[0-9]+)?(Ki|Mi|Gi|Ti|Pi|K|M|G|T|P)?$`)

// LoadApp reads donkey.yaml from dir. A missing file returns (nil, nil).
func LoadApp(dir string) (*AppConfig, error) {
	path := filepath.Join(dir, AppFileName)
	b, err := os.ReadFile(path)
	if os.IsNotExist(err) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var c AppConfig
	if err := yaml.UnmarshalStrict(b, &c); err != nil {
		return nil, fmt.Errorf("%s is not valid: %w", path, err)
	}
	c.ApplyDefaults()
	if err := c.Validate(); err != nil {
		return nil, fmt.Errorf("%s is not valid: %w", path, err)
	}
	return &c, nil
}

func (c *AppConfig) ApplyDefaults() {
	if c.Build.Type == "" {
		c.Build.Type = BuildAuto
	}
	if c.Storage != nil && c.Storage.Size == "" {
		c.Storage.Size = DefaultStorage
	}
	if c.Replicas == nil {
		r := DefaultReplicas
		c.Replicas = &r
	}
	if c.Resources == nil {
		c.Resources = &Resources{
			Requests: map[string]string{"cpu": defaultCPURequest, "memory": defaultMemRequest},
		}
	}
	for i := range c.Addons {
		if c.Addons[i].Name == "" {
			c.Addons[i].Name = c.Addons[i].Type
		}
		// Resolve the legacy default name first to preserve endpoints and disks.
		c.Addons[i].Type = NormalizeAddonType(c.Addons[i].Type)
	}
}

func (c *AppConfig) Validate() error {
	if c.Name == "" {
		return fmt.Errorf("name is required")
	}
	if err := ValidateAppName(c.Name); err != nil {
		return err
	}
	if c.Space != "" {
		if err := ValidateSpaceName(c.Space); err != nil {
			return err
		}
	}
	if c.Port < 1 || c.Port > 65535 {
		return fmt.Errorf("port must be between 1 and 65535 (the port your app listens on)")
	}
	if c.Internal && len(c.Routes) > 0 {
		return fmt.Errorf("an internal app cannot have gateway routes — remove `routes`, or set `internal: false` to expose the app")
	}
	if c.Internal && c.Public {
		return fmt.Errorf("an internal app has no address, so `public: true` has nothing to open — drop one of the two")
	}
	if c.Mode != "" && c.Mode != AppModeWeb && c.Mode != AppModeAPI {
		return fmt.Errorf("mode must be web or api")
	}
	if c.CORS != "" && c.CORS != CORSAllowAll {
		return fmt.Errorf("cors must be allowAll when set")
	}
	if c.Internal && c.Host != "" {
		return fmt.Errorf("an internal app has no address, so `host` has no effect — drop one of the two")
	}
	seenRoutes := map[string]bool{}
	for _, r := range c.Routes {
		if !nameRE.MatchString(r.App) {
			return fmt.Errorf("routes: %q is not an app name — name an app in this space whose address this one should join", r.App)
		}
		if r.App == c.Name {
			return fmt.Errorf("routes: an app cannot join its own address (%q)", r.App)
		}
		if !strings.HasPrefix(r.Path, "/") || r.Path == "/" {
			return fmt.Errorf("routes: path %q must be a prefix below the root, e.g. /api", r.Path)
		}
		key := r.App + r.Path
		if seenRoutes[key] {
			return fmt.Errorf("routes: %s%s is listed twice", r.App, r.Path)
		}
		seenRoutes[key] = true
	}
	if err := c.Storage.validate(); err != nil {
		return err
	}
	if c.HasStorage() && c.Replicas != nil && *c.Replicas > 1 {
		return fmt.Errorf("an app with storage runs as a single copy — its disk can only be attached to one machine at a time (set replicas: 1, or drop storage.paths)")
	}
	switch c.Build.Type {
	case BuildAuto, BuildDockerfile, BuildAI:
	default:
		return fmt.Errorf("build.type %q is not one of auto, ai, dockerfile", c.Build.Type)
	}
	if c.Replicas != nil && *c.Replicas < 0 {
		return fmt.Errorf("replicas cannot be negative")
	}
	if p := c.Healthcheck.Path; p != "" && !strings.HasPrefix(p, "/") {
		return fmt.Errorf("healthcheck.path must start with a slash, e.g. /healthz")
	}
	if err := validateHost(c.Host); err != nil {
		return err
	}
	seen := map[string]bool{}
	for _, a := range c.Addons {
		if a.Type == "" {
			return fmt.Errorf("addon %q needs a type (e.g. postgres)", a.Name)
		}
		if err := ValidateServiceName(a.Name); err != nil {
			return err
		}
		if seen[a.Name] {
			return fmt.Errorf("two addons share the name %q — give them distinct names", a.Name)
		}
		seen[a.Name] = true
	}
	for key, value := range c.Env {
		if service, _, ref := ParseEnvRef(value); ref && (!envNameRE.MatchString(key) || !seen[service]) {
			return fmt.Errorf("env.%s references undeclared addon %s", key, service)
		}
		if _, _, ref := ParseEnvRef(value); ref {
			if _, baked := c.Build.Env[key]; baked {
				return fmt.Errorf("remove service variable %s from build.env; it is resolved at runtime", key)
			}
			if PublicEnv(key) {
				return fmt.Errorf("env.%s cannot expose a service binding to the browser", key)
			}
		}
	}
	for key, value := range c.Build.Env {
		if _, _, ref := ParseEnvRef(value); ref {
			return fmt.Errorf("build.env.%s cannot contain a runtime service reference", key)
		}
	}
	return nil
}

var envNameRE = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]*$`)

// PublicEnv identifies framework variables that are exposed to browser code.
func PublicEnv(key string) bool {
	for _, prefix := range []string{"VITE_", "NEXT_PUBLIC_", "PUBLIC_", "REACT_APP_", "NUXT_PUBLIC_"} {
		if strings.HasPrefix(key, prefix) {
			return true
		}
	}
	return false
}

// HasStorage reports whether the app asked for persistent disk.
func (c *AppConfig) HasStorage() bool {
	return c.Storage != nil && len(c.Storage.Paths) > 0
}

func (s *StorageConfig) validate() error {
	if s == nil {
		return nil
	}
	if s.Size != "" && !sizeRE.MatchString(s.Size) {
		return fmt.Errorf("storage.size %q is not a size — write it like 5Gi or 500Mi", s.Size)
	}
	seen := map[string]string{}
	for _, p := range s.Paths {
		clean := path.Clean(p)
		if !path.IsAbs(clean) || clean == "/" {
			return fmt.Errorf("storage path %q must be an absolute folder inside the container, e.g. /app/data", p)
		}
		sub := StorageSubPath(clean)
		if other, dup := seen[sub]; dup {
			return fmt.Errorf("storage paths %q and %q would share the same folder on the disk — rename one", other, p)
		}
		seen[sub] = p
	}
	return nil
}

// StorageSubPath is the folder a mounted path gets on the app's single
// volume. It must stay stable across deploys — data lives there — so it is a
// pure function of the path: /app/data → app-data.
func StorageSubPath(p string) string {
	slug := subPathCleanRE.ReplaceAllString(strings.ToLower(strings.Trim(path.Clean(p), "/")), "-")
	slug = strings.Trim(dashRunRE.ReplaceAllString(slug, "-"), "-")
	if slug == "" {
		return "data"
	}
	return slug
}

var (
	subPathCleanRE = regexp.MustCompile(`[^a-z0-9]+`)
	dashRunRE      = regexp.MustCompile(`-{2,}`)
)

// Save writes donkey.yaml into dir.
func (c *AppConfig) Save(dir string) error {
	b, err := c.Render()
	if err != nil {
		return err
	}
	return os.WriteFile(filepath.Join(dir, AppFileName), b, 0o644)
}

// Render is the donkey.yaml this config amounts to, defaults stripped. The
// console shows it for copying into the project — a browser deploy has no
// folder to write back to.
func (c *AppConfig) Render() ([]byte, error) {
	out := *c
	if out.Build.Type == BuildAuto {
		out.Build.Type = ""
	}
	if out.Storage != nil {
		storage := *out.Storage
		if storage.Size == DefaultStorage {
			storage.Size = ""
		}
		out.Storage = &storage
	}
	if out.Replicas != nil && *out.Replicas == DefaultReplicas {
		out.Replicas = nil
	}
	if out.Resources != nil &&
		len(out.Resources.Requests) == 2 &&
		out.Resources.Requests["cpu"] == defaultCPURequest &&
		out.Resources.Requests["memory"] == defaultMemRequest &&
		len(out.Resources.Limits) == 0 {
		out.Resources = nil
	}
	out.Addons = make([]AddonConfig, len(c.Addons))
	copy(out.Addons, c.Addons)
	for i := range out.Addons {
		if out.Addons[i].Name == out.Addons[i].Type {
			out.Addons[i].Name = ""
		}
	}
	b, err := yaml.Marshal(&out)
	if err != nil {
		return nil, err
	}
	// Empty struct fields marshal as "field: {}" noise — drop those lines.
	var lines []string
	for _, line := range strings.Split(string(b), "\n") {
		if strings.HasSuffix(line, ": {}") && !strings.HasPrefix(line, " ") {
			continue
		}
		lines = append(lines, line)
	}
	return []byte(strings.Join(lines, "\n")), nil
}
