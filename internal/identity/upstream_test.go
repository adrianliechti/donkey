package identity

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// fakeIssuer answers discovery and the refresh grant: refresh token "good"
// redeems for "access-<scope>", anything else is refused like Entra does.
func fakeIssuer(t *testing.T, calls *atomic.Int32) *httptest.Server {
	t.Helper()
	var srv *httptest.Server
	srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/.well-known/openid-configuration":
			_ = json.NewEncoder(w).Encode(map[string]string{
				"issuer": srv.URL, "authorization_endpoint": srv.URL + "/authorize",
				"token_endpoint": srv.URL + "/token", "jwks_uri": srv.URL + "/keys",
			})
		case "/token":
			calls.Add(1)
			if err := r.ParseForm(); err != nil {
				t.Error(err)
			}
			if r.Form.Get("client_id") != "donkey" || r.Form.Get("client_secret") != "secret" {
				t.Errorf("unexpected client: %v", r.Form)
			}
			if r.Form.Get("grant_type") != "refresh_token" {
				t.Errorf("unexpected grant: %v", r.Form)
			}
			if r.Form.Get("refresh_token") != "good" {
				w.WriteHeader(http.StatusBadRequest)
				_, _ = w.Write([]byte(`{"error":"invalid_grant","error_description":"AADSTS70008: The refresh token has expired.\r\nTrace ID: 1"}`))
				return
			}
			// Entra's v1 endpoint quotes expires_in; both forms must work.
			_, _ = w.Write([]byte(`{"access_token":"access-` + r.Form.Get("scope") + `","expires_in":"3600","token_type":"Bearer"}`))
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(srv.Close)
	return srv
}

func TestUpstreamRedeemsOncePerRefreshToken(t *testing.T) {
	var calls atomic.Int32
	srv := fakeIssuer(t, &calls)
	u := &Upstream{Issuer: srv.URL, ClientID: "donkey", ClientSecret: "secret", Scope: "api://donkey/default"}

	var wg sync.WaitGroup
	for range 20 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			tok, err := u.AccessToken(context.Background(), "good")
			if err != nil || tok != "access-api://donkey/default" {
				t.Errorf("AccessToken = %q, %v", tok, err)
			}
		}()
	}
	wg.Wait()
	if _, err := u.AccessToken(context.Background(), "good"); err != nil {
		t.Fatal(err)
	}
	if n := calls.Load(); n != 1 {
		t.Fatalf("a burst and a repeat should cost one token call, got %d", n)
	}
}

func TestUpstreamRefusalIsCachedAndSaysSignIn(t *testing.T) {
	var calls atomic.Int32
	srv := fakeIssuer(t, &calls)
	u := &Upstream{Issuer: srv.URL, ClientID: "donkey", ClientSecret: "secret", Scope: "api://donkey/default"}
	for range 3 {
		_, err := u.AccessToken(context.Background(), "revoked")
		if !errors.Is(err, ErrUpstreamToken) {
			t.Fatalf("a refused refresh token must read as ErrUpstreamToken, got %v", err)
		}
		if !strings.Contains(err.Error(), "invalid_grant: AADSTS70008") || strings.Contains(err.Error(), "Trace ID") {
			t.Errorf("the error should keep the IdP's reason, not its trace: %v", err)
		}
	}
	if n := calls.Load(); n != 1 {
		t.Fatalf("a refusal must not be retried on every request, got %d calls", n)
	}
}

func TestUpstreamRememberAndDisabled(t *testing.T) {
	u := &Upstream{Issuer: "https://id.invalid", ClientID: "donkey", ClientSecret: "secret", Scope: "api://donkey/default"}
	u.Remember("rt", "from-login", time.Now().Add(time.Hour))
	if tok, err := u.AccessToken(context.Background(), "rt"); err != nil || tok != "from-login" {
		t.Fatalf("the login's own access token should be served from the cache, got %q, %v", tok, err)
	}
	for _, off := range []*Upstream{nil, {}, {Issuer: "https://id.invalid", ClientID: "donkey", ClientSecret: "secret"}} {
		if off.Enabled() || off.LoginScopes() != nil {
			t.Errorf("%+v should be off", off)
		}
		if _, err := off.AccessToken(context.Background(), "rt"); !errors.Is(err, ErrUpstreamToken) {
			t.Errorf("an unconfigured upstream must refuse, got %v", err)
		}
	}
	if _, err := u.AccessToken(context.Background(), ""); !errors.Is(err, ErrUpstreamToken) {
		t.Error("a sign-in without a refresh token has nothing to redeem")
	}
}

func TestCacheUntilKeepsShortTokens(t *testing.T) {
	if until := time.Until(cacheUntil(time.Now().Add(time.Minute))); until < 20*time.Second {
		t.Errorf("a one-minute token should be cached for half its life, got %s", until)
	}
	if until := time.Until(cacheUntil(time.Now().Add(time.Hour))); until > time.Hour-upstreamSkew {
		t.Errorf("an hour token should retire %s early, got %s", upstreamSkew, until)
	}
}
