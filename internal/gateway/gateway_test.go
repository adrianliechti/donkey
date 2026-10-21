package gateway

import (
	"bytes"
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/json"
	"encoding/pem"
	"fmt"
	"math/big"
	"net/http"
	"net/http/httptest"
	"net/url"
	"slices"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/coreos/go-oidc/v3/oidc"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes/fake"

	"github.com/adrianliechti/donkey/internal/config"
	"github.com/adrianliechti/donkey/internal/identity"
	k8s "github.com/adrianliechti/donkey/pkg/runtime/kubernetes"
	"github.com/adrianliechti/donkey/pkg/runtime/kubernetes/manifests"
)

var testKey = bytes.Repeat([]byte{7}, 32)

func testServer() *Server {
	return &Server{cfg: &Config{SessionKey: testKey}}
}

func TestTLSModeDefaultsToACME(t *testing.T) {
	t.Setenv("DONKEY_TLS_MODE", "")
	t.Setenv("DONKEY_SESSION_KEY", fmt.Sprintf("%x", testKey))
	t.Setenv("DONKEY_TELEMETRY_OTLP_ENDPOINT", "")
	cfg, err := FromEnv()
	if err != nil {
		t.Fatal(err)
	}
	if cfg.TLSMode != "acme" {
		t.Fatalf("default TLS mode = %q, want acme", cfg.TLSMode)
	}
}

// Both cookies are sealed with the same key, so the only thing keeping one
// from being replayed as the other is the purpose bound into each. The
// login-state cookie is handed to every anonymous visitor before they sign
// in, so a state value accepted as a session is an unauthenticated way into
// every protected app that has no access list.
func TestSealedValuesDoNotCrossPurposes(t *testing.T) {
	state := loginState{State: "s", Nonce: "n", Host: "app.example.com", Return: "/",
		Expires: time.Now().Add(loginTTL).Unix()}
	sealedState, err := identity.Seal(testKey, purposeLogin, state)
	if err != nil {
		t.Fatal(err)
	}
	var asSession session
	if err := identity.Unseal(testKey, purposeSession, sealedState, &asSession); err == nil {
		t.Fatalf("a login-state cookie unsealed as a session: %+v", asSession)
	}

	sess := session{Subject: "u1", Expires: time.Now().Add(sessionTTL).Unix()}
	sealedSession, err := identity.Seal(testKey, purposeSession, sess)
	if err != nil {
		t.Fatal(err)
	}
	var asState loginState
	if err := identity.Unseal(testKey, purposeLogin, sealedSession, &asState); err == nil {
		t.Fatalf("a session cookie unsealed as a login state: %+v", asState)
	}
}

// The end-to-end shape of the bypass: what startLogin hands an anonymous
// visitor, presented back as the session cookie.
func TestStateCookieIsNotAcceptedAsSession(t *testing.T) {
	s := testServer()
	state := loginState{State: randomHex(16), Nonce: randomHex(16),
		Host: "app.example.com", Return: "/", Expires: time.Now().Add(loginTTL).Unix()}
	sealed, err := identity.Seal(testKey, purposeLogin, state)
	if err != nil {
		t.Fatal(err)
	}
	r, _ := http.NewRequest("GET", "https://app.example.com/", nil)
	r.AddCookie(&http.Cookie{Name: sessionCookie, Value: sealed})
	if sess := s.sessionFrom(r); sess != nil {
		t.Fatalf("anonymous login-state cookie accepted as a session: %+v", *sess)
	}
}

// A session carries the ID token's subject; a blank one was never vouched
// for by the IdP, whatever else it decodes into.
func TestSessionWithoutSubjectIsRejected(t *testing.T) {
	s := testServer()
	sealed, err := identity.Seal(testKey, purposeSession, session{Expires: time.Now().Add(sessionTTL).Unix()})
	if err != nil {
		t.Fatal(err)
	}
	r, _ := http.NewRequest("GET", "https://app.example.com/", nil)
	r.AddCookie(&http.Cookie{Name: sessionCookie, Value: sealed})
	if sess := s.sessionFrom(r); sess != nil {
		t.Fatalf("subject-less session accepted: %+v", *sess)
	}
}

// A CLI token is a first-class way in: identityFrom accepts it as Bearer —
// and a session cookie sealed for the browser must not pass as one.
func TestIdentityFromBearer(t *testing.T) {
	s := testServer()
	tok, err := identity.SealToken(testKey, identity.Identity{
		Subject: "u1", Email: "ada@example.com", Expires: time.Now().Add(time.Hour).Unix(),
	})
	if err != nil {
		t.Fatal(err)
	}
	r, _ := http.NewRequest("GET", "https://app.example.com/", nil)
	r.Header.Set("Authorization", "Bearer "+tok)
	sess, fromToken := s.identityFrom(r)
	if sess == nil || !fromToken || sess.Email != "ada@example.com" {
		t.Fatalf("a valid CLI token should authenticate: %+v", sess)
	}

	cookie, err := identity.Seal(testKey, purposeSession, session{Subject: "u1", Expires: time.Now().Add(time.Hour).Unix()})
	if err != nil {
		t.Fatal(err)
	}
	r, _ = http.NewRequest("GET", "https://app.example.com/", nil)
	r.Header.Set("Authorization", "Bearer "+cookie)
	if sess, _ := s.identityFrom(r); sess != nil {
		t.Fatalf("a session cookie must not pass as a CLI token: %+v", sess)
	}
}

func TestAllowedAccess(t *testing.T) {
	sess := &session{Email: "Ada@Example.com", Groups: []string{"staff"}, Roles: []string{"developer"}}
	cases := []struct {
		name   string
		access *config.AccessConfig
		want   bool
	}{
		{"no rules lets any signed-in user pass", nil, true},
		{"empty rules too", &config.AccessConfig{}, true},
		{"email matches case-insensitively", &config.AccessConfig{Users: []string{"ada@example.com"}}, true},
		{"group match", &config.AccessConfig{Groups: []string{"staff"}}, true},
		{"role match", &config.AccessConfig{Roles: []string{"developer"}}, true},
		{"any list may match", &config.AccessConfig{Users: []string{"bob@example.com"}, Roles: []string{"developer"}}, true},
		{"no match denies", &config.AccessConfig{Users: []string{"bob@example.com"}, Groups: []string{"board"}, Roles: []string{"admin"}}, false},
		{"groups compare exactly", &config.AccessConfig{Groups: []string{"Staff"}}, false},
	}
	for _, c := range cases {
		if got := c.access.Allows(sess.Email, sess.Groups, sess.Roles); got != c.want {
			t.Errorf("%s: got %v", c.name, got)
		}
	}
}

// The most specific path wins — that is what lets /api be public while /
// needs a login (or the other way round).
func TestTableLookupPrefersLongestPrefix(t *testing.T) {
	api, _ := url.Parse("http://api.donkey-acme.svc.cluster.local:80")
	web, _ := url.Parse("http://web.donkey-acme.svc.cluster.local:80")
	s := &Server{routes: map[string][]route{
		"shop.example.com": {
			{Host: "shop.example.com", Path: "/api", Backend: api},
			{Host: "shop.example.com", Path: "/", Backend: web},
		},
	}}
	if rt, ok := s.tableLookup("shop.example.com", "/api/orders"); !ok || rt.Backend != api {
		t.Error("/api/orders should reach the api route")
	}
	if rt, ok := s.tableLookup("shop.example.com", "/apiary"); !ok || rt.Backend != web {
		t.Error("/apiary is not under /api and should reach the web route")
	}
	if rt, ok := s.tableLookup("shop.example.com", "/"); !ok || rt.Backend != web {
		t.Error("/ should reach the web route")
	}
	if _, ok := s.tableLookup("other.example.com", "/"); ok {
		t.Error("unknown hosts must miss")
	}
}

// One callback URL on the domain serves every hostname under it; custom
// domains keep their own. This is what lets a single registered redirect URI
// (and a single sign-in) cover the whole installation.
func TestSSOHost(t *testing.T) {
	cfg := &Config{Domain: "dihei.app"}
	for host, want := range map[string]bool{
		"dihei.app":             true,
		"web.acme.dihei.app":    true,
		"shop.example.com":      false,
		"dihei.app.example.com": false,
		"evildihei.app":         false,
	} {
		if got := cfg.ssoHost(host); got != want {
			t.Errorf("ssoHost(%q) = %v, want %v", host, got, want)
		}
	}
	if (&Config{}).ssoHost("web.acme.dihei.app") {
		t.Error("without a domain there is no shared login")
	}
}

// The callback may finish on a different hostname than the visitor started
// on — the return target must stay inside the installation.
func TestLoginReturnURL(t *testing.T) {
	cfg := &Config{Domain: "dihei.app"}
	cases := []struct {
		name  string
		state loginState
		want  string
	}{
		{"same host keeps a plain path", loginState{Host: "dihei.app", Return: "/x?y=1"}, "/x?y=1"},
		{"cross-host returns to the app", loginState{Host: "web.acme.dihei.app", Return: "/orders"}, "https://web.acme.dihei.app/orders"},
		{"a foreign host is not followed", loginState{Host: "evil.example.com", Return: "/x"}, "/x"},
		{"protocol-relative returns are cut", loginState{Host: "web.acme.dihei.app", Return: "//evil.example.com"}, "https://web.acme.dihei.app/"},
		{"legacy state without a host", loginState{Return: "/x"}, "/x"},
	}
	for _, c := range cases {
		if got := loginReturnURL(cfg, "dihei.app", c.state); got != c.want {
			t.Errorf("%s: got %q, want %q", c.name, got, c.want)
		}
	}
}

// The session cookie's Domain attribute is what turns a login into SSO — and
// what must never leak onto a custom domain outside the installation.
func TestCookieDomain(t *testing.T) {
	s := &Server{cfg: &Config{Domain: "dihei.app"}}
	if got := s.cookieDomain("web.acme.dihei.app"); got != "dihei.app" {
		t.Errorf("app hostnames share the domain cookie, got %q", got)
	}
	if got := s.cookieDomain("shop.example.com"); got != "" {
		t.Errorf("custom domains keep host-scoped cookies, got %q", got)
	}
}

func TestCanonicalHTTPMethodIncludesQuery(t *testing.T) {
	if got := canonicalHTTPMethod("QUERY"); got != "QUERY" {
		t.Fatalf("QUERY method=%q", got)
	}
	if got := canonicalHTTPMethod("BREW"); got != "_OTHER" {
		t.Fatalf("unknown method=%q", got)
	}
}

func TestRouteFromConfigMap(t *testing.T) {
	cm := corev1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{
			Labels: map[string]string{
				manifests.LabelSpace: "acme",
				manifests.LabelApp:   "shop",
			},
			Annotations: map[string]string{
				"donkey.local/backend":   "http://shop.donkey-acme.svc.cluster.local:80",
				"donkey.local/host":      "shop.example.com",
				"donkey.local/path":      "/api",
				"donkey.local/login":     "required",
				manifests.AnnotationMode: config.AppModeAPI,
				manifests.AnnotationCORS: config.CORSAllowAll,
				"donkey.local/access":    `{"users":["ada@example.com"],"roles":["developer"]}`,
			},
		},
	}
	rt, ok := routeFromConfigMap(cm)
	if !ok {
		t.Fatal("route with backend annotation should be picked up")
	}
	if rt.Host != "shop.example.com" || rt.Path != "/api" || !rt.Login || rt.Mode != config.AppModeAPI || rt.CORS != config.CORSAllowAll {
		t.Errorf("unexpected route: %+v", rt)
	}
	if rt.Space != "acme" || rt.App != "shop" {
		t.Errorf("route telemetry scope = %q/%q", rt.Space, rt.App)
	}
	if rt.Access == nil || len(rt.Access.Users) != 1 || len(rt.Access.Roles) != 1 {
		t.Errorf("access rules should be parsed: %+v", rt.Access)
	}

	// No backend annotation → not the gateway's route (an unrelated
	// ConfigMap that happens to carry donkey labels).
	plain := corev1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{
			Annotations: map[string]string{"donkey.local/host": "donkey.example.com"},
		},
	}
	if _, ok := routeFromConfigMap(plain); ok {
		t.Error("routes without a backend annotation are not ours to serve")
	}

	// Scope is mandatory: without it the route would work while gateway
	// request and view telemetry disappeared silently.
	unscoped := cm.DeepCopy()
	delete(unscoped.Labels, manifests.LabelApp)
	if _, ok := routeFromConfigMap(*unscoped); ok {
		t.Error("routes without a valid logical telemetry scope must not be served")
	}
	system := unscoped.DeepCopy()
	delete(system.Labels, manifests.LabelSpace)
	// Standard manager metadata is not routing authority; only Donkey identity
	// may distinguish the intentional unscoped system route.
	system.Labels[manifests.LabelManagedBy] = "Helm"
	system.Labels[manifests.LabelRole] = manifests.SystemRole(manifests.ServerName)
	system.Annotations[manifests.AnnotationToken] = manifests.TokenForward
	if route, ok := routeFromConfigMap(*system); !ok || !route.Token || route.Space != "" || route.App != "" {
		t.Errorf("the managed console route must remain system-scoped, got %+v, ok=%v", route, ok)
	}
	spoofedToken := cm.DeepCopy()
	spoofedToken.Annotations[manifests.AnnotationToken] = manifests.TokenForward
	if route, ok := routeFromConfigMap(*spoofedToken); !ok || route.Token {
		t.Errorf("a tenant route must not opt into admin-token forwarding, got %+v, ok=%v", route, ok)
	}

	// A non-http backend must be refused, not proxied.
	bad := cm.DeepCopy()
	bad.Annotations["donkey.local/backend"] = "https://evil.example.com"
	if _, ok := routeFromConfigMap(*bad); ok {
		t.Error("only in-cluster plain-http backends are proxied")
	}
}

func TestManagedCertificatesLoadOnlyOwnedGatewaySecrets(t *testing.T) {
	owned := tlsSecret(t, "shop.example.com", map[string]string{
		manifests.LabelInstallation: "donkey-system",
		manifests.LabelRole:         manifests.RoleEdge,
	})
	owned.Name, owned.Namespace = "shop-tls", "donkey-system"
	foreign := tlsSecret(t, "evil.example.com", map[string]string{
		manifests.LabelInstallation: "another-donkey",
		manifests.LabelRole:         manifests.RoleEdge,
	})
	foreign.Name, foreign.Namespace = "evil-tls", "donkey-system"
	unrelated := tlsSecret(t, "config.example.com", map[string]string{
		manifests.LabelInstallation: "donkey-system",
		manifests.LabelRole:         manifests.RoleConfig,
	})
	unrelated.Name, unrelated.Namespace = "unrelated-tls", "donkey-system"

	s := &Server{
		cfg: &Config{SystemNamespace: "donkey-system"},
		k8s: &k8s.Client{Clientset: fake.NewSimpleClientset(owned, foreign, unrelated)},
	}
	certificates := &managedCertificates{server: s, byHost: map[string]*tls.Certificate{}}
	if err := certificates.reload(context.Background()); err != nil {
		t.Fatal(err)
	}
	if cert, err := certificates.getCertificate(&tls.ClientHelloInfo{ServerName: "SHOP.EXAMPLE.COM"}); err != nil || cert == nil {
		t.Fatalf("owned app certificate was not loaded: cert=%v err=%v", cert != nil, err)
	}
	for _, host := range []string{"evil.example.com", "config.example.com"} {
		if cert := certificates.find(host); cert != nil {
			t.Errorf("certificate for %s crossed the ownership/role boundary", host)
		}
	}
}

func tlsSecret(t *testing.T, host string, labels map[string]string) *corev1.Secret {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	template := &x509.Certificate{
		SerialNumber: big.NewInt(1), Subject: pkix.Name{CommonName: host}, DNSNames: []string{host},
		NotBefore: time.Now().Add(-time.Hour), NotAfter: time.Now().Add(time.Hour),
		KeyUsage: x509.KeyUsageDigitalSignature, ExtKeyUsage: []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
	}
	der, err := x509.CreateCertificate(rand.Reader, template, template, &key.PublicKey, key)
	if err != nil {
		t.Fatal(err)
	}
	keyDER, err := x509.MarshalECPrivateKey(key)
	if err != nil {
		t.Fatal(err)
	}
	return &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Labels: labels},
		Data: map[string][]byte{
			"tls.crt": pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}),
			"tls.key": pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: keyDER}),
		},
	}
}

func TestProtocolVersionUsesOpenTelemetryValues(t *testing.T) {
	for proto, want := range map[string]string{"HTTP/1.1": "1.1", "HTTP/2.0": "2", "HTTP/3.0": "3"} {
		req := &http.Request{Proto: proto}
		if _, err := fmt.Sscanf(proto, "HTTP/%d.%d", &req.ProtoMajor, &req.ProtoMinor); err != nil {
			t.Fatal(err)
		}
		if got := protocolVersion(req); got != want {
			t.Errorf("protocolVersion(%s) = %q, want %q", proto, got, want)
		}
	}
}

// The proxy's identity contract: forged inbound identity is dropped on every
// route, only routes behind a login learn who the visitor is, and the sealed
// credential goes only to the route marked for it — an app handed one could
// replay its visitors against the API.
func TestProxyIdentityAndToken(t *testing.T) {
	var got http.Header
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		got = r.Header.Clone()
	}))
	defer backend.Close()
	target, err := url.Parse(backend.URL)
	if err != nil {
		t.Fatal(err)
	}
	s := testServer()
	sess := &session{Subject: "u1", Email: "u1@example.com", PreferredUsername: "ada", Name: "Ada\r\nLovelace",
		Groups: []string{"dev"}, Expires: time.Now().Add(time.Hour).Unix()}

	// Names donkey sets, in both of oauth2-proxy's families, and names it
	// never sets but an app may trust — however they are spelled.
	forged := []string{"X-Auth-Request-User", "X-Auth-Request-Email", "X-Auth-Request-Groups", "X-Auth-Request-Roles",
		"X-Auth-Request-Access-Token", "X-Auth-Request-Preferred-Username", "X-Auth-Request-Redirect", accessTokenHeader,
		"X-Forwarded-User", "X-Forwarded-Email", "X-Forwarded-Groups", "X-Forwarded-Roles", "X-Forwarded-Preferred-Username",
		"X-Forwarded-Name", "X-Forwarded-Access-Token"}
	send := func(rt route, sess *session) {
		req := httptest.NewRequest(http.MethodGet, "https://app.example.com/x", nil)
		// A hostile client types identity — all of it must vanish.
		for _, h := range forged {
			req.Header.Set(h, "forged")
		}
		req.Header["x-auth-request-preferred-username"] = []string{"forged"}
		s.proxy(rt, sess, false, "").ServeHTTP(httptest.NewRecorder(), req)
	}

	send(route{Backend: target}, sess)
	for _, name := range forged {
		if got.Get(name) != "" {
			t.Errorf("a public route must learn nothing about a signed-in visitor, got %s: %q", name, got.Get(name))
		}
	}
	if got.Get("X-Forwarded-For") == "" || got.Get("X-Forwarded-Proto") != "https" {
		t.Error("the proxy's own X-Forwarded headers are not identity and must stay")
	}

	send(route{Backend: target, Login: true}, sess)
	for name, want := range map[string]string{
		"User": "u1", "Email": "u1@example.com", "Preferred-Username": "ada", "Name": "AdaLovelace", "Groups": "dev",
		"Roles": "", "Access-Token": "", "Redirect": "",
	} {
		// X-Forwarded-* first, X-Auth-Request-* as the fallback, alike.
		if got.Get("X-Forwarded-"+name) != want || got.Get("X-Auth-Request-"+name) != want {
			t.Errorf("%s: got %q and %q, want %q", name, got.Get("X-Forwarded-"+name), got.Get("X-Auth-Request-"+name), want)
		}
	}

	// Entra's sub is pairwise per app: X-Forwarded-User is its object ID,
	// while the fallback keeps the sub it always carried.
	entra := *sess
	entra.ObjectID = "6e1c0a4f-0000-4000-8000-0000000000aa"
	send(route{Backend: target, Login: true}, &entra)
	if got.Get("X-Forwarded-User") != entra.ObjectID || got.Get("X-Auth-Request-User") != "u1" {
		t.Errorf("Entra user: got %q and %q", got.Get("X-Forwarded-User"), got.Get("X-Auth-Request-User"))
	}
	if got.Get("X-Access-Token") != "" {
		t.Error("an app must never receive a credential — forged or minted")
	}

	send(route{Backend: target, Login: true, Token: true}, sess)
	tok := got.Get("X-Access-Token")
	if tok == "" || tok == "forged" {
		t.Fatalf("the API route should carry a minted token, got %q", tok)
	}
	id, err := identity.VerifyToken(testKey, tok)
	if err != nil {
		t.Fatalf("the minted token must verify under the session key: %v", err)
	}
	if id.Subject != "u1" || id.Email != "u1@example.com" {
		t.Errorf("the token must carry the session's identity, got %+v", id)
	}
	if remaining := time.Until(time.Unix(id.Expires, 0)); remaining > forwardTokenTTL+time.Minute {
		t.Errorf("a forwarded token must be short-lived, got %s", remaining)
	}

	send(route{Backend: target, Login: true, Token: true}, nil)
	if got.Get("X-Access-Token") != "" || got.Get("X-Auth-Request-User") != "" {
		t.Error("no session, no identity — nothing may reach the backend")
	}
}

// With auth.scope configured every login-protected app gets the visitor's IdP
// access token — never the refresh token behind it, never a forged one, never
// on a public route the session cookie merely reaches — and the API
// route's sealed token carries the access token, never the refresh token: the
// server hands it on and never mints one itself.
func TestProxyForwardsIdentityProviderToken(t *testing.T) {
	var got http.Header
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		got = r.Header.Clone()
	}))
	defer backend.Close()
	target, err := url.Parse(backend.URL)
	if err != nil {
		t.Fatal(err)
	}
	s := testServer()
	s.upstream = &identity.Upstream{Issuer: "https://id.invalid", ClientID: "donkey", ClientSecret: "secret", Scope: "api://donkey/default"}
	s.upstream.Remember(identity.Grant("rt-u1"), "rt-u1", "entra-u1", time.Now().Add(time.Hour), time.Now().Add(time.Hour))
	sess := &session{Subject: "u1", Email: "u1@example.com", RefreshToken: "rt-u1", Grant: identity.Grant("rt-u1"),
		Expires: time.Now().Add(time.Hour).Unix()}

	send := func(rt route, sess *session) {
		req := httptest.NewRequest(http.MethodGet, "https://app.example.com/x", nil)
		req.Header.Set(idpAccessTokenHeader, "forged")
		req.AddCookie(&http.Cookie{Name: sessionCookie, Value: "sealed-0"})
		req.AddCookie(&http.Cookie{Name: sessionCookie + "_1", Value: "sealed-1"})
		req.AddCookie(&http.Cookie{Name: "app", Value: "keep"})
		rec := httptest.NewRecorder()
		s.proxy(rt, sess, false, s.idpToken(rec, req, rt, sess, false, time.Time{})).ServeHTTP(rec, req)
	}

	send(route{Backend: target}, sess)
	if tok := got.Get(idpAccessTokenHeader); tok != "" {
		t.Fatalf("a public route must not receive the visitor's access token, got %q", tok)
	}
	if got.Get("X-Auth-Request-User") != "" {
		t.Errorf("a public route must not learn who the visitor is")
	}

	send(route{Backend: target, Login: true}, sess)
	if tok := got.Get(idpAccessTokenHeader); tok != "entra-u1" || got.Get("X-Auth-Request-Access-Token") != tok {
		t.Fatalf("a login-protected app should receive the visitor's access token, got %q", tok)
	}
	if h := fmt.Sprint(got); strings.Contains(h, "rt-u1") || strings.Contains(h, "sealed-") {
		t.Errorf("the refresh token or a session chunk reached the app: %s", h)
	}
	if !strings.Contains(got.Get("Cookie"), "app=keep") {
		t.Errorf("the app's own cookies must survive, got %q", got.Get("Cookie"))
	}

	send(route{Backend: target, Login: true, Token: true}, sess)
	id, err := identity.VerifyToken(testKey, got.Get("X-Access-Token"))
	if err != nil || id.AccessToken != "entra-u1" || id.RefreshToken != "" {
		t.Fatalf("the API's sealed token should carry the access token alone, got %+v, %v", id, err)
	}

	for _, sess := range []*session{nil, {Subject: "u2", Expires: time.Now().Add(time.Hour).Unix()}} {
		send(route{Backend: target, Login: true}, sess)
		if tok := got.Get(idpAccessTokenHeader); tok != "" {
			t.Errorf("no refresh token, no access token — got %q", tok)
		}
	}

	s.upstream = &identity.Upstream{}
	send(route{Backend: target, Login: true}, sess)
	if tok := got.Get(idpAccessTokenHeader); tok != "" {
		t.Errorf("without auth.scope nothing is forwarded, got %q", tok)
	}
}

// A session with a refresh token outgrows one cookie; it is split, read back
// whole, and a shorter session clears the chunks a longer one left.
func TestSessionCookieChunks(t *testing.T) {
	s := testServer()
	jar := func(rec *httptest.ResponseRecorder, prev []*http.Cookie) []*http.Cookie {
		kept := map[string]*http.Cookie{}
		for _, c := range prev {
			kept[c.Name] = c
		}
		for _, c := range rec.Result().Cookies() {
			if c.MaxAge < 0 {
				delete(kept, c.Name)
			} else {
				kept[c.Name] = c
			}
		}
		var out []*http.Cookie
		for _, name := range sessionCookieNames() {
			if c, ok := kept[name]; ok {
				out = append(out, c)
			}
		}
		return out
	}
	request := func(cookies []*http.Cookie) *http.Request {
		r := httptest.NewRequest(http.MethodGet, "https://app.example.com/", nil)
		for _, c := range cookies {
			r.AddCookie(&http.Cookie{Name: c.Name, Value: c.Value})
		}
		return r
	}

	big := &session{Subject: "u1", RefreshToken: strings.Repeat("r", 9000), Expires: time.Now().Add(time.Hour).Unix()}
	rec := httptest.NewRecorder()
	if err := s.setSession(rec, request(nil), big, "example.com"); err != nil {
		t.Fatal(err)
	}
	cookies := jar(rec, nil)
	if len(cookies) < 3 {
		t.Fatalf("a 9 KB refresh token should span several cookies, got %d", len(cookies))
	}
	for _, c := range cookies {
		if len(c.Value) > sessionChunkSize || c.Domain != "example.com" || !c.HttpOnly || !c.Secure {
			t.Errorf("chunk %s: %d bytes, domain %q", c.Name, len(c.Value), c.Domain)
		}
	}
	if sess := s.sessionFrom(request(cookies)); sess == nil || sess.RefreshToken != big.RefreshToken {
		t.Fatal("a chunked session should read back whole")
	}

	small := &session{Subject: "u1", Expires: time.Now().Add(time.Hour).Unix()}
	rec = httptest.NewRecorder()
	if err := s.setSession(rec, request(cookies), small, "example.com"); err != nil {
		t.Fatal(err)
	}
	cookies = jar(rec, cookies)
	if len(cookies) != 1 || cookies[0].Name != sessionCookie {
		t.Fatalf("a small session must clear the stale chunks, left %d cookies", len(cookies))
	}
	if sess := s.sessionFrom(request(cookies)); sess == nil || sess.RefreshToken != "" {
		t.Fatal("the small session should read back on its own")
	}

	huge := &session{Subject: "u1", RefreshToken: strings.Repeat("r", sessionChunkSize*sessionChunks)}
	if err := s.setSession(httptest.NewRecorder(), request(nil), huge, ""); err == nil {
		t.Error("a session beyond every chunk must fail, not truncate")
	}
}

// Shutting telemetry down must never panic a request that finishes at the
// same moment: the view queue stays open forever; only the exporter stops
// reading it. Before this held, a drain during shutdown was a send on a
// closed channel — a crash of the whole edge.
func TestTelemetryRecordAfterShutdownDoesNotPanic(t *testing.T) {
	tel, err := newEdgeTelemetry(&Config{
		TelemetryEndpoint: "http://127.0.0.1:1",
		TelemetryKey:      testKey,
		SystemNamespace:   "donkey-system",
	})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	_ = tel.Shutdown(ctx) // canceled context: closes the queue path, skips network flushes
	r := httptest.NewRequest(http.MethodGet, "https://shop.acme.example.com/", nil)
	r.Header.Set("Accept", "text/html")
	// Past the queue size, so both the enqueue and the backpressure paths run.
	for range viewQueueSize + 8 {
		tel.Record(r, route{Space: "acme", App: "shop", Path: "/"}, nil, http.StatusOK, time.Millisecond, "text/html; charset=utf-8")
	}
}

// The browser keeps the newest refresh token: a rotation re-seals the session
// cookie, a request still carrying the old one is served from the cache and
// healed, and a token-authenticated request (no cookie) is left alone.
func TestSessionTakesRotatedRefreshToken(t *testing.T) {
	var calls atomic.Int32
	var issuer *httptest.Server
	issuer = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if r.URL.Path != "/token" {
			_ = json.NewEncoder(w).Encode(map[string]any{"issuer": issuer.URL, "token_endpoint": issuer.URL + "/token"})
			return
		}
		calls.Add(1)
		_ = r.ParseForm()
		if r.Form.Get("refresh_token") != "rt-0" {
			w.WriteHeader(http.StatusBadRequest)
			fmt.Fprint(w, `{"error":"invalid_grant"}`)
			return
		}
		fmt.Fprint(w, `{"access_token":"access-1","refresh_token":"rt-1","expires_in":3600}`)
	}))
	defer issuer.Close()
	s := testServer()
	s.upstream = &identity.Upstream{Issuer: issuer.URL, ClientID: "donkey", ClientSecret: "secret", Scope: "api"}
	rt := route{Login: true}
	sess := &session{Subject: "u1", RefreshToken: "rt-0", Grant: identity.Grant("rt-0"), Expires: time.Now().Add(time.Hour).Unix()}

	sessionAfter := func(rec *httptest.ResponseRecorder) *session {
		req := httptest.NewRequest(http.MethodGet, "https://app.example.com/", nil)
		for _, c := range rec.Result().Cookies() {
			req.AddCookie(c)
		}
		return s.sessionFrom(req)
	}
	for range 2 {
		rec := httptest.NewRecorder()
		req := httptest.NewRequest(http.MethodGet, "https://app.example.com/", nil)
		if tok := s.idpToken(rec, req, rt, sess, false, time.Time{}); tok != "access-1" {
			t.Fatalf("token = %q", tok)
		}
		got := sessionAfter(rec)
		if got == nil || got.RefreshToken != "rt-1" || got.Grant != sess.Grant || got.Subject != "u1" {
			t.Fatalf("the cookie should carry the rotated refresh token under the same grant, got %+v", got)
		}
	}
	if calls.Load() != 1 {
		t.Fatalf("the stale cookie should be served from the cache, got %d refreshes", calls.Load())
	}
	rec := httptest.NewRecorder()
	if tok := s.idpToken(rec, httptest.NewRequest(http.MethodGet, "https://app.example.com/", nil), rt, sess, true, time.Time{}); tok != "access-1" {
		t.Fatalf("a CLI token's request should get the token too, got %q", tok)
	}
	if len(rec.Result().Cookies()) != 0 {
		t.Error("a token-authenticated request must not be handed a session cookie")
	}
}

// Installations without auth.scope sign in exactly as before — no
// offline_access a strict IdP could refuse; with it, the refresh token and
// the scope are asked for on top.
func TestLoginScopesWithoutAuthScopeAreUnchanged(t *testing.T) {
	provider := (&oidc.ProviderConfig{IssuerURL: "https://id.example.com", AuthURL: "https://id.example.com/authorize", TokenURL: "https://id.example.com/token"}).NewProvider(context.Background())
	a := &authenticator{cfg: &Config{}, provider: provider, groupsScope: true}
	for _, tc := range []struct {
		upstream *identity.Upstream
		want     []string
	}{
		{nil, []string{"openid", "profile", "email", "groups"}},
		{&identity.Upstream{Issuer: "https://id.example.com", ClientID: "donkey", ClientSecret: "secret"}, []string{"openid", "profile", "email", "groups"}},
		{&identity.Upstream{Issuer: "https://id.example.com", ClientID: "donkey", ClientSecret: "secret", Scope: "api://donkey/default"},
			[]string{"openid", "profile", "email", "groups", "offline_access", "api://donkey/default"}},
	} {
		if got := a.oauthConfig("app.example.com", tc.upstream).Scopes; !slices.Equal(got, tc.want) {
			t.Errorf("scopes = %v, want %v", got, tc.want)
		}
	}
}

// Sessions, CLI and OAuth tokens share identity.Identity, embedded where a
// token adds fields: the JSON keys stay exactly what each sealed format had,
// so cookies and tokens issued before still open, and new ones read the same.
func TestSealedFormatsKeepTheirKeys(t *testing.T) {
	flat := map[string]any{"client_id": "c", "sub": "u1", "email": "u1@example.com", "name": "U", "preferred_username": "u",
		"oid": "o", "groups": []string{"g"}, "roles": []string{"r"}, "resource": "https://api.example.com", "exp": 123, "rt": "refresh", "grant": "gr"}
	sealed, err := identity.Seal(testKey, purposeOAuthRefresh, flat)
	if err != nil {
		t.Fatal(err)
	}
	var refresh oauthRefreshToken
	if err := identity.Unseal(testKey, purposeOAuthRefresh, sealed, &refresh); err != nil || refresh.ClientID != "c" || refresh.Subject != "u1" ||
		refresh.ObjectID != "o" || refresh.RefreshToken != "refresh" || refresh.Grant != "gr" || refresh.Resource != "https://api.example.com" || refresh.Expires != 123 {
		t.Fatalf("a flat token no longer opens: %+v %v", refresh, err)
	}
	sealed, err = identity.Seal(testKey, purposeOAuthToken, oauthAccessToken{Identity: identity.Identity{Subject: "u1", Expires: 1, RefreshToken: "rt"}, Resource: "r"})
	if err != nil {
		t.Fatal(err)
	}
	var keys map[string]any
	if err := identity.Unseal(testKey, purposeOAuthToken, sealed, &keys); err != nil {
		t.Fatal(err)
	}
	for _, key := range []string{"sub", "exp", "rt", "resource"} {
		if _, ok := keys[key]; !ok {
			t.Errorf("missing %q in %v", key, keys)
		}
	}
	if _, ok := keys["Identity"]; ok {
		t.Errorf("the embedded identity must flatten: %v", keys)
	}
}
