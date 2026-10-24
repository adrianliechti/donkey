package gateway

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"log"
	"net/http"
	"net/url"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/coreos/go-oidc/v3/oidc"
	"golang.org/x/oauth2"

	"github.com/adrianliechti/donkey/internal/identity"
)

// authenticator wraps the OIDC provider. Discovery happens lazily with
// retries — the IdP may come up after the gateway (it often runs on the same
// cluster), and a gateway that crash-loops until then helps nobody.
type authenticator struct {
	cfg *Config

	mu       sync.Mutex
	provider *oidc.Provider
	verifier *oidc.IDTokenVerifier
	// groupsScope is set when the issuer advertises a "groups" scope —
	// asking for it unprompted makes strict IdPs fail the whole login.
	groupsScope bool
	lastAttempt time.Time
	lastErr     error
}

func newAuthenticator(cfg *Config) *authenticator {
	return &authenticator{cfg: cfg}
}

func (a *authenticator) ready(ctx context.Context) error {
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.provider != nil {
		return nil
	}
	// Back off between discovery attempts so a down IdP doesn't get hammered
	// by every incoming request.
	if time.Since(a.lastAttempt) < 5*time.Second {
		return a.lastErr
	}
	a.lastAttempt = time.Now()
	dctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	provider, err := oidc.NewProvider(dctx, a.cfg.OIDCIssuer)
	if err != nil {
		a.lastErr = fmt.Errorf("the identity provider %s doesn't answer: %w", a.cfg.OIDCIssuer, err)
		return a.lastErr
	}
	var claims struct {
		ScopesSupported []string `json:"scopes_supported"`
	}
	_ = provider.Claims(&claims)
	a.groupsScope = slices.Contains(claims.ScopesSupported, "groups")
	a.provider = provider
	a.verifier = provider.Verifier(&oidc.Config{ClientID: a.cfg.OIDCClientID})
	a.lastErr = nil
	return nil
}

// oauthConfig is per request: hostnames of the base domain share the central
// callback — one redirect URI to register at the IdP — while a custom domain
// outside it falls back to a callback on its own hostname (which the IdP
// must then know about).
func (a *authenticator) oauthConfig(host string, upstream *identity.Upstream) *oauth2.Config {
	scopes := []string{oidc.ScopeOpenID, "profile", "email"}
	if a.groupsScope {
		scopes = append(scopes, "groups")
	}
	scopes = append(scopes, upstream.LoginScopes()...)
	callbackHost := host
	if a.cfg.ssoHost(host) {
		callbackHost = a.cfg.Domain
	}
	return &oauth2.Config{
		ClientID:     a.cfg.OIDCClientID,
		ClientSecret: a.cfg.OIDCClientSecret,
		Endpoint:     a.provider.Endpoint(),
		RedirectURL:  "https://" + callbackHost + "/oauth2/callback",
		Scopes:       scopes,
	}
}

// startLogin sends the visitor to the IdP, leaving a sealed state cookie
// behind so the callback can verify the round trip.
func (s *Server) startLogin(w http.ResponseWriter, r *http.Request) {
	s.beginLogin(w, r, 0, r.URL.RequestURI())
}

// startCLILogin begins a login on behalf of `donkey login`, which waits on
// 127.0.0.1:<port> for the sealed token the callback mints. Anyone the IdP
// admits may hold a token — what it may do stays a per-route decision.
func (s *Server) startCLILogin(w http.ResponseWriter, r *http.Request) {
	port, err := strconv.Atoi(r.URL.Query().Get("port"))
	if err != nil || port < 1 || port > 65535 {
		s.errorPage(w, http.StatusBadRequest, "This sign-in link is incomplete.", "Run `donkey login` again.")
		return
	}
	s.beginLogin(w, r, port, "/")
}

func (s *Server) beginLogin(w http.ResponseWriter, r *http.Request, cliPort int, ret string) {
	if err := s.auth.ready(r.Context()); err != nil {
		s.errorPage(w, http.StatusBadGateway, "The sign-in service isn't reachable.", err.Error())
		return
	}
	host := requestHost(r)
	state := loginState{
		State:   randomHex(16),
		Nonce:   randomHex(16),
		Host:    host,
		Return:  ret,
		CLIPort: cliPort,
		Expires: time.Now().Add(loginTTL).Unix(),
	}
	sealed, err := identity.Seal(s.cfg.SessionKey, purposeLogin, state)
	if err != nil {
		s.errorPage(w, http.StatusInternalServerError, "Sign-in couldn't start.", err.Error())
		return
	}
	// On the shared domain the callback arrives on the central host — the
	// state cookie has to travel there, so it gets the domain too.
	http.SetCookie(w, &http.Cookie{
		Name: stateCookie, Value: sealed, Path: "/oauth2/",
		Domain: s.cookieDomain(host),
		Secure: true, HttpOnly: true, SameSite: http.SameSiteLaxMode,
		MaxAge: int(loginTTL / time.Second),
	})
	url := s.auth.oauthConfig(host, s.upstream).AuthCodeURL(state.State, oidc.Nonce(state.Nonce))
	http.Redirect(w, r, url, http.StatusFound)
}

// handleCallback finishes the login: code for tokens, ID token verified,
// identity sealed into the session cookie, back to where the visitor was.
func (s *Server) handleCallback(w http.ResponseWriter, r *http.Request) {
	if err := s.auth.ready(r.Context()); err != nil {
		s.errorPage(w, http.StatusBadGateway, "The sign-in service isn't reachable.", err.Error())
		return
	}
	c, err := r.Cookie(stateCookie)
	if err != nil {
		s.errorPage(w, http.StatusBadRequest, "This sign-in attempt is stale.", "Start again from the app's address.")
		return
	}
	var state loginState
	if err := identity.Unseal(s.cfg.SessionKey, purposeLogin, c.Value, &state); err != nil || time.Now().Unix() >= state.Expires {
		s.errorPage(w, http.StatusBadRequest, "This sign-in attempt is stale.", "Start again from the app's address.")
		return
	}
	// startLogin always fills both, so a blank one cannot be a state this
	// gateway issued — and comparing a blank State or Nonce with != would
	// wave through a callback that carries neither.
	if state.State == "" || state.Nonce == "" {
		s.errorPage(w, http.StatusBadRequest, "This sign-in attempt is stale.", "Start again from the app's address.")
		return
	}
	clearCookie(w, stateCookie, "/oauth2/", s.cookieDomain(requestHost(r)))
	if errCode := r.URL.Query().Get("error"); errCode != "" {
		s.errorPage(w, http.StatusBadRequest, "The identity provider refused the sign-in.",
			errCode+": "+r.URL.Query().Get("error_description"))
		return
	}
	if r.URL.Query().Get("state") != state.State {
		s.errorPage(w, http.StatusBadRequest, "This sign-in attempt doesn't match.", "Start again from the app's address.")
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 15*time.Second)
	defer cancel()
	token, err := s.auth.oauthConfig(requestHost(r), s.upstream).Exchange(ctx, r.URL.Query().Get("code"))
	if err != nil {
		s.errorPage(w, http.StatusBadGateway, "The identity provider didn't hand out a token.", err.Error())
		return
	}
	rawID, _ := token.Extra("id_token").(string)
	if rawID == "" {
		s.errorPage(w, http.StatusBadGateway, "The identity provider answered without an ID token.", "Check that the client is registered as an OpenID Connect app.")
		return
	}
	idToken, err := s.auth.verifier.Verify(ctx, rawID)
	if err != nil {
		s.errorPage(w, http.StatusBadGateway, "The ID token didn't verify.", err.Error())
		return
	}
	if idToken.Nonce != state.Nonce {
		s.errorPage(w, http.StatusBadRequest, "This sign-in attempt doesn't match.", "Start again from the app's address.")
		return
	}
	var claims struct {
		Email             string `json:"email"`
		Name              string `json:"name"`
		PreferredUsername string `json:"preferred_username"`
		// ObjectID is Entra's tenant-wide user ID (oid, with the profile
		// scope); its sub is pairwise per app registration.
		ObjectID string   `json:"oid"`
		Groups   []string `json:"groups"`
	}
	if err := idToken.Claims(&claims); err != nil {
		s.errorPage(w, http.StatusBadGateway, "The ID token couldn't be read.", err.Error())
		return
	}
	name := claims.Name
	if name == "" {
		name = claims.PreferredUsername
	}
	sess := &session{
		Subject: idToken.Subject,
		Email:   claims.Email,
		Name:    name,
		// Kept apart too: apps get it as X-Forwarded-Preferred-Username.
		PreferredUsername: claims.PreferredUsername,
		ObjectID:          claims.ObjectID,
		Groups:            claims.Groups,
		Expires:           time.Now().Add(sessionTTL).Unix(),
	}
	if s.upstream.Enabled() {
		// Kept for the token apps and viewers hand on; the login's own
		// access token is already for the configured scope.
		sess.RefreshToken = token.RefreshToken
		if sess.RefreshToken == "" {
			log.Printf("login for %s: the identity provider returned no refresh token — apps get no X-Forwarded-Access-Token (is offline_access allowed?)", sess.Subject)
		} else {
			sess.Grant = identity.Grant(sess.RefreshToken)
			s.upstream.Remember(sess.Grant, token.RefreshToken, token.AccessToken, token.Expiry, time.Unix(sess.Expires, 0))
		}
	}
	if err := s.setSession(w, r, sess, s.cookieDomain(requestHost(r))); err != nil {
		s.errorPage(w, http.StatusInternalServerError, "The session couldn't be stored.", err.Error())
		return
	}
	// A CLI round trip ends on the loopback: the terminal gets a sealed
	// token — and the browser keeps its session too, it just signed in.
	if state.CLIPort != 0 {
		cli := *sess
		cli.Expires = time.Now().Add(identity.TokenTTL).Unix()
		tok, err := identity.SealToken(s.cfg.SessionKey, cli)
		if err != nil {
			s.errorPage(w, http.StatusInternalServerError, "The sign-in couldn't be handed to the CLI.", err.Error())
			return
		}
		http.Redirect(w, r, fmt.Sprintf("http://127.0.0.1:%d/callback?token=%s", state.CLIPort, url.QueryEscape(tok)), http.StatusFound)
		return
	}
	http.Redirect(w, r, loginReturnURL(s.cfg, requestHost(r), state), http.StatusFound)
}

// loginReturnURL is where a finished login lands. The state is sealed, so its
// content is the gateway's own — but an open redirect is cheap to rule out
// anyway: paths only on the callback's own host, and a cross-host return only
// onto another hostname of the base domain.
func loginReturnURL(cfg *Config, callbackHost string, state loginState) string {
	path := state.Return
	if !strings.HasPrefix(path, "/") || strings.HasPrefix(path, "//") {
		path = "/"
	}
	if state.Host == "" || state.Host == callbackHost {
		return path
	}
	if !cfg.ssoHost(state.Host) {
		return path
	}
	return "https://" + state.Host + path
}

// handleLogout clears the session — on the shared domain that is one cookie
// for every app, so signing out anywhere signs out everywhere.
func (s *Server) handleLogout(w http.ResponseWriter, r *http.Request) {
	clearSession(w, r, s.cookieDomain(requestHost(r)))
	s.messagePage(w, http.StatusOK, "Signed out.", "Close the tab, or open the app again to sign back in.")
}

func randomHex(n int) string {
	b := make([]byte, n)
	_, _ = rand.Read(b)
	return hex.EncodeToString(b)
}
