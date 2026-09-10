package config

import (
	"fmt"
	"github.com/adrianliechti/donkey/internal/identity"
	"strings"
)

// SpaceConfig grants management rights to signed-in identities. Empty lists
// grant nobody a role; platform admins always retain access.
// App visitor access is configured separately in AppConfig.Access.
type SpaceConfig struct {
	Admins     AccessConfig `json:"admins" yaml:"admins"`
	Developers AccessConfig `json:"developers" yaml:"developers"`
	// Domains lists domain suffixes. Each suffix admits itself and its
	// subdomains; an empty list allows any hostname.
	Domains []string `json:"domains" yaml:"domains"`
}

func DefaultSpaceConfig(space, domain string) SpaceConfig {
	policy := SpaceConfig{Domains: []string{}}
	if domain != "" {
		policy.Domains = []string{space + "." + domain}
	}
	return policy
}

const (
	SpaceAdmin     = "admin"
	SpaceDeveloper = "developer"
)

func (a SpaceConfig) Role(id *identity.Identity) string {
	if managementMember(a.Admins, id) {
		return SpaceAdmin
	}
	if managementMember(a.Developers, id) {
		return SpaceDeveloper
	}
	return ""
}

func (a SpaceConfig) Validate() error {
	if len(a.Domains) > 64 {
		return fmt.Errorf("domains supports at most 64 domain suffixes")
	}
	for _, domain := range a.Domains {
		if len(domain) > 253 || !domainRE.MatchString(domain) {
			return fmt.Errorf("allowed domain %q must be a lowercase domain such as team.example.com, without a wildcard, scheme, port or path", domain)
		}
	}
	for name, members := range map[string]*AccessConfig{"admins": &a.Admins, "developers": &a.Developers} {
		if err := members.Validate(); err != nil {
			return fmt.Errorf("%s: %w", name, err)
		}
		if len(members.Users)+len(members.Groups)+len(members.Roles) > 256 {
			return fmt.Errorf("%s supports at most 256 entries", name)
		}
		for _, list := range [][]string{members.Users, members.Groups, members.Roles} {
			for _, entry := range list {
				if len(entry) > 256 {
					return fmt.Errorf("%s entries must be at most 256 bytes", name)
				}
			}
		}
	}
	return nil
}

func (a SpaceConfig) CheckHost(host string) error {
	if host == "" || len(a.Domains) == 0 {
		return nil
	}
	for _, domain := range a.Domains {
		if host == domain || strings.HasSuffix(host, "."+domain) {
			return nil
		}
	}
	return fmt.Errorf("hostname %q is not allowed in this space", host)
}

func (a SpaceConfig) CheckAppHost(app *AppConfig, space, domain string) error {
	if !app.Internal && a.Domains != nil && len(a.Domains) == 0 &&
		!strings.Contains(app.Host, ".") {
		return fmt.Errorf("this space allows any domain — enter the full app hostname, such as api.example.com")
	}
	return a.CheckHost(AppHost(app, space, domain))
}
