package identity

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
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
			_ = json.NewEncoder(w).Encode(map[string]any{
				"issuer": srv.URL, "authorization_endpoint": srv.URL + "/authorize",
				"token_endpoint": srv.URL + "/token", "jwks_uri": srv.URL + "/keys",
				"token_endpoint_auth_methods_supported": []string{"client_secret_post"},
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
			_, _ = w.Write([]byte(`{"access_token":"access-api://donkey/default","expires_in":"3600","token_type":"Bearer"}`))
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
			tok, _, err := u.AccessToken(context.Background(), "", "good", time.Time{})
			if err != nil || tok != "access-api://donkey/default" {
				t.Errorf("AccessToken = %q, %v", tok, err)
			}
		}()
	}
	wg.Wait()
	if _, _, err := u.AccessToken(context.Background(), "", "good", time.Time{}); err != nil {
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
		_, _, err := u.AccessToken(context.Background(), "", "revoked", time.Time{})
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
	u.Remember(Grant("rt"), "rt", "from-login", time.Now().Add(time.Hour), time.Now().Add(time.Hour))
	if tok, _, err := u.AccessToken(context.Background(), Grant("rt"), "rt", time.Time{}); err != nil || tok != "from-login" {
		t.Fatalf("the login's own access token should be served from the cache, got %q, %v", tok, err)
	}
	for _, off := range []*Upstream{nil, {}, {Issuer: "https://id.invalid", ClientID: "donkey", ClientSecret: "secret"}} {
		if off.Enabled() || off.LoginScopes() != nil {
			t.Errorf("%+v should be off", off)
		}
		if _, _, err := off.AccessToken(context.Background(), "", "rt", time.Time{}); !errors.Is(err, ErrUpstreamToken) {
			t.Errorf("an unconfigured upstream must refuse, got %v", err)
		}
	}
	if _, _, err := u.AccessToken(context.Background(), "", "", time.Time{}); !errors.Is(err, ErrUpstreamToken) {
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

func TestUpstreamClientAuthentication(t *testing.T) {
	for _, tc := range []struct {
		name      string
		methods   []string
		basic     bool
		wantCalls int32
	}{
		{"basic", []string{"client_secret_basic"}, true, 1},
		{"post", []string{"client_secret_post"}, false, 1},
		{"post client on a server supporting both", []string{"client_secret_basic", "client_secret_post"}, false, 2},
		{"post autodetection", nil, false, 2},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var calls atomic.Int32
			var issuer *httptest.Server
			issuer = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Content-Type", "application/json")
				if r.URL.Path != "/token" {
					_ = json.NewEncoder(w).Encode(map[string]any{
						"issuer": issuer.URL, "token_endpoint": issuer.URL + "/token",
						"token_endpoint_auth_methods_supported": tc.methods,
					})
					return
				}
				calls.Add(1)
				_ = r.ParseForm()
				user, secret, basic := r.BasicAuth()
				valid := basic && tc.basic && user == "donkey" && secret == "secret" && r.Form.Get("client_secret") == ""
				valid = valid || !basic && !tc.basic && r.Form.Get("client_id") == "donkey" && r.Form.Get("client_secret") == "secret"
				if !valid {
					w.WriteHeader(http.StatusUnauthorized)
					fmt.Fprint(w, `{"error":"invalid_client"}`)
					return
				}
				if r.Form.Get("refresh_token") != "original" || r.Form.Get("grant_type") != "refresh_token" {
					t.Errorf("wrong refresh grant: %v", r.Form)
				}
				fmt.Fprint(w, `{"access_token":"access","expires_in":3600}`)
			}))
			defer issuer.Close()
			u := &Upstream{Issuer: issuer.URL, ClientID: "donkey", ClientSecret: "secret", Scope: "api"}
			for range 2 {
				if token, _, err := u.AccessToken(context.Background(), "", "original", time.Time{}); err != nil || token != "access" {
					t.Fatalf("refresh: %q %v", token, err)
				}
				u.mu.Lock()
				token := u.cache[Grant("original")]
				token.expiry = time.Now().Add(-time.Minute)
				u.cache[Grant("original")] = token
				u.mu.Unlock()
			}
			if got := calls.Load(); got != tc.wantCalls+1 {
				t.Fatalf("authentication should be remembered: %d token requests", got)
			}
		})
	}
}

// The IdP accepts each refresh token once. Callers keep presenting the
// login's refresh token (a stale cookie, a CLI token); the cache redeems the
// newest instead, once per expired access token, and hands it back for the
// credential to keep. After a restart only the re-sealed credential's token
// works — the login's is spent.
func TestUpstreamFollowsRotation(t *testing.T) {
	var generation atomic.Int32
	var issuer *httptest.Server
	issuer = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if r.URL.Path != "/token" {
			_ = json.NewEncoder(w).Encode(map[string]any{
				"issuer": issuer.URL, "token_endpoint": issuer.URL + "/token",
				"token_endpoint_auth_methods_supported": []string{"client_secret_post"},
			})
			return
		}
		_ = r.ParseForm()
		old := generation.Load()
		if r.Form.Get("refresh_token") != fmt.Sprintf("refresh-%d", old) || !generation.CompareAndSwap(old, old+1) {
			w.WriteHeader(http.StatusBadRequest)
			fmt.Fprint(w, `{"error":"invalid_grant"}`)
			return
		}
		fmt.Fprintf(w, `{"access_token":"access-%d","refresh_token":"refresh-%d","expires_in":3600}`, old+1, old+1)
	}))
	defer issuer.Close()
	newProcess := func() *Upstream {
		return &Upstream{Issuer: issuer.URL, ClientID: "donkey", ClientSecret: "secret", Scope: "api"}
	}
	u := newProcess()
	grant, until := Grant("refresh-0"), time.Now().Add(time.Hour)
	for round := 1; round <= 3; round++ {
		var wg sync.WaitGroup
		for range 20 {
			wg.Go(func() {
				token, latest, err := u.AccessToken(context.Background(), grant, "refresh-0", until)
				if err != nil || token != fmt.Sprintf("access-%d", round) || latest != fmt.Sprintf("refresh-%d", round) {
					t.Errorf("round %d: %q %q %v", round, token, latest, err)
				}
			})
		}
		wg.Wait()
		if got := generation.Load(); got != int32(round) {
			t.Fatalf("only one refresh per expired token: %d, want %d", got, round)
		}
		u.mu.Lock()
		token := u.cache[grant]
		token.expiry = time.Now().Add(-time.Minute)
		u.cache[grant] = token
		u.mu.Unlock()
	}
	if latest := u.Latest(grant, "refresh-0"); latest != "refresh-3" {
		t.Fatalf("Latest = %q", latest)
	}

	u = newProcess()
	if _, _, err := u.AccessToken(context.Background(), grant, "refresh-0", until); !errors.Is(err, ErrUpstreamToken) {
		t.Fatalf("after a restart the login's spent token must be refused, got %v", err)
	}
	u = newProcess()
	if token, latest, err := u.AccessToken(context.Background(), grant, "refresh-3", until); err != nil || token != "access-4" || latest != "refresh-4" {
		t.Fatalf("after a restart the re-sealed token should redeem: %q %q %v", token, latest, err)
	}
}

// An entry lives as long as some credential can still present its grant —
// a 12-hour browser session's dies with it, a CLI token's lasts longer.
func TestUpstreamForgetsEndedSignIns(t *testing.T) {
	u := &Upstream{Issuer: "https://id.invalid", ClientID: "donkey", ClientSecret: "secret", Scope: "api"}
	past := time.Now().Add(-time.Second)
	u.Remember(Grant("ended"), "ended-rotated", "access", past, past)
	u.Remember(Grant("cli"), "cli-rotated", "access", time.Now().Add(time.Hour), time.Now().Add(-time.Second))
	// A CLI request extends the entry to its own token's lifetime.
	if _, _, err := u.AccessToken(context.Background(), Grant("cli"), "cli", time.Now().Add(30*24*time.Hour)); err != nil {
		t.Fatal(err)
	}
	u.Remember(Grant("other"), "other", "access", time.Now().Add(time.Hour), time.Now().Add(time.Hour))
	if got := u.Latest(Grant("ended"), "ended"); got != "ended" {
		t.Errorf("an ended sign-in should be forgotten, got %q", got)
	}
	u.mu.Lock()
	retain := u.cache[Grant("cli")].retainUntil
	u.mu.Unlock()
	if time.Until(retain) < 29*24*time.Hour {
		t.Errorf("the CLI token's request should extend the entry, got %s", time.Until(retain))
	}
}
