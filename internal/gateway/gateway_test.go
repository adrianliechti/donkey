package gateway

import (
	"bytes"
	"net/http"
	"net/url"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/adrianliechti/donkey/internal/config"
	"github.com/adrianliechti/donkey/internal/identity"
)

var testKey = bytes.Repeat([]byte{7}, 32)

func testServer() *Server {
	return &Server{cfg: &Config{SessionKey: testKey}}
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

func TestRouteFromConfigMap(t *testing.T) {
	cm := corev1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{
			Annotations: map[string]string{
				"donkey.dev/backend": "http://shop.donkey-acme.svc.cluster.local:80",
				"donkey.dev/host":    "shop.example.com",
				"donkey.dev/path":    "/api",
				"donkey.dev/login":   "required",
				"donkey.dev/access":  `{"users":["ada@example.com"],"roles":["developer"]}`,
			},
		},
	}
	rt, ok := routeFromConfigMap(cm)
	if !ok {
		t.Fatal("route with backend annotation should be picked up")
	}
	if rt.Host != "shop.example.com" || rt.Path != "/api" || !rt.Login {
		t.Errorf("unexpected route: %+v", rt)
	}
	if rt.Access == nil || len(rt.Access.Users) != 1 || len(rt.Access.Roles) != 1 {
		t.Errorf("access rules should be parsed: %+v", rt.Access)
	}

	// No backend annotation → not the gateway's route (an unrelated
	// ConfigMap that happens to carry donkey labels).
	plain := corev1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{
			Annotations: map[string]string{"donkey.dev/host": "donkey.example.com"},
		},
	}
	if _, ok := routeFromConfigMap(plain); ok {
		t.Error("routes without a backend annotation are not ours to serve")
	}

	// A non-http backend must be refused, not proxied.
	bad := cm.DeepCopy()
	bad.Annotations["donkey.dev/backend"] = "https://evil.example.com"
	if _, ok := routeFromConfigMap(*bad); ok {
		t.Error("only in-cluster plain-http backends are proxied")
	}
}
