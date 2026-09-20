package config

import (
	"encoding/json"
	"fmt"
	"net"
	"strconv"
	"strings"
)

// AddonPlugin names an operator-configured gRPC service. TLS with system
// trust roots is the default; plaintext must be chosen explicitly.
type AddonPlugin struct {
	Type     string `json:"type"`
	Address  string `json:"address"`
	Insecure bool   `json:"insecure,omitempty"`
}

type AddonPlugins []AddonPlugin

func (plugins AddonPlugins) Validate() error {
	seen := map[string]bool{}
	for _, p := range plugins {
		if !ValidName(p.Type) || seen[p.Type] {
			return fmt.Errorf("addonPlugins: type %q must be a unique lowercase name", p.Type)
		}
		seen[p.Type] = true
		host, port, err := net.SplitHostPort(p.Address)
		n, _ := strconv.Atoi(port)
		if err != nil || host == "" || strings.ContainsAny(host, "/?#@ \t\r\n") || n < 1 || n > 65535 {
			return fmt.Errorf("addonPlugins.%s: address must be host:port", p.Type)
		}
	}
	return nil
}

func (plugins AddonPlugins) JSON() string {
	data, _ := json.Marshal(plugins)
	return string(data)
}
