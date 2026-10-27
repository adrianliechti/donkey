package gateway

import (
	"crypto/sha256"
	"crypto/subtle"
	_ "embed"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"net/url"
	"regexp"
	"strings"
	"time"

	"github.com/adrianliechti/donkey/internal/config"
	"github.com/adrianliechti/donkey/internal/identity"
	"github.com/adrianliechti/donkey/internal/webpage"
)

const (
	oauthCodeTTL         = 5 * time.Minute
	oauthAccessTokenTTL  = time.Hour
	oauthRefreshTokenTTL = 30 * 24 * time.Hour

	purposeOAuthClient  = "donkey/oauth-client/v1"
	purposeOAuthConsent = "donkey/oauth-consent/v1"
	purposeOAuthToken   = "donkey/oauth-token/v1"
	purposeOAuthRefresh = "donkey/oauth-refresh/v1"
)

var pkceVerifierRE = regexp.MustCompile(`^[A-Za-z0-9._~-]{43,128}$`)

type oauthClient struct {
	Name         string   `json:"name"`
	RedirectURIs []string `json:"redirect_uris"`
	GrantTypes   []string `json:"grant_types,omitempty"`
}

type oauthConsent struct {
	Subject     string `json:"sub"`
	ClientID    string `json:"client_id"`
	RedirectURI string `json:"redirect_uri"`
	Resource    string `json:"resource"`
	State       string `json:"state,omitempty"`
	Challenge   string `json:"code_challenge"`
	Expires     int64  `json:"exp"`
}

type oauthAuthorizationCode struct {
	ClientID    string
	RedirectURI string
	Resource    string
	Challenge   string
	Identity    session
	Expires     int64
}

// OAuth tokens carry the visitor's identity — with the IdP's refresh token,
// so an OAuth client's requests forward an access token like a browser's
// do — and the resource they were issued for. Expires is the token's own.
type oauthAccessToken struct {
	identity.Identity
	Resource string `json:"resource"`
}

type oauthRefreshToken struct {
	ClientID string `json:"client_id"`
	identity.Identity
	Resource string `json:"resource"`
}

func routeUsesOAuth(rt route) bool {
	return rt.Login && rt.Mode == config.AppModeAPI
}

func routeAllowsAllOrigins(rt route) bool { return rt.CORS == config.CORSAllowAll }

func requestAuthority(r *http.Request) string {
	if host, port, err := net.SplitHostPort(r.Host); err == nil {
		host = strings.ToLower(strings.TrimSuffix(host, "."))
		return net.JoinHostPort(host, port)
	}
	return requestHost(r)
}

func (s *Server) authorizationIssuer() string {
	host := s.cfg.Domain
	if host == "" {
		host = s.cfg.ServerHost
	}
	return "https://" + host
}

func (s *Server) onAuthorizationHost(r *http.Request) bool {
	u, err := url.Parse(s.authorizationIssuer())
	return err == nil && u.Hostname() != "" && requestHost(r) == strings.ToLower(u.Hostname())
}

func writeOAuthJSON(w http.ResponseWriter, status int, value any) {
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("Pragma", "no-cache")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(value)
}

func oauthError(w http.ResponseWriter, status int, code, description string) {
	writeOAuthJSON(w, status, map[string]string{"error": code, "error_description": description})
}

// handleOAuthResourceMetadata implements RFC 9728. A resource below the host
// root moves its path behind the well-known prefix, for example /mcp becomes
// /.well-known/oauth-protected-resource/mcp.
func (s *Server) handleOAuthResourceMetadata(w http.ResponseWriter, r *http.Request) {
	escapedPath := strings.TrimPrefix(r.URL.EscapedPath(), "/.well-known/oauth-protected-resource")
	lookupPath := strings.TrimPrefix(r.URL.Path, "/.well-known/oauth-protected-resource")
	if escapedPath == "" {
		escapedPath = "/"
		lookupPath = "/"
	}
	rt, ok := s.lookup(r.Context(), requestHost(r), lookupPath)
	if !ok || !routeUsesOAuth(rt) {
		http.NotFound(w, r)
		return
	}
	if routeAllowsAllOrigins(rt) {
		setCORSHeaders(w.Header())
	}
	writeOAuthJSON(w, http.StatusOK, map[string]any{
		"resource":                 oauthResourceURL(requestAuthority(r), escapedPath),
		"authorization_servers":    []string{s.authorizationIssuer()},
		"bearer_methods_supported": []string{"header"},
	})
}

func (s *Server) handleOAuthAuthorizationServerMetadata(w http.ResponseWriter, r *http.Request) {
	setCORSHeaders(w.Header())
	if !s.onAuthorizationHost(r) {
		http.NotFound(w, r)
		return
	}
	issuer := s.authorizationIssuer()
	writeOAuthJSON(w, http.StatusOK, map[string]any{
		"issuer":                                         issuer,
		"authorization_endpoint":                         issuer + "/oauth2/authorize",
		"token_endpoint":                                 issuer + "/oauth2/token",
		"registration_endpoint":                          issuer + "/oauth2/register",
		"response_types_supported":                       []string{"code"},
		"grant_types_supported":                          []string{"authorization_code", "refresh_token"},
		"code_challenge_methods_supported":               []string{"S256"},
		"token_endpoint_auth_methods_supported":          []string{"none", "client_secret_basic", "client_secret_post"},
		"authorization_response_iss_parameter_supported": true,
	})
}

type oauthRegistrationRequest struct {
	RedirectURIs            []string `json:"redirect_uris"`
	ClientName              string   `json:"client_name"`
	GrantTypes              []string `json:"grant_types"`
	ResponseTypes           []string `json:"response_types"`
	TokenEndpointAuthMethod string   `json:"token_endpoint_auth_method"`
}

func (s *Server) handleOAuthRegister(w http.ResponseWriter, r *http.Request) {
	setCORSHeaders(w.Header())
	if !s.onAuthorizationHost(r) {
		http.NotFound(w, r)
		return
	}
	var req oauthRegistrationRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		oauthError(w, http.StatusBadRequest, "invalid_client_metadata", "the registration body is not valid JSON")
		return
	}
	if len(req.RedirectURIs) == 0 {
		oauthError(w, http.StatusBadRequest, "invalid_redirect_uri", "at least one redirect_uri is required")
		return
	}
	for _, redirect := range req.RedirectURIs {
		if !validOAuthRedirectURI(redirect) {
			oauthError(w, http.StatusBadRequest, "invalid_redirect_uri", "redirect URIs must use HTTPS, or HTTP on localhost")
			return
		}
	}
	if req.TokenEndpointAuthMethod != "" && req.TokenEndpointAuthMethod != "none" {
		oauthError(w, http.StatusBadRequest, "invalid_client_metadata", "only public clients are supported")
		return
	}
	grantTypes, ok := registrationGrantTypes(req.GrantTypes)
	if !ok || !onlyEmptyOr(req.ResponseTypes, "code") {
		oauthError(w, http.StatusBadRequest, "invalid_client_metadata", "only authorization_code with optional refresh_token is supported")
		return
	}
	if strings.TrimSpace(req.ClientName) == "" {
		req.ClientName = "API client"
	}
	client := oauthClient{Name: req.ClientName, RedirectURIs: req.RedirectURIs, GrantTypes: grantTypes}
	sealed, err := identity.Seal(s.cfg.SessionKey, purposeOAuthClient, client)
	if err != nil {
		oauthError(w, http.StatusInternalServerError, "server_error", "the client could not be registered")
		return
	}
	writeOAuthJSON(w, http.StatusCreated, map[string]any{
		"client_id":                  "donkey_api_" + sealed,
		"client_id_issued_at":        time.Now().Unix(),
		"client_name":                client.Name,
		"redirect_uris":              client.RedirectURIs,
		"grant_types":                client.GrantTypes,
		"response_types":             []string{"code"},
		"token_endpoint_auth_method": "none",
	})
}

func registrationGrantTypes(values []string) ([]string, bool) {
	if len(values) == 0 {
		return []string{"authorization_code"}, true
	}
	seen := map[string]bool{}
	for _, value := range values {
		if value != "authorization_code" && value != "refresh_token" {
			return nil, false
		}
		seen[value] = true
	}
	if !seen["authorization_code"] {
		return nil, false
	}
	out := []string{"authorization_code"}
	if seen["refresh_token"] {
		out = append(out, "refresh_token")
	}
	return out, true
}

func onlyEmptyOr(values []string, allowed string) bool {
	if len(values) == 0 {
		return true
	}
	return len(values) == 1 && values[0] == allowed
}

func validOAuthRedirectURI(raw string) bool {
	u, err := url.Parse(raw)
	if err != nil || u.Host == "" || u.User != nil || u.Fragment != "" {
		return false
	}
	if u.Scheme == "https" {
		return true
	}
	if u.Scheme != "http" {
		return false
	}
	host := strings.ToLower(u.Hostname())
	return host == "localhost" || host == "localhost." || net.ParseIP(host) != nil && net.ParseIP(host).IsLoopback()
}

func (s *Server) registeredOAuthClient(clientID string) (*oauthClient, error) {
	sealed, ok := strings.CutPrefix(clientID, "donkey_api_")
	if !ok || sealed == "" {
		return nil, fmt.Errorf("unknown client")
	}
	var client oauthClient
	if err := identity.Unseal(s.cfg.SessionKey, purposeOAuthClient, sealed, &client); err != nil || len(client.RedirectURIs) == 0 {
		return nil, fmt.Errorf("unknown client")
	}
	return &client, nil
}

func clientSupportsGrant(client *oauthClient, grant string) bool {
	if client == nil {
		return false
	}
	// Client IDs sealed before refresh-token support did not carry this field.
	if len(client.GrantTypes) == 0 {
		return grant == "authorization_code"
	}
	return contains(client.GrantTypes, grant)
}

func (s *Server) handleOAuthAuthorize(w http.ResponseWriter, r *http.Request) {
	if !s.onAuthorizationHost(r) {
		http.NotFound(w, r)
		return
	}
	q := r.URL.Query()
	if strings.HasPrefix(q.Get("client_id"), "donkey_web_") {
		s.handleOIDCAuthorize(w, r, q)
		return
	}
	client, err := s.registeredOAuthClient(q.Get("client_id"))
	redirect := q.Get("redirect_uri")
	if err != nil || !contains(clientRedirects(client), redirect) {
		oauthError(w, http.StatusBadRequest, "invalid_request", "the client or redirect_uri is not registered")
		return
	}
	fail := func(code, description string) {
		redirectOAuthError(w, r, redirect, q.Get("state"), code, description, s.authorizationIssuer())
	}
	if q.Get("response_type") != "code" {
		fail("unsupported_response_type", "only response_type=code is supported")
		return
	}
	if q.Get("code_challenge_method") != "S256" || !validPKCEChallenge(q.Get("code_challenge")) {
		fail("invalid_request", "PKCE with code_challenge_method=S256 is required")
		return
	}
	if q.Get("scope") != "" {
		fail("invalid_scope", "this authorization server does not define OAuth scopes")
		return
	}
	resource, rt, ok := s.validOAuthResource(r, q.Get("resource"))
	if !ok {
		fail("invalid_target", "resource must be the exact URL of an API-mode app on this installation")
		return
	}
	if !s.cfg.authConfigured() {
		fail("temporarily_unavailable", "the installation has no complete identity provider configuration")
		return
	}
	sess := s.sessionFrom(r)
	if sess == nil {
		s.beginLogin(w, r, 0, r.URL.RequestURI())
		return
	}
	if !rt.allows(sess.Subject, sess.Email, sess.Groups) {
		fail("access_denied", "your identity is not allowed to use this API")
		return
	}
	consent := oauthConsent{
		Subject:  sess.Subject,
		ClientID: q.Get("client_id"), RedirectURI: redirect, Resource: resource,
		State: q.Get("state"), Challenge: q.Get("code_challenge"),
		Expires: time.Now().Add(oauthCodeTTL).Unix(),
	}
	sealed, err := identity.Seal(s.cfg.SessionKey, purposeOAuthConsent, consent)
	if err != nil {
		oauthError(w, http.StatusInternalServerError, "server_error", "authorization could not start")
		return
	}
	renderOAuthConsent(w, client.Name, resource, redirect, sealed)
}

func clientRedirects(client *oauthClient) []string {
	if client == nil {
		return nil
	}
	return client.RedirectURIs
}

func contains(values []string, wanted string) bool {
	for _, value := range values {
		if value == wanted {
			return true
		}
	}
	return false
}

func validPKCEChallenge(challenge string) bool {
	if len(challenge) != 43 {
		return false
	}
	_, err := base64.RawURLEncoding.DecodeString(challenge)
	return err == nil
}

func (s *Server) validOAuthResource(r *http.Request, raw string) (string, route, bool) {
	u, err := url.Parse(raw)
	if err != nil || !strings.EqualFold(u.Scheme, "https") || u.Host == "" || u.User != nil || u.RawQuery != "" || u.Fragment != "" {
		return "", route{}, false
	}
	host := strings.ToLower(strings.TrimSuffix(u.Hostname(), "."))
	if host == "" {
		return "", route{}, false
	}
	authority := host
	if port := u.Port(); port != "" {
		authority = net.JoinHostPort(host, port)
	}
	path := u.EscapedPath()
	if path == "" {
		path = "/"
	}
	canonical := oauthResourceURL(authority, path)
	rt, ok := s.lookup(r.Context(), host, u.Path)
	return canonical, rt, ok && routeUsesOAuth(rt)
}

func oauthResourceURL(host, path string) string {
	if path == "" || path == "/" {
		return "https://" + host
	}
	return "https://" + host + path
}

func redirectOAuthError(w http.ResponseWriter, r *http.Request, redirect, state, code, description, issuer string) {
	u, err := url.Parse(redirect)
	if err != nil {
		oauthError(w, http.StatusBadRequest, code, description)
		return
	}
	q := u.Query()
	q.Set("error", code)
	q.Set("error_description", description)
	if state != "" {
		q.Set("state", state)
	}
	q.Set("iss", issuer)
	u.RawQuery = q.Encode()
	http.Redirect(w, r, u.String(), http.StatusFound)
}

// The consent form wears the console's shell (internal/webpage); only the
// question and its two answers are its own.
//
//go:embed consent.html
var consentHTML string

var oauthConsentPage = webpage.Parse(consentHTML)

func renderOAuthConsent(w http.ResponseWriter, client, resource, redirect, request string) {
	webpage.Write(w, http.StatusOK, oauthConsentPage, map[string]string{
		"Title": "Authorize API client", "Client": client, "Resource": resource, "Redirect": redirect, "Request": request,
	})
}

func (s *Server) handleOAuthAuthorizeConsent(w http.ResponseWriter, r *http.Request) {
	if !s.onAuthorizationHost(r) {
		http.NotFound(w, r)
		return
	}
	r.Body = http.MaxBytesReader(w, r.Body, 64*1024)
	if err := r.ParseForm(); err != nil {
		oauthError(w, http.StatusBadRequest, "invalid_request", "the consent response could not be read")
		return
	}
	if r.PostForm.Get("flow") == "oidc" {
		s.handleOIDCConsent(w, r)
		return
	}
	if strings.HasPrefix(r.PostForm.Get("client_id"), "donkey_web_") {
		if r.URL.RawQuery != "" {
			oauthError(w, http.StatusBadRequest, "invalid_request", "authorization parameters must be in the form body")
			return
		}
		s.handleOIDCAuthorize(w, r, r.PostForm)
		return
	}
	var consent oauthConsent
	if identity.Unseal(s.cfg.SessionKey, purposeOAuthConsent, r.Form.Get("request"), &consent) != nil || time.Now().Unix() >= consent.Expires {
		oauthError(w, http.StatusBadRequest, "invalid_request", "this authorization request is stale")
		return
	}
	client, err := s.registeredOAuthClient(consent.ClientID)
	if err != nil || !contains(clientRedirects(client), consent.RedirectURI) {
		oauthError(w, http.StatusBadRequest, "invalid_request", "the client is no longer valid")
		return
	}
	if r.Form.Get("decision") != "approve" {
		redirectOAuthError(w, r, consent.RedirectURI, consent.State, "access_denied", "the user declined access", s.authorizationIssuer())
		return
	}
	sess := s.sessionFrom(r)
	if sess == nil || sess.Subject != consent.Subject {
		oauthError(w, http.StatusUnauthorized, "login_required", "sign in again before approving access")
		return
	}
	resource, rt, ok := s.validOAuthResource(r, consent.Resource)
	if !ok || !rt.allows(sess.Subject, sess.Email, sess.Groups) {
		redirectOAuthError(w, r, consent.RedirectURI, consent.State, "access_denied", "access to this API is no longer available", s.authorizationIssuer())
		return
	}
	code := randomHex(32)
	s.oauthMu.Lock()
	now := time.Now().Unix()
	for key, old := range s.oauthCodes {
		if now >= old.Expires {
			delete(s.oauthCodes, key)
		}
	}
	s.oauthCodes[code] = oauthAuthorizationCode{
		ClientID: consent.ClientID, RedirectURI: consent.RedirectURI, Resource: resource,
		Challenge: consent.Challenge, Identity: *sess,
		Expires: time.Now().Add(oauthCodeTTL).Unix(),
	}
	s.oauthMu.Unlock()
	u, _ := url.Parse(consent.RedirectURI)
	q := u.Query()
	q.Set("code", code)
	if consent.State != "" {
		q.Set("state", consent.State)
	}
	q.Set("iss", s.authorizationIssuer())
	u.RawQuery = q.Encode()
	http.Redirect(w, r, u.String(), http.StatusFound)
}

func (s *Server) handleOAuthToken(w http.ResponseWriter, r *http.Request) {
	setCORSHeaders(w.Header())
	if !s.onAuthorizationHost(r) {
		http.NotFound(w, r)
		return
	}
	r.Body = http.MaxBytesReader(w, r.Body, 64*1024)
	if err := r.ParseForm(); err != nil {
		oauthError(w, http.StatusBadRequest, "invalid_request", "the token request could not be read")
		return
	}
	if strings.HasPrefix(oidcRequestClientID(r), "donkey_web_") {
		s.handleOIDCToken(w, r)
		return
	}
	if r.Header.Get("Authorization") != "" {
		oauthError(w, http.StatusUnauthorized, "invalid_client", "API clients authenticate without a secret")
		return
	}
	switch r.Form.Get("grant_type") {
	case "authorization_code":
		s.exchangeOAuthAuthorizationCode(w, r)
	case "refresh_token":
		s.exchangeOAuthRefreshToken(w, r)
	default:
		oauthError(w, http.StatusBadRequest, "unsupported_grant_type", "grant_type must be authorization_code or refresh_token")
	}
}

func (s *Server) exchangeOAuthAuthorizationCode(w http.ResponseWriter, r *http.Request) {
	codeValue := r.Form.Get("code")
	s.oauthMu.Lock()
	code, ok := s.oauthCodes[codeValue]
	if ok {
		delete(s.oauthCodes, codeValue)
	}
	s.oauthMu.Unlock()
	if !ok || time.Now().Unix() >= code.Expires {
		oauthError(w, http.StatusBadRequest, "invalid_grant", "the authorization code is invalid, expired or already used")
		return
	}
	client, err := s.registeredOAuthClient(r.Form.Get("client_id"))
	if err != nil || !clientSupportsGrant(client, "authorization_code") {
		oauthError(w, http.StatusBadRequest, "invalid_grant", "the authorization code was not issued to this client")
		return
	}
	resource, rt, resourceOK := s.validOAuthResource(r, r.Form.Get("resource"))
	if r.Form.Get("client_id") != code.ClientID || r.Form.Get("redirect_uri") != code.RedirectURI || !resourceOK || resource != code.Resource {
		oauthError(w, http.StatusBadRequest, "invalid_grant", "the authorization code does not match this request")
		return
	}
	verifier := r.Form.Get("code_verifier")
	hash := sha256.Sum256([]byte(verifier))
	want := base64.RawURLEncoding.EncodeToString(hash[:])
	if !pkceVerifierRE.MatchString(verifier) || subtle.ConstantTimeCompare([]byte(want), []byte(code.Challenge)) != 1 {
		oauthError(w, http.StatusBadRequest, "invalid_grant", "the PKCE verifier does not match")
		return
	}
	if code.Identity.Expires <= time.Now().Unix() {
		oauthError(w, http.StatusBadRequest, "invalid_grant", "the signed-in session has expired")
		return
	}
	if !rt.allows(code.Identity.Subject, code.Identity.Email, code.Identity.Groups) {
		oauthError(w, http.StatusForbidden, "access_denied", "this identity is no longer allowed to use the API")
		return
	}
	response, err := s.issueOAuthTokens(code.Identity, code.ClientID, code.Resource,
		clientSupportsGrant(client, "refresh_token"), code.Identity.Expires)
	if err != nil {
		oauthError(w, http.StatusInternalServerError, "server_error", "the access token could not be issued")
		return
	}
	writeOAuthJSON(w, http.StatusOK, response)
}

func (s *Server) exchangeOAuthRefreshToken(w http.ResponseWriter, r *http.Request) {
	raw := r.Form.Get("refresh_token")
	var refresh oauthRefreshToken
	if raw == "" || identity.Unseal(s.cfg.SessionKey, purposeOAuthRefresh, raw, &refresh) != nil ||
		refresh.Subject == "" || refresh.Resource == "" || time.Now().Unix() >= refresh.Expires {
		oauthError(w, http.StatusBadRequest, "invalid_grant", "the refresh token is invalid or expired")
		return
	}
	client, err := s.registeredOAuthClient(r.Form.Get("client_id"))
	if err != nil || refresh.ClientID != r.Form.Get("client_id") || !clientSupportsGrant(client, "refresh_token") {
		oauthError(w, http.StatusBadRequest, "invalid_grant", "the refresh token was not issued to this client")
		return
	}
	requestedResource := r.Form.Get("resource")
	if requestedResource == "" {
		requestedResource = refresh.Resource
	}
	resource, rt, ok := s.validOAuthResource(r, requestedResource)
	if !ok || resource != refresh.Resource {
		oauthError(w, http.StatusBadRequest, "invalid_target", "the refresh token belongs to another resource")
		return
	}
	if !rt.allows(refresh.Subject, refresh.Email, refresh.Groups) {
		oauthError(w, http.StatusForbidden, "access_denied", "this identity is no longer allowed to use the API")
		return
	}
	if !s.consumeOAuthRefreshToken(raw, refresh.Expires) {
		oauthError(w, http.StatusBadRequest, "invalid_grant", "the refresh token has already been used")
		return
	}
	sess := refresh.Identity
	// The client keeps the newest IdP refresh token from here on.
	sess.RefreshToken = s.upstream.Latest(sess.Grant, sess.RefreshToken)
	response, err := s.issueOAuthTokens(sess, refresh.ClientID, refresh.Resource, true, 0)
	if err != nil {
		oauthError(w, http.StatusInternalServerError, "server_error", "the access token could not be refreshed")
		return
	}
	writeOAuthJSON(w, http.StatusOK, response)
}

func (s *Server) issueOAuthTokens(sess session, clientID, resource string, includeRefresh bool, accessCeiling int64) (map[string]any, error) {
	now := time.Now().Unix()
	expires := now + int64(oauthAccessTokenTTL/time.Second)
	if accessCeiling > 0 && accessCeiling < expires {
		expires = accessCeiling
	}
	if expires <= now {
		return nil, fmt.Errorf("the identity grant has expired")
	}
	access := sess
	access.Expires = expires
	accessToken, err := identity.Seal(s.cfg.SessionKey, purposeOAuthToken, oauthAccessToken{Identity: access, Resource: resource})
	if err != nil {
		return nil, err
	}
	response := map[string]any{
		"access_token": accessToken, "token_type": "Bearer", "expires_in": expires - now,
	}
	if includeRefresh {
		refresh := sess
		refresh.Expires = time.Now().Add(oauthRefreshTokenTTL).Unix()
		refreshToken, err := identity.Seal(s.cfg.SessionKey, purposeOAuthRefresh, oauthRefreshToken{ClientID: clientID, Identity: refresh, Resource: resource})
		if err != nil {
			return nil, err
		}
		response["refresh_token"] = refreshToken
	}
	return response, nil
}

func (s *Server) consumeOAuthRefreshToken(raw string, expires int64) bool {
	digest := sha256.Sum256([]byte(raw))
	key := base64.RawURLEncoding.EncodeToString(digest[:])
	now := time.Now().Unix()
	s.oauthMu.Lock()
	defer s.oauthMu.Unlock()
	if s.oauthRefreshUsed == nil {
		s.oauthRefreshUsed = map[string]int64{}
	}
	for old, expiry := range s.oauthRefreshUsed {
		if now >= expiry {
			delete(s.oauthRefreshUsed, old)
		}
	}
	if _, used := s.oauthRefreshUsed[key]; used {
		return false
	}
	s.oauthRefreshUsed[key] = expires
	return true
}

func (s *Server) handleOAuthPreflight(w http.ResponseWriter, r *http.Request) {
	if r.URL.Path == "/oauth2/authorize" {
		http.NotFound(w, r)
		return
	}
	if !s.onAuthorizationHost(r) {
		http.NotFound(w, r)
		return
	}
	serveCORSPreflight(w, r)
}

func (s *Server) serveOAuthApp(w http.ResponseWriter, r *http.Request, rt route) (sess *session) {
	resource := oauthResourceURL(requestAuthority(r), r.URL.EscapedPath())
	metadata := "https://" + requestAuthority(r) + "/.well-known/oauth-protected-resource"
	if r.URL.EscapedPath() != "" && r.URL.EscapedPath() != "/" {
		metadata += r.URL.EscapedPath()
	}
	challenge := fmt.Sprintf(`Bearer resource_metadata="%s"`, metadata)
	deny := func(status int, code, description string) {
		w.Header().Set("WWW-Authenticate", challenge+fmt.Sprintf(`, error="%s", error_description="%s"`, code, description))
		oauthError(w, status, code, description)
	}
	if !s.cfg.authConfigured() {
		oauthError(w, http.StatusServiceUnavailable, "temporarily_unavailable", "the installation has no complete identity provider configuration")
		return nil
	}
	raw, ok := strings.CutPrefix(r.Header.Get("Authorization"), "Bearer ")
	if !ok || raw == "" {
		w.Header().Set("WWW-Authenticate", challenge)
		oauthError(w, http.StatusUnauthorized, "invalid_token", "a bearer access token is required")
		return nil
	}
	var token oauthAccessToken
	if identity.Unseal(s.cfg.SessionKey, purposeOAuthToken, raw, &token) != nil || token.Subject == "" || time.Now().Unix() >= token.Expires || token.Resource != resource {
		deny(http.StatusUnauthorized, "invalid_token", "the bearer token is invalid, expired or belongs to another resource")
		return nil
	}
	sess = &token.Identity
	if !rt.allows(sess.Subject, sess.Email, sess.Groups) {
		w.Header().Set("WWW-Authenticate", challenge)
		oauthError(w, http.StatusForbidden, "access_denied", "this identity is not allowed to use the API")
		return sess
	}
	if routeAllowsAllOrigins(rt) {
		clearCORSHeaders(w.Header())
	}
	// An OAuth client presents this grant for as long as its refresh token
	// lasts, well past the access token in hand.
	idpToken := s.idpToken(w, r, rt, sess, true, time.Now().Add(oauthRefreshTokenTTL))
	s.proxy(rt, sess, true, idpToken).ServeHTTP(w, r)
	return sess
}
