package machine

import (
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"github.com/adrianliechti/donkey/internal/config"
	"github.com/adrianliechti/donkey/pkg/runtime"
)

const (
	testToken = "the-gateways-credential"
	testHost  = "machine.example.com"
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
// It learns that much and nothing more — not even whether a folder is open:
// a machine whose agent has not been started yet is up all the same.
func TestGuardHealth(t *testing.T) {
	door, seen := guarded(t)
	res := doorRequest(t, door, "GET", runtime.MachineHealthPath, nil)
	if res.StatusCode != http.StatusOK {
		t.Fatalf("status = %d", res.StatusCode)
	}
	if len(*seen) != 0 {
		t.Fatal("the health check was passed on to the agent")
	}
	idle := httptest.NewServer((&Guard{Token: testToken, Host: testHost, Upstream: &url.URL{Scheme: "http", Host: "127.0.0.1:1"}}).Handler())
	defer idle.Close()
	if res := doorRequest(t, idle, "GET", runtime.MachineHealthPath, nil); res.StatusCode != http.StatusOK {
		t.Fatalf("a machine with no folder open reads as %d", res.StatusCode)
	}
	// The owner who goes there anyway is told what is missing.
	res = doorRequest(t, idle, "GET", "/", map[string]string{config.MachineGuardHeader: testToken})
	body, _ := io.ReadAll(res.Body)
	if res.StatusCode != http.StatusBadGateway || !strings.Contains(string(body), "Nothing is open in this machine yet") {
		t.Fatalf("no folder open: %d %s", res.StatusCode, body)
	}
}

func TestAgentEnv(t *testing.T) {
	env := agentEnv([]string{
		"PATH=/bin", "DONKEY_MACHINE_GUARD_TOKEN=secret", "DONKEY_MACHINE_CONTROL_TOKEN=secret", "DONKEY_TOKEN=sealed",
		"OPENAI_BASE_URL=http://donkey-ai:8089/v1",
	}, map[string]string{"OPENAI_BASE_URL": "http://127.0.0.1:9001/v1", "NOT_SET": "stays unset"})
	joined := strings.Join(env, "\n")
	if strings.Contains(joined, "DONKEY_MACHINE_GUARD_TOKEN") || strings.Contains(joined, "DONKEY_MACHINE_CONTROL_TOKEN") {
		t.Error("the front door's credentials were handed to the agent")
	}
	if !strings.Contains(joined, "DONKEY_TOKEN=sealed") || !strings.Contains(joined, "WINGMAN_SANDBOX=off") {
		t.Errorf("env = %v", env)
	}
	// The door stands in for the model's address, and adds nothing the
	// machine was not given.
	if !strings.Contains(joined, "OPENAI_BASE_URL=http://127.0.0.1:9001/v1") || strings.Contains(joined, "donkey-ai") || strings.Contains(joined, "NOT_SET") {
		t.Errorf("env = %v", env)
	}
	// An image that decides otherwise is left alone.
	kept := agentEnv([]string{"WINGMAN_SANDBOX=on"}, nil)
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

// once does one request at the door without following what it answers.
func once(t *testing.T, door *httptest.Server, path string, headers map[string]string) *http.Response {
	t.Helper()
	req, err := http.NewRequest("GET", door.URL+path, nil)
	if err != nil {
		t.Fatal(err)
	}
	req.Host = testHost
	for name, value := range headers {
		req.Header.Set(name, value)
	}
	client := &http.Client{CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	res, err := client.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { res.Body.Close() })
	return res
}

var withCredential = map[string]string{config.MachineGuardHeader: testToken}

// The agent serves its HTML preview from a second loopback port. That port
// answers on the machine's own address, under a path, with the path taken
// off again before the request reaches it.
func TestGuardServesOtherLoopbackPortsUnderAPath(t *testing.T) {
	var seen []*http.Request
	second := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		seen = append(seen, r.Clone(r.Context()))
		if r.URL.Path == "/__start" {
			http.SetCookie(w, &http.Cookie{Name: "preview", Value: "signed", Path: "/", HttpOnly: true, SameSite: http.SameSiteStrictMode})
			http.Redirect(w, r, "/index.html", http.StatusSeeOther)
			return
		}
		_, _ = w.Write([]byte("second"))
	}))
	defer second.Close()
	port := second.Listener.Addr().(*net.TCPAddr).Port
	prefix := PortPrefix + strconv.Itoa(port)
	door, agent := guarded(t)

	res := once(t, door, prefix+"/a%20b/x%2Fy.js?q=1", map[string]string{
		config.MachineGuardHeader: testToken, "Origin": "https://" + testHost, "X-Forwarded-User": "owner",
	})
	if res.StatusCode != http.StatusOK || len(seen) != 1 || len(*agent) != 0 {
		t.Fatalf("status = %d, port saw %d requests, agent saw %d", res.StatusCode, len(seen), len(*agent))
	}
	got := seen[0]
	if got.URL.EscapedPath() != "/a%20b/x%2Fy.js" || got.URL.RawQuery != "q=1" {
		t.Errorf("the port was asked for %q?%s", got.URL.EscapedPath(), got.URL.RawQuery)
	}
	if got.Host != "127.0.0.1:"+strconv.Itoa(port) || got.Header.Get("Origin") != "http://"+got.Host {
		t.Errorf("host %q, origin %q — a development server only answers its own name", got.Host, got.Header.Get("Origin"))
	}
	if got.Header.Get(config.MachineGuardHeader) != "" || got.Header.Get("X-Forwarded-User") != "" {
		t.Error("the door's credential or the visitor's identity reached the port")
	}

	// The bare path is the port's root.
	once(t, door, prefix, withCredential)
	if len(seen) != 2 || seen[1].URL.Path != "/" {
		t.Fatalf("the bare prefix was asked as %q", seen[len(seen)-1].URL.Path)
	}

	// What the port redirects to and stores stays under its path: it shares
	// the address with the agent and with every other port.
	res = once(t, door, prefix+"/__start?token=abc&path=index.html", withCredential)
	if res.StatusCode != http.StatusSeeOther || res.Header.Get("Location") != prefix+"/index.html" {
		t.Errorf("redirect = %d %q", res.StatusCode, res.Header.Get("Location"))
	}
	cookies := res.Cookies()
	if len(cookies) != 1 || cookies[0].Path != prefix+"/" || !cookies[0].HttpOnly || cookies[0].SameSite != http.SameSiteStrictMode {
		t.Errorf("cookie = %+v", cookies)
	}
}

// The path is one more way to the machine's loopback, not a way around the
// door: it takes the same credential, reaches loopback ports only, and never
// the agent — whose pages would look for their scripts in the wrong place.
func TestGuardPortPathIsNotAWayAround(t *testing.T) {
	door, agent := guarded(t)
	if res := once(t, door, PortPrefix+"5173/", nil); res.StatusCode != http.StatusForbidden {
		t.Errorf("without the credential: %d", res.StatusCode)
	}
	for _, path := range []string{
		PortPrefix, PortPrefix + "/", PortPrefix + "0/", PortPrefix + "65536/", PortPrefix + "0080/",
		PortPrefix + "http/", PortPrefix + "80:81/", PortPrefix + "-1/", PortPrefix + "8080@evil.example/",
	} {
		if res := once(t, door, path, withCredential); res.StatusCode != http.StatusNotFound {
			t.Errorf("%s: %d, want 404", path, res.StatusCode)
		}
	}
	if len(*agent) != 0 {
		t.Fatalf("%d of those reached the agent", len(*agent))
	}
	// Nothing listening is said plainly.
	if res := once(t, door, PortPrefix+"1/", withCredential); res.StatusCode != http.StatusBadGateway {
		t.Errorf("a closed port: %d, want 502", res.StatusCode)
	}
}

// Software in the machine names itself by its loopback address — which, in
// the owner's browser, is the owner's own computer. Such a redirect is moved
// onto the machine's address; everything else is left as written.
func TestGuardPublicLocation(t *testing.T) {
	g := &Guard{Host: testHost, Upstream: &url.URL{Scheme: "http", Host: "127.0.0.1:9000"}}
	for _, tc := range []struct{ location, prefix, want string }{
		// What the agent answers a preview with.
		{"http://127.0.0.1:45678/__start?token=abc&path=site%2Findex.html", "", PortPrefix + "45678/__start?token=abc&path=site%2Findex.html"},
		{"http://localhost:5173/", "", PortPrefix + "5173/"},
		{"http://[::1]:5173/app#top", "", PortPrefix + "5173/app#top"},
		{"http://127.0.0.1:5173", "", PortPrefix + "5173/"},
		// The agent naming itself is the machine's own address.
		{"http://127.0.0.1:9000/api/x?y=1", "", "/api/x?y=1"},
		{"http://127.0.0.1:9000/api/x", PortPrefix + "5173", "/api/x"},
		// A path from the root stays with whoever wrote it.
		{"/login", "", "/login"},
		{"/index.html", PortPrefix + "45678", PortPrefix + "45678/index.html"},
		// One port sending the browser to another.
		{"http://127.0.0.1:3000/cb", PortPrefix + "5173", PortPrefix + "3000/cb"},
		// Not the door's to rewrite.
		{"index.html", PortPrefix + "45678", "index.html"},
		{"../up", "", "../up"},
		{"https://example.org/x", "", "https://example.org/x"},
		{"//example.org/x", PortPrefix + "45678", "//example.org/x"},
		{"https://127.0.0.1:8443/x", "", "https://127.0.0.1:8443/x"},
		{"http://10.0.0.5:5173/x", "", "http://10.0.0.5:5173/x"},
		{"http://127.0.0.1/x", "", "http://127.0.0.1/x"},
	} {
		if got := g.publicLocation(tc.location, tc.prefix); got != tc.want {
			t.Errorf("publicLocation(%q, %q) = %q, want %q", tc.location, tc.prefix, got, tc.want)
		}
	}
}

// The whole of what a preview does, at the door: the agent's redirect to its
// preview port comes back as a path on the machine's own address.
func TestGuardRewritesTheAgentsPreviewRedirect(t *testing.T) {
	agent := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, "http://127.0.0.1:45678/__start?token=abc&path=index.html", http.StatusTemporaryRedirect)
	}))
	defer agent.Close()
	upstream, _ := url.Parse(agent.URL)
	door := httptest.NewServer((&Guard{Token: testToken, Host: testHost, Upstream: upstream}).Handler())
	defer door.Close()
	res := once(t, door, "/preview?path=index.html", withCredential)
	if want := PortPrefix + "45678/__start?token=abc&path=index.html"; res.StatusCode != http.StatusTemporaryRedirect || res.Header.Get("Location") != want {
		t.Fatalf("redirect = %d %q, want %q", res.StatusCode, res.Header.Get("Location"), want)
	}
	// The agent's own port has no path of its own.
	if res := once(t, door, PortPrefix+upstream.Port()+"/api/workspace", withCredential); res.StatusCode != http.StatusNotFound {
		t.Fatalf("the agent under a port path: %d, want 404", res.StatusCode)
	}
}

// A fresh disk becomes its owner's home: the CLI knows its server, and the
// agent finds the donkey skill where agents look for a person's own.
func TestPrepareMakesAHome(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	workspace := filepath.Join(home, "workspace")
	env := map[string]string{"DONKEY_SERVER_URL": "http://donkey-server:8088/"}
	var log strings.Builder
	if err := prepare(Options{Workspace: workspace, Getenv: func(key string) string { return env[key] }, Log: &log}); err != nil {
		t.Fatal(err)
	}
	if info, err := os.Stat(workspace); err != nil || !info.IsDir() {
		t.Fatalf("no workspace: %v", err)
	}
	cfg, err := os.ReadFile(filepath.Join(home, ".config", "donkey", "config.yaml"))
	if err != nil || !strings.Contains(string(cfg), "url: http://donkey-server:8088\n") || strings.Contains(string(cfg), "token") {
		t.Fatalf("CLI config = %q (%v) — the address goes in, the sign-in stays in the environment", cfg, err)
	}
	skill, err := os.ReadFile(filepath.Join(home, ".agents", "skills", "donkey", "SKILL.md"))
	if err != nil || !strings.Contains(string(skill), "name: donkey") {
		t.Fatalf("the donkey skill was not installed: %v", err)
	}
	// A second start changes nothing and keeps what the owner did meanwhile.
	note := filepath.Join(workspace, "note")
	if err := os.WriteFile(note, []byte("mine"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := prepare(Options{Workspace: workspace, Getenv: func(key string) string { return env[key] }, Log: &log}); err != nil {
		t.Fatal(err)
	}
	if kept, _ := os.ReadFile(note); string(kept) != "mine" {
		t.Fatal("a restart touched the workspace")
	}
	if log.Len() != 0 {
		t.Errorf("prepare complained: %s", log.String())
	}
}
