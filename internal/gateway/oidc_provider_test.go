package gateway

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/coreos/go-oidc/v3/oidc"
	"github.com/go-jose/go-jose/v4"
	"golang.org/x/oauth2"

	"github.com/adrianliechti/donkey/internal/config"
	"github.com/adrianliechti/donkey/internal/identity"
	"github.com/adrianliechti/donkey/internal/storage"
)

const webClientID = "donkey_web_acme_web"
const webCallback = "https://web.acme.example/callback"
const webVerifier = "abcdefghijklmnopqrstuvwxyzABCDEFGHIJKLMNOPQRSTUVWXYZ0123456789"

func webOIDCTestServer(t *testing.T) *Server {
	t.Helper()
	backend, _ := url.Parse("http://app.internal:8000")
	s := mcpTestServer(t, backend)
	s.routes["web.acme.example"] = []route{{Space: "acme", App: "web", Host: "web.acme.example", Path: "/", Backend: backend,
		OIDC:   &config.OIDCClient{RedirectURIs: []string{webCallback}},
		Policy: &config.SpaceConfig{Viewers: config.AccessConfig{Users: []string{"ada@example.com"}}},
	}}
	s.oidcStore = &storage.File{Path: filepath.Join(t.TempDir(), "state.json")}
	refreshWebRoutes(t, s)
	return s
}

func webSession(t *testing.T, s *Server) *http.Cookie {
	t.Helper()
	sealed, err := identity.Seal(s.cfg.SessionKey, purposeSession, session{Subject: "upstream-1", Email: "ada@example.com", Name: "Ada", PreferredUsername: "ada", Groups: []string{"engineers"},
		AuthTime: time.Now().Add(-time.Minute).Unix(), Expires: time.Now().Add(sessionTTL).Unix(), RefreshToken: "upstream-secret", AccessToken: "upstream-access", Grant: "upstream-grant"})
	if err != nil {
		t.Fatal(err)
	}
	return &http.Cookie{Name: sessionCookie, Value: sealed}
}

func webAuthorizeQuery(scope string) url.Values {
	return url.Values{"client_id": {webClientID}, "redirect_uri": {webCallback}, "response_type": {"code"}, "scope": {scope}, "state": {"client-state"}, "nonce": {"client-nonce"},
		"code_challenge_method": {"S256"}, "code_challenge": {oidcDigest(webVerifier)}}
}

func webAuthorize(t *testing.T, s *Server, scope string) string {
	t.Helper()
	q := webAuthorizeQuery(scope)
	rec := mcpRequest(t, s.handler(), "GET", "https://auth.example/oauth2/authorize?"+q.Encode(), "auth.example", "", "", webSession(t, s))
	if rec.Code != http.StatusOK {
		t.Fatalf("authorize: %d %s %s", rec.Code, rec.Header().Get("Location"), rec.Body.String())
	}
	if rec.Header().Get("Access-Control-Allow-Origin") != "" {
		t.Fatal("authorization endpoint allowed CORS")
	}
	match := regexp.MustCompile(`name="request" value="([^"]+)"`).FindStringSubmatch(rec.Body.String())
	if len(match) != 2 {
		t.Fatalf("no consent form: %s", rec.Body.String())
	}
	return match[1]
}

func webConsent(t *testing.T, s *Server, consent string) *httptest.ResponseRecorder {
	t.Helper()
	return mcpRequest(t, s.handler(), "POST", "https://auth.example/oauth2/authorize", "auth.example", url.Values{"flow": {"oidc"}, "request": {consent}, "decision": {"approve"}}.Encode(), "application/x-www-form-urlencoded", webSession(t, s))
}

func webCode(t *testing.T, s *Server, scope string) string {
	t.Helper()
	rec := webConsent(t, s, webAuthorize(t, s, scope))
	u, err := url.Parse(rec.Header().Get("Location"))
	if err != nil || rec.Code != http.StatusFound || u.Query().Get("code") == "" {
		t.Fatalf("consent: %d %s %s", rec.Code, rec.Header().Get("Location"), rec.Body.String())
	}
	if u.Query().Get("state") != "client-state" || u.Query().Get("iss") != "https://auth.example" {
		t.Fatal("authorization response lost state or issuer")
	}
	return u.Query().Get("code")
}

func webTokenForm(code string) url.Values {
	return url.Values{"client_id": {webClientID}, "grant_type": {"authorization_code"}, "code": {code}, "redirect_uri": {webCallback}, "code_verifier": {webVerifier}}
}

func webTokenRequest(t *testing.T, s *Server, form url.Values) *httptest.ResponseRecorder {
	t.Helper()
	return mcpRequest(t, s.handler(), "POST", "https://auth.example/oauth2/token", "auth.example", form.Encode(), "application/x-www-form-urlencoded")
}

func webUserInfo(s *Server, token string) *httptest.ResponseRecorder {
	req := httptest.NewRequest("GET", "https://auth.example/oauth2/userinfo", nil)
	req.Header.Set("Authorization", "Bearer "+token)
	rec := httptest.NewRecorder()
	s.handler().ServeHTTP(rec, req)
	return rec
}

func webTokenResponse(t *testing.T, rec *httptest.ResponseRecorder) map[string]any {
	t.Helper()
	var values map[string]any
	if rec.Code != http.StatusOK || json.Unmarshal(rec.Body.Bytes(), &values) != nil {
		t.Fatalf("token response: %d %s", rec.Code, rec.Body.String())
	}
	return values
}

type oidcHandlerTransport struct{ handler http.Handler }

func (t oidcHandlerTransport) RoundTrip(r *http.Request) (*http.Response, error) {
	w := httptest.NewRecorder()
	t.handler.ServeHTTP(w, r)
	res := w.Result()
	res.Request = r
	return res, nil
}

func TestWebOIDCStandardClient(t *testing.T) {
	for _, confidential := range []bool{false, true} {
		t.Run(fmt.Sprint("confidential=", confidential), func(t *testing.T) {
			s := webOIDCTestServer(t)
			secret := ""
			if confidential {
				secret = strings.Repeat("s+/&=", 12)
				digest := sha256.Sum256([]byte(secret))
				s.routes["web.acme.example"][0].OIDC.ClientSecretSHA256 = hex.EncodeToString(digest[:])
				refreshWebRoutes(t, s)
			}
			httpClient := &http.Client{Transport: oidcHandlerTransport{s.handler()}}
			ctx := oidc.ClientContext(context.Background(), httpClient)
			provider, err := oidc.NewProvider(ctx, "https://auth.example")
			if err != nil {
				t.Fatal(err)
			}
			client := &oauth2.Config{ClientID: webClientID, ClientSecret: secret, RedirectURL: webCallback, Endpoint: provider.Endpoint()}
			code := webCode(t, s, "openid profile email offline_access")
			token, err := client.Exchange(ctx, code, oauth2.VerifierOption(webVerifier))
			if err != nil {
				t.Fatal(err)
			}
			rawID, _ := token.Extra("id_token").(string)
			idToken, err := provider.Verifier(&oidc.Config{ClientID: webClientID}).Verify(ctx, rawID)
			if err != nil {
				t.Fatal(err)
			}
			if idToken.Nonce != "client-nonce" || idToken.Subject == "" || token.RefreshToken == "" {
				t.Fatalf("missing claims or refresh token: %+v", idToken)
			}
			if _, err := provider.Verifier(&oidc.Config{ClientID: "another-app"}).Verify(ctx, rawID); err == nil {
				t.Fatal("wrong audience accepted")
			}
			info, err := provider.UserInfo(ctx, oauth2.StaticTokenSource(token))
			if err != nil {
				t.Fatal(err)
			}
			if info.Subject != idToken.Subject || info.Email != "ada@example.com" {
				t.Fatalf("userinfo: %+v", info)
			}
			var claims map[string]any
			if err := idToken.Claims(&claims); err != nil {
				t.Fatal(err)
			}
			for _, forbidden := range []string{"rt", "at", "grant", "groups", "email_verified"} {
				if _, exists := claims[forbidden]; exists {
					t.Fatalf("unexpected claim %s", forbidden)
				}
			}
			if _, err := client.Exchange(ctx, code, oauth2.VerifierOption(webVerifier)); err == nil {
				t.Fatal("code replay succeeded")
			}
			// Standard SDK refresh and ID-token verification work after the
			// gateway is recreated with the same persistent storage.
			s.oidcStore = &storage.File{Path: s.oidcStore.(*storage.File).Path}
			refreshWebRoutes(t, s)
			oldRefresh := token.RefreshToken
			token.Expiry = time.Now().Add(-time.Minute)
			refreshed, err := client.TokenSource(ctx, token).Token()
			if err != nil {
				t.Fatal(err)
			}
			if refreshed.RefreshToken == oldRefresh {
				t.Fatal("refresh token did not rotate")
			}
			if _, err := provider.Verifier(&oidc.Config{ClientID: webClientID}).Verify(ctx, refreshed.Extra("id_token").(string)); err != nil {
				t.Fatal(err)
			}
		})
	}
}

func TestWebOIDCTokenBoundariesAndRevocation(t *testing.T) {
	s := webOIDCTestServer(t)
	response := webTokenResponse(t, webTokenRequest(t, s, webTokenForm(webCode(t, s, "openid offline_access"))))
	access, refresh := response["access_token"].(string), response["refresh_token"].(string)
	info := webUserInfo(s, access)
	if info.Code != 200 || strings.Contains(info.Body.String(), "email") || strings.Contains(info.Body.String(), "name") {
		t.Fatalf("scope leakage: %d %s", info.Code, info.Body.String())
	}
	for _, wrong := range []string{refresh, response["id_token"].(string)} {
		if webUserInfo(s, wrong).Code != 401 {
			t.Fatal("non-access token accepted by UserInfo")
		}
	}
	request := httptest.NewRequest("POST", "https://tools.example/mcp", nil)
	request.Header.Set("Authorization", "Bearer "+access)
	rec := httptest.NewRecorder()
	s.handler().ServeHTTP(rec, request)
	if rec.Code != 401 {
		t.Fatal("OIDC token accepted as API credential")
	}
	form := url.Values{"client_id": {webClientID}, "grant_type": {"refresh_token"}, "refresh_token": {refresh}}
	rotated := webTokenResponse(t, webTokenRequest(t, s, form))
	if replay := webTokenRequest(t, s, form); replay.Code != 400 {
		t.Fatal("refresh replay succeeded")
	}
	if webUserInfo(s, rotated["access_token"].(string)).Code != 401 {
		t.Fatal("replay did not revoke the family")
	}
	form.Set("refresh_token", rotated["refresh_token"].(string))
	if webTokenRequest(t, s, form).Code != 400 {
		t.Fatal("refresh family survived replay")
	}

	response = webTokenResponse(t, webTokenRequest(t, s, webTokenForm(webCode(t, s, "openid profile email"))))
	if _, exists := response["refresh_token"]; exists {
		t.Fatal("refresh token issued without offline_access")
	}
	form = url.Values{"client_id": {webClientID}, "token": {response["access_token"].(string)}}
	rec = mcpRequest(t, s.handler(), "POST", "https://auth.example/oauth2/revoke", "auth.example", form.Encode(), "application/x-www-form-urlencoded")
	if rec.Code != 200 || webUserInfo(s, response["access_token"].(string)).Code != 401 {
		t.Fatal("revocation did not invalidate access")
	}
}

func TestWebOIDCRequestValidation(t *testing.T) {
	s := webOIDCTestServer(t)
	for _, tc := range []struct{ key, value, error string }{
		{"redirect_uri", webCallback + "/other", "invalid_request"},
		{"scope", "openid groups", "invalid_scope"},
		{"scope", "email", "invalid_scope"},
		{"code_challenge_method", "plain", "invalid_request"},
		{"response_type", "id_token", "unsupported_response_type"},
		{"resource", "https://tools.example/mcp", "invalid_request"},
		{"prompt", "none consent", "invalid_request"},
		{"prompt", "none", "consent_required"},
		{"max_age", "-1", "invalid_request"},
	} {
		t.Run(tc.key+"="+tc.value, func(t *testing.T) {
			q := webAuthorizeQuery("openid")
			q.Set(tc.key, tc.value)
			rec := mcpRequest(t, s.handler(), "GET", "https://auth.example/oauth2/authorize?"+q.Encode(), "auth.example", "", "", webSession(t, s))
			location, _ := url.Parse(rec.Header().Get("Location"))
			if location.Query().Get("error") != tc.error && !strings.Contains(rec.Body.String(), tc.error) {
				t.Fatalf("unexpected response: %d %s %s", rec.Code, location, rec.Body.String())
			}
			if tc.key == "redirect_uri" && rec.Header().Get("Location") != "" {
				t.Fatal("redirected to unregistered callback")
			}
		})
	}
	q := webAuthorizeQuery("openid")
	q.Set("prompt", "none")
	rec := mcpRequest(t, s.handler(), "GET", "https://auth.example/oauth2/authorize?"+q.Encode(), "auth.example", "", "")
	if !strings.Contains(rec.Header().Get("Location"), "login_required") {
		t.Fatal("silent request redirected to interactive login")
	}
	consent := webAuthorize(t, s, "openid")
	if webConsent(t, s, consent).Code != 302 || !strings.Contains(webConsent(t, s, consent).Header().Get("Location"), "invalid_request") {
		t.Fatal("consent replay accepted")
	}
	form := webTokenForm(webCode(t, s, "openid"))
	form.Set("code_verifier", strings.Repeat("x", 43))
	if webTokenRequest(t, s, form).Code != 400 {
		t.Fatal("wrong PKCE verifier accepted")
	}
	form = webTokenForm(webCode(t, s, "openid"))
	form.Set("redirect_uri", webCallback+"/other")
	if webTokenRequest(t, s, form).Code != 400 {
		t.Fatal("changed callback accepted")
	}
	form = webTokenForm(webCode(t, s, "openid"))
	form.Add("client_id", webClientID)
	if rec := webTokenRequest(t, s, form); rec.Code != 400 || !strings.Contains(rec.Body.String(), "invalid_request") {
		t.Fatal("duplicate token parameters accepted")
	}
}

type fixedWebRoutes []route

func (routes fixedWebRoutes) list(context.Context) ([]route, error) { return routes, nil }

func refreshWebRoutes(t *testing.T, s *Server) {
	t.Helper()
	var routes fixedWebRoutes
	for _, entries := range s.routes {
		routes = append(routes, entries...)
	}
	s.source = routes
	if err := s.refreshRoutes(context.Background()); err != nil {
		t.Fatal(err)
	}
}

func TestWebOIDCRegistrationRestorationDoesNotReviveGrants(t *testing.T) {
	for _, change := range []string{"remove", "edit"} {
		t.Run(change, func(t *testing.T) {
			s := webOIDCTestServer(t)
			refreshWebRoutes(t, s)
			original := s.routes["web.acme.example"][0]
			consent := webAuthorize(t, s, "openid")
			pending := webCode(t, s, "openid")
			response := webTokenResponse(t, webTokenRequest(t, s, webTokenForm(webCode(t, s, "openid offline_access"))))
			if change == "remove" {
				delete(s.routes, "web.acme.example")
			} else {
				s.routes["web.acme.example"][0].OIDC = &config.OIDCClient{RedirectURIs: []string{webCallback + "/new"}}
			}
			refreshWebRoutes(t, s)
			s.oidcStore = &storage.File{Path: s.oidcStore.(*storage.File).Path}
			s.routes["web.acme.example"] = []route{original}
			refreshWebRoutes(t, s)
			if webUserInfo(s, response["access_token"].(string)).Code != 401 {
				t.Fatal("restoring registration revived an old access token")
			}
			if webTokenRequest(t, s, webTokenForm(pending)).Code != 400 {
				t.Fatal("restoring registration revived a pending code")
			}
			if rec := webConsent(t, s, consent); rec.Code == 302 && !strings.Contains(rec.Header().Get("Location"), "error=") {
				t.Fatal("restoring registration revived an old consent form")
			}
			form := url.Values{"client_id": {webClientID}, "grant_type": {"refresh_token"}, "refresh_token": {response["refresh_token"].(string)}}
			if webTokenRequest(t, s, form).Code != 400 {
				t.Fatal("restoring registration revived an old refresh token")
			}
			webTokenResponse(t, webTokenRequest(t, s, webTokenForm(webCode(t, s, "openid"))))
		})
	}
}

func TestWebOIDCFormAuthorization(t *testing.T) {
	s := webOIDCTestServer(t)
	q := webAuthorizeQuery("openid")
	q.Set("acr_values", "urn:example:optional-context")
	rec := mcpRequest(t, s.handler(), "POST", "https://auth.example/oauth2/authorize", "auth.example", q.Encode(), "application/x-www-form-urlencoded", webSession(t, s))
	if rec.Code != 200 || !strings.Contains(rec.Body.String(), "Sign in to web") {
		t.Fatalf("form authorization failed: %d %s", rec.Code, rec.Body.String())
	}
}

func TestWebOIDCRegistrationSyncFailureAndRecovery(t *testing.T) {
	s := webOIDCTestServer(t)
	response := webTokenResponse(t, webTokenRequest(t, s, webTokenForm(webCode(t, s, "openid offline_access"))))
	routes := s.source
	store := s.oidcStore
	path := store.(*storage.File).Path
	before, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	for range 3 {
		if err := s.refreshRoutes(context.Background()); err != nil {
			t.Fatal(err)
		}
	}
	after, err := os.Stat(path)
	if err != nil || !os.SameFile(before, after) {
		t.Fatal("unchanged route polls rewrote persistent auth state")
	}
	for _, failure := range []string{"runtime", "storage"} {
		if failure == "runtime" {
			s.source = unavailablePolicySource{}
		} else {
			s.oidcStore = failedOIDCStore{}
		}
		if s.refreshRoutes(context.Background()) == nil {
			t.Fatal("refresh failure was ignored")
		}
		if webUserInfo(s, response["access_token"].(string)).Code != 401 {
			t.Fatal("OIDC served with an unverified registration snapshot")
		}
		s.source, s.oidcStore = routes, store
		if err := s.refreshRoutes(context.Background()); err != nil {
			t.Fatal(err)
		}
		if webUserInfo(s, response["access_token"].(string)).Code != 200 {
			t.Fatal("transient outage permanently revoked the registration")
		}
	}
}

func TestWebOIDCConsentRechecksDurableRegistration(t *testing.T) {
	s := webOIDCTestServer(t)
	consent := webAuthorize(t, s, "openid")
	oldClients := s.oidcClients
	s.routes["web.acme.example"][0].OIDC = &config.OIDCClient{RedirectURIs: []string{webCallback + "/new"}}
	refreshWebRoutes(t, s)
	// Model a request that read the old cache before reconciliation committed.
	s.oidcClients = oldClients
	rec := webConsent(t, s, consent)
	u, err := url.Parse(rec.Header().Get("Location"))
	if err != nil || u.Query().Get("code") != "" || u.Query().Get("error") != "invalid_request" {
		t.Fatalf("stale request issued a code: %d %s", rec.Code, rec.Header().Get("Location"))
	}
}

func TestWebOIDCMembershipAndClientChanges(t *testing.T) {
	for _, change := range []string{"membership", "registration", "deleted"} {
		t.Run(change, func(t *testing.T) {
			s := webOIDCTestServer(t)
			response := webTokenResponse(t, webTokenRequest(t, s, webTokenForm(webCode(t, s, "openid offline_access"))))
			pending := webCode(t, s, "openid")
			switch change {
			case "membership":
				s.routes["web.acme.example"][0].Policy = &config.SpaceConfig{}
			case "registration":
				s.routes["web.acme.example"][0].OIDC.RedirectURIs = []string{webCallback + "/new"}
			case "deleted":
				delete(s.routes, "web.acme.example")
			}
			refreshWebRoutes(t, s)
			if webTokenRequest(t, s, webTokenForm(pending)).Code == 200 {
				t.Fatal("pending code survived access removal")
			}
			if webUserInfo(s, response["access_token"].(string)).Code != 401 {
				t.Fatal("access token survived access removal")
			}
			form := url.Values{"client_id": {webClientID}, "grant_type": {"refresh_token"}, "refresh_token": {response["refresh_token"].(string)}}
			if webTokenRequest(t, s, form).Code == 200 {
				t.Fatal("refresh token survived access removal")
			}
		})
	}
}

func TestWebOIDCPersistenceAndKeyRotation(t *testing.T) {
	s := webOIDCTestServer(t)
	dir := filepath.Dir(s.oidcStore.(*storage.File).Path)
	code := webCode(t, s, "openid email offline_access")
	s.oidcStore = &storage.File{Path: filepath.Join(dir, "state.json")}
	refreshWebRoutes(t, s)
	response := webTokenResponse(t, webTokenRequest(t, s, webTokenForm(code)))
	first := mcpRequest(t, s.handler(), "GET", "https://auth.example/oauth2/jwks", "auth.example", "", "")
	var old jose.JSONWebKeySet
	if err := json.Unmarshal(first.Body.Bytes(), &old); err != nil || len(old.Keys) != 1 || !old.Keys[0].IsPublic() {
		t.Fatal("invalid public JWKS")
	}
	if err := s.updateOIDCState(context.Background(), func(state *oidcProviderState) error {
		state.Keys[0].Created = time.Now().Add(-oidcKeyLifetime).Unix()
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	s.oidcStore = &storage.File{Path: filepath.Join(dir, "state.json")}
	second := mcpRequest(t, s.handler(), "GET", "https://auth.example/oauth2/jwks", "auth.example", "", "")
	var keys jose.JSONWebKeySet
	if err := json.Unmarshal(second.Body.Bytes(), &keys); err != nil || len(keys.Keys) != 2 || keys.Keys[0].KeyID != old.Keys[0].KeyID {
		t.Fatal("rotation did not retain previous public key")
	}
	token, err := jose.ParseSigned(response["id_token"].(string), []jose.SignatureAlgorithm{jose.RS256})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := token.Verify(keys.Keys[0]); err != nil {
		t.Fatal("rotation invalidated an outstanding ID token")
	}
	raw, err := os.ReadFile(filepath.Join(dir, "state.json"))
	if err != nil {
		t.Fatal(err)
	}
	for _, secret := range []string{"upstream-secret", "upstream-access", "upstream-grant", code, response["refresh_token"].(string)} {
		if strings.Contains(string(raw), secret) {
			t.Fatal("OIDC store retained a raw upstream credential, code or refresh token")
		}
	}
	info, err := os.Stat(filepath.Join(dir, "state.json"))
	if err != nil || info.Mode().Perm() != 0o600 {
		t.Fatal("OIDC state permissions are not private")
	}
}

func TestWebOIDCAtomicCodeConsumption(t *testing.T) {
	s := webOIDCTestServer(t)
	form := webTokenForm(webCode(t, s, "openid"))
	var wg sync.WaitGroup
	results := make(chan int, 2)
	for range 2 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			results <- webTokenRequest(t, s, form).Code
		}()
	}
	wg.Wait()
	close(results)
	successes := 0
	for status := range results {
		if status == 200 {
			successes++
		} else if status != 400 {
			t.Fatalf("unexpected status: %d", status)
		}
	}
	if successes != 1 {
		t.Fatalf("code consumed %d times", successes)
	}
}

func TestWebOIDCConfidentialClientAuthentication(t *testing.T) {
	s := webOIDCTestServer(t)
	secret := strings.Repeat("secret", 8)
	digest := sha256.Sum256([]byte(secret))
	s.routes["web.acme.example"][0].OIDC.ClientSecretSHA256 = hex.EncodeToString(digest[:])
	refreshWebRoutes(t, s)
	form := webTokenForm(webCode(t, s, "openid"))
	if webTokenRequest(t, s, form).Code != 401 {
		t.Fatal("confidential client accepted without authentication")
	}
	form.Set("client_secret", "wrong")
	if webTokenRequest(t, s, form).Code != 401 {
		t.Fatal("wrong client secret accepted")
	}
	form.Set("client_secret", secret)
	if webTokenRequest(t, s, form).Code != 200 {
		t.Fatal("form authentication failed or bad credentials consumed the code")
	}
}

func TestWebOIDCConcurrentRefreshRevokesFamily(t *testing.T) {
	s := webOIDCTestServer(t)
	response := webTokenResponse(t, webTokenRequest(t, s, webTokenForm(webCode(t, s, "openid offline_access"))))
	form := url.Values{"client_id": {webClientID}, "grant_type": {"refresh_token"}, "refresh_token": {response["refresh_token"].(string)}}
	results := make(chan *httptest.ResponseRecorder, 2)
	for range 2 {
		go func() { results <- webTokenRequest(t, s, form) }()
	}
	successes := 0
	var access string
	for range 2 {
		rec := <-results
		if rec.Code == 200 {
			successes++
			access = webTokenResponse(t, rec)["access_token"].(string)
		} else if rec.Code != 400 {
			t.Fatalf("unexpected status: %d", rec.Code)
		}
	}
	if successes != 1 || webUserInfo(s, access).Code != 401 {
		t.Fatal("concurrent refresh failed to detect replay and revoke the family")
	}
}

type failedOIDCStore struct{}

func (failedOIDCStore) Update(context.Context, func([]byte) ([]byte, error)) error {
	return errors.New("store unavailable")
}

func TestWebOIDCStorageFailureAndHostIsolation(t *testing.T) {
	s := webOIDCTestServer(t)
	form := webTokenForm(webCode(t, s, "openid"))
	store := s.oidcStore
	s.oidcStore = failedOIDCStore{}
	if rec := webTokenRequest(t, s, form); rec.Code != 503 || strings.Contains(rec.Body.String(), "access_token") {
		t.Fatal("token issued without durable state")
	}
	s.oidcStore = store
	if webTokenRequest(t, s, form).Code != 200 {
		t.Fatal("storage failure consumed a code")
	}
	for _, path := range []string{"/.well-known/openid-configuration", "/oauth2/jwks", "/oauth2/userinfo"} {
		if rec := mcpRequest(t, s.handler(), "GET", "https://web.acme.example"+path, "web.acme.example", "", ""); rec.Code != 404 {
			t.Fatalf("provider endpoint served on app host: %s", path)
		}
	}
	for _, scope := range []string{"openid", "openid email"} {
		verified := false
		claims := s.oidcClaims(session{Subject: "user", Email: "ada@example.com", EmailVerified: &verified}, scope)
		value, present := claims["email_verified"]
		if present != strings.Contains(scope, "email") || present && value != false {
			t.Fatal("email verification claim was fabricated or leaked without scope")
		}
	}
}
