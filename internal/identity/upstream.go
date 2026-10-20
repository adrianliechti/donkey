package identity

import (
	"context"
	"crypto/sha256"
	"encoding/base64"
	"errors"
	"fmt"
	"log"
	"net/http"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/coreos/go-oidc/v3/oidc"
	"golang.org/x/oauth2"
	"golang.org/x/sync/singleflight"
)

// ErrUpstreamToken means there is no IdP access token to be had for this
// identity: the installation names no auth.scope, the login predates it, or
// the IdP stopped honouring the refresh token. Signing in again is the fix
// for all but the first.
var ErrUpstreamToken = errors.New("no identity provider token for this sign-in")

// Upstream turns the IdP refresh token a login left inside donkey's sealed
// credentials into the user's access token for donkey's own API (Scope) —
// what login-protected apps receive as X-Auth-Request-Access-Token and the
// Trino console sends as Authorization: Bearer to its add-on. Services
// exchange it on-behalf-of for their own APIs.
//
// Only the gateway mints, so one process serializes every refresh. The
// credentials themselves are the durable store: when the IdP rotates a
// refresh token, the gateway re-seals the browser's session with the new
// one. This cache holds the access token until shortly before it expires and
// the newest refresh token per sign-in, so requests that still carry an
// older one (in flight during a rotation, or a CLI token) redeem the newest.
type Upstream struct {
	Issuer       string
	ClientID     string
	ClientSecret string
	// Scope is donkey's own API, e.g. api://<donkey-client-id>/<scope>.
	// Empty turns the whole mechanism off.
	Scope string

	group  singleflight.Group
	mu     sync.Mutex
	config *oauth2.Config
	cache  map[string]upstreamToken // by Grant
}

type upstreamToken struct {
	value  string
	err    error
	expiry time.Time
	// refreshToken is the newest the IdP handed out for this sign-in.
	refreshToken string
	// retainUntil is the latest expiry of a credential seen carrying this
	// sign-in: past it, nothing can present the grant any more.
	retainUntil time.Time
}

const (
	// upstreamSkew retires a token before the IdP does, so one handed out
	// is still good for the request that carries it.
	upstreamSkew = 2 * time.Minute
	// upstreamFailureTTL keeps a refused grant from turning every request
	// into a call to the IdP.
	upstreamFailureTTL = 30 * time.Second
	upstreamTimeout    = 15 * time.Second
)

var upstreamHTTP = &http.Client{Timeout: upstreamTimeout}

// Grant names a sign-in's IdP grant for as long as it lasts: derived from
// the login's refresh token and sealed beside it, so every later generation
// of that token still finds the same cache entry.
func Grant(refreshToken string) string {
	digest := sha256.Sum256([]byte(refreshToken))
	return base64.RawURLEncoding.EncodeToString(digest[:])
}

// Enabled reports whether logins should ask for, and keep, a refresh token.
func (u *Upstream) Enabled() bool {
	return u != nil && u.Scope != "" && u.Issuer != "" && u.ClientID != "" && u.ClientSecret != ""
}

// LoginScopes are what a login adds to openid/profile/email: a refresh token,
// and the scope itself so the IdP asks for consent up front and the login's
// own access token is already the right one.
func (u *Upstream) LoginScopes() []string {
	if !u.Enabled() {
		return nil
	}
	return []string{"offline_access", u.Scope}
}

// Remember seeds the cache with the access token the login itself returned.
func (u *Upstream) Remember(grant, refreshToken, accessToken string, expiry, until time.Time) {
	if !u.Enabled() || refreshToken == "" || accessToken == "" {
		return
	}
	if expiry.IsZero() {
		expiry = time.Now().Add(5 * time.Minute)
	}
	u.store(grant, upstreamToken{value: accessToken, expiry: cacheUntil(expiry), refreshToken: refreshToken}, until)
}

// AccessToken returns a current access token for Scope on behalf of whoever
// signed in, and the refresh token to keep from now on — a new one once the
// IdP rotated it. refreshToken is what the caller's credential carries; the
// cache's is newer whenever it has one. Concurrent callers for one sign-in
// share a single refresh, and the entry stays at least until `until`.
func (u *Upstream) AccessToken(ctx context.Context, grant, refreshToken string, until time.Time) (string, string, error) {
	if !u.Enabled() || refreshToken == "" {
		return "", refreshToken, ErrUpstreamToken
	}
	if grant == "" {
		grant = Grant(refreshToken)
	}
	if tok, ok := u.cached(grant, until); ok {
		return tok.value, tok.refreshToken, tok.err
	}
	v, _, _ := u.group.Do(grant, func() (any, error) {
		if tok, ok := u.cached(grant, until); ok {
			return tok, nil
		}
		// The first caller's cancellation must not fail everyone waiting.
		rctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), upstreamTimeout)
		defer cancel()
		tok, err := u.refresh(rctx, u.Latest(grant, refreshToken))
		if err != nil {
			tok.err = err
			tok.expiry = time.Now().Add(upstreamFailureTTL)
			log.Printf("no %s token for this sign-in (retrying in %s): %v", u.Scope, upstreamFailureTTL, err)
		}
		u.store(grant, tok, until)
		return tok, nil
	})
	tok := v.(upstreamToken)
	return tok.value, tok.refreshToken, tok.err
}

// Latest is the newest refresh token known for the sign-in: the cache's, or
// refreshToken when a restart emptied it and the credential is all there is.
func (u *Upstream) Latest(grant, refreshToken string) string {
	if u == nil {
		return refreshToken
	}
	u.mu.Lock()
	defer u.mu.Unlock()
	if latest := u.cache[grant].refreshToken; latest != "" {
		return latest
	}
	return refreshToken
}

func (u *Upstream) cached(grant string, until time.Time) (upstreamToken, bool) {
	u.mu.Lock()
	defer u.mu.Unlock()
	tok, ok := u.cache[grant]
	if !ok || !time.Now().Before(tok.expiry) {
		return upstreamToken{}, false
	}
	if until.After(tok.retainUntil) {
		tok.retainUntil = until
		u.cache[grant] = tok
	}
	return tok, true
}

func (u *Upstream) store(grant string, tok upstreamToken, until time.Time) {
	u.mu.Lock()
	defer u.mu.Unlock()
	if u.cache == nil {
		u.cache = map[string]upstreamToken{}
	}
	now := time.Now()
	for k, old := range u.cache {
		if !now.Before(old.retainUntil) {
			delete(u.cache, k)
		}
	}
	tok.retainUntil = latestOf(u.cache[grant].retainUntil, until, tok.expiry)
	u.cache[grant] = tok
}

func latestOf(times ...time.Time) time.Time {
	var out time.Time
	for _, t := range times {
		if t.After(out) {
			out = t
		}
	}
	return out
}

// cacheUntil retires a token upstreamSkew early, or halfway through its life
// when that is shorter — a one-minute token must still be cached.
func cacheUntil(expiry time.Time) time.Time {
	now := time.Now()
	until := expiry.Add(-upstreamSkew)
	if half := now.Add(expiry.Sub(now) / 2); until.Before(half) {
		return half
	}
	return until
}

func (u *Upstream) oauthConfig(ctx context.Context) (*oauth2.Config, error) {
	u.mu.Lock()
	config := u.config
	u.mu.Unlock()
	if config != nil {
		return config, nil
	}
	provider, err := oidc.NewProvider(ctx, u.Issuer)
	if err != nil {
		return nil, fmt.Errorf("the identity provider %s doesn't answer: %w", u.Issuer, err)
	}
	if provider.Endpoint().TokenURL == "" {
		return nil, fmt.Errorf("the identity provider %s names no token endpoint", u.Issuer)
	}
	u.mu.Lock()
	defer u.mu.Unlock()
	if u.config == nil {
		endpoint := provider.Endpoint()
		var metadata struct {
			Methods []string `json:"token_endpoint_auth_methods_supported"`
		}
		if err := provider.Claims(&metadata); err != nil {
			return nil, fmt.Errorf("cannot read identity provider token authentication methods")
		}
		switch {
		case slices.Contains(metadata.Methods, "client_secret_basic") && !slices.Contains(metadata.Methods, "client_secret_post"):
			endpoint.AuthStyle = oauth2.AuthStyleInHeader
		case slices.Contains(metadata.Methods, "client_secret_post") && !slices.Contains(metadata.Methods, "client_secret_basic"):
			endpoint.AuthStyle = oauth2.AuthStyleInParams
		}
		u.config = &oauth2.Config{ClientID: u.ClientID, ClientSecret: u.ClientSecret, Endpoint: endpoint}
	}
	return u.config, nil
}

// TokenSource uses the same Basic/form authentication negotiation as login,
// retains replacement refresh tokens, and preserves the previous one when
// providers omit it. Omitting scope preserves the login grant's API scopes.
func (u *Upstream) refresh(ctx context.Context, refreshToken string) (upstreamToken, error) {
	out := upstreamToken{refreshToken: refreshToken}
	config, err := u.oauthConfig(ctx)
	if err != nil {
		return out, err
	}
	ctx = context.WithValue(ctx, oauth2.HTTPClient, upstreamHTTP)
	token, err := config.TokenSource(ctx, &oauth2.Token{RefreshToken: refreshToken}).Token()
	if err != nil {
		var response *oauth2.RetrieveError
		if errors.As(err, &response) {
			description, _, _ := strings.Cut(response.ErrorDescription, "\r\n")
			return out, fmt.Errorf("%w: the identity provider refused a token for %s (%s: %s)", ErrUpstreamToken, u.Scope, response.ErrorCode, description)
		}
		return out, fmt.Errorf("the identity provider's token endpoint did not return a token")
	}
	if token.Expiry.IsZero() {
		token.Expiry = time.Now().Add(5 * time.Minute)
	}
	out.value, out.refreshToken, out.expiry = token.AccessToken, token.RefreshToken, cacheUntil(token.Expiry)
	return out, nil
}
