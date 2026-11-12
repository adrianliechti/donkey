package config

import (
	"fmt"
	"net"
	"strings"
)

// InstallExternalDNS describes the optional external-dns DNSEndpoint source.
// Targets are explicit because a DNSEndpoint cannot refer to Service status;
// in production this is normally the gateway LoadBalancer's stable VIP.
type InstallExternalDNS struct {
	Targets    []string `json:"targets,omitempty"`
	RecordType string   `json:"recordType,omitempty"`
	TTL        int64    `json:"ttl,omitempty"`
}

func (d InstallExternalDNS) Enabled() bool { return len(d.Targets) > 0 }

// EffectiveRecordType infers the common VIP cases while leaving an explicit
// type available for provider-specific setups.
func (d InstallExternalDNS) EffectiveRecordType() string {
	if d.RecordType != "" {
		return strings.ToUpper(d.RecordType)
	}
	if len(d.Targets) == 0 {
		return ""
	}
	typ := "CNAME"
	if ip := net.ParseIP(d.Targets[0]); ip != nil {
		if ip.To4() != nil {
			typ = "A"
		} else {
			typ = "AAAA"
		}
	}
	return typ
}

func (d InstallExternalDNS) Validate() error {
	if !d.Enabled() {
		if d.RecordType != "" || d.TTL != 0 {
			return fmt.Errorf("gateway.externalDNS.recordType and ttl need at least one target")
		}
		return nil
	}
	if d.TTL < 0 {
		return fmt.Errorf("gateway.externalDNS.ttl cannot be negative")
	}
	typ := d.EffectiveRecordType()
	switch typ {
	case "A", "AAAA", "CNAME":
	default:
		return fmt.Errorf("gateway.externalDNS.recordType must be A, AAAA or CNAME, not %q", d.RecordType)
	}
	for _, target := range d.Targets {
		if target == "" || strings.TrimSpace(target) != target {
			return fmt.Errorf("gateway.externalDNS.targets contains an empty or whitespace-padded target")
		}
		ip := net.ParseIP(target)
		switch typ {
		case "A":
			if ip == nil || ip.To4() == nil {
				return fmt.Errorf("gateway.externalDNS target %q is not an IPv4 address for an A record", target)
			}
		case "AAAA":
			if ip == nil || ip.To4() != nil {
				return fmt.Errorf("gateway.externalDNS target %q is not an IPv6 address for an AAAA record", target)
			}
		case "CNAME":
			name := strings.TrimSuffix(target, ".")
			if ip != nil || !domainRE.MatchString(name) || len(name) > 253 {
				return fmt.Errorf("gateway.externalDNS target %q is not a DNS hostname for a CNAME record", target)
			}
		}
	}
	return nil
}

// validKubernetesObjectName follows Kubernetes' DNS-subdomain object-name
// shape. Keep it local to configuration validation: workload names use the
// intentionally narrower ValidName rule instead.
func validKubernetesObjectName(name string) bool {
	if name == "" || len(name) > 253 {
		return false
	}
	for _, label := range strings.Split(name, ".") {
		if !ValidNamespace(label) {
			return false
		}
	}
	return true
}
