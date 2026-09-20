package addons

import (
	"context"
	"crypto/tls"
	"fmt"
	"net"
	"net/netip"
	"net/url"
	"regexp"
	"strings"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials"
	"google.golang.org/grpc/credentials/insecure"
	"k8s.io/apimachinery/pkg/util/validation"

	"github.com/adrianliechti/donkey/internal/config"
	addonv1 "github.com/adrianliechti/donkey/pkg/addons/v1"
)

// Plugin adapts a remotely provisioned addon to the local catalog. Binding
// returns only the declared keys; actual values come from Provision.
type Plugin struct {
	info   *addonv1.DescribeResponse
	client addonv1.AddonPluginClient
}

var pluginEnvName = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]*$`)

func (p *Plugin) Type() string           { return p.info.Type }
func (p *Plugin) Protocol() string       { return NormalizeProtocol(p.info.Protocol) }
func (p *Plugin) Title() string          { return p.info.Title }
func (p *Plugin) Description() string    { return p.info.Description }
func (p *Plugin) DefaultVersion() string { return p.info.DefaultVersion }
func (p *Plugin) DefaultStorage() string { return p.info.DefaultStorage }
func (p *Plugin) CredentialKey() string  { return "" }
func (p *Plugin) Binding(Input) map[string]string {
	values := make(map[string]string, len(p.info.EnvKeys))
	for _, key := range p.info.EnvKeys {
		values[key] = ""
	}
	return values
}
func (p *Plugin) ExternalBinding(string) (map[string]string, error) {
	return nil, fmt.Errorf("addon plugin %s provisions its own connections; external URL bindings are not supported", p.Type())
}
func (p *Plugin) Workload(Input) (*Workload, error) {
	return nil, fmt.Errorf("addon plugin %s must be provisioned over gRPC", p.Type())
}

// ConnectPlugins registers catalog entries before the server starts handling
// requests. A failed startup or a shutdown closes every connection it opened.
func ConnectPlugins(ctx context.Context, configs config.AddonPlugins) (func(), error) {
	if err := configs.Validate(); err != nil {
		return nil, err
	}
	var connections []*grpc.ClientConn
	registered := map[string]*Plugin{}
	closeAll := func() {
		registryMu.Lock()
		for name, p := range registered {
			if Registry[name] == p {
				delete(Registry, name)
			}
		}
		registryMu.Unlock()
		for _, conn := range connections {
			_ = conn.Close()
		}
	}
	for _, cfg := range configs {
		if _, err := Get(cfg.Type); err == nil {
			closeAll()
			return nil, fmt.Errorf("addon plugin type %q is already registered", cfg.Type)
		}
		var transport credentials.TransportCredentials = credentials.NewTLS(&tls.Config{MinVersion: tls.VersionTLS12})
		if cfg.Insecure {
			transport = insecure.NewCredentials()
		}
		conn, err := grpc.NewClient(cfg.Address, grpc.WithTransportCredentials(transport))
		if err != nil {
			closeAll()
			return nil, fmt.Errorf("addon plugin %s: %w", cfg.Type, err)
		}
		connections = append(connections, conn)
		client := addonv1.NewAddonPluginClient(conn)
		callCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
		info, err := client.Describe(callCtx, &addonv1.DescribeRequest{})
		cancel()
		if err == nil {
			err = validateDescription(cfg.Type, info)
		}
		if err != nil {
			closeAll()
			return nil, fmt.Errorf("addon plugin %s: %w", cfg.Type, err)
		}
		p := &Plugin{info: info, client: client}
		registryMu.Lock()
		_, exists := Registry[cfg.Type]
		if !exists {
			Registry[cfg.Type] = p
			registered[cfg.Type] = p
		}
		registryMu.Unlock()
		if exists {
			closeAll()
			return nil, fmt.Errorf("addon plugin type %q is already registered", cfg.Type)
		}
	}
	return closeAll, nil
}

func validateDescription(typ string, info *addonv1.DescribeResponse) error {
	if info == nil || info.Type != typ || info.Title == "" || len(info.EnvKeys) == 0 {
		return fmt.Errorf("Describe must return the configured type, a title and environment keys")
	}
	if err := ValidateProtocol(info.Protocol); err != nil {
		return err
	}
	seen := map[string]bool{}
	for _, key := range info.EnvKeys {
		if !pluginEnvName.MatchString(key) || seen[key] {
			return fmt.Errorf("Describe returned an invalid or duplicate environment key")
		}
		seen[key] = true
	}
	return nil
}

func (p *Plugin) Provision(ctx context.Context, req *addonv1.ProvisionRequest) (*addonv1.ProvisionResponse, error) {
	ctx, cancel := context.WithTimeout(ctx, 5*time.Minute)
	defer cancel()
	info, err := p.client.Provision(ctx, req)
	if err != nil {
		return nil, fmt.Errorf("provisioning addon %s: %w", p.Type(), err)
	}
	if err := ValidateProvisioning(info); err != nil {
		return nil, fmt.Errorf("addon plugin %s returned invalid provisioning info: %w", p.Type(), err)
	}
	if info.Protocol == "" {
		info.Protocol = p.Protocol()
	}
	info.Protocol = NormalizeProtocol(info.Protocol)
	if p.Protocol() != "" && info.Protocol != p.Protocol() {
		return nil, fmt.Errorf("addon plugin %s returned a protocol that differs from Describe", p.Type())
	}
	keys := p.Binding(Input{})
	if len(info.Env) != len(keys) {
		return nil, fmt.Errorf("addon plugin %s returned environment keys that differ from Describe", p.Type())
	}
	for key := range info.Env {
		if _, ok := keys[key]; !ok {
			return nil, fmt.Errorf("addon plugin %s returned an undeclared environment key", p.Type())
		}
	}
	return info, nil
}

func (p *Plugin) Deprovision(ctx context.Context, instance *addonv1.Instance, keepData bool) error {
	ctx, cancel := context.WithTimeout(ctx, 2*time.Minute)
	defer cancel()
	_, err := p.client.Deprovision(ctx, &addonv1.DeprovisionRequest{Instance: instance, KeepData: keepData})
	if err != nil {
		return fmt.Errorf("deprovisioning addon %s: %w", p.Type(), err)
	}
	return nil
}

// Provisioning is stored separately from the environment binding so metadata
// never appears in envFrom. Save intent before the RPC so even a lost response
// can be retried or deprovisioned using the stable instance identity.
type Provisioning struct {
	Type     string                     `json:"type"`
	Instance *addonv1.Instance          `json:"instance"`
	Info     *addonv1.ProvisionResponse `json:"info,omitempty"`
	Stopped  bool                       `json:"stopped,omitempty"`
}

func (p *Provisioning) Plugin() (*Plugin, error) {
	a, err := Get(p.Type)
	if err != nil {
		return nil, err
	}
	plugin, ok := a.(*Plugin)
	if !ok {
		return nil, fmt.Errorf("addon %s no longer has its provisioning plugin configured", p.Type)
	}
	return plugin, nil
}

// ValidateProvisioning never includes response values in errors: they may
// contain credentials. Missing policy targets are allowed only when the
// runtime does not restrict egress; that check lives at the policy boundary.
func ValidateProvisioning(info *addonv1.ProvisionResponse) error {
	if info == nil || len(info.Env) == 0 || len(info.Endpoints) == 0 {
		return fmt.Errorf("environment and endpoints are required")
	}
	if err := ValidateProtocol(info.Protocol); err != nil {
		return err
	}
	for key, value := range info.Env {
		if !pluginEnvName.MatchString(key) || strings.ContainsRune(value, '\x00') {
			return fmt.Errorf("invalid environment entry")
		}
	}
	for _, raw := range info.Urls {
		u, err := url.Parse(raw)
		if err != nil || u.Scheme == "" || u.Hostname() == "" {
			return fmt.Errorf("invalid connection URL")
		}
	}
	for _, e := range info.Endpoints {
		if e == nil || e.Host == "" || (net.ParseIP(e.Host) == nil && len(validation.IsDNS1123Subdomain(e.Host)) != 0) || e.Port < 1 || e.Port > 65535 {
			return fmt.Errorf("endpoint requires a valid host and port")
		}
		if e.Protocol != "" && e.Protocol != "TCP" && e.Protocol != "UDP" && e.Protocol != "SCTP" {
			return fmt.Errorf("invalid endpoint protocol")
		}
		if k := e.Kubernetes; k != nil {
			if len(e.Cidrs) > 0 || k.Namespace == "" || len(validation.IsDNS1123Label(k.Namespace)) != 0 || len(k.PodLabels) == 0 || k.Port < 0 || k.Port > 65535 {
				return fmt.Errorf("Kubernetes destination requires one namespace, nonempty pod labels and a valid port; it cannot also specify CIDRs")
			}
			for key, value := range k.PodLabels {
				if len(validation.IsQualifiedName(key)) != 0 || len(validation.IsValidLabelValue(value)) != 0 {
					return fmt.Errorf("invalid Kubernetes destination label")
				}
			}
		}
		for _, cidr := range e.Cidrs {
			prefix, err := netip.ParsePrefix(cidr)
			if err != nil || prefix.Bits() == 0 || prefix != prefix.Masked() {
				return fmt.Errorf("destination CIDRs must be canonical, non-default ranges")
			}
		}
	}
	return nil
}
