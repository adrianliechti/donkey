package gateway

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"net/http"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/coreos/go-oidc/v3/oidc"
	"golang.org/x/oauth2"
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

// oauthConfig is per request: the redirect URL has to be on the hostname the
// browser is actually visiting.
func (a *authenticator) oauthConfig(host string) *oauth2.Config {
	scopes := []string{oidc.ScopeOpenID, "profile", "email"}
	if a.groupsScope {
		scopes = append(scopes, "groups")
	}
	return &oauth2.Config{
		ClientID:     a.cfg.OIDCClientID,
		ClientSecret: a.cfg.OIDCClientSecret,
		Endpoint:     a.provider.Endpoint(),
		RedirectURL:  "https://" + host + "/oauth2/callback",
		Scopes:       scopes,
	}
}

// startLogin sends the visitor to the IdP, leaving a sealed state cookie
// behind so the callback can verify the round trip.
func (s *Server) startLogin(w http.ResponseWriter, r *http.Request) {
	if err := s.auth.ready(r.Context()); err != nil {
		s.errorPage(w, http.StatusBadGateway, "The sign-in service isn't reachable.", err.Error())
		return
	}
	state := loginState{
		State:   randomHex(16),
		Nonce:   randomHex(16),
		Return:  r.URL.RequestURI(),
		Expires: time.Now().Add(loginTTL).Unix(),
	}
	sealed, err := s.sealer.seal(state)
	if err != nil {
		s.errorPage(w, http.StatusInternalServerError, "Sign-in couldn't start.", err.Error())
		return
	}
	http.SetCookie(w, &http.Cookie{
		Name: stateCookie, Value: sealed, Path: "/oauth2/",
		Secure: true, HttpOnly: true, SameSite: http.SameSiteLaxMode,
		MaxAge: int(loginTTL / time.Second),
	})
	url := s.auth.oauthConfig(requestHost(r)).AuthCodeURL(state.State, oidc.Nonce(state.Nonce))
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
	if err := s.sealer.unseal(c.Value, &state); err != nil || time.Now().Unix() >= state.Expires {
		s.errorPage(w, http.StatusBadRequest, "This sign-in attempt is stale.", "Start again from the app's address.")
		return
	}
	clearCookie(w, stateCookie)
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
	token, err := s.auth.oauthConfig(requestHost(r)).Exchange(ctx, r.URL.Query().Get("code"))
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
		Email             string   `json:"email"`
		Name              string   `json:"name"`
		PreferredUsername string   `json:"preferred_username"`
		Groups            []string `json:"groups"`
		// Roles is where Entra puts app roles; Keycloak nests its realm
		// roles one level deeper — both count.
		Roles       []string `json:"roles"`
		RealmAccess struct {
			Roles []string `json:"roles"`
		} `json:"realm_access"`
	}
	if err := idToken.Claims(&claims); err != nil {
		s.errorPage(w, http.StatusBadGateway, "The ID token couldn't be read.", err.Error())
		return
	}
	name := claims.Name
	if name == "" {
		name = claims.PreferredUsername
	}
	roles := claims.Roles
	for _, r := range claims.RealmAccess.Roles {
		if !slices.Contains(roles, r) {
			roles = append(roles, r)
		}
	}
	sess := &session{
		Subject: idToken.Subject,
		Email:   claims.Email,
		Name:    name,
		Groups:  claims.Groups,
		Roles:   roles,
		Expires: time.Now().Add(sessionTTL).Unix(),
	}
	if err := s.setSession(w, sess); err != nil {
		s.errorPage(w, http.StatusInternalServerError, "The session couldn't be stored.", err.Error())
		return
	}
	// Return is sealed and was written by this gateway, but an open redirect
	// is cheap to rule out anyway: same-host paths only.
	target := state.Return
	if !strings.HasPrefix(target, "/") || strings.HasPrefix(target, "//") {
		target = "/"
	}
	http.Redirect(w, r, target, http.StatusFound)
}

func (s *Server) handleLogout(w http.ResponseWriter, r *http.Request) {
	clearCookie(w, sessionCookie)
	s.messagePage(w, http.StatusOK, "Signed out.", "Close the tab, or open the app again to sign back in.")
}

func randomHex(n int) string {
	b := make([]byte, n)
	_, _ = rand.Read(b)
	return hex.EncodeToString(b)
}
