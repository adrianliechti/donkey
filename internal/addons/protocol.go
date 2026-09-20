package addons

import (
	"fmt"
	"net/url"
	"regexp"
)

const (
	ProtocolPostgres = "postgres"
	ProtocolMySQL    = "mysql"
)

// Metadata describes the service independently of who provisions it. It
// must stay outside the environment binding injected into applications.
type Metadata struct {
	Type     string `json:"type,omitempty"`
	Protocol string `json:"protocol,omitempty"`
}

func MetadataFor(a Addon) Metadata {
	return Metadata{Type: a.Type(), Protocol: a.Protocol()}
}

var protocolName = regexp.MustCompile(`^[a-z][a-z0-9-]{0,62}$`)

// Unknown protocols are allowed; clients simply offer no specialized UI.
func ValidateProtocol(protocol string) error {
	if protocol != "" && !protocolName.MatchString(protocol) {
		return fmt.Errorf("protocol must be a lowercase name, for example postgres or mysql")
	}
	return nil
}

func NormalizeProtocol(protocol string) string {
	switch protocol {
	case "postgresql":
		return ProtocolPostgres
	case "mariadb":
		return ProtocolMySQL
	default:
		return protocol
	}
}

// LegacyProtocol upgrades bindings saved before explicit metadata existed.
// Explicit metadata always wins. DATABASE_URL alone is not a protocol.
func LegacyProtocol(binding map[string]string) string {
	if raw := binding["DATABASE_URL"]; raw != "" {
		if u, err := url.Parse(raw); err == nil {
			switch p := NormalizeProtocol(u.Scheme); p {
			case ProtocolPostgres, ProtocolMySQL:
				return p
			}
		}
	}
	for _, entry := range []struct{ key, protocol string }{
		{"REDIS_URL", "redis"}, {"AWS_ENDPOINT_URL_S3", "s3"}, {"OPENAI_BASE_URL", "openai"},
	} {
		if binding[entry.key] != "" {
			return entry.protocol
		}
	}
	return ""
}
