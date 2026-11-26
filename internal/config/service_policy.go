package config

import (
	"encoding/json"
	"fmt"
	"strings"
)

// ServicePolicy is managed by the installation operator. Offering IDs are the
// catalog IDs shared by built-ins, proxy services and plugin offerings.
type ServicePolicy struct {
	Rego      string                            `json:"rego"`
	RegoFile  string                            `json:"regoFile,omitempty"`
	Offerings map[string]OfferingPolicyMetadata `json:"offerings,omitempty"`
}

type OfferingPolicyMetadata struct {
	Tags          []string `json:"tags,omitempty"`
	Authorization string   `json:"authorization,omitempty"`
}

func (m OfferingPolicyMetadata) Validate() error {
	switch m.Authorization {
	case "", "unknown", "shared", "delegated":
	default:
		return fmt.Errorf("authorization must be unknown, shared or delegated")
	}
	for _, tag := range m.Tags {
		if strings.TrimSpace(tag) == "" || strings.TrimSpace(tag) != tag {
			return fmt.Errorf("tags must contain nonempty, trimmed strings")
		}
	}
	return nil
}

func (p *ServicePolicy) JSON() string {
	if p == nil {
		return ""
	}
	data, _ := json.Marshal(p)
	return string(data)
}
