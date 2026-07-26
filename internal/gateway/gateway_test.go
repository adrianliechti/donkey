package gateway

import (
	"bytes"
	"net/url"
	"testing"
	"time"

	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"

	"github.com/adrianliechti/donkey/internal/config"
)

func testSealer() *sealer {
	return newSealer(bytes.Repeat([]byte{7}, 32))
}

// The cookie is the session store, so sealing has to round-trip — and refuse
// anything tampered with.
func TestSessionSealRoundTrip(t *testing.T) {
	s := testSealer()
	in := session{Subject: "u1", Email: "ada@example.com", Groups: []string{"staff"}, Roles: []string{"dev"}, Expires: time.Now().Add(time.Hour).Unix()}
	sealed, err := s.seal(in)
	if err != nil {
		t.Fatal(err)
	}
	var out session
	if err := s.unseal(sealed, &out); err != nil {
		t.Fatal(err)
	}
	if out.Email != in.Email || out.Subject != in.Subject || len(out.Groups) != 1 || len(out.Roles) != 1 {
		t.Errorf("round trip lost data: %+v", out)
	}
	if err := s.unseal(sealed[:len(sealed)-2]+"xx", &out); err == nil {
		t.Error("a tampered value must not unseal")
	}
	other := newSealer(bytes.Repeat([]byte{9}, 32))
	if err := other.unseal(sealed, &out); err == nil {
		t.Error("a different key must not unseal")
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
		if got := allowedAccess(c.access, sess); got != c.want {
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

func TestRouteFromObject(t *testing.T) {
	obj := unstructured.Unstructured{Object: map[string]any{
		"metadata": map[string]any{
			"annotations": map[string]any{
				"donkey.dev/backend": "http://shop.donkey-acme.svc.cluster.local:80",
				"donkey.dev/login":   "required",
				"donkey.dev/access":  `{"users":["ada@example.com"],"roles":["developer"]}`,
			},
		},
		"spec": map[string]any{
			"hostnames": []any{"shop.example.com"},
			"rules": []any{map[string]any{
				"matches": []any{map[string]any{
					"path": map[string]any{"type": "PathPrefix", "value": "/api"},
				}},
			}},
		},
	}}
	rt, ok := routeFromObject(obj)
	if !ok {
		t.Fatal("route with backend annotation should be picked up")
	}
	if rt.Host != "shop.example.com" || rt.Path != "/api" || !rt.Login {
		t.Errorf("unexpected route: %+v", rt)
	}
	if rt.Access == nil || len(rt.Access.Users) != 1 || len(rt.Access.Roles) != 1 {
		t.Errorf("access rules should be parsed: %+v", rt.Access)
	}

	// No backend annotation → not the gateway's route (e.g. donkey's own
	// API route, or a leftover from an older version).
	plain := unstructured.Unstructured{Object: map[string]any{
		"spec": map[string]any{"hostnames": []any{"donkey.example.com"}},
	}}
	if _, ok := routeFromObject(plain); ok {
		t.Error("routes without a backend annotation are not ours to serve")
	}

	// A non-http backend must be refused, not proxied.
	bad := obj.DeepCopy()
	bad.SetAnnotations(map[string]string{"donkey.dev/backend": "https://evil.example.com"})
	if _, ok := routeFromObject(*bad); ok {
		t.Error("only in-cluster plain-http backends are proxied")
	}
}
