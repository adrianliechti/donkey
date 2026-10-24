package addons

import (
	"encoding/json"
	"slices"
	"strings"
	"testing"
)

// Each protocol's tunnel reaches the CLI with the binding: its port, hint and
// how requests travel — never the server-side path and header rules.
func TestTunnelFor(t *testing.T) {
	if got := TunnelFor("", "postgresql"); got.HTTP || got.Port != 5432 || !strings.Contains(got.Hint, "$DATABASE_URL") {
		t.Errorf("postgres: %+v", got)
	}
	if got := TunnelFor("", "custom-thing"); got.HTTP || got.Port != 0 {
		t.Errorf("an unknown protocol should carry bytes to the service's own port: %+v", got)
	}
	trino := TunnelFor("", ProtocolTrino)
	if !trino.HTTP || !trino.UserToken || trino.URLKey != "TRINO_URL" || trino.Port != 8080 {
		t.Fatalf("trino: %+v", trino)
	}
	encoded, _ := json.Marshal(trino)
	for _, hidden := range []string{"paths", "dropHeaders", "/v1/statement", "X-Trino-User", "credential", "proxyGrant"} {
		if strings.Contains(string(encoded), hidden) {
			t.Errorf("server-side rule %q reached the CLI: %s", hidden, encoded)
		}
	}
	if !slices.Contains(HTTPTunnelProtocols(), ProtocolTrino) || slices.Contains(HTTPTunnelProtocols(), ProtocolPostgres) {
		t.Errorf("HTTP tunnel protocols: %v", HTTPTunnelProtocols())
	}
}

func TestTunnelAllows(t *testing.T) {
	trino := TunnelFor("", ProtocolTrino)
	for path, want := range map[string]bool{
		"v1/statement": true, "/v1/statement/queued/q1": true,
		"v1/statements": false, "v1/info": false, "/": false, "v1/statement/../info": false,
	} {
		if trino.Allows(path) != want {
			t.Errorf("Allows(%q) = %t", path, !want)
		}
	}
	if !(Tunnel{}).Allows("/anything") {
		t.Error("a tunnel without paths passes everything")
	}
}

// OpenAI and Exa tunnels add the space's key from the binding; the binding of
// an endpoint attached from elsewhere has none, so it gets no tunnel. A
// configured proxy names its URL entry after the offering.
func TestTunnelCredentials(t *testing.T) {
	openai := TunnelFor("openai", ProtocolOpenAI)
	if !openai.HTTP || openai.URLKey != "OPENAI_BASE_URL" {
		t.Fatalf("openai: %+v", openai)
	}
	name, value, ok := openai.CredentialHeader(map[string]string{"OPENAI_BASE_URL": "http://ai", "OPENAI_API_KEY": "space-key"})
	if !ok || name != "Authorization" || value != "Bearer space-key" {
		t.Errorf("managed openai: %q %q %t", name, value, ok)
	}
	if _, _, ok := openai.CredentialHeader(map[string]string{"OPENAI_BASE_URL": "https://api.example.com/v1"}); ok {
		t.Error("an attached endpoint has no key and must get no tunnel")
	}
	if name, value, ok := TunnelFor("exa", ProtocolExa).CredentialHeader(map[string]string{"EXA_API_KEY": "k"}); !ok || name != "x-api-key" || value != "k" {
		t.Errorf("exa: %q %q %t", name, value, ok)
	}
	if name, _, ok := TunnelFor("", ProtocolTrino).CredentialHeader(nil); !ok || name != "" {
		t.Error("a tunnel without a credential adds none")
	}
	proxy := TunnelFor("acme-erp", ProtocolHTTP)
	if !proxy.HTTP || !proxy.ProxyGrant || proxy.URLKey != "ACME_ERP_URL" || proxy.Port != 0 {
		t.Errorf("proxy: %+v", proxy)
	}
}
