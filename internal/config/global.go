package config

import (
	"crypto/tls"
	"crypto/x509"
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

// ResolveToken returns the API token: $DONKEY_TOKEN wins over the config file.
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

// Password resolves the registry credential: $DONKEY_REGISTRY_PASSWORD wins,
// then PasswordFile. Empty result means anonymous / pre-authenticated registry.
func (r Registry) Password() (string, error) {
	if p := os.Getenv("DONKEY_REGISTRY_PASSWORD"); p != "" {
		return p, nil
	}
	if r.PasswordFile == "" {
		return "", nil
	}
	b, err := os.ReadFile(r.PasswordFile)
	if err != nil {
		return "", fmt.Errorf("reading registry password file: %w", err)
	}
	return strings.TrimSpace(string(b)), nil
}

const (
	DefaultNamespacePrefix = "donkey-"
	DefaultAIModel         = "gpt-5.5"
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
