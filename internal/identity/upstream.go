package identity

import (
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"

	"github.com/coreos/go-oidc/v3/oidc"
	"golang.org/x/sync/singleflight"
)

// ErrUpstreamToken means there is no IdP access token to be had for this
// identity: the installation names no auth.scope, the login predates it, or
// the IdP stopped honouring the refresh token. Signing in again is the fix
// for all but the first.
var ErrUpstreamToken = errors.New("no identity provider token for this sign-in")

// Upstream turns the IdP refresh token a login left inside donkey's sealed
// credentials into the user's access token for donkey's own API (Scope) —
// what apps and viewers hand on as X-Auth-Request-Access-Token, and what
// services behind them exchange on-behalf-of for their own APIs. The refresh
// token never leaves donkey; access tokens are cached per sign-in until
// shortly before they expire, so a busy visitor costs one token call an hour.
type Upstream struct {
	Issuer       string
	ClientID     string
	ClientSecret string
	// Scope is donkey's own API, e.g. api://<donkey-client-id>/<scope>.
	// Empty turns the whole mechanism off.
	Scope string

	group    singleflight.Group
	mu       sync.Mutex
	endpoint string
	cache    map[string]upstreamToken
}

type upstreamToken struct {
	value  string
	err    error
	expiry time.Time
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
func (u *Upstream) Remember(refreshToken, accessToken string, expiry time.Time) {
	if !u.Enabled() || refreshToken == "" || accessToken == "" {
		return
	}
	if expiry.IsZero() {
		expiry = time.Now().Add(5 * time.Minute)
	}
	u.store(upstreamKey(refreshToken), upstreamToken{value: accessToken, expiry: cacheUntil(expiry)})
}

// AccessToken returns a current access token for Scope on behalf of whoever
// refreshToken belongs to. Concurrent callers for one sign-in share a single
// refresh.
func (u *Upstream) AccessToken(ctx context.Context, refreshToken string) (string, error) {
	if !u.Enabled() || refreshToken == "" {
		return "", ErrUpstreamToken
	}
	key := upstreamKey(refreshToken)
	if tok, ok := u.cached(key); ok {
		return tok.value, tok.err
	}
	v, _, _ := u.group.Do(key, func() (any, error) {
		if tok, ok := u.cached(key); ok {
			return tok, nil
		}
		// The first caller's cancellation must not fail everyone waiting.
		rctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), upstreamTimeout)
		defer cancel()
		value, expiry, err := u.refresh(rctx, refreshToken)
		tok := upstreamToken{value: value, err: err, expiry: cacheUntil(expiry)}
		if err != nil {
			tok.expiry = time.Now().Add(upstreamFailureTTL)
			log.Printf("no %s token for this sign-in (retrying in %s): %v", u.Scope, upstreamFailureTTL, err)
		}
		u.store(key, tok)
		return tok, nil
	})
	tok := v.(upstreamToken)
	return tok.value, tok.err
}

func (u *Upstream) cached(key string) (upstreamToken, bool) {
	u.mu.Lock()
	defer u.mu.Unlock()
	tok, ok := u.cache[key]
	if !ok || !time.Now().Before(tok.expiry) {
		return upstreamToken{}, false
	}
	return tok, true
}

func (u *Upstream) store(key string, tok upstreamToken) {
	u.mu.Lock()
	defer u.mu.Unlock()
	if u.cache == nil {
		u.cache = map[string]upstreamToken{}
	}
	now := time.Now()
	for k, old := range u.cache {
		if !now.Before(old.expiry) {
			delete(u.cache, k)
		}
	}
	u.cache[key] = tok
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

// upstreamKey: the cache holds no refresh tokens, only their digests.
func upstreamKey(refreshToken string) string {
	digest := sha256.Sum256([]byte(refreshToken))
	return base64.RawURLEncoding.EncodeToString(digest[:])
}

func (u *Upstream) tokenEndpoint(ctx context.Context) (string, error) {
	u.mu.Lock()
	endpoint := u.endpoint
	u.mu.Unlock()
	if endpoint != "" {
		return endpoint, nil
	}
	provider, err := oidc.NewProvider(ctx, u.Issuer)
	if err != nil {
		return "", fmt.Errorf("the identity provider %s doesn't answer: %w", u.Issuer, err)
	}
	endpoint = provider.Endpoint().TokenURL
	if endpoint == "" {
		return "", fmt.Errorf("the identity provider %s names no token endpoint", u.Issuer)
	}
	u.mu.Lock()
	u.endpoint = endpoint
	u.mu.Unlock()
	return endpoint, nil
}

// refresh redeems the refresh token for Scope with donkey's client
// credentials. Entra's rotated refresh token in the answer is dropped: the
// one sealed into the session stays valid until it expires, and the session
// is not ours to rewrite here.
func (u *Upstream) refresh(ctx context.Context, refreshToken string) (string, time.Time, error) {
	endpoint, err := u.tokenEndpoint(ctx)
	if err != nil {
		return "", time.Time{}, err
	}
	form := url.Values{
		"grant_type":    {"refresh_token"},
		"refresh_token": {refreshToken},
		"client_id":     {u.ClientID},
		"client_secret": {u.ClientSecret},
		"scope":         {u.Scope},
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, strings.NewReader(form.Encode()))
	if err != nil {
		return "", time.Time{}, err
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("Accept", "application/json")
	res, err := upstreamHTTP.Do(req)
	if err != nil {
		return "", time.Time{}, fmt.Errorf("the identity provider's token endpoint didn't answer: %w", err)
	}
	defer res.Body.Close()
	var body struct {
		AccessToken string `json:"access_token"`
		// json.Number takes both: Entra's v1 endpoint quotes it.
		ExpiresIn   json.Number `json:"expires_in"`
		Error       string      `json:"error"`
		Description string      `json:"error_description"`
	}
	if err := json.NewDecoder(io.LimitReader(res.Body, 1<<20)).Decode(&body); err != nil {
		return "", time.Time{}, fmt.Errorf("the identity provider's token endpoint answered %s without a token", res.Status)
	}
	if body.Error != "" || body.AccessToken == "" {
		code := body.Error
		if code == "" {
			code = res.Status
		}
		// The description's first line is the readable part; Entra appends
		// trace and correlation ids after it.
		description, _, _ := strings.Cut(body.Description, "\r\n")
		return "", time.Time{}, fmt.Errorf("%w: the identity provider refused a token for %s (%s: %s)", ErrUpstreamToken, u.Scope, code, description)
	}
	lifetime := 5 * time.Minute
	if seconds, err := body.ExpiresIn.Int64(); err == nil && seconds > 0 {
		lifetime = time.Duration(seconds) * time.Second
	}
	return body.AccessToken, time.Now().Add(lifetime), nil
}
