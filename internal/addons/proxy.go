package addons

import (
	"context"
	"fmt"
	"strings"
	"sync"

	"github.com/adrianliechti/donkey/internal/config"
	addonv1 "github.com/adrianliechti/donkey/pkg/addons/v1"
)

var proxyCatalog = struct {
	sync.RWMutex
	entries map[string]Builtin
}{}

// RegisterProxyServices makes configured endpoints local catalog offerings.
// Only display metadata is retained; upstream addresses stay in the proxy.
func RegisterProxyServices(services config.ProxyServices) (func(), error) {
	if err := services.Validate(); err != nil {
		return nil, err
	}
	catalogsMu.Lock()
	defer catalogsMu.Unlock()
	entries := map[string]Builtin{}
	for _, service := range services {
		if _, reserved := builtins[service.Name]; reserved {
			return nil, fmt.Errorf("proxyServices: %s is a reserved built-in offering", service.Name)
		}
		for _, provider := range allProviders() {
			for _, id := range provider.knownIDs() {
				if id == service.Name {
					return nil, fmt.Errorf("proxyServices: %s is already offered by provider %s", id, provider.name)
				}
			}
		}
		entries[service.Name] = proxyService{name: service.Name, description: service.Description}
	}
	proxyCatalog.Lock()
	previous := proxyCatalog.entries
	proxyCatalog.entries = entries
	proxyCatalog.Unlock()
	return func() {
		proxyCatalog.Lock()
		defer proxyCatalog.Unlock()
		proxyCatalog.entries = previous
	}, nil
}

func proxyBuiltin(id string) Builtin {
	proxyCatalog.RLock()
	defer proxyCatalog.RUnlock()
	return proxyCatalog.entries[id]
}

func proxyBuiltins() map[string]Builtin {
	proxyCatalog.RLock()
	defer proxyCatalog.RUnlock()
	out := map[string]Builtin{}
	for id, entry := range proxyCatalog.entries {
		out[id] = entry
	}
	return out
}

// ProxyEnvKey names the URL injected for a configured HTTP offering.
func ProxyEnvKey(id string) string {
	base := strings.ToUpper(strings.ReplaceAll(id, "-", "_"))
	if base != "" && base[0] >= '0' && base[0] <= '9' {
		base = "SERVICE_" + base
	}
	return base + "_URL"
}

func ProxyBindingURL(base, id string) string {
	return strings.TrimRight(base, "/") + "/services/" + id + "/"
}

// IsProxyBinding recognizes issued grants even after an offering has been
// removed from the catalog, so deletion cannot retain access as stored data.
func IsProxyBinding(base, id string, binding map[string]string) bool {
	return binding[ProxyEnvKey(id)] == ProxyBindingURL(base, id)
}

type proxyService struct{ name, description string }

func (p proxyService) Type() string          { return p.name }
func (p proxyService) CredentialKey() string { return ProxyEnvKey(p.name) }
func (p proxyService) Binding(in Input) map[string]string {
	return map[string]string{ProxyEnvKey(p.name): ProxyBindingURL(in.ServiceProxyURL, p.name)}
}
func (p proxyService) Describe(context.Context) (*addonv1.Offering, error) {
	desc := description(p, p.name, p.description, "api", "http", ObjectSchema(nil))
	desc.Shared, desc.Capabilities = true, nil
	return desc, nil
}
func (p proxyService) ExternalBinding(string) (map[string]string, error) {
	return nil, fmt.Errorf("%s is a configured proxy service; external URL bindings are not supported", p.name)
}
func (p proxyService) Workload(Input) (*Workload, error) { return nil, nil }
