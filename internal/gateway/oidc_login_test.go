package gateway

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/coreos/go-oidc/v3/oidc"
	"github.com/go-jose/go-jose/v4"
)

func TestWebOIDCUpstreamLoginPKCEAndMaxAge(t *testing.T) {
	for _, method := range []string{"GET", "POST"} {
		for _, authTime := range []string{"recent", "old", "missing"} {
			t.Run(method+"/"+authTime, func(t *testing.T) {
				s := webOIDCTestServer(t)
				s.auth = newAuthenticator(s.cfg)
				var keys oidcProviderState
				if err := ensureOIDCKeys(&keys); err != nil {
					t.Fatal(err)
				}
				key := keys.Keys[0].Key
				signer, err := jose.NewSigner(jose.SigningKey{Algorithm: jose.RS256, Key: key}, nil)
				if err != nil {
					t.Fatal(err)
				}
				var nonce, challenge string
				upstream := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					w.Header().Set("Content-Type", "application/json")
					switch r.URL.Path {
					case "/.well-known/openid-configuration":
						_ = json.NewEncoder(w).Encode(map[string]any{"issuer": s.cfg.OIDCIssuer, "authorization_endpoint": s.cfg.OIDCIssuer + "/authorize", "token_endpoint": s.cfg.OIDCIssuer + "/token", "jwks_uri": s.cfg.OIDCIssuer + "/jwks", "id_token_signing_alg_values_supported": []string{"RS256"}})
					case "/jwks":
						_ = json.NewEncoder(w).Encode(jose.JSONWebKeySet{Keys: []jose.JSONWebKey{key.Public()}})
					case "/token":
						if err := r.ParseForm(); err != nil {
							t.Fatal(err)
						}
						if verifier := r.PostForm.Get("code_verifier"); verifier == "" || oidcDigest(verifier) != challenge {
							t.Fatal("upstream code exchange lost PKCE")
						}
						claims := map[string]any{"iss": s.cfg.OIDCIssuer, "aud": s.cfg.OIDCClientID, "sub": "upstream-1", "email": "ada@example.com", "nonce": nonce, "iat": time.Now().Unix(), "exp": time.Now().Add(time.Hour).Unix()}
						if authTime == "recent" {
							claims["auth_time"] = time.Now().Unix()
						}
						if authTime == "old" {
							claims["auth_time"] = time.Now().Add(-time.Hour).Unix()
						}
						payload, _ := json.Marshal(claims)
						signed, err := signer.Sign(payload)
						if err != nil {
							t.Fatal(err)
						}
						raw, err := signed.CompactSerialize()
						if err != nil {
							t.Fatal(err)
						}
						_ = json.NewEncoder(w).Encode(map[string]any{"access_token": "idp-access", "token_type": "Bearer", "expires_in": 3600, "id_token": raw})
					default:
						t.Fatalf("unexpected upstream request: %s", r.URL)
					}
				})
				ctx := oidc.ClientContext(context.Background(), &http.Client{Transport: oidcHandlerTransport{upstream}})
				request := func(method, target string, cookies ...*http.Cookie) *httptest.ResponseRecorder {
					body := ""
					if method == "POST" {
						u, _ := url.Parse(target)
						body, u.RawQuery = u.RawQuery, ""
						target = u.String()
					}
					r := httptest.NewRequest(method, target, strings.NewReader(body)).WithContext(ctx)
					if method == "POST" {
						r.Header.Set("Content-Type", "application/x-www-form-urlencoded")
					}
					for _, cookie := range cookies {
						r.AddCookie(cookie)
					}
					w := httptest.NewRecorder()
					s.handler().ServeHTTP(w, r)
					return w
				}
				q := webAuthorizeQuery("openid email")
				q.Set("prompt", "login")
				q.Set("max_age", "60")
				login := request(method, "https://auth.example/oauth2/authorize?"+q.Encode(), webSession(t, s))
				if login.Code != 302 {
					t.Fatalf("login: %d %s", login.Code, login.Body.String())
				}
				location, _ := url.Parse(login.Header().Get("Location"))
				params := location.Query()
				if params.Get("prompt") != "login" || params.Get("max_age") != "60" || params.Get("code_challenge_method") != "S256" {
					t.Fatalf("upstream parameters: %v", params)
				}
				nonce, challenge = params.Get("nonce"), params.Get("code_challenge")
				callback := request("GET", "https://auth.example/oauth2/callback?"+url.Values{"state": {params.Get("state")}, "code": {"upstream-code"}}.Encode(), login.Result().Cookies()...)
				if authTime != "recent" {
					if callback.Code != 502 || !strings.Contains(callback.Body.String(), "recent sign-in") {
						t.Fatalf("invalid auth_time accepted: %d %s", callback.Code, callback.Body.String())
					}
					return
				}
				if callback.Code != 302 {
					t.Fatalf("callback: %d %s", callback.Code, callback.Body.String())
				}
				ret := callback.Header().Get("Location")
				if strings.Contains(ret, "max_age") || strings.Contains(ret, "prompt=login") {
					t.Fatal("completed login would loop")
				}
				if strings.HasPrefix(ret, "/") {
					ret = "https://auth.example" + ret
				}
				consent := request("GET", ret, callback.Result().Cookies()...)
				if consent.Code != 200 || !strings.Contains(consent.Body.String(), "Sign in to web") {
					t.Fatalf("login did not resume consent: %d %s", consent.Code, consent.Body.String())
				}
			})
		}
	}
}

func TestWebOIDCRejectsOversizedLoginCookie(t *testing.T) {
	s := webOIDCTestServer(t)
	q := webAuthorizeQuery("openid")
	q.Set("state", strings.Repeat("x", 4000))
	rec := mcpRequest(t, s.handler(), "GET", "https://auth.example/oauth2/authorize?"+q.Encode(), "auth.example", "", "")
	if rec.Code != http.StatusBadRequest || !strings.Contains(rec.Body.String(), "too large") || rec.Header().Get("Location") != "" || len(rec.Result().Cookies()) != 0 {
		t.Fatalf("oversized login was not rejected before redirect: %d %s", rec.Code, rec.Body.String())
	}
}
