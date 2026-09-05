package config

import (
	"crypto/tls"
	"crypto/x509"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"sigs.k8s.io/yaml"
)

// GlobalConfig is the client-side configuration at ~/.config/donkey/config.yaml.
// Since donkey moved its control plane server-side, the client only needs to
// know its server. KubeContext exists solely for the one-time
// `donkey server install` bootstrap (and re-runs of it).
type GlobalConfig struct {
	Server      ServerConfig `json:"server,omitempty"`
	KubeContext string       `json:"kubeContext,omitempty"`
}

type ServerConfig struct {
	// URL is the donkey-server API address, e.g. https://donkey.apps.example.com.
	URL string `json:"url,omitempty"`
	// Token authenticates the CLI. $DONKEY_TOKEN overrides.
	Token string `json:"token,omitempty"`
	// CAFile is a PEM file to trust on top of the system roots — the usual
	// case for an installation whose certificates come from a private CA.
	CAFile string `json:"caFile,omitempty"`
}

// ResolveToken returns the CLI's sealed login token: $DONKEY_TOKEN wins over
// the config file.
func (s ServerConfig) ResolveToken() string {
	if t := os.Getenv("DONKEY_TOKEN"); t != "" {
		return t
	}
	return s.Token
}

// Registry describes an image registry. It is server-side configuration now,
// but the type is shared by the build pipeline and manifests.
type Registry struct {
	// URL is the image prefix, e.g. "registry.example.com/donkey".
	URL          string `json:"url"`
	Username     string `json:"username,omitempty"`
	PasswordFile string `json:"passwordFile,omitempty"`
	// DockerConfigDir contains one or more dockerconfigjson files. It is read
	// for the push registry and for distinct BuildKit mirror hosts; projected
	// Kubernetes Secret updates are picked up without restarting the server.
	DockerConfigDir string `json:"dockerConfigDir,omitempty"`
	// Insecure marks a plain-HTTP registry (local/testing setups only).
	Insecure bool `json:"insecure,omitempty"`
	// Immutable marks a registry that refuses to overwrite an existing tag
	// (ECR tag immutability, GHCR immutable tags, a Harbor rule). Donkey then
	// makes every image tag unique instead of letting two deploys of one
	// commit in the same second collide — such a push would be rejected.
	Immutable bool `json:"immutable,omitempty"`
	// CAFile is a PEM bundle to trust for this registry — the usual case for
	// an in-cluster or corporate registry whose certificate comes from a
	// private CA. Server-side; the file is mounted into donkey-server and the
	// build engine.
	CAFile string `json:"caFile,omitempty"`
}

// TLSConfig returns the transport settings for talking to this registry.
func (r Registry) TLSConfig() (*tls.Config, error) {
	if r.CAFile == "" {
		return nil, nil
	}
	pem, err := os.ReadFile(r.CAFile)
	if err != nil {
		return nil, fmt.Errorf("reading the registry's certificate authority: %w", err)
	}
	pool, err := x509.SystemCertPool()
	if err != nil {
		pool = x509.NewCertPool()
	}
	if !pool.AppendCertsFromPEM(pem) {
		return nil, fmt.Errorf("%s doesn't contain a usable certificate (expected PEM)", r.CAFile)
	}
	return &tls.Config{RootCAs: pool}, nil
}

// Host returns the registry host part of URL (everything before the first slash).
func (r Registry) Host() string {
	host, _, _ := strings.Cut(r.URL, "/")
	return host
}

// Validate rejects registry URLs that cannot work as image prefixes: an OCI
// reference carries no scheme and allows no uppercase — with one, every
// build would fail with a cryptic push error long after install.
func (r Registry) Validate() error {
	if r.URL == "" {
		return nil
	}
	if strings.Contains(r.URL, "://") {
		return fmt.Errorf("registry.url %q must not have a scheme — write it like registry.example.com/donkey (https is implied unless registry.insecure is set)", r.URL)
	}
	if r.URL != strings.ToLower(r.URL) {
		return fmt.Errorf("registry.url %q must be lowercase — image references don't allow uppercase letters", r.URL)
	}
	return nil
}

// Password preserves the password-only interface used by callers that do not
// need a username derived from a Docker config file.
func (r Registry) Password() (string, error) {
	_, password, err := r.Credentials()
	return password, err
}

// Credentials resolves the registry credential on demand, so a mounted Secret
// can rotate without restarting donkey-server. Explicit env/file credentials
// retain precedence over a Docker config file.
func (r Registry) Credentials() (string, string, error) {
	if p := os.Getenv("DONKEY_REGISTRY_PASSWORD"); p != "" {
		return r.Username, p, nil
	}
	if r.PasswordFile != "" {
		b, err := os.ReadFile(r.PasswordFile)
		if err != nil {
			return "", "", fmt.Errorf("reading registry password file: %w", err)
		}
		return r.Username, strings.TrimSpace(string(b)), nil
	}
	if r.DockerConfigDir == "" {
		return r.Username, "", nil
	}
	username, password, found, err := DockerConfigDirCredentials(r.DockerConfigDir, r.Host())
	if err != nil {
		return "", "", err
	}
	if !found {
		return "", "", fmt.Errorf("docker config directory %s has no credentials for %s", r.DockerConfigDir, r.Host())
	}
	return username, password, nil
}

// DockerConfigDirCredentials scans mounted Docker config files and returns the
// entry matching host. Kubernetes updates projected Secret files in place, so
// callers see rotated credentials without a pod restart.
func DockerConfigDirCredentials(dir, host string) (string, string, bool, error) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return "", "", false, fmt.Errorf("reading docker config directory %s: %w", dir, err)
	}
	for _, entry := range entries {
		// Kubernetes projected volumes contain implementation symlinks such as
		// ..data. The visible Secret keys are symlinks too, so use os.Stat to
		// follow them and accept only regular files.
		if strings.HasPrefix(entry.Name(), "..") {
			continue
		}
		path := filepath.Join(dir, entry.Name())
		info, err := os.Stat(path)
		if err != nil {
			return "", "", false, fmt.Errorf("reading docker config %s: %w", path, err)
		}
		if !info.Mode().IsRegular() {
			continue
		}
		username, password, found, err := DockerConfigCredentials(path, host)
		if err != nil {
			return "", "", false, err
		}
		if found {
			return username, password, true, nil
		}
	}
	return "", "", false, nil
}

// DockerConfigCredentials reads one registry's credentials from a standard
// kubernetes.io/dockerconfigjson file.
func DockerConfigCredentials(path, host string) (string, string, bool, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return "", "", false, fmt.Errorf("reading docker config %s: %w", path, err)
	}
	return DockerConfigDataCredentials(b, path, host)
}

// DockerConfigDataCredentials resolves one auths entry from Docker config
// JSON already supplied by a Kubernetes Secret or another trusted source.
func DockerConfigDataCredentials(b []byte, source, host string) (string, string, bool, error) {
	var cfg struct {
		Auths map[string]struct {
			Username string `json:"username"`
			Password string `json:"password"`
			Auth     string `json:"auth"`
		} `json:"auths"`
	}
	if err := json.Unmarshal(b, &cfg); err != nil {
		return "", "", false, fmt.Errorf("reading docker config %s: %w", source, err)
	}
	want := registryConfigHost(host)
	for address, auth := range cfg.Auths {
		if registryConfigHost(address) != want {
			continue
		}
		if auth.Username != "" || auth.Password != "" {
			if auth.Username == "" || auth.Password == "" {
				return "", "", false, fmt.Errorf("credentials for %s in %s need both username and password", host, source)
			}
			return auth.Username, auth.Password, true, nil
		}
		decoded, err := base64.StdEncoding.DecodeString(auth.Auth)
		if err != nil {
			return "", "", false, fmt.Errorf("decoding credentials for %s in %s: %w", host, source, err)
		}
		username, password, ok := strings.Cut(string(decoded), ":")
		if !ok {
			return "", "", false, fmt.Errorf("credentials for %s in %s are not username:password", host, source)
		}
		return username, password, true, nil
	}
	return "", "", false, nil
}

func registryConfigHost(address string) string {
	address = strings.TrimSpace(address)
	if _, rest, ok := strings.Cut(address, "://"); ok {
		address = rest
	}
	host, _, _ := strings.Cut(strings.Trim(address, "/"), "/")
	if host == "registry-1.docker.io" || host == "index.docker.io" {
		return "docker.io"
	}
	return strings.ToLower(host)
}

const (
	DefaultNamespacePrefix = "donkey-"
	// DefaultSystemNamespace is where the control plane lives unless an
	// installation says otherwise. It doubles as the installation's identity,
	// so two donkeys on one cluster cannot both keep it.
	DefaultSystemNamespace = "donkey-system"
	DefaultAIModel         = "gpt-5.6-sol"
)

// DefaultGlobalPath returns ~/.config/donkey/config.yaml. We deliberately use
// ~/.config on every platform (not os.UserConfigDir) so docs and support
// answers are the same everywhere.
func DefaultGlobalPath() (string, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(home, ".config", "donkey", "config.yaml"), nil
}

// LoadGlobal reads the global config. A missing file returns (nil, nil) so
// callers can distinguish "not set up yet" from a broken file.
func LoadGlobal(path string) (*GlobalConfig, error) {
	b, err := os.ReadFile(path)
	if os.IsNotExist(err) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var c GlobalConfig
	if err := yaml.UnmarshalStrict(b, &c); err != nil {
		return nil, fmt.Errorf("%s is not valid: %w", path, err)
	}
	if err := c.Validate(); err != nil {
		return nil, fmt.Errorf("%s is not valid: %w", path, err)
	}
	return &c, nil
}

func (c *GlobalConfig) Validate() error {
	if u := c.Server.URL; u != "" && !strings.HasPrefix(u, "http://") && !strings.HasPrefix(u, "https://") {
		return fmt.Errorf("server.url must start with http:// or https://")
	}
	return nil
}

// Save writes the config with private permissions, creating the directory.
func (c *GlobalConfig) Save(path string) error {
	b, err := yaml.Marshal(c)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}
	return os.WriteFile(path, b, 0o600)
}
