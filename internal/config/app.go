package config

import (
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"

	"sigs.k8s.io/yaml"
)

// AppFileName is the per-app config file, committed to the repo. No secrets.
const AppFileName = "donkey.yaml"

type AppConfig struct {
	Name string `json:"name"`
	Port int    `json:"port"`
	// Public opts the app out of the login gateway: anyone can reach it.
	Public   bool   `json:"public,omitempty"`
	Replicas *int32 `json:"replicas,omitempty"`
	Build       BuildConfig       `json:"build,omitempty"`
	Command     []string          `json:"command,omitempty"`
	Healthcheck Healthcheck       `json:"healthcheck,omitempty"`
	Env         map[string]string `json:"env,omitempty"`
	Addons      []AddonConfig     `json:"addons,omitempty"`
	Domain      string            `json:"domain,omitempty"`
	Resources   *Resources        `json:"resources,omitempty"`
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

type AddonConfig struct {
	Name     string `json:"name"`
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
	defaultCPURequest = "100m"
	defaultMemRequest = "128Mi"
	defaultMemLimit   = "512Mi"
)

// nameRE follows RFC 1123 DNS labels: the app name becomes part of namespace,
// hostnames and object names.
var nameRE = regexp.MustCompile(`^[a-z0-9]([a-z0-9-]{0,38}[a-z0-9])?$`)

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
	if c.Port < 1 || c.Port > 65535 {
		return fmt.Errorf("port must be between 1 and 65535 (the port your app listens on)")
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
	seen := map[string]bool{}
	for _, a := range c.Addons {
		if a.Type == "" {
			return fmt.Errorf("addon %q needs a type (e.g. postgres)", a.Name)
		}
		if seen[a.Name] {
			return fmt.Errorf("two addons share the name %q — give them distinct names", a.Name)
		}
		seen[a.Name] = true
	}
	return nil
}

// Save writes donkey.yaml into dir, stripping values that equal the defaults
// so the file stays as small as the user wrote it.
func (c *AppConfig) Save(dir string) error {
	out := *c
	if out.Build.Type == BuildAuto {
		out.Build.Type = ""
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
