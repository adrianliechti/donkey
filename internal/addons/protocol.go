package addons

import (
	"fmt"
	"net/url"
	"regexp"

	addonv1 "github.com/adrianliechti/donkey/pkg/addons/v1"
)

const (
	ProtocolPostgres = "postgres"
	ProtocolMySQL    = "mysql"
	ProtocolRedis    = "redis"
	ProtocolS3       = "s3"
	ProtocolOpenAI   = "openai"
)

// Metadata describes the service independently of who provisions it. It
// must stay outside the environment binding injected into applications.
type Metadata struct {
	Type     string `json:"type,omitempty"`
	Protocol string `json:"protocol,omitempty"`
	// Parameters are the resolved values the instance was provisioned with.
	Parameters Parameters `json:"parameters,omitempty"`
}

// MetadataFor records a descriptor's identity and the parameters in use.
func MetadataFor(desc *addonv1.DescribeResponse, params Parameters) Metadata {
	return Metadata{Type: desc.GetType(), Protocol: NormalizeProtocol(desc.GetProtocol()), Parameters: params}
}

// Resolve preserves an explicit protocol, including an unknown one. Only
// bindings that predate metadata may infer their protocol from the environment.
func (m Metadata) Resolve(binding map[string]string) Metadata {
	m.Protocol = NormalizeProtocol(m.Protocol)
	if m.Protocol == "" {
		m.Protocol = LegacyProtocol(binding)
	}
	return m
}

// ConnectionKey is the environment contract used by a protocol's console.
// Providers may expose additional variables for their application clients.
func ConnectionKey(protocol string) string {
	switch NormalizeProtocol(protocol) {
	case ProtocolPostgres, ProtocolMySQL:
		return "DATABASE_URL"
	case ProtocolRedis:
		return "REDIS_URL"
	case ProtocolS3:
		return "AWS_ENDPOINT_URL_S3"
	case ProtocolOpenAI:
		return "OPENAI_BASE_URL"
	default:
		return ""
	}
}

// ValidateBinding checks the connection contract without leaking URL credentials
// into errors. Custom protocols have no built-in connection requirements.
func ValidateBinding(protocol string, binding map[string]string) error {
	protocol = NormalizeProtocol(protocol)
	key := ConnectionKey(protocol)
	if key == "" {
		return nil
	}
	u, err := url.Parse(binding[key])
	if err != nil || u.Hostname() == "" || u.Fragment != "" {
		return fmt.Errorf("protocol %s requires a usable %s URL", protocol, key)
	}
	valid := false
	switch protocol {
	case ProtocolPostgres, ProtocolMySQL:
		valid = NormalizeProtocol(u.Scheme) == protocol
	case ProtocolRedis:
		valid = u.Scheme == "redis" || u.Scheme == "rediss"
	case ProtocolS3:
		valid = (u.Scheme == "http" || u.Scheme == "https") && u.User == nil && (u.Path == "" || u.Path == "/") && u.RawQuery == ""
	case ProtocolOpenAI:
		valid = (u.Scheme == "http" || u.Scheme == "https") && u.User == nil && u.RawQuery == ""
	}
	if !valid {
		return fmt.Errorf("%s is not a usable %s connection URL", key, protocol)
	}
	return nil
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
		{"REDIS_URL", ProtocolRedis}, {"AWS_ENDPOINT_URL_S3", ProtocolS3}, {"OPENAI_BASE_URL", ProtocolOpenAI},
	} {
		if binding[entry.key] != "" {
			return entry.protocol
		}
	}
	return ""
}
