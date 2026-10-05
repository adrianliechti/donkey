package addons

import (
	"context"
	"strings"
	"testing"

	"github.com/adrianliechti/donkey/internal/config"
)

func TestConfiguredProxyOfferingContract(t *testing.T) {
	cleanup, err := RegisterProxyServices(config.ProxyServices{{Name: "corporate-api", Description: "Enterprise API", TargetURL: "https://user:upstream-password@private.local/base", ProxyURL: "https://proxy:outbound-password@proxy.local"}})
	if err != nil {
		t.Fatal(err)
	}
	defer cleanup()
	entry, err := Get(context.Background(), "corporate-api")
	if err != nil {
		t.Fatal(err)
	}
	builtin := entry.(Builtin)
	desc, err := entry.Describe(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if desc.Protocol != "http" || !desc.Shared || desc.Description != "Enterprise API" || desc.Capabilities != nil {
		t.Fatalf("proxy descriptor = %+v", desc)
	}
	if text := desc.String(); strings.Contains(text, "upstream-password") || strings.Contains(text, "outbound-password") || strings.Contains(text, "private.local") {
		t.Fatal("catalog disclosed outbound credentials or addresses")
	}
	binding := builtin.Binding(Input{Password: "binding-token", ServiceProxyURL: "http://donkey-proxy:8091"})
	if binding["CORPORATE_API_URL"] != "http://donkey-proxy:8091/services/corporate-api/" || len(binding) != 1 {
		t.Fatalf("proxy binding = %v", binding)
	}
	if workload, err := builtin.Workload(input); workload != nil || err != nil {
		t.Fatal("proxy offerings must not create per-space workloads")
	}
	if _, err := builtin.ExternalBinding("https://unconfigured.local"); err == nil {
		t.Fatal("proxy offering accepted an app-chosen target")
	}
	if !IsBuiltin("corporate-api") {
		t.Fatal("proxy offering was not reserved from plugin catalogs")
	}
	if _, err := RegisterProxyServices(config.ProxyServices{{Name: "postgres", TargetURL: "https://example.com"}}); err == nil {
		t.Fatal("proxy offering shadowed a built-in")
	}
}
