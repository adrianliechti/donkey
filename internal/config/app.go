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
	// Space groups apps that belong together — typically everything in one
	// repository. A space is one Kubernetes namespace (apps inside reach each
	// other by plain name) and one level of the address: <app>.<space>.<domain>.
	// Empty means "work it out from the folder" (see ResolveSpace).
	Space string `json:"space,omitempty"`
	Port  int    `json:"port"`
	// Public opts the app out of the login gateway: anyone can reach it.
	Public bool `json:"public,omitempty"`
	// Internal keeps the app off the gateway altogether: no hostname, no
	// certificate, no login — only the apps of its own space reach it, by
	// name. This is what a backend behind a frontend wants: one address for
	// the whole thing, and nothing else on the internet.
	Internal bool        `json:"internal,omitempty"`
	Replicas *int32      `json:"replicas,omitempty"`
	Build    BuildConfig `json:"build,omitempty"`
	Command  []string    `json:"command,omitempty"`
	// PreDeploy runs once against the new image before the new version starts
	// serving — database migrations, above all. It has the app's env, its
	// addon bindings and its disk. If it fails, the deploy stops and the
	// version already running keeps serving.
	PreDeploy []string `json:"preDeploy,omitempty"`
	// Routes put this app on another app's hostname under a path prefix, so
	// a frontend and its API can share one address.
	Routes      []RouteConfig     `json:"routes,omitempty"`
	Healthcheck Healthcheck       `json:"healthcheck,omitempty"`
	Env         map[string]string `json:"env,omitempty"`
	Addons      []AddonConfig     `json:"addons,omitempty"`
	Storage     *StorageConfig    `json:"storage,omitempty"`
	Domain      string            `json:"domain,omitempty"`
	// Access narrows the login gateway from "anyone your IdP knows" to the
	// people and groups named here. Omitted means every signed-in user.
	Access    *AccessConfig `json:"access,omitempty"`
	Resources *Resources    `json:"resources,omitempty"`
}

// AccessConfig says who may open a login-protected app. It refines the login
// gateway, so it means nothing on a `public` app — and it is enforced with
// the claims of the IdP's ID token, which is where emails, groups and roles
// live. A visitor passes by matching ANY of the lists.
type AccessConfig struct {
	// Users are exact email addresses (the IdP's `email` claim).
	Users []string `json:"users,omitempty"`
	// Groups match against the IdP's `groups` claim.
	Groups []string `json:"groups,omitempty"`
	// Roles match against the IdP's `roles` claim (Entra app roles) or
	// Keycloak's realm roles.
	Roles []string `json:"roles,omitempty"`
}

// Empty reports whether the access config restricts nobody — the same as not
// having one.
func (a *AccessConfig) Empty() bool {
	return a == nil || (len(a.Users) == 0 && len(a.Groups) == 0 && len(a.Roles) == 0)
}

// Allows reports whether an identity satisfies the rules — by matching any
// of them; empty rules allow every signed-in identity. Emails compare
// case-insensitively (IdPs disagree on casing); groups and roles compare
// exactly (they are identifiers, often GUIDs).
func (a *AccessConfig) Allows(email string, groups, roles []string) bool {
	if a.Empty() {
		return true
	}
	for _, u := range a.Users {
		if email != "" && strings.EqualFold(u, email) {
			return true
		}
	}
	return anyOverlap(a.Groups, groups) || anyOverlap(a.Roles, roles)
}

func anyOverlap(wanted, have []string) bool {
	if len(wanted) == 0 || len(have) == 0 {
		return false
	}
	mine := make(map[string]bool, len(have))
	for _, v := range have {
		mine[v] = true
	}
	for _, v := range wanted {
		if mine[v] {
			return true
		}
	}
	return false
}

// accessEntryRE keeps user and group entries to things that can appear in a
// JWT claim comparison: no whitespace, no quotes, printable ASCII.
var accessEntryRE = regexp.MustCompile(`^[\x21-\x7e]+$`)

// Validate is exported because the console submits access rules on their
// own, outside a whole donkey.yaml.
func (a *AccessConfig) Validate() error {
	if a == nil {
		return nil
	}
	for _, u := range a.Users {
		if !accessEntryRE.MatchString(u) {
			return fmt.Errorf("access.users entry %q must be a plain email address without spaces", u)
		}
	}
	for _, g := range a.Groups {
		if !accessEntryRE.MatchString(g) {
			return fmt.Errorf("access.groups entry %q must be a plain group name without spaces", g)
		}
	}
	for _, r := range a.Roles {
		if !accessEntryRE.MatchString(r) {
			return fmt.Errorf("access.roles entry %q must be a plain role name without spaces", r)
		}
	}
	return nil
}

// StorageConfig asks for disk that survives restarts and redeploys. donkey
// creates exactly ONE volume per app and gives every path its own folder on
// it (a subPath mount), so an app can keep several data directories without
// anyone having to think about several disks.
//
// A nil Storage means "nobody decided yet" — the assistant may then propose
// paths. An empty (but present) paths list means "this app needs no disk"
// and is left alone.
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
	BuildRailpack   BuildStrategy = "railpack"
	BuildDockerfile BuildStrategy = "dockerfile"
)

type BuildConfig struct {
	Type BuildStrategy `json:"type,omitempty"`
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
	Path string `json:"path"`
}

type AddonConfig struct {
	// Name is optional in donkey.yaml (it defaults to the type), so Save
	// must be able to drop it again — omitempty keeps a minimized entry
	// from serializing as `name: ""`.
	Name     string `json:"name,omitempty"`
	Type     string `json:"type"`
	External bool   `json:"external,omitempty"`
	Version  string `json:"version,omitempty"`
	Storage  string `json:"storage,omitempty"`
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
	defaultMemLimit   = "512Mi"
)

// nameRE follows RFC 1123 DNS labels: app and space names become part of
// namespace, hostnames and object names.
var nameRE = regexp.MustCompile(`^[a-z0-9]([a-z0-9-]{0,38}[a-z0-9])?$`)

// ValidName reports whether s is a name donkey accepts for apps, spaces and
// addons. The server checks URL path values against it — they feed namespace,
// hostname and object names, so they follow donkey.yaml's rules.
func ValidName(s string) bool { return nameRE.MatchString(s) }

// domainRE is a plain DNS hostname of at least two labels. The value becomes
// a route hostname, a name on a certificate and part of the login callback
// URL — none of which accept a wildcard, a scheme, a port or a path, and all
// of which are lowercase.
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
			Limits:   map[string]string{"memory": defaultMemLimit},
		}
	}
	for i := range c.Addons {
		if c.Addons[i].Name == "" {
			c.Addons[i].Name = c.Addons[i].Type
		}
	}
}

func (c *AppConfig) Validate() error {
	if c.Name == "" {
		return fmt.Errorf("name is required")
	}
	if !nameRE.MatchString(c.Name) {
		return fmt.Errorf("name %q must be lowercase letters, digits and dashes (max 40 chars), e.g. %q", c.Name, "my-app")
	}
	if c.Space != "" && !nameRE.MatchString(c.Space) {
		return fmt.Errorf("space %q must be lowercase letters, digits and dashes (max 40 chars), e.g. %q", c.Space, "acme")
	}
	if c.Port < 1 || c.Port > 65535 {
		return fmt.Errorf("port must be between 1 and 65535 (the port your app listens on)")
	}
	// `internal` says "no address of my own"; `public` says "no login on the
	// addresses I do have". Together they are only contradictory when the app
	// has no routes either — then there is nothing for `public` to open up.
	if c.Internal && c.Public && len(c.Routes) == 0 {
		return fmt.Errorf("an internal app has no address, so `public: true` has nothing to open — drop it, or give the app a `routes:` entry on another app's address")
	}
	if c.Internal && c.Domain != "" {
		return fmt.Errorf("an internal app has no address, so `domain` has no effect — drop one of the two")
	}
	if !c.Access.Empty() {
		if c.Public {
			return fmt.Errorf("`access` needs the login gateway, which `public: true` turns off — drop one of the two")
		}
		if c.Internal && len(c.Routes) == 0 {
			return fmt.Errorf("an internal app has no address, so `access` has nobody to let in — drop it, or give the app a `routes:` entry")
		}
	}
	if err := c.Access.Validate(); err != nil {
		return err
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
	case BuildAuto, BuildRailpack, BuildDockerfile:
	default:
		return fmt.Errorf("build.type %q is not one of auto, railpack, dockerfile", c.Build.Type)
	}
	if c.Replicas != nil && *c.Replicas < 0 {
		return fmt.Errorf("replicas cannot be negative")
	}
	if p := c.Healthcheck.Path; p != "" && !strings.HasPrefix(p, "/") {
		return fmt.Errorf("healthcheck.path must start with a slash, e.g. /healthz")
	}
	// A wildcard would ask cert-manager for a certificate no HTTP-01 solver
	// can issue, and would hand the app every hostname under it.
	if d := c.Domain; d != "" && (!domainRE.MatchString(d) || len(d) > 253) {
		return fmt.Errorf("domain %q must be a plain hostname like %q — lowercase, no wildcard, no https:// and no path", d, "app.example.com")
	}
	seen := map[string]bool{}
	for _, a := range c.Addons {
		if a.Type == "" {
			return fmt.Errorf("addon %q needs a type (e.g. postgres)", a.Name)
		}
		if !nameRE.MatchString(a.Name) {
			return fmt.Errorf("addon name %q must be lowercase letters, digits and dashes (max 40 chars)", a.Name)
		}
		if seen[a.Name] {
			return fmt.Errorf("two addons share the name %q — give them distinct names", a.Name)
		}
		seen[a.Name] = true
	}
	return nil
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

// Save writes donkey.yaml into dir, stripping values that equal the defaults
// so the file stays as small as the user wrote it.
func (c *AppConfig) Save(dir string) error {
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
		len(out.Resources.Limits) == 1 &&
		out.Resources.Limits["memory"] == defaultMemLimit {
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
		return err
	}
	// Empty struct fields marshal as "field: {}" noise — drop those lines.
	var lines []string
	for _, line := range strings.Split(string(b), "\n") {
		if strings.HasSuffix(line, ": {}") && !strings.HasPrefix(line, " ") {
			continue
		}
		lines = append(lines, line)
	}
	return os.WriteFile(filepath.Join(dir, AppFileName), []byte(strings.Join(lines, "\n")), 0o644)
}
