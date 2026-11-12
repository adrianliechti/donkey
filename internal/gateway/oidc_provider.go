package gateway

import (
	"crypto"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"net/url"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/go-jose/go-jose/v4"
	"golang.org/x/oauth2"

	"github.com/adrianliechti/donkey/internal/identity"
	"github.com/adrianliechti/donkey/internal/webpage"
)

const (
	purposeOIDCConsent = "donkey/oidc-consent/v1"
	purposeOIDCAccess  = "donkey/oidc-access/v1"
	purposeOIDCRefresh = "donkey/oidc-refresh/v1"
	oidcKeyLifetime    = 30 * 24 * time.Hour
)

type oidcSigningKey struct {
	Key     jose.JSONWebKey `json:"key"`
	Created int64           `json:"created"`
}

type oidcClient struct {
	ID      string
	Version string
	Route   route
}

type oidcConsent struct {
	ID          string `json:"id"`
	Subject     string `json:"sub"`
	ClientID    string `json:"client_id"`
	Version     string `json:"version"`
	RedirectURI string `json:"redirect_uri"`
	Scope       string `json:"scope"`
	State       string `json:"state,omitempty"`
	Nonce       string `json:"nonce,omitempty"`
	Challenge   string `json:"challenge"`
	Expires     int64  `json:"exp"`
}

type oidcCode struct {
	Request  oidcConsent `json:"request"`
	Identity session     `json:"identity"`
	Expires  int64       `json:"exp"`
}

type oidcGrant struct {
	ClientID    string  `json:"client_id"`
	Version     string  `json:"version"`
	Identity    session `json:"identity"`
	Scope       string  `json:"scope"`
	Nonce       string  `json:"nonce,omitempty"`
	RefreshHash string  `json:"refresh_hash,omitempty"`
	Expires     int64   `json:"exp"`
}

type oidcCredential struct {
	Grant    string `json:"grant"`
	ClientID string `json:"client_id"`
	Expires  int64  `json:"exp"`
	Random   string `json:"random,omitempty"`
}

func oidcDigest(raw string) string {
	digest := sha256.Sum256([]byte(raw))
	return base64.RawURLEncoding.EncodeToString(digest[:])
}

func (s *Server) handleOIDCDiscovery(w http.ResponseWriter, r *http.Request) {
	if !s.onAuthorizationHost(r) {
		http.NotFound(w, r)
		return
	}
	setCORSHeaders(w.Header())
	i := s.authorizationIssuer()
	writeOAuthJSON(w, http.StatusOK, map[string]any{
		"issuer":                 i,
		"authorization_endpoint": i + "/oauth2/authorize", "token_endpoint": i + "/oauth2/token",
		"userinfo_endpoint": i + "/oauth2/userinfo", "jwks_uri": i + "/oauth2/jwks",
		"revocation_endpoint":      i + "/oauth2/revoke",
		"response_types_supported": []string{"code"}, "response_modes_supported": []string{"query"},
		"grant_types_supported":   []string{"authorization_code", "refresh_token"},
		"subject_types_supported": []string{"public"}, "id_token_signing_alg_values_supported": []string{"RS256"},
		"scopes_supported":                               []string{"openid", "profile", "email", "offline_access"},
		"claims_supported":                               []string{"iss", "sub", "aud", "exp", "iat", "nonce", "auth_time", "name", "preferred_username", "email", "email_verified"},
		"code_challenge_methods_supported":               []string{"S256"},
		"token_endpoint_auth_methods_supported":          []string{"none", "client_secret_basic", "client_secret_post"},
		"revocation_endpoint_auth_methods_supported":     []string{"none", "client_secret_basic", "client_secret_post"},
		"authorization_response_iss_parameter_supported": true,
		"request_parameter_supported":                    false, "request_uri_parameter_supported": false, "claims_parameter_supported": false,
	})
}

// Transactions have a bounded wait even if a storage backend is unavailable.
func (s *Server) oidcTransaction(r *http.Request, fn func(*oidcProviderState) error) error {
	return s.updateOIDCState(r.Context(), fn)
}

func oidcStorageError(w http.ResponseWriter, err error) {
	log.Printf("OIDC state: %v", err)
	oauthError(w, http.StatusServiceUnavailable, "temporarily_unavailable", "the sign-in service could not persist its state; try again shortly")
}

func ensureOIDCKeys(state *oidcProviderState) error {
	now := time.Now()
	state.Keys = slices.DeleteFunc(state.Keys, func(k oidcSigningKey) bool {
		return now.After(time.Unix(k.Created, 0).Add(oidcKeyLifetime + oauthAccessTokenTTL + 5*time.Minute))
	})
	if len(state.Keys) > 0 && now.Before(time.Unix(state.Keys[len(state.Keys)-1].Created, 0).Add(oidcKeyLifetime)) {
		return nil
	}
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		return err
	}
	jwk := jose.JSONWebKey{Key: key, Use: "sig", Algorithm: string(jose.RS256)}
	thumbprint, err := jwk.Thumbprint(crypto.SHA256)
	if err != nil {
		return err
	}
	jwk.KeyID = base64.RawURLEncoding.EncodeToString(thumbprint)
	state.Keys = append(state.Keys, oidcSigningKey{Key: jwk, Created: now.Unix()})
	return nil
}

func (s *Server) handleOIDCJWKS(w http.ResponseWriter, r *http.Request) {
	if !s.onAuthorizationHost(r) {
		http.NotFound(w, r)
		return
	}
	setCORSHeaders(w.Header())
	var keys jose.JSONWebKeySet
	if err := s.oidcTransaction(r, func(state *oidcProviderState) error {
		keys.Keys = nil
		if err := ensureOIDCKeys(state); err != nil {
			return err
		}
		for _, key := range state.Keys {
			keys.Keys = append(keys.Keys, key.Key.Public())
		}
		return nil
	}); err != nil {
		oidcStorageError(w, err)
		return
	}
	writeOAuthJSON(w, http.StatusOK, keys)
}

func oidcScopes(raw string) (string, bool) {
	fields := strings.Fields(raw)
	for _, scope := range fields {
		if !contains([]string{"openid", "profile", "email", "offline_access"}, scope) {
			return "", false
		}
	}
	if !contains(fields, "openid") {
		return "", false
	}
	slices.Sort(fields)
	return strings.Join(slices.Compact(fields), " "), true
}

func uniqueOAuthParameters(values url.Values) bool {
	for _, values := range values {
		if len(values) != 1 {
			return false
		}
	}
	return true
}

var oidcConsentPage = webpage.Parse(`<h1>Sign in to {{.Client}}?</h1>
<p>This app will receive your sign-in identifier{{if .Profile}}, name and username{{end}}{{if .Email}}, and email address{{end}}.</p>
{{if .Offline}}<p>It can continue to access this information for up to 30 days without asking you to sign in again.</p>{{end}}
<p>After approval, the browser returns to<br><code>{{.Redirect}}</code></p>
<form method="post" action="/oauth2/authorize">
<input type="hidden" name="flow" value="oidc">
<input type="hidden" name="request" value="{{.Request}}">
<div class="actions"><button class="primary" name="decision" value="approve">Sign in</button><button name="decision" value="deny">Cancel</button></div>
</form><div class="foot">donkey gateway</div>`)

func (s *Server) handleOIDCAuthorize(w http.ResponseWriter, r *http.Request, q url.Values) {
	client, ok := s.oidcClient(q.Get("client_id"))
	redirect := q.Get("redirect_uri")
	if !ok || !contains(client.Route.OIDC.RedirectURIs, redirect) {
		oauthError(w, http.StatusBadRequest, "invalid_request", "the client or redirect_uri is not registered")
		return
	}
	fail := func(code, description string) {
		redirectOAuthError(w, r, redirect, q.Get("state"), code, description, s.authorizationIssuer())
	}
	if !uniqueOAuthParameters(q) || len(q.Encode()) > 16*1024 {
		fail("invalid_request", "invalid authorization parameters")
		return
	}
	if q.Get("response_type") != "code" {
		fail("unsupported_response_type", "only response_type=code is supported")
		return
	}
	if q.Get("response_mode") != "" && q.Get("response_mode") != "query" {
		fail("invalid_request", "only query response mode is supported")
		return
	}
	if q.Get("code_challenge_method") != "S256" || !validPKCEChallenge(q.Get("code_challenge")) {
		fail("invalid_request", "S256 PKCE is required")
		return
	}
	scope, ok := oidcScopes(q.Get("scope"))
	if !ok {
		fail("invalid_scope", "request openid with optional profile, email and offline_access")
		return
	}
	for _, unsupported := range []string{"resource", "request", "request_uri", "claims"} {
		if q.Has(unsupported) {
			fail("invalid_request", unsupported+" is not supported for web-app login")
			return
		}
	}
	prompts := strings.Fields(q.Get("prompt"))
	for _, prompt := range prompts {
		if !contains([]string{"none", "login", "consent"}, prompt) {
			fail("invalid_request", "unsupported prompt")
			return
		}
	}
	if contains(prompts, "none") && len(prompts) != 1 {
		fail("invalid_request", "prompt=none cannot be combined with another prompt")
		return
	}
	var maxAge *int64
	if q.Has("max_age") {
		value, err := strconv.ParseInt(q.Get("max_age"), 10, 64)
		if err != nil || value < 0 {
			fail("invalid_request", "max_age must be a nonnegative integer")
			return
		}
		maxAge = &value
	}
	if !s.cfg.authConfigured() {
		fail("temporarily_unavailable", "no identity provider is configured")
		return
	}
	sess := s.sessionFrom(r)
	needsLogin := sess == nil || contains(prompts, "login") || maxAge != nil && (sess.AuthTime == 0 || *maxAge == 0 || time.Now().Unix()-sess.AuthTime > *maxAge)
	if needsLogin {
		if contains(prompts, "none") {
			fail("login_required", "an interactive sign-in is required")
			return
		}
		// The upstream callback validates max_age. Remove the forcing parameters
		// from its sealed return URL so the completed login cannot loop.
		ret := *r.URL
		q.Del("max_age")
		q.Set("prompt", "consent")
		ret.RawQuery = q.Encode()
		var options []oauth2.AuthCodeOption
		if contains(prompts, "login") {
			options = append(options, oauth2.SetAuthURLParam("prompt", "login"))
		}
		s.beginLoginOptions(w, r, 0, ret.RequestURI(), maxAge, options...)
		return
	}
	if !client.Route.allows(sess.Subject, sess.Email, sess.Groups) {
		fail("access_denied", "your identity is not allowed to use this app")
		return
	}
	if contains(prompts, "none") {
		fail("consent_required", "approval is required to share your identity with this app")
		return
	}
	consent := oidcConsent{ID: randomHex(32), Subject: sess.Subject, ClientID: client.ID, Version: client.Version,
		RedirectURI: redirect, Scope: scope, State: q.Get("state"), Nonce: q.Get("nonce"), Challenge: q.Get("code_challenge"), Expires: time.Now().Add(oauthCodeTTL).Unix()}
	sealed, err := identity.Seal(s.cfg.SessionKey, purposeOIDCConsent, consent)
	if err != nil {
		oidcStorageError(w, err)
		return
	}
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("Referrer-Policy", "no-referrer")
	w.Header().Set("Content-Security-Policy", "frame-ancestors 'none'")
	scopes := strings.Fields(scope)
	webpage.Write(w, http.StatusOK, oidcConsentPage, map[string]any{
		"Title": "Sign in to app", "Client": client.Route.App + " (" + client.Route.Space + ")", "Redirect": redirect, "Request": sealed,
		"Profile": contains(scopes, "profile"), "Email": contains(scopes, "email"), "Offline": contains(scopes, "offline_access"),
	})
}

func (s *Server) handleOIDCConsent(w http.ResponseWriter, r *http.Request) {
	var consent oidcConsent
	if !uniqueOAuthParameters(r.PostForm) || identity.Unseal(s.cfg.SessionKey, purposeOIDCConsent, r.PostForm.Get("request"), &consent) != nil || consent.Expires <= time.Now().Unix() || consent.ID == "" {
		oauthError(w, http.StatusBadRequest, "invalid_request", "this authorization request is invalid or expired")
		return
	}
	client, ok := s.oidcClient(consent.ClientID)
	if !ok || client.Version != consent.Version || !contains(client.Route.OIDC.RedirectURIs, consent.RedirectURI) {
		oauthError(w, http.StatusBadRequest, "invalid_client", "the app registration has changed")
		return
	}
	fail := func(code, description string) {
		redirectOAuthError(w, r, consent.RedirectURI, consent.State, code, description, s.authorizationIssuer())
	}
	sess := s.sessionFrom(r)
	if sess == nil || sess.Subject != consent.Subject {
		fail("login_required", "sign in again before approving access")
		return
	}
	if r.PostForm.Get("decision") != "approve" {
		fail("access_denied", "the user declined access")
		return
	}
	if !client.Route.allows(sess.Subject, sess.Email, sess.Groups) {
		fail("access_denied", "your identity is not allowed to use this app")
		return
	}
	// Web-app login grants carry claims only, never upstream credentials.
	id := *sess
	id.RefreshToken, id.AccessToken, id.Grant = "", "", ""
	code := randomHex(32)
	used := false
	current := false
	err := s.oidcTransaction(r, func(state *oidcProviderState) error {
		current = state.currentClient(client)
		if !current {
			return nil
		}
		_, used = state.Consents[consent.ID]
		if used {
			return nil
		}
		state.Consents[consent.ID] = consent.Expires
		state.Codes[oidcDigest(code)] = oidcCode{Request: consent, Identity: id, Expires: consent.Expires}
		return nil
	})
	if err != nil {
		oidcStorageError(w, err)
		return
	}
	if !current {
		fail("invalid_request", "the app registration has changed")
		return
	}
	if used {
		fail("invalid_request", "this approval has already been used")
		return
	}
	u, _ := url.Parse(consent.RedirectURI)
	q := u.Query()
	q.Set("code", code)
	if consent.State != "" {
		q.Set("state", consent.State)
	}
	q.Set("iss", s.authorizationIssuer())
	u.RawQuery = q.Encode()
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("Referrer-Policy", "no-referrer")
	http.Redirect(w, r, u.String(), http.StatusFound)
}

// OAuth's Basic credentials are form-encoded before base64 encoding.
func oidcRequestClientID(r *http.Request) string {
	if name, _, ok := r.BasicAuth(); ok {
		decoded, err := url.QueryUnescape(name)
		if err == nil {
			return decoded
		}
	}
	return r.PostForm.Get("client_id")
}

func (s *Server) authenticateOIDCClient(w http.ResponseWriter, r *http.Request) (*oidcClient, bool) {
	fail := func() (*oidcClient, bool) {
		w.Header().Set("WWW-Authenticate", `Basic realm="donkey"`)
		oauthError(w, http.StatusUnauthorized, "invalid_client", "the client credentials are invalid")
		return nil, false
	}
	if !uniqueOAuthParameters(r.PostForm) || r.URL.RawQuery != "" {
		oauthError(w, http.StatusBadRequest, "invalid_request", "client parameters must appear once in the form body")
		return nil, false
	}
	if r.Header.Get("Authorization") != "" && r.PostForm.Has("client_secret") {
		oauthError(w, http.StatusBadRequest, "invalid_request", "use one client authentication method")
		return nil, false
	}
	client, ok := s.oidcClient(oidcRequestClientID(r))
	if !ok {
		return fail()
	}
	secret := r.PostForm.Get("client_secret")
	if r.Header.Get("Authorization") != "" {
		_, password, basic := r.BasicAuth()
		if !basic || r.PostForm.Has("client_id") && r.PostForm.Get("client_id") != client.ID {
			return fail()
		}
		var err error
		secret, err = url.QueryUnescape(password)
		if err != nil {
			return fail()
		}
	}
	expected := client.Route.OIDC.ClientSecretSHA256
	if expected == "" {
		if r.Header.Get("Authorization") != "" || r.PostForm.Has("client_secret") {
			return fail()
		}
	} else {
		digest := sha256.Sum256([]byte(secret))
		want, _ := hex.DecodeString(expected)
		if len(secret) < 32 || subtle.ConstantTimeCompare(digest[:], want) != 1 {
			return fail()
		}
	}
	return client, true
}

func (s *Server) oidcClaims(id session, scope string) map[string]any {
	claims := map[string]any{"sub": oidcDigest(s.cfg.OIDCIssuer + "\x00" + id.Subject)}
	if contains(strings.Fields(scope), "email") && id.Email != "" {
		claims["email"] = id.Email
		if id.EmailVerified != nil {
			claims["email_verified"] = *id.EmailVerified
		}
	}
	if contains(strings.Fields(scope), "profile") {
		if id.Name != "" {
			claims["name"] = id.Name
		}
		if id.PreferredUsername != "" {
			claims["preferred_username"] = id.PreferredUsername
		}
	}
	return claims
}

func (s *Server) issueOIDCTokens(state *oidcProviderState, grantID string, grant oidcGrant) (map[string]any, error) {
	if err := ensureOIDCKeys(state); err != nil {
		return nil, err
	}
	now := time.Now().Unix()
	expires := min(now+int64(oauthAccessTokenTTL/time.Second), grant.Expires)
	if expires <= now {
		return nil, fmt.Errorf("the identity grant has expired")
	}
	credential := oidcCredential{Grant: grantID, ClientID: grant.ClientID, Expires: expires}
	access, err := identity.Seal(s.cfg.SessionKey, purposeOIDCAccess, credential)
	if err != nil {
		return nil, err
	}
	claims := s.oidcClaims(grant.Identity, grant.Scope)
	claims["iss"], claims["aud"], claims["iat"], claims["exp"] = s.authorizationIssuer(), grant.ClientID, now, expires
	if grant.Nonce != "" {
		claims["nonce"] = grant.Nonce
	}
	if grant.Identity.AuthTime != 0 {
		claims["auth_time"] = grant.Identity.AuthTime
	}
	key := state.Keys[len(state.Keys)-1].Key
	signer, err := jose.NewSigner(jose.SigningKey{Algorithm: jose.RS256, Key: key}, (&jose.SignerOptions{}).WithType("JWT"))
	if err != nil {
		return nil, err
	}
	payload, err := json.Marshal(claims)
	if err != nil {
		return nil, err
	}
	signed, err := signer.Sign(payload)
	if err != nil {
		return nil, err
	}
	idToken, err := signed.CompactSerialize()
	if err != nil {
		return nil, err
	}
	response := map[string]any{"access_token": access, "id_token": idToken, "token_type": "Bearer", "expires_in": expires - now, "scope": grant.Scope}
	if contains(strings.Fields(grant.Scope), "offline_access") {
		credential.Expires, credential.Random = grant.Expires, randomHex(32)
		refresh, err := identity.Seal(s.cfg.SessionKey, purposeOIDCRefresh, credential)
		if err != nil {
			return nil, err
		}
		response["refresh_token"] = refresh
		grant.RefreshHash = oidcDigest(refresh)
	}
	state.Grants[grantID] = grant
	return response, nil
}

func (s *Server) handleOIDCToken(w http.ResponseWriter, r *http.Request) {
	client, ok := s.authenticateOIDCClient(w, r)
	if !ok {
		return
	}
	grantType := r.PostForm.Get("grant_type")
	if grantType != "authorization_code" && grantType != "refresh_token" {
		oauthError(w, http.StatusBadRequest, "unsupported_grant_type", "use authorization_code or refresh_token")
		return
	}
	if r.PostForm.Has("resource") {
		oauthError(w, http.StatusBadRequest, "invalid_target", "web-app tokens are for UserInfo")
		return
	}
	var response map[string]any
	failure := ""
	err := s.oidcTransaction(r, func(state *oidcProviderState) error {
		response, failure = nil, "invalid_grant"
		if !state.currentClient(client) {
			return nil
		}
		var grant oidcGrant
		var grantID string
		if grantType == "authorization_code" {
			key := oidcDigest(r.PostForm.Get("code"))
			code, found := state.Codes[key]
			if !found || code.Request.ClientID != client.ID {
				return nil
			}
			delete(state.Codes, key)
			verifier := r.PostForm.Get("code_verifier")
			challenge := oidcDigest(verifier)
			if code.Request.Version != client.Version || code.Identity.Expires <= time.Now().Unix() ||
				code.Request.RedirectURI != r.PostForm.Get("redirect_uri") || !pkceVerifierRE.MatchString(verifier) ||
				subtle.ConstantTimeCompare([]byte(challenge), []byte(code.Request.Challenge)) != 1 {
				return nil
			}
			grantID = randomHex(32)
			grant = oidcGrant{ClientID: client.ID, Version: client.Version, Identity: code.Identity, Scope: code.Request.Scope, Nonce: code.Request.Nonce, Expires: code.Identity.Expires}
			if contains(strings.Fields(grant.Scope), "offline_access") {
				grant.Expires = time.Now().Add(oauthRefreshTokenTTL).Unix()
			} else {
				grant.Expires = min(grant.Expires, time.Now().Add(oauthAccessTokenTTL).Unix())
			}
		} else {
			raw := r.PostForm.Get("refresh_token")
			var credential oidcCredential
			if identity.Unseal(s.cfg.SessionKey, purposeOIDCRefresh, raw, &credential) != nil || credential.ClientID != client.ID || credential.Expires <= time.Now().Unix() {
				return nil
			}
			grantID = credential.Grant
			var found bool
			grant, found = state.Grants[grantID]
			if !found || grant.ClientID != client.ID || grant.Version != client.Version || grant.RefreshHash == "" {
				return nil
			}
			if subtle.ConstantTimeCompare([]byte(grant.RefreshHash), []byte(oidcDigest(raw))) != 1 {
				// Reuse of any older token revokes the entire family, including
				// access tokens and the refresh token issued to the first caller.
				delete(state.Grants, grantID)
				return nil
			}
			if requested := r.PostForm.Get("scope"); requested != "" {
				scope, valid := oidcScopes(requested)
				if !valid {
					failure = "invalid_scope"
					return nil
				}
				for _, value := range strings.Fields(scope) {
					if !contains(strings.Fields(grant.Scope), value) {
						failure = "invalid_scope"
						return nil
					}
				}
				grant.Scope = scope
			}
			grant.RefreshHash = ""
		}
		if !client.Route.allows(grant.Identity.Subject, grant.Identity.Email, grant.Identity.Groups) {
			delete(state.Grants, grantID)
			failure = "access_denied"
			return nil
		}
		var err error
		response, err = s.issueOIDCTokens(state, grantID, grant)
		if err == nil {
			failure = ""
		}
		return err
	})
	if err != nil {
		oidcStorageError(w, err)
		return
	}
	if failure != "" {
		oauthError(w, http.StatusBadRequest, failure, "the grant is invalid, expired, revoked or does not match this request")
		return
	}
	writeOAuthJSON(w, http.StatusOK, response)
}

func (s *Server) handleOIDCUserInfo(w http.ResponseWriter, r *http.Request) {
	if !s.onAuthorizationHost(r) {
		http.NotFound(w, r)
		return
	}
	setCORSHeaders(w.Header())
	fail := func() {
		w.Header().Set("WWW-Authenticate", `Bearer error="invalid_token"`)
		oauthError(w, http.StatusUnauthorized, "invalid_token", "a valid web-app access token is required")
	}
	parts := strings.Fields(r.Header.Get("Authorization"))
	var credential oidcCredential
	if len(parts) != 2 || !strings.EqualFold(parts[0], "Bearer") || identity.Unseal(s.cfg.SessionKey, purposeOIDCAccess, parts[1], &credential) != nil || credential.Expires <= time.Now().Unix() {
		fail()
		return
	}
	client, ok := s.oidcClient(credential.ClientID)
	if !ok {
		fail()
		return
	}
	var claims map[string]any
	err := s.oidcTransaction(r, func(state *oidcProviderState) error {
		claims = nil
		grant, found := state.Grants[credential.Grant]
		if found && state.currentClient(client) && grant.ClientID == client.ID && grant.Version == client.Version && client.Route.allows(grant.Identity.Subject, grant.Identity.Email, grant.Identity.Groups) {
			claims = s.oidcClaims(grant.Identity, grant.Scope)
		}
		return nil
	})
	if err != nil {
		oidcStorageError(w, err)
		return
	}
	if claims == nil {
		fail()
		return
	}
	writeOAuthJSON(w, http.StatusOK, claims)
}

func (s *Server) handleOIDCRevoke(w http.ResponseWriter, r *http.Request) {
	if !s.onAuthorizationHost(r) {
		http.NotFound(w, r)
		return
	}
	setCORSHeaders(w.Header())
	r.Body = http.MaxBytesReader(w, r.Body, 64*1024)
	if r.ParseForm() != nil {
		oauthError(w, http.StatusBadRequest, "invalid_request", "invalid revocation request")
		return
	}
	client, ok := s.authenticateOIDCClient(w, r)
	if !ok {
		return
	}
	var credential oidcCredential
	raw := r.PostForm.Get("token")
	if raw == "" {
		oauthError(w, http.StatusBadRequest, "invalid_request", "token is required")
		return
	}
	err := identity.Unseal(s.cfg.SessionKey, purposeOIDCRefresh, raw, &credential)
	if err != nil {
		err = identity.Unseal(s.cfg.SessionKey, purposeOIDCAccess, raw, &credential)
	}
	if err == nil && credential.ClientID == client.ID {
		if err := s.oidcTransaction(r, func(state *oidcProviderState) error { delete(state.Grants, credential.Grant); return nil }); err != nil {
			oidcStorageError(w, err)
			return
		}
	}
	writeOAuthJSON(w, http.StatusOK, map[string]any{})
}
