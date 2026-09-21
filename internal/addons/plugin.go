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
	"slices"
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

const (
	// describeTimeout bounds one Offerings call, and how long a caller that
	// needs an answer now (a deploy naming the type) waits for it.
	describeTimeout = 10 * time.Second
	// catalogWait is the most a listing waits for a provider that has never
	// answered; one with a cached catalog never waits.
	catalogWait      = 3 * time.Second
	provisionTimeout = 5 * time.Minute
	removeTimeout    = 2 * time.Minute
)

var (
	// offeringsTTL is how long a catalog is served without refreshing it in
	// the background.
	offeringsTTL = 30 * time.Second
	// A failing provider is retried with growing gaps, so a dead one costs
	// one probe per gap rather than one per listing.
	backoffMin = 30 * time.Second
	backoffMax = 5 * time.Minute
)

var pluginEnvName = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]*$`)

// Provider is one configured gRPC service and its cached catalog. It is
// only called when something needs its offerings or an instance of them:
// never at startup, and never twice at once. A stale catalog is served while
// a refresh runs, and the last good one outlives an outage, so a provider
// that is down slows nothing but its own provisioning.
type Provider struct {
	name   string
	client addonv1.ServiceProviderClient

	mu        sync.Mutex
	offerings []*addonv1.Offering // the last good catalog; nil until the first answer
	err       error               // the last failure, cleared by a success
	checked   time.Time           // when the catalog was last (re)fetched or the fetch failed
	failures  int
	nextTry   time.Time
	inflight  chan struct{} // closed when the running refresh completes; nil when idle
}

// Name is the configured provider name.
func (p *Provider) Name() string { return p.name }

// refresh starts a catalog fetch when the cache is stale and no fetch is
// running (and the backoff after a failure has elapsed), then waits up to
// wait for a running fetch, but only while there is no catalog to serve.
func (p *Provider) refresh(ctx context.Context, wait time.Duration) {
	p.mu.Lock()
	now := time.Now()
	if p.inflight == nil && now.Sub(p.checked) >= offeringsTTL && !now.Before(p.nextTry) {
		done := make(chan struct{})
		p.inflight = done
		go p.fetch(done)
	}
	done, cached := p.inflight, p.offerings != nil
	p.mu.Unlock()
	if done == nil || cached || wait <= 0 {
		return
	}
	timer := time.NewTimer(wait)
	defer timer.Stop()
	select {
	case <-done:
	case <-timer.C:
	case <-ctx.Done():
	}
}

// fetch runs one Offerings call on its own deadline: the caller that
// triggered it may give up long before the provider answers.
func (p *Provider) fetch(done chan struct{}) {
	ctx, cancel := context.WithTimeout(context.Background(), describeTimeout)
	defer cancel()
	resp, err := p.client.Offerings(ctx, &addonv1.OfferingsRequest{})
	if err == nil {
		err = validateOfferings(p, resp.GetOfferings())
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	p.checked = time.Now()
	if err != nil {
		p.failures++
		backoff := backoffMax
		if p.failures <= 8 && backoffMin<<(p.failures-1) < backoffMax {
			backoff = backoffMin << (p.failures - 1)
		}
		p.nextTry = p.checked.Add(backoff)
		p.err = fmt.Errorf("provider %s is unavailable: %w", p.name, err)
		log.Printf("addon provider %s: %v", p.name, err)
	} else {
		p.offerings, p.err, p.failures, p.nextTry = resp.GetOfferings(), nil, 0, time.Time{}
	}
	close(done)
	p.inflight = nil
}

// catalog returns the cached offerings, refreshing when stale; the error is
// the last failure when there is nothing cached.
func (p *Provider) catalog(ctx context.Context, wait time.Duration) ([]*addonv1.Offering, error) {
	p.refresh(ctx, wait)
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.offerings != nil {
		return p.offerings, nil
	}
	if p.err != nil {
		return nil, p.err
	}
	return nil, fmt.Errorf("provider %s has not answered yet", p.name)
}

// entries are the catalog rows of this provider: its offerings, or one
// unavailable placeholder named after the provider until it answers.
func (p *Provider) entries(ctx context.Context, wait time.Duration) []Addon {
	offerings, err := p.catalog(ctx, wait)
	if err != nil {
		return []Addon{&Plugin{provider: p, id: p.name}}
	}
	out := make([]Addon, 0, len(offerings))
	for _, o := range offerings {
		out = append(out, &Plugin{provider: p, id: o.Id})
	}
	return out
}

// offering resolves one id from the (possibly refreshed) catalog.
func (p *Provider) offering(ctx context.Context, id string, wait time.Duration) *Plugin {
	offerings, err := p.catalog(ctx, wait)
	if err != nil {
		return nil
	}
	for _, o := range offerings {
		if o.Id == id {
			return &Plugin{provider: p, id: id}
		}
	}
	return nil
}

// knownIDs lists the cached offering ids without asking the provider.
func (p *Provider) knownIDs() []string {
	p.mu.Lock()
	defer p.mu.Unlock()
	ids := make([]string, 0, len(p.offerings))
	for _, o := range p.offerings {
		ids = append(ids, o.Id)
	}
	return ids
}

// Plugin is one offering of a provider as a catalog entry. A pinned
// offering (recorded when an instance was provisioned) answers Describe
// without the provider, so an instance can be removed, resumed or retried
// while its provider is down; the provisioning call itself reports that.
type Plugin struct {
	provider *Provider
	id       string
	pinned   *addonv1.Offering
}

func (p *Plugin) Type() string     { return p.id }
func (p *Plugin) Provider() string { return p.provider.name }

// Describe answers from the provider's cached catalog, refreshing it in the
// background when stale, or from the pinned offering.
func (p *Plugin) Describe(ctx context.Context) (*addonv1.Offering, error) {
	if p.pinned != nil {
		return p.pinned, nil
	}
	offerings, err := p.provider.catalog(ctx, describeTimeout)
	if err != nil {
		return nil, err
	}
	for _, o := range offerings {
		if o.Id == p.id {
			return o, nil
		}
	}
	return nil, fmt.Errorf("provider %s no longer offers %s", p.provider.name, p.id)
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

// ConnectPlugins registers every configured provider. Nothing is called:
// connections are lazy and a provider's catalog is fetched the first time
// something asks for it, so a provider that is down cannot delay startup.
// Only a misconfiguration is fatal. The returned function unregisters the
// providers and closes their connections.
func ConnectPlugins(configs config.AddonPlugins) (func(), error) {
	if err := configs.Validate(); err != nil {
		return nil, err
	}
	var connections []*grpc.ClientConn
	var registered []*Provider
	closeAll := func() {
		providersMu.Lock()
		providers = slices.DeleteFunc(providers, func(p *Provider) bool { return slices.Contains(registered, p) })
		providersMu.Unlock()
		for _, conn := range connections {
			_ = conn.Close()
		}
	}
	for _, cfg := range configs {
		if IsBuiltin(cfg.Name) || providerNamed(cfg.Name) != nil {
			closeAll()
			return nil, fmt.Errorf("addon provider name %q is already taken", cfg.Name)
		}
		options, err := dialOptions(cfg)
		if err != nil {
			closeAll()
			return nil, fmt.Errorf("addon provider %s: %w", cfg.Name, err)
		}
		conn, err := grpc.NewClient(cfg.Address, options...)
		if err != nil {
			closeAll()
			return nil, fmt.Errorf("addon provider %s: %w", cfg.Name, err)
		}
		connections = append(connections, conn)
		p := &Provider{name: cfg.Name, client: addonv1.NewServiceProviderClient(conn)}
		providersMu.Lock()
		providers = append(providers, p)
		providersMu.Unlock()
		registered = append(registered, p)
	}
	return closeAll, nil
}

var lowercaseName = regexp.MustCompile(`^[a-z][a-z0-9-]{0,62}$`)

// validateOfferings checks a provider's catalog as a whole: ids must be
// valid, unique, and not taken by a built-in or another provider.
func validateOfferings(p *Provider, offerings []*addonv1.Offering) error {
	if len(offerings) == 0 {
		return fmt.Errorf("Offerings returned an empty catalog")
	}
	seen := map[string]bool{}
	for _, o := range offerings {
		if err := validateOffering(o); err != nil {
			return err
		}
		if seen[o.Id] || IsBuiltin(o.Id) {
			return fmt.Errorf("offering id %q is a duplicate or a built-in type", o.Id)
		}
		seen[o.Id] = true
		for _, other := range allProviders() {
			if other != p && slices.Contains(other.knownIDs(), o.Id) {
				return fmt.Errorf("offering id %q is already offered by provider %s", o.Id, other.name)
			}
		}
	}
	return nil
}

func validateOffering(o *addonv1.Offering) error {
	if o == nil || !config.ValidName(o.Id) || o.Title == "" || len(o.Env) == 0 {
		return fmt.Errorf("every offering needs a lowercase id, a title and an environment contract")
	}
	if err := ValidateProtocol(o.Protocol); err != nil {
		return fmt.Errorf("offering %s: %w", o.Id, err)
	}
	for _, name := range append([]string{o.Category, o.Icon}, o.Tags...) {
		if name != "" && !lowercaseName.MatchString(name) {
			return fmt.Errorf("offering %s: category, icon and tags must be lowercase names", o.Id)
		}
	}
	if o.DocumentationUrl != "" {
		if u, err := url.Parse(o.DocumentationUrl); err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
			return fmt.Errorf("offering %s: documentation_url must be an http(s) URL", o.Id)
		}
	}
	seen := map[string]bool{}
	for _, v := range o.Env {
		if v == nil || !pluginEnvName.MatchString(v.Name) || seen[v.Name] {
			return fmt.Errorf("offering %s: invalid or duplicate environment key", o.Id)
		}
		seen[v.Name] = true
	}
	if key := ConnectionKey(o.Protocol); key != "" && !seen[key] {
		return fmt.Errorf("offering %s: protocol %s requires %s in the environment contract", o.Id, NormalizeProtocol(o.Protocol), key)
	}
	if err := ValidateSchema(o.Parameters); err != nil {
		return fmt.Errorf("offering %s: invalid parameter schema: %w", o.Id, err)
	}
	return nil
}

// Provision streams the provider's progress to report (nil means the log)
// and returns the validated result once the service is ready.
func (p *Plugin) Provision(ctx context.Context, req *addonv1.ProvisionRequest, report func(string)) (*addonv1.ProvisionResponse, error) {
	desc, err := p.Describe(ctx)
	if err != nil {
		return nil, err
	}
	if report == nil {
		report = func(message string) { log.Printf("addon %s: %s", p.id, message) }
	}
	ctx, cancel := context.WithTimeout(ctx, provisionTimeout)
	defer cancel()
	stream, err := p.provider.client.Provision(ctx, req)
	if err != nil {
		return nil, fmt.Errorf("provisioning addon %s: %w", p.id, err)
	}
	var result *addonv1.ProvisionResponse
	for result == nil {
		event, err := stream.Recv()
		if errors.Is(err, io.EOF) {
			return nil, fmt.Errorf("provisioning addon %s: the provider ended without a result", p.id)
		}
		if err != nil {
			return nil, fmt.Errorf("provisioning addon %s: %w", p.id, err)
		}
		if event.Message != "" {
			report(event.Message)
		}
		result = event.Result
	}
	if err := ValidateProvisioning(result); err != nil {
		return nil, fmt.Errorf("addon %s returned invalid provisioning info: %w", p.id, err)
	}
	declared := map[string]*addonv1.EnvVar{}
	for _, v := range desc.Env {
		declared[v.Name] = v
	}
	for key := range result.Env {
		if _, ok := declared[key]; !ok {
			return nil, fmt.Errorf("addon %s returned an undeclared environment key", p.id)
		}
	}
	for _, v := range desc.Env {
		if _, ok := result.Env[v.Name]; !ok && !v.Optional {
			return nil, fmt.Errorf("addon %s did not return the required environment key %s", p.id, v.Name)
		}
	}
	if err := ValidateBinding(desc.Protocol, result.Env); err != nil {
		return nil, fmt.Errorf("addon %s: %w", p.id, err)
	}
	return result, nil
}

// Deprovision removes (or, with keepData, stops) the instance. The retain
// capability is read from the recorded offering, never from the provider.
func (p *Plugin) Deprovision(ctx context.Context, instance *addonv1.Instance, keepData bool) error {
	if keepData {
		desc, err := p.Describe(ctx)
		if err != nil {
			return err
		}
		if !desc.GetCapabilities().GetRetainData() {
			return fmt.Errorf("addon %s does not retain data; remove the service without keeping its data", p.id)
		}
	}
	ctx, cancel := context.WithTimeout(ctx, removeTimeout)
	defer cancel()
	_, err := p.provider.client.Deprovision(ctx, &addonv1.DeprovisionRequest{Instance: instance, KeepData: keepData})
	if err != nil {
		return fmt.Errorf("deprovisioning addon %s: %w", p.id, err)
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
// can be retried or deprovisioned using the stable instance identity. The
// offering is recorded as it was when the instance was (last) provisioned:
// its capabilities and contract decide removal and resumption without
// asking the provider again.
type Provisioning struct {
	Type     string
	Provider string
	Protocol string
	Instance *addonv1.Instance
	Offering *addonv1.Offering
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
	Provider   string          `json:"provider,omitempty"`
	Protocol   string          `json:"protocol,omitempty"`
	Instance   json.RawMessage `json:"instance"`
	Offering   json.RawMessage `json:"offering,omitempty"`
	Parameters Parameters      `json:"parameters,omitempty"`
	Info       json.RawMessage `json:"info,omitempty"`
	Stopped    bool            `json:"stopped,omitempty"`
	Error      string          `json:"error,omitempty"`
}

func (p Provisioning) MarshalJSON() ([]byte, error) {
	out := provisioningJSON{Type: p.Type, Provider: p.Provider, Protocol: p.Protocol, Parameters: p.Parameters, Stopped: p.Stopped, Error: p.Error}
	var err error
	if p.Instance != nil {
		if out.Instance, err = protojson.Marshal(p.Instance); err != nil {
			return nil, err
		}
	}
	if p.Offering != nil {
		if out.Offering, err = protojson.Marshal(p.Offering); err != nil {
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
	*p = Provisioning{Type: in.Type, Provider: in.Provider, Protocol: in.Protocol, Parameters: in.Parameters, Stopped: in.Stopped, Error: in.Error}
	if len(in.Instance) > 0 {
		p.Instance = &addonv1.Instance{}
		if err := protojson.Unmarshal(in.Instance, p.Instance); err != nil {
			return err
		}
	}
	if len(in.Offering) > 0 {
		p.Offering = &addonv1.Offering{}
		if err := protojson.Unmarshal(in.Offering, p.Offering); err != nil {
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

// Reason tells the provider what a repeat Provision means for this record.
func (p *Provisioning) Reason() addonv1.ProvisionReason {
	switch {
	case p == nil:
		return addonv1.ProvisionReason_PROVISION_REASON_CREATE
	case p.Stopped:
		return addonv1.ProvisionReason_PROVISION_REASON_RESUME
	case p.Error != "" || p.Info == nil:
		return addonv1.ProvisionReason_PROVISION_REASON_RETRY
	default:
		return addonv1.ProvisionReason_PROVISION_REASON_UPDATE
	}
}

// Endpoint is the primary TCP destination, in the provider's declared order.
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

// Plugin is the record's offering pinned to what was provisioned: it needs
// the provider configured, not answering.
func (p *Provisioning) Plugin() (*Plugin, error) {
	provider := providerNamed(p.Provider)
	if provider == nil {
		return nil, fmt.Errorf("addon %s no longer has its provider %q configured", p.Type, p.Provider)
	}
	if p.Offering == nil {
		return nil, fmt.Errorf("service %s has no recorded offering", p.Instance.GetName())
	}
	return &Plugin{provider: provider, id: p.Type, pinned: p.Offering}, nil
}

// PlanProvision resolves the parameters of a provision request against the
// instance's record and decides what the request means to the provider. A
// running instance may only change parameters when its offering supports
// updates; the schema's immutable values are enforced regardless.
func PlanProvision(desc *addonv1.Offering, given map[string]any, previous *Provisioning) (Parameters, addonv1.ProvisionReason, error) {
	var stored Parameters
	if previous != nil {
		stored = previous.Parameters
	}
	params, err := ResolveInstanceParameters(desc, given, stored)
	if err != nil {
		return nil, addonv1.ProvisionReason_PROVISION_REASON_UNSPECIFIED, err
	}
	reason := previous.Reason()
	if reason == addonv1.ProvisionReason_PROVISION_REASON_UPDATE && !desc.GetCapabilities().GetUpdate() && !equalParameters(stored, params) {
		return nil, reason, fmt.Errorf("the %s service cannot change its parameters once it exists; remove it and add it again", desc.GetId())
	}
	return params, reason, nil
}

func equalParameters(a, b Parameters) bool {
	if len(a) != len(b) {
		return false
	}
	for k, v := range a {
		if !parameterEqual(v, b[k]) {
			return false
		}
	}
	return true
}

func parameterEqual(a, b any) bool {
	x, errX := json.Marshal(a)
	y, errY := json.Marshal(b)
	return errX == nil && errY == nil && string(x) == string(y)
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
