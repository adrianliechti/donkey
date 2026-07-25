package config

import (
	"fmt"
	"os"

	"sigs.k8s.io/yaml"
)

// InstallFile is the values file for `donkey server install -f server.yaml`:
// everything an admin configures about a donkey installation. Flags override
// individual fields; secrets can come from env instead
// (DONKEY_REGISTRY_PASSWORD, DONKEY_OIDC_CLIENT_SECRET, OPENAI_API_KEY).
type InstallFile struct {
	// Image is the donkey-server image, pullable by the cluster.
	Image    string          `json:"image,omitempty"`
	Registry InstallRegistry `json:"registry,omitempty"`
	// BaseDomain: apps become <app>.<space>.<baseDomain>.
	BaseDomain string     `json:"baseDomain,omitempty"`
	TLS        InstallTLS `json:"tls,omitempty"`
	// DefaultPlatform is the build platform of the cluster's nodes
	// (default linux/amd64).
	DefaultPlatform string `json:"defaultPlatform,omitempty"`

	Auth InstallAuth `json:"auth,omitempty"`
	AI   InstallAI   `json:"ai,omitempty"`
}

// InstallRegistry describes the registry app images are pushed to. CASecret
// covers the common in-house case: a registry whose certificate comes from
// the cluster's own CA, which the server and the build engine must trust.
type InstallRegistry struct {
	Registry
	// CASecret is a secret in donkey-system holding ca.crt.
	CASecret string `json:"caSecret,omitempty"`
}

// InstallAuth configures the login gateway. With an empty issuer (and no
// --demo), no gateway is installed and apps are reachable without login.
type InstallAuth struct {
	Issuer       string `json:"issuer,omitempty"`
	ClientID     string `json:"clientId,omitempty"`
	ClientSecret string `json:"clientSecret,omitempty"`
}

// InstallTLS names the cert-manager issuer that signs app certificates.
// donkey always serves apps over https — the login gateway's cookies require
// it — and always asks cert-manager for one certificate per app hostname, so
// any solver works (HTTP-01 included, which no wildcard could use).
type InstallTLS struct {
	// Issuer is the cert-manager issuer's name.
	Issuer string `json:"issuer,omitempty"`
	// Kind is ClusterIssuer (default) or Issuer.
	Kind string `json:"kind,omitempty"`
}

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
