package server

import (
	"bytes"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/adrianliechti/donkey/internal/addons"
	"github.com/adrianliechti/donkey/internal/ai"
	"github.com/adrianliechti/donkey/internal/config"
	"github.com/adrianliechti/donkey/internal/identity"
	"github.com/adrianliechti/donkey/internal/serviceproxy"
	"github.com/adrianliechti/donkey/pkg/runtime"
)

func tunnelCall(t *testing.T, s *Server, method, service, path, body string, header http.Header) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(method, "/v1/spaces/default/services/"+service+"/tunnel/"+path, strings.NewReader(body))
	for k, v := range header {
		req.Header[k] = v
	}
	asAdmin(t, req)
	rec := httptest.NewRecorder()
	s.routes().ServeHTTP(rec, req)
	return rec
}

// The platform's AI proxy takes the space's key: the tunnel adds the
// binding's, whatever the local client sends, and streams answers through.
// An endpoint attached from elsewhere has no key in its binding and gets no
// tunnel — donkey-server must not become a way into its own network.
func TestAITunnelAddsTheSpaceKey(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.RequestURI() != "/v1/chat/completions?stream=1" || r.Header.Get("Authorization") != "Bearer space-key" {
			t.Errorf("unexpected %s %s %q", r.Method, r.URL.RequestURI(), r.Header.Get("Authorization"))
		}
		if r.Header.Get("X-Access-Token") != "" || r.Header.Get(addons.TunnelAuthorizationHeader) != "" || r.Header.Get("X-Auth-Request-Preferred-Username") != "" {
			t.Error("leaked donkey's credentials or identity headers")
		}
		w.Header().Set("Content-Type", "text/event-stream")
		fmt.Fprint(w, "data: {}\n\n")
	}))
	defer upstream.Close()
	binding := map[string]string{"OPENAI_BASE_URL": upstream.URL + "/v1", "OPENAI_API_KEY": "space-key"}
	s := planServer(consoleRuntime{connection: runtime.ServiceConnection{Type: "openai", Protocol: "openai", Binding: binding}}, &ai.Analysis{})
	rec := tunnelCall(t, s, "POST", "openai", "chat/completions?stream=1", `{}`, http.Header{
		addons.TunnelAuthorizationHeader:    {"Bearer whatever-the-sdk-sends"},
		"X-Auth-Request-Preferred-Username": {"forged"},
	})
	if rec.Code != 200 || rec.Body.String() != "data: {}\n\n" {
		t.Fatalf("tunnel: %d %s", rec.Code, rec.Body)
	}

	attached := planServer(consoleRuntime{connection: runtime.ServiceConnection{Type: "openai", Protocol: "openai",
		Binding: map[string]string{"OPENAI_BASE_URL": "http://169.254.169.254/latest"}}}, &ai.Analysis{})
	if rec := tunnelCall(t, attached, "GET", "openai", "meta-data", "", nil); rec.Code != 400 || !strings.Contains(rec.Body.String(), "attached from elsewhere") {
		t.Fatalf("attached endpoint: %d %s", rec.Code, rec.Body)
	}
}

// A configured proxy service knows apps by their pod's address; the tunnel
// presents the console's grant for this space, offering, method and URL
// instead, and hands the local client's own Authorization to the upstream.
func TestProxyServiceTunnel(t *testing.T) {
	key := bytes.Repeat([]byte{8}, 32)
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		if r.Method != "PUT" || r.URL.RequestURI() != "/services/erp/items/1?x=yes" || string(body) != "payload" {
			t.Errorf("request = %s %s %q", r.Method, r.URL.RequestURI(), body)
		}
		if r.Header.Get("Authorization") != "Bearer app-key" || r.Header.Get("X-Access-Token") != "" {
			t.Errorf("unexpected credentials: %v", r.Header)
		}
		var grant struct{ Space, Offering, Method, URI string }
		if err := identity.Unseal(key, "donkey/proxy-test/v1", r.Header.Get(serviceproxy.TestRequestHeader), &grant); err != nil ||
			grant.Space != "default" || grant.Offering != "erp" || grant.Method != r.Method || grant.URI != r.URL.RequestURI() {
			t.Errorf("grant not scoped to this request: %+v %v", grant, err)
		}
		w.Header().Set("Set-Cookie", "provider=private")
		fmt.Fprint(w, "ok")
	}))
	defer upstream.Close()
	s := httpConsoleServer(upstream.URL, key)
	rec := tunnelCall(t, s, "PUT", "erp", "items/1?x=yes", "payload", http.Header{
		addons.TunnelAuthorizationHeader: {"Bearer app-key"},
		serviceproxy.TestRequestHeader:   {"forged"},
	})
	if rec.Code != 200 || rec.Body.String() != "ok" || rec.Header().Get("Set-Cookie") != "" {
		t.Fatalf("tunnel: %d %v %s", rec.Code, rec.Header(), rec.Body)
	}

	unconfigured := httpConsoleServer(upstream.URL, key)
	unconfigured.cfg.ProxyServices = config.ProxyServices{}
	if rec := tunnelCall(t, unconfigured, "GET", "erp", "", "", nil); rec.Code != 400 {
		t.Fatalf("an unconfigured proxy must get no tunnel: %d %s", rec.Code, rec.Body)
	}
}
