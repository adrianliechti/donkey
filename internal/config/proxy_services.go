package config

import (
	"encoding/json"
	"fmt"
	"net/url"
)

// ProxyService offers one fixed HTTP endpoint to selected spaces. ProxyURL
// is an optional outbound HTTP(S) proxy, including CONNECT for HTTPS targets.
type ProxyService struct {
	Name        string `json:"name"`
	Description string `json:"description,omitempty"`
	TargetURL   string `json:"targetUrl"`
	ProxyURL    string `json:"proxyUrl,omitempty"`
}

type ProxyServices []ProxyService

func (services ProxyServices) Validate() error {
	if len(services) > 256 {
		return fmt.Errorf("proxyServices supports at most 256 entries")
	}
	seen := map[string]bool{}
	for _, service := range services {
		if !ValidName(service.Name) || seen[service.Name] {
			return fmt.Errorf("proxyServices: name %q must be a unique lowercase name", service.Name)
		}
		seen[service.Name] = true
		for field, raw := range map[string]string{"targetUrl": service.TargetURL, "proxyUrl": service.ProxyURL} {
			if raw == "" && field == "proxyUrl" {
				continue
			}
			u, err := url.Parse(raw)
			if err != nil || u.Hostname() == "" || (u.Scheme != "http" && u.Scheme != "https") || u.Opaque != "" || u.Fragment != "" {
				return fmt.Errorf("proxyServices.%s.%s must be an HTTP or HTTPS URL without a fragment", service.Name, field)
			}
			if field == "proxyUrl" && ((u.Path != "" && u.Path != "/") || u.RawQuery != "") {
				return fmt.Errorf("proxyServices.%s.proxyUrl must name a proxy host, without a path or query", service.Name)
			}
		}
	}
	return nil
}

func (services ProxyServices) JSON() string {
	data, _ := json.Marshal(services)
	return string(data)
}
