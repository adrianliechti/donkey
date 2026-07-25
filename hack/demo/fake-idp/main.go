// DEMO ONLY — a deliberately trivial OIDC identity provider.
//
// It implements just enough of OpenID Connect for donkey's login gateway
// demo: discovery, an /authorize endpoint that instantly "logs in" a demo
// user (no password, any client, any redirect URI), a token endpoint and a
// JWKS. Do not let anything like this near production.
package main

import (
	"crypto"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"log"
	"math/big"
	"net/http"
	"net/url"
	"os"
	"sync"
	"time"
)

var (
	issuer = envOr("ISSUER", "http://id.127.0.0.1.nip.io")
	listen = envOr("LISTEN", ":8080")

	key *rsa.PrivateKey

	mu    sync.Mutex
	codes = map[string]string{} // code -> nonce
)

func envOr(k, def string) string {
	if v := os.Getenv(k); v != "" {
		return v
	}
	return def
}

func main() {
	var err error
	if key, err = rsa.GenerateKey(rand.Reader, 2048); err != nil {
		log.Fatal(err)
	}

	http.HandleFunc("GET /.well-known/openid-configuration", discovery)
	http.HandleFunc("GET /authorize", loginPage)
	http.HandleFunc("GET /approve", approve)
	http.HandleFunc("POST /token", token)
	http.HandleFunc("GET /jwks", jwks)
	http.HandleFunc("GET /", func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprintln(w, "<h1>donkey demo identity provider</h1><p>Everyone is instantly logged in as demo@example.com. Not for production.</p>")
	})

	log.Printf("fake-idp for %s listening on %s", issuer, listen)
	log.Fatal(http.ListenAndServe(listen, nil))
}

func discovery(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, map[string]any{
		"issuer":                                issuer,
		"authorization_endpoint":                issuer + "/authorize",
		"token_endpoint":                        issuer + "/token",
		"jwks_uri":                              issuer + "/jwks",
		"response_types_supported":              []string{"code"},
		"subject_types_supported":               []string{"public"},
		"id_token_signing_alg_values_supported": []string{"RS256"},
		"scopes_supported":                      []string{"openid", "email", "profile"},
		"grant_types_supported":                 []string{"authorization_code"},
		"token_endpoint_auth_methods_supported": []string{"client_secret_basic", "client_secret_post"},
	})
}

// loginPage shows a one-button login so the demo feels like a real IdP.
// The button links to /approve with the identical query string.
func loginPage(w http.ResponseWriter, r *http.Request) {
	if r.URL.Query().Get("redirect_uri") == "" {
		http.Error(w, "redirect_uri missing", http.StatusBadRequest)
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	fmt.Fprintf(w, `<!doctype html>
<meta name="viewport" content="width=device-width, initial-scale=1">
<title>Sign in — donkey demo</title>
<style>
  body{font-family:-apple-system,system-ui,sans-serif;display:flex;min-height:100vh;
       align-items:center;justify-content:center;margin:0;background:#f4f4f5;color:#18181b}
  .card{background:#fff;border-radius:14px;padding:2.5rem 3rem;box-shadow:0 8px 30px rgba(0,0,0,.08);
        text-align:center;max-width:22rem}
  h1{font-size:1.2rem;margin:0 0 .3rem}
  p{color:#71717a;font-size:.9rem;margin:.2rem 0 1.5rem}
  .user{background:#f4f4f5;border-radius:8px;padding:.6rem;margin-bottom:1.4rem;font-size:.95rem}
  a.btn{display:block;background:#18181b;color:#fff;text-decoration:none;border-radius:8px;
        padding:.8rem;font-weight:600}
  a.btn:hover{background:#3f3f46}
  .note{margin-top:1.2rem;font-size:.75rem;color:#a1a1aa}
</style>
<div class="card">
  <h1>🫏 donkey demo sign-in</h1>
  <p>An app wants you to log in</p>
  <div class="user">demo@example.com</div>
  <a class="btn" href="/approve?%s">Sign in</a>
  <div class="note">Demo identity provider — accepts everyone. Not for production.</div>
</div>`, r.URL.RawQuery)
}

// approve issues the code and sends the user back — any client, any
// redirect URI.
func approve(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	redirect := q.Get("redirect_uri")
	if redirect == "" {
		http.Error(w, "redirect_uri missing", http.StatusBadRequest)
		return
	}
	code := randomHex(16)
	mu.Lock()
	codes[code] = q.Get("nonce")
	mu.Unlock()

	u, err := url.Parse(redirect)
	if err != nil {
		http.Error(w, "bad redirect_uri", http.StatusBadRequest)
		return
	}
	out := u.Query()
	out.Set("code", code)
	out.Set("state", q.Get("state"))
	u.RawQuery = out.Encode()
	http.Redirect(w, r, u.String(), http.StatusFound)
}

func token(w http.ResponseWriter, r *http.Request) {
	_ = r.ParseForm()
	code := r.PostFormValue("code")
	mu.Lock()
	nonce, ok := codes[code]
	delete(codes, code)
	mu.Unlock()
	if !ok {
		writeJSONStatus(w, http.StatusBadRequest, map[string]any{"error": "invalid_grant"})
		return
	}

	clientID, _, _ := r.BasicAuth()
	if clientID == "" {
		clientID = r.PostFormValue("client_id")
	}
	now := time.Now()
	claims := map[string]any{
		"iss":            issuer,
		"aud":            clientID,
		"sub":            "demo-user",
		"email":          "demo@example.com",
		"name":           "Demo User",
		"iat":            now.Unix(),
		"exp":            now.Add(12 * time.Hour).Unix(),
		"email_verified": true,
	}
	if nonce != "" {
		claims["nonce"] = nonce
	}
	idToken, err := signJWT(claims)
	if err != nil {
		writeJSONStatus(w, http.StatusInternalServerError, map[string]any{"error": "server_error"})
		return
	}
	writeJSON(w, map[string]any{
		"access_token": randomHex(24),
		"token_type":   "Bearer",
		"expires_in":   43200,
		"id_token":     idToken,
	})
}

func jwks(w http.ResponseWriter, r *http.Request) {
	pub := key.Public().(*rsa.PublicKey)
	writeJSON(w, map[string]any{
		"keys": []map[string]any{{
			"kty": "RSA",
			"alg": "RS256",
			"use": "sig",
			"kid": "demo",
			"n":   base64.RawURLEncoding.EncodeToString(pub.N.Bytes()),
			"e":   base64.RawURLEncoding.EncodeToString(big.NewInt(int64(pub.E)).Bytes()),
		}},
	})
}

func signJWT(claims map[string]any) (string, error) {
	header := map[string]any{"alg": "RS256", "typ": "JWT", "kid": "demo"}
	hb, _ := json.Marshal(header)
	cb, _ := json.Marshal(claims)
	signingInput := base64.RawURLEncoding.EncodeToString(hb) + "." + base64.RawURLEncoding.EncodeToString(cb)
	digest := sha256.Sum256([]byte(signingInput))
	sig, err := rsa.SignPKCS1v15(rand.Reader, key, crypto.SHA256, digest[:])
	if err != nil {
		return "", err
	}
	return signingInput + "." + base64.RawURLEncoding.EncodeToString(sig), nil
}

func randomHex(n int) string {
	b := make([]byte, n)
	_, _ = rand.Read(b)
	return hex.EncodeToString(b)
}

func writeJSON(w http.ResponseWriter, v any) { writeJSONStatus(w, http.StatusOK, v) }

func writeJSONStatus(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}
