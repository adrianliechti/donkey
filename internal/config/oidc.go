package config

import (
	"encoding/hex"
	"fmt"
	"net"
	"net/url"
	"strings"
)

// OIDCClient registers this app as a relying party of the gateway. Only a
// digest of a confidential client's randomly generated secret belongs in
// donkey.yaml; the secret itself belongs in the app's secret environment.
type OIDCClient struct {
	RedirectURIs       []string `json:"redirectURIs" yaml:"redirectURIs"`
	ClientSecretSHA256 string   `json:"clientSecretSHA256,omitempty" yaml:"clientSecretSHA256,omitempty"`
}

func OIDCClientID(space, app string) string { return "donkey_web_" + space + "_" + app }

func (c *OIDCClient) Validate() error {
	if c == nil {
		return nil
	}
	if len(c.RedirectURIs) == 0 || len(c.RedirectURIs) > 20 {
		return fmt.Errorf("oidc.redirectURIs must contain between 1 and 20 exact callback URLs")
	}
	seen := map[string]bool{}
	for _, raw := range c.RedirectURIs {
		u, err := url.Parse(raw)
		if err != nil || len(raw) > 2048 || u.Host == "" || u.Hostname() == "" || u.User != nil || strings.Contains(raw, "#") || strings.ContainsAny(raw, "*\r\n\t ") {
			return fmt.Errorf("oidc.redirectURIs contains an invalid callback URL: %q", raw)
		}
		if strings.HasPrefix(u.Path, "/oauth2/") {
			return fmt.Errorf("oidc.redirectURIs must use an app callback outside the gateway's reserved /oauth2/ paths")
		}
		ip := net.ParseIP(u.Hostname())
		loopback := u.Hostname() == "localhost" || ip != nil && ip.IsLoopback()
		if u.Scheme != "https" && !(u.Scheme == "http" && loopback) {
			return fmt.Errorf("oidc.redirectURIs must use HTTPS (HTTP is allowed on loopback for development)")
		}
		if seen[raw] {
			return fmt.Errorf("oidc.redirectURIs contains a duplicate callback URL")
		}
		seen[raw] = true
	}
	if c.ClientSecretSHA256 != "" {
		if digest, err := hex.DecodeString(c.ClientSecretSHA256); err != nil || len(digest) != 32 {
			return fmt.Errorf("oidc.clientSecretSHA256 must be the 64-character hex SHA-256 digest of a random client secret")
		}
	}
	return nil
}
