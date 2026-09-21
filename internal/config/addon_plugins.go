package config

import (
	"encoding/json"
	"fmt"
	"net"
	"strconv"
	"strings"
)

// AddonPlugin names an operator-configured gRPC service provider. Its
// offerings (the catalog entries) come from the service itself; the name
// only identifies the provider in logs and listings. TLS with system trust
// roots is the default; plaintext must be chosen explicitly. The file paths
// are read by donkey-server, so on Kubernetes they name mounted secrets, not
// files on the installer's machine.
type AddonPlugin struct {
	Name     string `json:"name"`
	Address  string `json:"address"`
	Insecure bool   `json:"insecure,omitempty"`
	// Token (or the contents of TokenFile) is sent as a bearer token on every
	// call, so the plugin can tell donkey-server from anyone else who can
	// reach it. TokenFile keeps the secret out of the installation settings.
	Token     string `json:"token,omitempty"`
	TokenFile string `json:"tokenFile,omitempty"`
	// CAFile replaces the system trust roots; CertFile and KeyFile present a
	// client certificate (mutual TLS).
	CAFile   string `json:"caFile,omitempty"`
	CertFile string `json:"certFile,omitempty"`
	KeyFile  string `json:"keyFile,omitempty"`
}

type AddonPlugins []AddonPlugin

func (plugins AddonPlugins) Validate() error {
	seen := map[string]bool{}
	for _, p := range plugins {
		if !ValidName(p.Name) || seen[p.Name] {
			return fmt.Errorf("addonPlugins: name %q must be a unique lowercase name", p.Name)
		}
		seen[p.Name] = true
		host, port, err := net.SplitHostPort(p.Address)
		n, _ := strconv.Atoi(port)
		if err != nil || host == "" || strings.ContainsAny(host, "/?#@ \t\r\n") || n < 1 || n > 65535 {
			return fmt.Errorf("addonPlugins.%s: address must be host:port", p.Name)
		}
		if p.Token != "" && p.TokenFile != "" {
			return fmt.Errorf("addonPlugins.%s: give either token or tokenFile", p.Name)
		}
		if (p.CertFile == "") != (p.KeyFile == "") {
			return fmt.Errorf("addonPlugins.%s: certFile and keyFile go together", p.Name)
		}
		if p.Insecure && (p.CAFile != "" || p.CertFile != "") {
			return fmt.Errorf("addonPlugins.%s: insecure is plaintext; TLS files need insecure: false", p.Name)
		}
	}
	return nil
}

func (plugins AddonPlugins) JSON() string {
	data, _ := json.Marshal(plugins)
	return string(data)
}
