package config

import (
	"fmt"
	"maps"
	"strings"
	"time"

	"github.com/adrianliechti/donkey/internal/identity"
)

// SpaceConfig controls app access and management rights. Empty lists grant
// nobody a role; platform admins always retain access.
type SpaceConfig struct {
	Viewers AccessConfig `json:"viewers" yaml:"viewers"`
	// AllowPublic permits apps to opt out of login. Off by default.
	AllowPublic bool         `json:"allowPublic" yaml:"allowPublic"`
	Admins      AccessConfig `json:"admins" yaml:"admins"`
	Developers  AccessConfig `json:"developers" yaml:"developers"`
	// Domains lists domain suffixes. Each suffix admits itself and its
	// subdomains; an empty list allows any hostname.
	Domains []string `json:"domains" yaml:"domains"`
	// Offerings adds to the common catalog and customizes offerings in this
	// space. Config is set by admins; Defaults may be overridden by apps.
	Offerings map[string]SpaceOffering `json:"offerings,omitempty" yaml:"offerings,omitempty"`
	// SecurityWaivers let named apps deploy despite a scanner finding that
	// would otherwise refuse them. They are a space admin's decision, kept
	// here rather than in donkey.yaml: a developer who can lift the gate
	// from the app's own config has no gate.
	SecurityWaivers []SecurityWaiver `json:"securityWaivers,omitempty" yaml:"securityWaivers,omitempty"`
}

// SecurityWaiver accepts one finding (a CVE or a secret rule) for one app,
// with the reason on record and an expiry so it is revisited.
type SecurityWaiver struct {
	App    string `json:"app" yaml:"app"`
	ID     string `json:"id" yaml:"id"`
	Reason string `json:"reason" yaml:"reason"`
	// By is who recorded it; the server fills it in from the sign-in.
	By string `json:"by,omitempty" yaml:"by,omitempty"`
	// Expires is a date (2026-11-24, valid through that day) or an RFC 3339
	// time. Empty never expires — allowed, but the console says so.
	Expires string `json:"expires,omitempty" yaml:"expires,omitempty"`
}

// ExpiresAt resolves the expiry; zero means never.
func (w SecurityWaiver) ExpiresAt() (time.Time, error) {
	if w.Expires == "" {
		return time.Time{}, nil
	}
	if t, err := time.Parse("2006-01-02", w.Expires); err == nil {
		// A date means "through that day".
		return t.Add(24 * time.Hour), nil
	}
	t, err := time.Parse(time.RFC3339, w.Expires)
	if err != nil {
		return time.Time{}, fmt.Errorf("expires %q must be a date (2026-11-24) or an RFC 3339 time", w.Expires)
	}
	return t, nil
}

// Active says whether the waiver still applies at the given time.
func (w SecurityWaiver) Active(now time.Time) bool {
	at, err := w.ExpiresAt()
	if err != nil {
		return false
	}
	return at.IsZero() || now.Before(at)
}

// WaiversFor is the app's waivers still in force.
func (a SpaceConfig) WaiversFor(app string, now time.Time) []SecurityWaiver {
	var out []SecurityWaiver
	for _, w := range a.SecurityWaivers {
		if w.App == app && w.Active(now) {
			out = append(out, w)
		}
	}
	return out
}

type SpaceOffering struct {
	Defaults map[string]any `json:"defaults,omitempty" yaml:"defaults,omitempty"`
	Config   map[string]any `json:"config,omitempty" yaml:"config,omitempty"`
}

func (a SpaceConfig) OfferingAllowed(common CommonOfferings, offering string) bool {
	offering = NormalizeAddonType(offering)
	if len(common) == 0 {
		return true
	}
	for _, id := range common {
		if NormalizeAddonType(id) == offering {
			return true
		}
	}
	_, enabled := a.Offering(offering)
	return enabled
}

// Offering also reads settings stored under the historical RustFS catalog id.
func (a SpaceConfig) Offering(id string) (SpaceOffering, bool) {
	id = NormalizeAddonType(id)
	entry, ok := a.Offerings[id]
	if !ok && id == "s3" {
		entry, ok = a.Offerings["rustfs"]
	}
	return entry, ok
}

// NormalizeOfferings exposes one catalog id while keeping saved defaults.
// Canonical settings take precedence if both ids were explicitly configured.
func (a SpaceConfig) NormalizeOfferings() SpaceConfig {
	if legacy, ok := a.Offerings["rustfs"]; ok {
		a.Offerings = maps.Clone(a.Offerings)
		if _, canonical := a.Offerings["s3"]; !canonical {
			a.Offerings["s3"] = legacy
		}
		delete(a.Offerings, "rustfs")
	}
	return a
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
	SpaceViewer    = "viewer"
)

func (a SpaceConfig) Role(id *identity.Identity) string {
	if managementMember(a.Admins, id) {
		return SpaceAdmin
	}
	if managementMember(a.Developers, id) {
		return SpaceDeveloper
	}
	if managementMember(a.Viewers, id) {
		return SpaceViewer
	}
	return ""
}

func (a SpaceConfig) Validate() error {
	if len(a.Offerings) > 256 {
		return fmt.Errorf("offerings supports at most 256 entries")
	}
	for id := range a.Offerings {
		if !ValidName(id) {
			return fmt.Errorf("offering %q must be a lowercase name", id)
		}
	}
	if len(a.Domains) > 64 {
		return fmt.Errorf("domains supports at most 64 domain suffixes")
	}
	for _, domain := range a.Domains {
		if len(domain) > 253 || !domainRE.MatchString(domain) {
			return fmt.Errorf("allowed domain %q must be a lowercase domain such as team.example.com, without a wildcard, scheme, port or path", domain)
		}
	}
	if len(a.SecurityWaivers) > 256 {
		return fmt.Errorf("securityWaivers supports at most 256 entries")
	}
	for i, w := range a.SecurityWaivers {
		if !ValidName(w.App) {
			return fmt.Errorf("securityWaivers[%d]: app %q must be a lowercase app name", i, w.App)
		}
		id := strings.TrimSpace(w.ID)
		if id == "" || len(id) > 128 || strings.ContainsAny(id, " \t\n") {
			return fmt.Errorf("securityWaivers[%d]: id must name one finding, such as CVE-2026-12345", i)
		}
		if strings.TrimSpace(w.Reason) == "" || len(w.Reason) > 500 {
			return fmt.Errorf("securityWaivers[%d]: a reason is required (up to 500 characters)", i)
		}
		if _, err := w.ExpiresAt(); err != nil {
			return fmt.Errorf("securityWaivers[%d]: %w", i, err)
		}
	}
	for name, members := range map[string]*AccessConfig{"admins": &a.Admins, "developers": &a.Developers, "viewers": &a.Viewers} {
		if err := members.Validate(); err != nil {
			return fmt.Errorf("%s: %w", name, err)
		}
		if len(members.Users)+len(members.Groups) > 256 {
			return fmt.Errorf("%s supports at most 256 entries", name)
		}
		for _, list := range [][]string{members.Users, members.Groups} {
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
