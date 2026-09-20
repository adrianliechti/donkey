package addons

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net"
	"net/netip"
	"net/url"
	"os"
	"regexp"
	"strings"
	"sync"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/protobuf/encoding/protojson"
	"k8s.io/apimachinery/pkg/util/validation"

	"github.com/adrianliechti/donkey/internal/config"
	addonv1 "github.com/adrianliechti/donkey/pkg/addons/v1"
)

// Plugin adapts a remotely provisioned addon to the local catalog. Its
// description is fetched lazily and cached briefly, so donkey-server starts
// (and keeps running) whether or not the plugin answers right now.
type Plugin struct {
	typ    string
	client addonv1.AddonPluginClient

	mu      sync.Mutex
	info    *addonv1.DescribeResponse
	fetched time.Time
	err     error
}

const (
	describeTimeout  = 10 * time.Second
	provisionTimeout = 5 * time.Minute
	removeTimeout    = 2 * time.Minute
)

// describeTTL bounds how long a description (or a failure) is trusted.
var describeTTL = 30 * time.Second

var pluginEnvName = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]*$`)

func (p *Plugin) Type() string          { return p.typ }
func (p *Plugin) CredentialKey() string { return "" }

// Describe asks the plugin, caching both answers and failures for a short
// while. A stale description outlives a transient outage: the catalog keeps
// its entry, and Provision reports the outage itself.
func (p *Plugin) Describe(ctx context.Context) (*addonv1.DescribeResponse, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if time.Since(p.fetched) < describeTTL {
		if p.info != nil {
			return p.info, nil
		}
		return nil, p.err
	}
	callCtx, cancel := context.WithTimeout(ctx, describeTimeout)
	defer cancel()
	info, err := p.client.Describe(callCtx, &addonv1.DescribeRequest{})
	if err == nil {
		err = validateDescription(p.typ, info)
	}
	p.fetched = time.Now()
	if err != nil {
		p.err = fmt.Errorf("addon plugin %s is unavailable: %w", p.typ, err)
		log.Printf("addon plugin %s: %v", p.typ, err)
		if p.info != nil {
			return p.info, nil
		}
		return nil, p.err
	}
	p.info, p.err = info, nil
	return info, nil
}

// cached is the last good description, or nil.
func (p *Plugin) cached() *addonv1.DescribeResponse {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.info
}

// Binding returns only the declared keys; actual values come from Provision.
func (p *Plugin) Binding(Input) map[string]string {
	values := map[string]string{}
	for _, key := range EnvNames(p.cached()) {
		values[key] = ""
	}
	return values
}

func (p *Plugin) ExternalBinding(string) (map[string]string, error) {
	return nil, fmt.Errorf("addon plugin %s provisions its own connections; external URL bindings are not supported", p.typ)
}

func (p *Plugin) Workload(Input) (*Workload, error) {
	return nil, fmt.Errorf("addon plugin %s must be provisioned over gRPC", p.typ)
}

// bearerToken authenticates donkey-server to a plugin. Plaintext connections
// may carry it only when the operator chose them explicitly.
type bearerToken struct {
	token  string
	secure bool
}

func (b bearerToken) GetRequestMetadata(context.Context, ...string) (map[string]string, error) {
	return map[string]string{"authorization": "Bearer " + b.token}, nil
}

func (b bearerToken) RequireTransportSecurity() bool { return b.secure }

func dialOptions(cfg config.AddonPlugin) ([]grpc.DialOption, error) {
	var transport credentials.TransportCredentials
	if cfg.Insecure {
		transport = insecure.NewCredentials()
	} else {
		tlsConfig := &tls.Config{MinVersion: tls.VersionTLS12}
		if cfg.CAFile != "" {
			pem, err := os.ReadFile(cfg.CAFile)
			if err != nil {
				return nil, err
			}
			pool := x509.NewCertPool()
			if !pool.AppendCertsFromPEM(pem) {
				return nil, fmt.Errorf("%s contains no certificates", cfg.CAFile)
			}
			tlsConfig.RootCAs = pool
		}
		if cfg.CertFile != "" {
			cert, err := tls.LoadX509KeyPair(cfg.CertFile, cfg.KeyFile)
			if err != nil {
				return nil, err
			}
			tlsConfig.Certificates = []tls.Certificate{cert}
		}
		transport = credentials.NewTLS(tlsConfig)
	}
	options := []grpc.DialOption{grpc.WithTransportCredentials(transport)}
	token := cfg.Token
	if cfg.TokenFile != "" {
		raw, err := os.ReadFile(cfg.TokenFile)
		if err != nil {
			return nil, err
		}
		token = strings.TrimSpace(string(raw))
	}
	if token != "" {
		options = append(options, grpc.WithPerRPCCredentials(bearerToken{token: token, secure: !cfg.Insecure}))
	}
	return options, nil
}

// ConnectPlugins registers every configured plugin in the catalog. A plugin
// that does not answer yet is registered anyway and joins the catalog as
// soon as it does; only a misconfiguration is fatal. The returned function
// unregisters them and closes their connections.
func ConnectPlugins(ctx context.Context, configs config.AddonPlugins) (func(), error) {
	if err := configs.Validate(); err != nil {
		return nil, err
	}
	var connections []*grpc.ClientConn
	registered := map[string]*Plugin{}
	closeAll := func() {
		registryMu.Lock()
		for name, p := range registered {
			if registry[name] == p {
				delete(registry, name)
			}
		}
		registryMu.Unlock()
		for _, conn := range connections {
			_ = conn.Close()
		}
	}
	for _, cfg := range configs {
		options, err := dialOptions(cfg)
		if err != nil {
			closeAll()
			return nil, fmt.Errorf("addon plugin %s: %w", cfg.Type, err)
		}
		conn, err := grpc.NewClient(cfg.Address, options...)
		if err != nil {
			closeAll()
			return nil, fmt.Errorf("addon plugin %s: %w", cfg.Type, err)
		}
		connections = append(connections, conn)
		p := &Plugin{typ: cfg.Type, client: addonv1.NewAddonPluginClient(conn)}
		registryMu.Lock()
		_, exists := registry[cfg.Type]
		if !exists {
			registry[cfg.Type] = p
			registered[cfg.Type] = p
		}
		registryMu.Unlock()
		if exists {
			closeAll()
			return nil, fmt.Errorf("addon plugin type %q is already registered", cfg.Type)
		}
		if _, err := p.Describe(ctx); err != nil {
			log.Printf("addon plugin %s: not answering yet; it joins the catalog once it does", cfg.Type)
		}
	}
	return closeAll, nil
}

func validateDescription(typ string, info *addonv1.DescribeResponse) error {
	if info == nil || info.Type != typ || info.Title == "" || len(info.Env) == 0 {
		return fmt.Errorf("Describe must return the configured type, a title and an environment contract")
	}
	if err := ValidateProtocol(info.Protocol); err != nil {
		return err
	}
	seen := map[string]bool{}
	for _, v := range info.Env {
		if v == nil || !pluginEnvName.MatchString(v.Name) || seen[v.Name] {
			return fmt.Errorf("Describe returned an invalid or duplicate environment key")
		}
		seen[v.Name] = true
	}
	if key := ConnectionKey(info.Protocol); key != "" && !seen[key] {
		return fmt.Errorf("protocol %s requires %s in the environment contract", NormalizeProtocol(info.Protocol), key)
	}
	if err := ValidateSchema(info.Parameters); err != nil {
		return fmt.Errorf("Describe returned an invalid parameter schema: %w", err)
	}
	for _, c := range info.Capabilities {
		if c == "" || strings.ToLower(c) != c || strings.ContainsAny(c, " \t\r\n") {
			return fmt.Errorf("Describe returned an invalid capability")
		}
	}
	return nil
}

// Provision streams the plugin's progress to report (nil means the log) and
// returns the validated result once the service is ready.
func (p *Plugin) Provision(ctx context.Context, req *addonv1.ProvisionRequest, report func(string)) (*addonv1.ProvisionResponse, error) {
	desc, err := p.Describe(ctx)
	if err != nil {
		return nil, err
	}
	if report == nil {
		report = func(message string) { log.Printf("addon plugin %s: %s", p.typ, message) }
	}
	ctx, cancel := context.WithTimeout(ctx, provisionTimeout)
	defer cancel()
	stream, err := p.client.Provision(ctx, req)
	if err != nil {
		return nil, fmt.Errorf("provisioning addon %s: %w", p.typ, err)
	}
	var result *addonv1.ProvisionResponse
	for result == nil {
		event, err := stream.Recv()
		if errors.Is(err, io.EOF) {
			return nil, fmt.Errorf("provisioning addon %s: the plugin ended without a result", p.typ)
		}
		if err != nil {
			return nil, fmt.Errorf("provisioning addon %s: %w", p.typ, err)
		}
		if event.Message != "" {
			report(event.Message)
		}
		result = event.Result
	}
	if err := ValidateProvisioning(result); err != nil {
		return nil, fmt.Errorf("addon plugin %s returned invalid provisioning info: %w", p.typ, err)
	}
	declared := map[string]*addonv1.EnvVar{}
	for _, v := range desc.Env {
		declared[v.Name] = v
	}
	for key := range result.Env {
		if _, ok := declared[key]; !ok {
			return nil, fmt.Errorf("addon plugin %s returned an undeclared environment key", p.typ)
		}
	}
	for _, v := range desc.Env {
		if _, ok := result.Env[v.Name]; !ok && !v.Optional {
			return nil, fmt.Errorf("addon plugin %s did not return the required environment key %s", p.typ, v.Name)
		}
	}
	if err := ValidateBinding(desc.Protocol, result.Env); err != nil {
		return nil, fmt.Errorf("addon plugin %s: %w", p.typ, err)
	}
	return result, nil
}

func (p *Plugin) Deprovision(ctx context.Context, instance *addonv1.Instance, keepData bool) error {
	if keepData {
		desc, err := p.Describe(ctx)
		if err != nil {
			return err
		}
		if !HasCapability(desc, CapabilityKeepData) {
			return fmt.Errorf("addon plugin %s does not retain data; remove the service without keeping its data", p.typ)
		}
	}
	ctx, cancel := context.WithTimeout(ctx, removeTimeout)
	defer cancel()
	_, err := p.client.Deprovision(ctx, &addonv1.DeprovisionRequest{Instance: instance, KeepData: keepData})
	if err != nil {
		return fmt.Errorf("deprovisioning addon %s: %w", p.typ, err)
	}
	return nil
}

// Lifecycle states of a provisioning record.
const (
	StateReady   = "ready"
	StatePending = "pending"
	StateFailed  = "failed"
	StateStopped = "stopped"
)

// Provisioning is stored separately from the environment binding so metadata
// never appears in envFrom. Save intent before the RPC so even a lost response
// can be retried or deprovisioned using the stable instance identity.
type Provisioning struct {
	Type     string
	Protocol string
	Instance *addonv1.Instance
	// Parameters are what the instance was (last) provisioned with; a repeat
	// request without explicit parameters reuses them.
	Parameters Parameters
	Info       *addonv1.ProvisionResponse
	Stopped    bool
	// Error is the last provisioning failure; cleared by a success.
	Error string
}

// The proto messages are persisted with protojson: encoding/json does not
// round-trip oneofs or well-known types such as Struct.
type provisioningJSON struct {
	Type       string          `json:"type"`
	Protocol   string          `json:"protocol,omitempty"`
	Instance   json.RawMessage `json:"instance"`
	Parameters Parameters      `json:"parameters,omitempty"`
	Info       json.RawMessage `json:"info,omitempty"`
	Stopped    bool            `json:"stopped,omitempty"`
	Error      string          `json:"error,omitempty"`
}

func (p Provisioning) MarshalJSON() ([]byte, error) {
	out := provisioningJSON{Type: p.Type, Protocol: p.Protocol, Parameters: p.Parameters, Stopped: p.Stopped, Error: p.Error}
	var err error
	if p.Instance != nil {
		if out.Instance, err = protojson.Marshal(p.Instance); err != nil {
			return nil, err
		}
	}
	if p.Info != nil {
		if out.Info, err = protojson.Marshal(p.Info); err != nil {
			return nil, err
		}
	}
	return json.Marshal(out)
}

func (p *Provisioning) UnmarshalJSON(data []byte) error {
	var in provisioningJSON
	if err := json.Unmarshal(data, &in); err != nil {
		return err
	}
	*p = Provisioning{Type: in.Type, Protocol: in.Protocol, Parameters: in.Parameters, Stopped: in.Stopped, Error: in.Error}
	if len(in.Instance) > 0 {
		p.Instance = &addonv1.Instance{}
		if err := protojson.Unmarshal(in.Instance, p.Instance); err != nil {
			return err
		}
	}
	if len(in.Info) > 0 {
		p.Info = &addonv1.ProvisionResponse{}
		if err := protojson.Unmarshal(in.Info, p.Info); err != nil {
			return err
		}
	}
	return nil
}

// State classifies the record for listings: stopped instances keep their
// data, failed ones their last error, pending ones only their intent.
func (p *Provisioning) State() string {
	switch {
	case p == nil:
		return ""
	case p.Stopped:
		return StateStopped
	case p.Error != "":
		return StateFailed
	case p.Info == nil:
		return StatePending
	default:
		return StateReady
	}
}

// Endpoint is the primary TCP destination, in the plugin's declared order.
func (p *Provisioning) Endpoint() (string, int32) {
	if p != nil && !p.Stopped && p.Info != nil {
		for _, endpoint := range p.Info.Endpoints {
			if endpoint != nil && (endpoint.Protocol == "" || endpoint.Protocol == "TCP") {
				return endpoint.Host, endpoint.Port
			}
		}
	}
	return "", 0
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
		if k := e.GetKubernetes(); k != nil {
			if k.Namespace == "" || len(validation.IsDNS1123Label(k.Namespace)) != 0 || len(k.PodLabels) == 0 || k.Port < 0 || k.Port > 65535 {
				return fmt.Errorf("Kubernetes destination requires one namespace, nonempty pod labels and a valid port")
			}
			for key, value := range k.PodLabels {
				if len(validation.IsQualifiedName(key)) != 0 || len(validation.IsValidLabelValue(value)) != 0 {
					return fmt.Errorf("invalid Kubernetes destination label")
				}
			}
		}
		if c := e.GetCidrs(); c != nil {
			if len(c.Ranges) == 0 {
				return fmt.Errorf("a CIDR destination needs at least one range")
			}
			for _, cidr := range c.Ranges {
				prefix, err := netip.ParsePrefix(cidr)
				if err != nil || prefix.Bits() == 0 || prefix != prefix.Masked() {
					return fmt.Errorf("destination CIDRs must be canonical, non-default ranges")
				}
			}
		}
	}
	return nil
}
