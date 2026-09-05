package gateway

import (
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/adrianliechti/donkey/internal/config"
	"github.com/adrianliechti/donkey/internal/identity"
)

func mcpTestServer(t *testing.T, backend *url.URL) *Server {
	t.Helper()
	return &Server{
		cfg: &Config{
			SessionKey: testKey, Domain: "auth.example", ServerHost: "auth.example",
			OIDCIssuer: "https://identity.example", OIDCClientID: "donkey", OIDCClientSecret: "secret",
		},
		routes: map[string][]route{
			"tools.example": {{Space: "acme", App: "tools", Host: "tools.example", Path: "/", Backend: backend, Login: true, Mode: config.AppModeAPI, CORS: config.CORSAllowAll}},
		},
		oauthCodes: map[string]oauthAuthorizationCode{},
	}
}

func mcpRequest(t *testing.T, h http.Handler, method, target, host, body string, contentType string, cookies ...*http.Cookie) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(method, target, strings.NewReader(body))
	req.Host = host
	if contentType != "" {
		req.Header.Set("Content-Type", contentType)
	}
	for _, cookie := range cookies {
		req.AddCookie(cookie)
	}
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	return rec
}

func registerMCPClient(t *testing.T, h http.Handler, redirect string) string {
	t.Helper()
	body, _ := json.Marshal(map[string]any{
		"client_name": "Test MCP client", "redirect_uris": []string{redirect},
		"grant_types": []string{"authorization_code", "refresh_token"}, "response_types": []string{"code"},
		"token_endpoint_auth_method": "none",
	})
	rec := mcpRequest(t, h, http.MethodPost, "https://auth.example/oauth2/register", "auth.example", string(body), "application/json")
	if rec.Code != http.StatusCreated {
		t.Fatalf("register status = %d: %s", rec.Code, rec.Body.String())
	}
	var response struct {
		ClientID string `json:"client_id"`
	}
	if json.Unmarshal(rec.Body.Bytes(), &response) != nil || !strings.HasPrefix(response.ClientID, "donkey_api_") {
		t.Fatalf("bad registration response: %s", rec.Body.String())
	}
	return response.ClientID
}

func TestMCPDiscoveryAndChallenge(t *testing.T) {
	target, _ := url.Parse("http://app.internal:8000")
	s := mcpTestServer(t, target)
	h := s.handler()

	metadata := mcpRequest(t, h, http.MethodGet, "https://tools.example/.well-known/oauth-protected-resource/mcp", "tools.example", "", "")
	if metadata.Code != http.StatusOK {
		t.Fatalf("metadata status = %d: %s", metadata.Code, metadata.Body.String())
	}
	var doc struct {
		Resource             string   `json:"resource"`
		AuthorizationServers []string `json:"authorization_servers"`
	}
	if err := json.Unmarshal(metadata.Body.Bytes(), &doc); err != nil {
		t.Fatal(err)
	}
	if doc.Resource != "https://tools.example/mcp" || len(doc.AuthorizationServers) != 1 || doc.AuthorizationServers[0] != "https://auth.example" {
		t.Fatalf("unexpected protected-resource metadata: %+v", doc)
	}

	challenge := mcpRequest(t, h, http.MethodPost, "https://tools.example/mcp", "tools.example", `{}`, "application/json")
	if challenge.Code != http.StatusUnauthorized {
		t.Fatalf("challenge status = %d: %s", challenge.Code, challenge.Body.String())
	}
	if challenge.Header().Get("Access-Control-Allow-Origin") != "*" {
		t.Fatalf("CORS missing from MCP challenge: %v", challenge.Header())
	}
	want := `resource_metadata="https://tools.example/.well-known/oauth-protected-resource/mcp"`
	if !strings.Contains(challenge.Header().Get("WWW-Authenticate"), want) {
		t.Fatalf("challenge = %q, want %s", challenge.Header().Get("WWW-Authenticate"), want)
	}
	if strings.Contains(challenge.Header().Get("WWW-Authenticate"), "scope=") || strings.Contains(metadata.Body.String(), "scopes_supported") {
		t.Fatalf("minimal MCP authorization must not invent a scope: challenge=%q metadata=%s", challenge.Header().Get("WWW-Authenticate"), metadata.Body.String())
	}

	as := mcpRequest(t, h, http.MethodGet, "https://auth.example/.well-known/oauth-authorization-server", "auth.example", "", "")
	if as.Code != http.StatusOK || !strings.Contains(as.Body.String(), `"code_challenge_methods_supported":["S256"]`) {
		t.Fatalf("authorization metadata = %d: %s", as.Code, as.Body.String())
	}
	if strings.Contains(as.Body.String(), "scopes_supported") {
		t.Fatalf("authorization server metadata advertised invented scopes: %s", as.Body.String())
	}
}

func TestAPIModeUsesTheSameResourceBoundFlow(t *testing.T) {
	target, _ := url.Parse("http://app.internal:8000")
	s := mcpTestServer(t, target)
	s.routes["api.example"] = []route{{
		Space: "acme", App: "api", Host: "api.example", Path: "/", Backend: target,
		Login: true, Mode: config.AppModeAPI,
	}}
	h := s.handler()

	metadata := mcpRequest(t, h, http.MethodGet, "https://api.example/.well-known/oauth-protected-resource/v1", "api.example", "", "")
	if metadata.Code != http.StatusOK || !strings.Contains(metadata.Body.String(), `"resource":"https://api.example/v1"`) {
		t.Fatalf("OAuth API metadata = %d: %s", metadata.Code, metadata.Body.String())
	}
	challenge := mcpRequest(t, h, http.MethodGet, "https://api.example/v1", "api.example", "", "")
	if challenge.Code != http.StatusUnauthorized || !strings.Contains(challenge.Header().Get("WWW-Authenticate"), "resource_metadata=") {
		t.Fatalf("OAuth API challenge = %d, %q", challenge.Code, challenge.Header().Get("WWW-Authenticate"))
	}
}

func TestPublicAPIModeProxiesWithoutOAuth(t *testing.T) {
	called := false
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		called = true
		w.WriteHeader(http.StatusNoContent)
	}))
	defer backend.Close()
	target, _ := url.Parse(backend.URL)
	s := mcpTestServer(t, target)
	s.routes["public-api.example"] = []route{{
		Space: "acme", App: "api", Host: "public-api.example", Path: "/", Backend: target,
		Mode: config.AppModeAPI,
	}}
	h := s.handler()

	response := mcpRequest(t, h, http.MethodGet, "https://public-api.example/v1", "public-api.example", "", "")
	if response.Code != http.StatusNoContent || !called {
		t.Fatalf("public API response = %d, backend called = %v", response.Code, called)
	}
	metadata := mcpRequest(t, h, http.MethodGet, "https://public-api.example/.well-known/oauth-protected-resource/v1", "public-api.example", "", "")
	if metadata.Code != http.StatusNotFound {
		t.Fatalf("public API advertised protected-resource metadata: %d %s", metadata.Code, metadata.Body.String())
	}
}

func TestAPIResourceAcceptsCanonicalHostCaseAndPort(t *testing.T) {
	target, _ := url.Parse("http://app.internal:8000")
	s := mcpTestServer(t, target)
	req := httptest.NewRequest(http.MethodGet, "https://auth.example/oauth2/authorize", nil)

	resource, _, ok := s.validOAuthResource(req, "HTTPS://TOOLS.EXAMPLE:8443/mcp")
	if !ok || resource != "https://tools.example:8443/mcp" {
		t.Fatalf("canonical resource = %q, ok=%v", resource, ok)
	}
}

func TestMCPCORSPreflightDoesNotRequireAuthorization(t *testing.T) {
	target, _ := url.Parse("http://app.internal:8000")
	h := mcpTestServer(t, target).handler()
	req := httptest.NewRequest(http.MethodOptions, "https://tools.example/mcp", nil)
	req.Host = "tools.example"
	req.Header.Set("Origin", "https://client.example")
	req.Header.Set("Access-Control-Request-Method", "POST")
	req.Header.Set("Access-Control-Request-Headers", "authorization, content-type, mcp-protocol-version")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusNoContent || rec.Header().Get("Access-Control-Allow-Origin") != "*" {
		t.Fatalf("preflight = %d, headers=%v", rec.Code, rec.Header())
	}
	if got := rec.Header().Get("Access-Control-Allow-Headers"); got != "authorization, content-type, mcp-protocol-version" {
		t.Fatalf("allowed headers = %q", got)
	}
}

func TestOAuthServerDiscoveryCORSPreflightDoesNotRequireLogin(t *testing.T) {
	target, _ := url.Parse("http://app.internal:8000")
	h := mcpTestServer(t, target).handler()
	req := httptest.NewRequest(http.MethodOptions, "https://auth.example/.well-known/oauth-authorization-server", nil)
	req.Host = "auth.example"
	req.Header.Set("Origin", "https://browser-client.example")
	req.Header.Set("Access-Control-Request-Method", "GET")
	req.Header.Set("Access-Control-Request-Headers", "mcp-protocol-version")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusNoContent || rec.Header().Get("Access-Control-Allow-Origin") != "*" {
		t.Fatalf("authorization discovery preflight = %d, headers=%v", rec.Code, rec.Header())
	}
	if got := rec.Header().Get("Access-Control-Allow-Headers"); got != "mcp-protocol-version" {
		t.Fatalf("allowed headers = %q", got)
	}
}

func TestMCPRegistrationRejectsUnsafeRedirect(t *testing.T) {
	target, _ := url.Parse("http://app.internal:8000")
	h := mcpTestServer(t, target).handler()
	for _, redirect := range []string{"http://client.example/callback", "javascript:alert(1)", "https://client.example/callback#fragment"} {
		body, _ := json.Marshal(map[string]any{"redirect_uris": []string{redirect}})
		rec := mcpRequest(t, h, http.MethodPost, "https://auth.example/oauth2/register", "auth.example", string(body), "application/json")
		if rec.Code != http.StatusBadRequest {
			t.Errorf("redirect %q registered with status %d", redirect, rec.Code)
		}
	}
	for _, redirect := range []string{"http://localhost:31337/callback", "http://127.0.0.1:31337/callback", "https://client.example/callback"} {
		registerMCPClient(t, h, redirect)
	}
}

func TestMCPAuthorizationCodeFlowAndTokenIsolation(t *testing.T) {
	var backendAuthorization, backendUser string
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		backendAuthorization = r.Header.Get("Authorization")
		backendUser = r.Header.Get("X-Auth-Request-Email")
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"jsonrpc":"2.0","result":{"tools":[]},"id":1}`)
	}))
	defer backend.Close()
	target, _ := url.Parse(backend.URL)
	s := mcpTestServer(t, target)
	h := s.handler()
	redirect := "http://127.0.0.1:31337/callback"
	clientID := registerMCPClient(t, h, redirect)

	verifier := strings.Repeat("v", 64)
	hash := sha256.Sum256([]byte(verifier))
	challenge := base64.RawURLEncoding.EncodeToString(hash[:])
	q := url.Values{
		"response_type": {"code"}, "client_id": {clientID}, "redirect_uri": {redirect},
		"state": {"client-state"}, "resource": {"https://tools.example/mcp"},
		"code_challenge": {challenge}, "code_challenge_method": {"S256"},
	}
	sess := session{Subject: "user-1", Email: "ada@example.com", Expires: time.Now().Add(time.Hour).Unix()}
	sealedSession, err := identity.Seal(testKey, purposeSession, sess)
	if err != nil {
		t.Fatal(err)
	}
	cookie := &http.Cookie{Name: sessionCookie, Value: sealedSession}
	authorize := mcpRequest(t, h, http.MethodGet, "https://auth.example/oauth2/authorize?"+q.Encode(), "auth.example", "", "", cookie)
	if authorize.Code != http.StatusOK || !strings.Contains(authorize.Body.String(), "Allow Test MCP client") {
		t.Fatalf("authorize status = %d: %s", authorize.Code, authorize.Body.String())
	}
	match := regexp.MustCompile(`name="request" value="([^"]+)"`).FindStringSubmatch(authorize.Body.String())
	if len(match) != 2 {
		t.Fatalf("consent request is missing: %s", authorize.Body.String())
	}
	form := url.Values{"request": {match[1]}, "decision": {"approve"}}
	consent := mcpRequest(t, h, http.MethodPost, "https://auth.example/oauth2/authorize", "auth.example", form.Encode(), "application/x-www-form-urlencoded", cookie)
	if consent.Code != http.StatusFound {
		t.Fatalf("consent status = %d: %s", consent.Code, consent.Body.String())
	}
	location, err := url.Parse(consent.Header().Get("Location"))
	if err != nil || location.Query().Get("state") != "client-state" || location.Query().Get("iss") != "https://auth.example" {
		t.Fatalf("bad authorization response: %s", consent.Header().Get("Location"))
	}
	code := location.Query().Get("code")
	tokenForm := url.Values{
		"grant_type": {"authorization_code"}, "code": {code}, "client_id": {clientID},
		"redirect_uri": {redirect}, "code_verifier": {verifier}, "resource": {"https://tools.example/mcp"},
	}
	tokenResponse := mcpRequest(t, h, http.MethodPost, "https://auth.example/oauth2/token", "auth.example", tokenForm.Encode(), "application/x-www-form-urlencoded")
	if tokenResponse.Code != http.StatusOK {
		t.Fatalf("token status = %d: %s", tokenResponse.Code, tokenResponse.Body.String())
	}
	var token struct {
		AccessToken  string `json:"access_token"`
		TokenType    string `json:"token_type"`
		RefreshToken string `json:"refresh_token"`
	}
	if json.Unmarshal(tokenResponse.Body.Bytes(), &token) != nil || token.AccessToken == "" || token.RefreshToken == "" || token.TokenType != "Bearer" {
		t.Fatalf("bad token response: %s", tokenResponse.Body.String())
	}

	request := httptest.NewRequest(http.MethodPost, "https://tools.example/mcp", strings.NewReader(`{}`))
	request.Host = "tools.example"
	request.Header.Set("Authorization", "Bearer "+token.AccessToken)
	response := httptest.NewRecorder()
	h.ServeHTTP(response, request)
	if response.Code != http.StatusOK || backendAuthorization != "" || backendUser != "ada@example.com" {
		t.Fatalf("proxied MCP response=%d auth=%q user=%q body=%s", response.Code, backendAuthorization, backendUser, response.Body.String())
	}
	if origins := response.Header().Values("Access-Control-Allow-Origin"); len(origins) != 1 || origins[0] != "*" {
		t.Fatalf("proxied MCP response has invalid CORS origins: %q", origins)
	}

	replay := mcpRequest(t, h, http.MethodPost, "https://auth.example/oauth2/token", "auth.example", tokenForm.Encode(), "application/x-www-form-urlencoded")
	if replay.Code != http.StatusBadRequest || !strings.Contains(replay.Body.String(), "invalid_grant") {
		t.Fatalf("authorization code replay = %d: %s", replay.Code, replay.Body.String())
	}
	refreshForm := url.Values{
		"grant_type": {"refresh_token"}, "refresh_token": {token.RefreshToken},
		"client_id": {clientID}, "resource": {"https://tools.example/mcp"},
	}
	refreshed := mcpRequest(t, h, http.MethodPost, "https://auth.example/oauth2/token", "auth.example", refreshForm.Encode(), "application/x-www-form-urlencoded")
	if refreshed.Code != http.StatusOK {
		t.Fatalf("refresh status = %d: %s", refreshed.Code, refreshed.Body.String())
	}
	var rotated struct {
		AccessToken  string `json:"access_token"`
		RefreshToken string `json:"refresh_token"`
	}
	if json.Unmarshal(refreshed.Body.Bytes(), &rotated) != nil || rotated.AccessToken == "" || rotated.RefreshToken == "" || rotated.RefreshToken == token.RefreshToken {
		t.Fatalf("bad refresh response: %s", refreshed.Body.String())
	}
	refreshReplay := mcpRequest(t, h, http.MethodPost, "https://auth.example/oauth2/token", "auth.example", refreshForm.Encode(), "application/x-www-form-urlencoded")
	if refreshReplay.Code != http.StatusBadRequest || !strings.Contains(refreshReplay.Body.String(), "already been used") {
		t.Fatalf("refresh token replay = %d: %s", refreshReplay.Code, refreshReplay.Body.String())
	}
	wrongResource := httptest.NewRequest(http.MethodPost, "https://tools.example/other", strings.NewReader(`{}`))
	wrongResource.Host = "tools.example"
	wrongResource.Header.Set("Authorization", "Bearer "+token.AccessToken)
	wrongResponse := httptest.NewRecorder()
	h.ServeHTTP(wrongResponse, wrongResource)
	if wrongResponse.Code != http.StatusUnauthorized {
		t.Fatalf("resource-bound token accepted on another path: %d", wrongResponse.Code)
	}
}
