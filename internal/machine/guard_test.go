package machine

import (
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"

	"github.com/adrianliechti/donkey/internal/config"
	"github.com/adrianliechti/donkey/pkg/runtime"
)

const (
	testToken = "the-gateways-credential"
	testHost  = "m0123456789abcdef.platform.example.com"
)

// guarded is a front door before an agent that records what reaches it.
func guarded(t *testing.T) (*httptest.Server, *[]*http.Request) {
	t.Helper()
	var seen []*http.Request
	agent := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		seen = append(seen, r.Clone(r.Context()))
		_, _ = w.Write([]byte("agent"))
	}))
	t.Cleanup(agent.Close)
	upstream, err := url.Parse(agent.URL)
	if err != nil {
		t.Fatal(err)
	}
	door := httptest.NewServer((&Guard{Token: testToken, Host: testHost, Upstream: upstream}).Handler())
	t.Cleanup(door.Close)
	return door, &seen
}

func doorRequest(t *testing.T, door *httptest.Server, method, path string, headers map[string]string) *http.Response {
	t.Helper()
	req, err := http.NewRequest(method, door.URL+path, nil)
	if err != nil {
		t.Fatal(err)
	}
	req.Host = testHost
	for name, value := range headers {
		req.Header.Set(name, value)
	}
	res, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { res.Body.Close() })
	return res
}

// The agent trusts whoever reaches it, so reaching the machine must not be
// enough: without the gateway's credential nothing gets past the door.
func TestGuardRequiresTheGatewaysCredential(t *testing.T) {
	door, seen := guarded(t)
	for name, headers := range map[string]map[string]string{
		"no credential":    nil,
		"wrong credential": {config.MachineGuardHeader: "guess"},
		"empty credential": {config.MachineGuardHeader: ""},
		// What a neighbour might try instead.
		"bearer":           {"Authorization": "Bearer " + testToken},
		"identity headers": {"X-Forwarded-User": "owner", "X-Forwarded-Email": "owner@example.com"},
	} {
		t.Run(name, func(t *testing.T) {
			if res := doorRequest(t, door, "GET", "/api/workspace", headers); res.StatusCode != http.StatusForbidden {
				t.Fatalf("status = %d, want 403", res.StatusCode)
			}
		})
	}
	if len(*seen) != 0 {
		t.Fatalf("%d requests reached the agent", len(*seen))
	}
	// A door without a credential configured opens for nobody, rather than
	// for everybody who sends none.
	open := httptest.NewServer((&Guard{Host: testHost, Upstream: &url.URL{Scheme: "http", Host: "127.0.0.1:1"}}).Handler())
	defer open.Close()
	if res := doorRequest(t, open, "GET", "/", nil); res.StatusCode != http.StatusForbidden {
		t.Fatalf("a door without a credential answered %d", res.StatusCode)
	}
}

// The agent only answers to a loopback name and checks a page's origin
// against it, so both are respelled — after the public ones were checked.
// What the gateway said about the visitor stops at the door.
func TestGuardRewritesForTheAgent(t *testing.T) {
	door, seen := guarded(t)
	res := doorRequest(t, door, "POST", "/api/workspace/search?q=1", map[string]string{
		config.MachineGuardHeader: testToken,
		"Origin":                  "https://" + testHost,
		"X-Forwarded-User":        "owner",
		"X-Forwarded-Email":       "owner@example.com",
		"X-Auth-Request-User":     "owner",
		"Cookie":                  "theme=dark",
	})
	if res.StatusCode != http.StatusOK || len(*seen) != 1 {
		t.Fatalf("status = %d, requests = %d", res.StatusCode, len(*seen))
	}
	got := (*seen)[0]
	if !strings.HasPrefix(got.Host, "127.0.0.1:") {
		t.Errorf("the agent was asked as %q, which it refuses", got.Host)
	}
	if origin := got.Header.Get("Origin"); origin != "http://"+got.Host {
		t.Errorf("origin = %q, want the agent's own (http://%s)", origin, got.Host)
	}
	if got.URL.RequestURI() != "/api/workspace/search?q=1" {
		t.Errorf("path = %q", got.URL.RequestURI())
	}
	for _, name := range []string{config.MachineGuardHeader, "X-Forwarded-User", "X-Forwarded-Email", "X-Auth-Request-User"} {
		if got.Header.Get(name) != "" {
			t.Errorf("%s reached the agent", name)
		}
	}
	if got.Header.Get("Cookie") != "theme=dark" {
		t.Errorf("the agent's own cookie was lost: %q", got.Header.Get("Cookie"))
	}
}

// A page of another site must not drive the agent with the owner's session —
// and rewriting its origin would be exactly how it could.
func TestGuardRefusesOtherOrigins(t *testing.T) {
	door, seen := guarded(t)
	for _, origin := range []string{
		"https://evil.example.org",
		"https://example.com",        // the console embeds the machine; it never calls it
		"http://" + testHost,         // not the address the gateway serves
		"https://" + testHost + ":8", // another port is another origin
		"null",
	} {
		res := doorRequest(t, door, "POST", "/api/session", map[string]string{
			config.MachineGuardHeader: testToken, "Origin": origin,
		})
		if res.StatusCode != http.StatusForbidden {
			t.Errorf("origin %q: status = %d, want 403", origin, res.StatusCode)
		}
	}
	if len(*seen) != 0 {
		t.Fatalf("%d cross-origin requests reached the agent", len(*seen))
	}
}

// The runtime asks whether the machine is up without holding the credential.
// It learns that much and nothing more.
func TestGuardHealth(t *testing.T) {
	door, seen := guarded(t)
	res := doorRequest(t, door, "GET", runtime.MachineHealthPath, nil)
	if res.StatusCode != http.StatusOK {
		t.Fatalf("status = %d", res.StatusCode)
	}
	if len(*seen) != 0 {
		t.Fatal("the health check was passed on to the agent")
	}
	down := httptest.NewServer((&Guard{Token: testToken, Host: testHost, Upstream: &url.URL{Scheme: "http", Host: "127.0.0.1:1"}}).Handler())
	defer down.Close()
	if res := doorRequest(t, down, "GET", runtime.MachineHealthPath, nil); res.StatusCode != http.StatusServiceUnavailable {
		t.Fatalf("an agent that is not listening reads as %d", res.StatusCode)
	}
}

func TestAgentEnv(t *testing.T) {
	env := agentEnv([]string{"PATH=/bin", "DONKEY_MACHINE_GUARD_TOKEN=secret", "DONKEY_TOKEN=sealed"})
	joined := strings.Join(env, "\n")
	if strings.Contains(joined, "DONKEY_MACHINE_GUARD_TOKEN") {
		t.Error("the front door's credential was handed to the agent")
	}
	if !strings.Contains(joined, "DONKEY_TOKEN=sealed") || !strings.Contains(joined, "WINGMAN_SANDBOX=off") {
		t.Errorf("env = %v", env)
	}
	// An image that decides otherwise is left alone.
	kept := agentEnv([]string{"WINGMAN_SANDBOX=on"})
	if len(kept) != 1 || kept[0] != "WINGMAN_SANDBOX=on" {
		t.Errorf("env = %v", kept)
	}
}

func TestOptionsRefuseAnAgentOffTheLoopback(t *testing.T) {
	for _, upstream := range []string{"http://0.0.0.0:9000", "http://10.0.0.5:9000", "http://agent:9000", "https://127.0.0.1:9000", "http://127.0.0.1"} {
		opts := Options{Upstream: upstream}
		if err := opts.defaults(); err == nil {
			t.Errorf("%s accepted", upstream)
		}
	}
	opts := Options{}
	if err := opts.defaults(); err != nil {
		t.Fatal(err)
	}
	if got := strings.Join(opts.Command, " "); got != "wingman server --no-browser --port 9000" {
		t.Errorf("agent = %q", got)
	}
}
