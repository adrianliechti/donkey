//go:build e2e

package e2e

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"html"
	"io"
	"net"
	"net/http"
	"net/http/cookiejar"
	"net/url"
	"os"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/adrianliechti/donkey/internal/server"
	"github.com/adrianliechti/donkey/internal/telemetry"
)

// Exercise an already deployed sample without rebuilding or changing its exposure.
func TestFastMCPTelemetry(t *testing.T) {
	space := os.Getenv("DONKEY_E2E_MCP_SPACE")
	if space == "" || os.Getenv("DONKEY_E2E_IDP") == "" {
		t.Skip("set DONKEY_E2E_MCP_SPACE and DONKEY_E2E_IDP to verify a deployed FastMCP sample")
	}
	s := newSuite(t)
	if s.edge == nil {
		t.Fatal("DONKEY_E2E_GATEWAY is required")
	}
	endpoint := "https://fastmcp-sample." + space + "." + s.domain + "/mcp"
	token := s.authorizeMCP(t, endpoint)
	s.callFastMCP(t, endpoint, token)
	s.waitFastMCPTelemetry(t, space)
}

func TestBuilderFastMCPOAuth(t *testing.T) {
	if os.Getenv("DONKEY_E2E_BUILDER") != "true" {
		t.Skip("set DONKEY_E2E_BUILDER=true to build and authorize the FastMCP sample")
	}
	if os.Getenv("DONKEY_E2E_IDP") == "" {
		t.Skip("the kind demo identity provider is required for the interactive OAuth round trip")
	}
	s := newSuite(t)
	if s.edge == nil {
		t.Fatal("DONKEY_E2E_GATEWAY is required")
	}
	dir := t.TempDir()
	copyDir(t, "../testdata/fastmcp-sample", dir)
	plan, base := s.deployBuilderSample(t, dir, "e2e-fastmcp", "", "api", "protected")
	if plan.App.Mode != "api" || plan.App.Exposure != "protected" {
		t.Fatalf("plan mode/exposure = %q/%q, want api/protected", plan.App.Mode, plan.App.Exposure)
	}
	token := s.authorizeMCP(t, base+"/mcp")
	s.callFastMCP(t, base+"/mcp", token)
	s.waitFastMCPTelemetry(t, "e2e-fastmcp")

	path := "/v1/spaces/e2e-fastmcp/apps/fastmcp-sample/network"
	setNetwork := func(exposure string) *server.NetworkResponse {
		t.Helper()
		var out struct {
			Network *server.NetworkResponse `json:"network"`
		}
		s.raw(t, http.MethodPut, path, map[string]string{
			"mode": "api", "exposure": exposure, "cors": "allowAll",
		}, &out)
		return out.Network
	}

	public := setNetwork("public")
	if public.Mode != "api" || public.Exposure != "public" {
		t.Fatalf("public API network state = %+v", public)
	}
	s.waitMCPStatus(t, base+"/mcp", http.StatusOK)
	s.callFastMCP(t, base+"/mcp", "")

	internal := setNetwork("internal")
	if internal.Mode != "api" || internal.Exposure != "internal" || internal.URL != "" {
		t.Fatalf("internal API network state = %+v", internal)
	}
	s.waitMCPStatus(t, base+"/mcp", http.StatusNotFound)

	protected := setNetwork("protected")
	if protected.Mode != "api" || protected.Exposure != "protected" {
		t.Fatalf("protected API network state = %+v", protected)
	}
	s.waitMCPStatus(t, base+"/mcp", http.StatusUnauthorized)
	s.callFastMCP(t, base+"/mcp", token)
}

func (s *suite) authorizeMCP(t *testing.T, resource string) string {
	t.Helper()
	client := s.mcpBrowserClient(t)

	// Start where a real MCP client starts: an unauthenticated protocol
	// request and its RFC 9728 resource_metadata challenge.
	var challenge *http.Response
	deadline := time.Now().Add(time.Minute)
	for time.Now().Before(deadline) {
		req, _ := http.NewRequest(http.MethodPost, resource, strings.NewReader(`{"jsonrpc":"2.0","id":1,"method":"initialize","params":{}}`))
		req.Header.Set("Content-Type", "application/json")
		challenge, _ = client.Do(req)
		if challenge != nil && challenge.StatusCode == http.StatusUnauthorized {
			break
		}
		if challenge != nil {
			challenge.Body.Close()
		}
		time.Sleep(2 * time.Second)
	}
	if challenge == nil || challenge.StatusCode != http.StatusUnauthorized {
		t.Fatalf("MCP endpoint did not produce an OAuth challenge: %+v", challenge)
	}
	if challenge.Header.Get("Access-Control-Allow-Origin") != "*" {
		t.Fatalf("MCP challenge did not include permissive CORS: %v", challenge.Header)
	}
	preflight, _ := http.NewRequest(http.MethodOptions, resource, nil)
	preflight.Header.Set("Origin", "https://client.example")
	preflight.Header.Set("Access-Control-Request-Method", "POST")
	preflight.Header.Set("Access-Control-Request-Headers", "authorization, content-type, mcp-protocol-version")
	preflightResponse, err := client.Do(preflight)
	if err != nil {
		t.Fatal(err)
	}
	preflightResponse.Body.Close()
	if preflightResponse.StatusCode != http.StatusNoContent || preflightResponse.Header.Get("Access-Control-Allow-Origin") != "*" {
		t.Fatalf("MCP CORS preflight = %d, headers=%v", preflightResponse.StatusCode, preflightResponse.Header)
	}
	challengeHeader := challenge.Header.Get("WWW-Authenticate")
	challenge.Body.Close()
	if strings.Contains(challengeHeader, "scope=") {
		t.Fatalf("minimal API authorization advertised a scope it does not use: %q", challengeHeader)
	}
	match := regexp.MustCompile(`resource_metadata="([^"]+)"`).FindStringSubmatch(challengeHeader)
	if len(match) != 2 {
		t.Fatalf("MCP challenge has no resource metadata: %q", challengeHeader)
	}

	metadata := getMCPJSON(t, client, match[1])
	if _, exists := metadata["scopes_supported"]; exists {
		t.Fatalf("minimal API authorization advertised scopes: %#v", metadata["scopes_supported"])
	}
	if metadata["resource"] != resource {
		t.Fatalf("protected resource metadata names %v, want %s", metadata["resource"], resource)
	}
	servers, ok := metadata["authorization_servers"].([]any)
	if !ok || len(servers) != 1 {
		t.Fatalf("authorization_servers = %#v", metadata["authorization_servers"])
	}
	issuer := servers[0].(string)
	discoveryPreflight := doMCPPreflight(t, client, issuer+"/.well-known/oauth-authorization-server", "GET", "mcp-protocol-version")
	if discoveryPreflight.StatusCode != http.StatusNoContent {
		t.Fatalf("authorization discovery CORS preflight: HTTP %d: %s", discoveryPreflight.StatusCode, discoveryPreflight.Body)
	}
	authorization := getMCPJSON(t, client, issuer+"/.well-known/oauth-authorization-server")

	redirect := "http://127.0.0.1:49152/callback"
	registrationBody, _ := json.Marshal(map[string]any{
		"client_name": "Donkey FastMCP e2e", "redirect_uris": []string{redirect},
		"grant_types": []string{"authorization_code", "refresh_token"}, "response_types": []string{"code"},
		"token_endpoint_auth_method": "none", "application_type": "native",
	})
	registration := doMCP(t, client, http.MethodPost, authorization["registration_endpoint"].(string), "application/json", registrationBody)
	if registration.StatusCode != http.StatusCreated {
		t.Fatalf("client registration: HTTP %d: %s", registration.StatusCode, registration.Body)
	}
	var registered map[string]any
	if err := json.Unmarshal(registration.Body, &registered); err != nil {
		t.Fatal(err)
	}
	clientID, _ := registered["client_id"].(string)
	if clientID == "" {
		t.Fatalf("registration returned no client_id: %s", registration.Body)
	}

	verifier := strings.Repeat("fastmcp-e2e-verifier-", 3)
	digest := sha256.Sum256([]byte(verifier))
	values := url.Values{
		"response_type": {"code"}, "client_id": {clientID}, "redirect_uri": {redirect},
		"resource": {resource}, "state": {"e2e-state"},
		"code_challenge": {base64.RawURLEncoding.EncodeToString(digest[:])}, "code_challenge_method": {"S256"},
	}
	authorizeURL := authorization["authorization_endpoint"].(string) + "?" + values.Encode()

	// Donkey delegates the human login to the installation's existing OIDC
	// provider. The demo provider has one visible Sign in link, which this test
	// clicks before returning to Donkey's consent screen.
	start := doMCP(t, client, http.MethodGet, authorizeURL, "", nil)
	if start.StatusCode != http.StatusFound {
		t.Fatalf("authorization did not start OIDC: HTTP %d: %s", start.StatusCode, start.Body)
	}
	idpLogin := resolveLocation(t, authorizeURL, start.Location)
	loginPage := doMCP(t, client, http.MethodGet, idpLogin, "", nil)
	if loginPage.StatusCode != http.StatusOK {
		t.Fatalf("IdP login page: HTTP %d: %s", loginPage.StatusCode, loginPage.Body)
	}
	approveMatch := regexp.MustCompile(`href="([^"]*/approve\?[^"]+)"`).FindSubmatch(loginPage.Body)
	if len(approveMatch) != 2 {
		t.Fatalf("IdP approval link is missing: %s", loginPage.Body)
	}
	idpApprove := resolveLocation(t, idpLogin, html.UnescapeString(string(approveMatch[1])))
	approved := doMCP(t, client, http.MethodGet, idpApprove, "", nil)
	if approved.StatusCode != http.StatusFound {
		t.Fatalf("IdP approval: HTTP %d: %s", approved.StatusCode, approved.Body)
	}
	callbackURL := resolveLocation(t, idpApprove, approved.Location)
	callback := doMCP(t, client, http.MethodGet, callbackURL, "", nil)
	if callback.StatusCode != http.StatusFound {
		t.Fatalf("OIDC callback: HTTP %d: %s", callback.StatusCode, callback.Body)
	}
	consentURL := resolveLocation(t, callbackURL, callback.Location)
	consentPage := doMCP(t, client, http.MethodGet, consentURL, "", nil)
	if consentPage.StatusCode != http.StatusOK || !bytes.Contains(consentPage.Body, []byte("Allow Donkey FastMCP e2e")) {
		t.Fatalf("Donkey consent page: HTTP %d: %s", consentPage.StatusCode, consentPage.Body)
	}
	consentMatch := regexp.MustCompile(`name="request" value="([^"]+)"`).FindSubmatch(consentPage.Body)
	if len(consentMatch) != 2 {
		t.Fatalf("consent request is missing: %s", consentPage.Body)
	}
	consentForm := url.Values{"request": {string(consentMatch[1])}, "decision": {"approve"}}
	consent := doMCP(t, client, http.MethodPost, authorization["authorization_endpoint"].(string), "application/x-www-form-urlencoded", []byte(consentForm.Encode()))
	if consent.StatusCode != http.StatusFound {
		t.Fatalf("consent approval: HTTP %d: %s", consent.StatusCode, consent.Body)
	}
	clientCallback, err := url.Parse(consent.Location)
	if err != nil || clientCallback.Query().Get("state") != "e2e-state" || clientCallback.Query().Get("iss") != issuer {
		t.Fatalf("bad client callback: %s", consent.Location)
	}

	tokenForm := url.Values{
		"grant_type": {"authorization_code"}, "code": {clientCallback.Query().Get("code")},
		"client_id": {clientID}, "redirect_uri": {redirect}, "resource": {resource}, "code_verifier": {verifier},
	}
	tokenResponse := doMCP(t, client, http.MethodPost, authorization["token_endpoint"].(string), "application/x-www-form-urlencoded", []byte(tokenForm.Encode()))
	if tokenResponse.StatusCode != http.StatusOK {
		t.Fatalf("token exchange: HTTP %d: %s", tokenResponse.StatusCode, tokenResponse.Body)
	}
	var token struct {
		AccessToken  string `json:"access_token"`
		RefreshToken string `json:"refresh_token"`
	}
	if json.Unmarshal(tokenResponse.Body, &token) != nil || token.AccessToken == "" || token.RefreshToken == "" {
		t.Fatalf("token exchange returned no access token: %s", tokenResponse.Body)
	}
	refreshForm := url.Values{
		"grant_type": {"refresh_token"}, "refresh_token": {token.RefreshToken},
		"client_id": {clientID}, "resource": {resource},
	}
	refreshResponse := doMCP(t, client, http.MethodPost, authorization["token_endpoint"].(string), "application/x-www-form-urlencoded", []byte(refreshForm.Encode()))
	if refreshResponse.StatusCode != http.StatusOK {
		t.Fatalf("refresh exchange: HTTP %d: %s", refreshResponse.StatusCode, refreshResponse.Body)
	}
	var refreshed struct {
		AccessToken  string `json:"access_token"`
		RefreshToken string `json:"refresh_token"`
	}
	if json.Unmarshal(refreshResponse.Body, &refreshed) != nil || refreshed.AccessToken == "" || refreshed.RefreshToken == "" || refreshed.RefreshToken == token.RefreshToken {
		t.Fatalf("refresh exchange did not rotate tokens: %s", refreshResponse.Body)
	}
	return refreshed.AccessToken
}

type mcpResponse struct {
	StatusCode int
	Location   string
	Body       []byte
}

func doMCP(t *testing.T, client *http.Client, method, target, contentType string, body []byte) mcpResponse {
	t.Helper()
	req, err := http.NewRequest(method, target, bytes.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	if contentType != "" {
		req.Header.Set("Content-Type", contentType)
	}
	res, err := client.Do(req)
	if err != nil {
		t.Fatalf("%s %s: %v", method, target, err)
	}
	defer res.Body.Close()
	data, err := io.ReadAll(res.Body)
	if err != nil {
		t.Fatal(err)
	}
	return mcpResponse{StatusCode: res.StatusCode, Location: res.Header.Get("Location"), Body: data}
}

func doMCPPreflight(t *testing.T, client *http.Client, target, method, headers string) mcpResponse {
	t.Helper()
	req, err := http.NewRequest(http.MethodOptions, target, nil)
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Origin", "https://browser-client.example")
	req.Header.Set("Access-Control-Request-Method", method)
	if headers != "" {
		req.Header.Set("Access-Control-Request-Headers", headers)
	}
	res, err := client.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer res.Body.Close()
	body, _ := io.ReadAll(res.Body)
	if res.Header.Get("Access-Control-Allow-Origin") != "*" {
		t.Fatalf("CORS preflight returned no allow-origin header: %v", res.Header)
	}
	return mcpResponse{StatusCode: res.StatusCode, Location: res.Header.Get("Location"), Body: body}
}

func getMCPJSON(t *testing.T, client *http.Client, target string) map[string]any {
	t.Helper()
	res := doMCP(t, client, http.MethodGet, target, "", nil)
	if res.StatusCode != http.StatusOK {
		t.Fatalf("GET %s: HTTP %d: %s", target, res.StatusCode, res.Body)
	}
	var out map[string]any
	if err := json.Unmarshal(res.Body, &out); err != nil {
		t.Fatalf("GET %s returned invalid JSON: %v: %s", target, err, res.Body)
	}
	return out
}

func resolveLocation(t *testing.T, base, location string) string {
	t.Helper()
	b, err := url.Parse(base)
	if err != nil {
		t.Fatal(err)
	}
	l, err := url.Parse(location)
	if err != nil {
		t.Fatal(err)
	}
	return b.ResolveReference(l).String()
}

func (s *suite) mcpBrowserClient(t *testing.T) *http.Client {
	t.Helper()
	gateway := os.Getenv("DONKEY_E2E_GATEWAY")
	idp := os.Getenv("DONKEY_E2E_IDP")
	base, ok := s.edge.Transport.(*http.Transport)
	if !ok {
		t.Fatal("e2e edge transport is not HTTP")
	}
	transport := base.Clone()
	dialer := &net.Dialer{Timeout: 10 * time.Second}
	transport.DialContext = func(ctx context.Context, network, address string) (net.Conn, error) {
		host, _, _ := net.SplitHostPort(address)
		if host == "auth.donkey-identity" || host == "auth."+s.domain {
			return dialer.DialContext(ctx, network, idp)
		}
		return dialer.DialContext(ctx, network, gateway)
	}
	jar, err := cookiejar.New(nil)
	if err != nil {
		t.Fatal(err)
	}
	return &http.Client{
		Timeout: 30 * time.Second, Transport: transport, Jar: jar,
		CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
	}
}

func (s *suite) callFastMCP(t *testing.T, endpoint, token string) {
	t.Helper()
	call := func(id int, method string, params any) []byte {
		body, _ := json.Marshal(map[string]any{"jsonrpc": "2.0", "id": id, "method": method, "params": params})
		req, _ := http.NewRequest(http.MethodPost, endpoint, bytes.NewReader(body))
		if token != "" {
			req.Header.Set("Authorization", "Bearer "+token)
		}
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("Accept", "application/json, text/event-stream")
		req.Header.Set("MCP-Protocol-Version", "2025-06-18")
		res, err := s.edge.Do(req)
		if err != nil {
			t.Fatalf("MCP %s: %v", method, err)
		}
		defer res.Body.Close()
		data, _ := io.ReadAll(res.Body)
		if res.StatusCode != http.StatusOK {
			t.Fatalf("MCP %s: HTTP %d: %s", method, res.StatusCode, data)
		}
		if origins := res.Header.Values("Access-Control-Allow-Origin"); len(origins) != 1 || origins[0] != "*" {
			t.Fatalf("MCP %s returned invalid CORS origins: %q", method, origins)
		}
		return data
	}
	initialized := call(1, "initialize", map[string]any{
		"protocolVersion": "2025-06-18", "capabilities": map[string]any{},
		"clientInfo": map[string]string{"name": "donkey-e2e", "version": "1"},
	})
	if !bytes.Contains(initialized, []byte(`"serverInfo"`)) {
		t.Fatalf("FastMCP initialize response: %s", initialized)
	}
	tools := call(2, "tools/list", map[string]any{})
	if !bytes.Contains(tools, []byte(`"name":"add"`)) {
		t.Fatalf("FastMCP tools/list response: %s", tools)
	}
	result := call(3, "tools/call", map[string]any{"name": "add", "arguments": map[string]int{"a": 3, "b": 4}})
	if !bytes.Contains(result, []byte("7")) || bytes.Contains(result, []byte(`"isError":true`)) {
		t.Fatalf("FastMCP add response: %s", result)
	}
	failed := call(4, "tools/call", map[string]any{"name": "divide", "arguments": map[string]int{"a": 1, "b": 0}})
	if !bytes.Contains(failed, []byte(`"isError":true`)) {
		t.Fatalf("FastMCP divide(1, 0) did not return a tool error: %s", failed)
	}
	t.Logf("FastMCP add(3, 4) and divide(1, 0) passed at %s", endpoint)
}

func (s *suite) waitFastMCPTelemetry(t *testing.T, space string) {
	t.Helper()
	var report telemetry.TrafficReport
	deadline := time.Now().Add(90 * time.Second)
	for time.Now().Before(deadline) {
		s.raw(t, http.MethodGet, "/v1/spaces/"+space+"/apps/fastmcp-sample/traffic?range=48h&source=edge", nil, &report)
		var add, divide bool
		for _, operation := range report.MCP.Operations {
			if operation.Method == "tools/call" && operation.Target == "add" && operation.Operations > 0 && operation.Errors == 0 {
				add = true
			}
			if operation.Method == "tools/call" && operation.Target == "divide" && operation.Errors > 0 {
				divide = true
			}
		}
		if add && divide && report.Totals.AuthenticatedCallers > 0 && report.Totals.Requests > 0 {
			t.Logf("FastMCP telemetry: %d HTTP requests, %d authenticated callers, %d MCP operations, %d operation errors",
				report.Totals.Requests, report.Totals.AuthenticatedCallers, report.MCP.Totals.Operations, report.MCP.Totals.Errors)
			return
		}
		time.Sleep(2 * time.Second)
	}
	t.Fatalf("FastMCP telemetry did not arrive: %+v", report)
}

func (s *suite) waitMCPStatus(t *testing.T, endpoint string, want int) {
	t.Helper()
	deadline := time.Now().Add(time.Minute)
	var last int
	for time.Now().Before(deadline) {
		body := []byte(`{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":"2025-06-18","capabilities":{},"clientInfo":{"name":"donkey-e2e","version":"1"}}}`)
		req, _ := http.NewRequest(http.MethodPost, endpoint, bytes.NewReader(body))
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("Accept", "application/json, text/event-stream")
		req.Header.Set("MCP-Protocol-Version", "2025-06-18")
		res, err := s.edge.Do(req)
		if err == nil {
			last = res.StatusCode
			res.Body.Close()
			if last == want {
				return
			}
		}
		time.Sleep(2 * time.Second)
	}
	t.Fatalf("MCP endpoint status = %d, want %d", last, want)
}
