package gateway

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/adrianliechti/donkey/internal/config"
	"github.com/adrianliechti/donkey/internal/identity"
	"github.com/adrianliechti/donkey/pkg/runtime"
)

const (
	testMachineOwner = "owner-subject"
	testMachineHost  = "m0123456789abcdef.platform.example.com"
)

var testMachineID = "m0123456789abcdef"

// machineGateway is a gateway serving one machine, and the backend standing
// in for it, which keeps the headers of every request that got through.
func machineGateway(t *testing.T, extra ...config.GatewayRoute) (*Server, *[]http.Header) {
	t.Helper()
	var seen []http.Header
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		seen = append(seen, r.Header.Clone())
		w.Header().Set("X-Frame-Options", "DENY")
		_, _ = w.Write([]byte("machine"))
	}))
	t.Cleanup(backend.Close)
	routes := append([]config.GatewayRoute{
		config.MachineRoute(testMachineID, testMachineOwner, testMachineHost, backend.URL),
	}, extra...)
	for i := range routes {
		if routes[i].Backend == "" {
			routes[i].Backend = backend.URL
		}
	}
	s := testServer()
	s.cfg.Domain = "example.com"
	s.cfg.OIDCIssuer, s.cfg.OIDCClientID, s.cfg.OIDCClientSecret = "https://idp.example.com", "id", "secret"
	s.source = runtimeRoutes{source: &snapshotSource{snapshot: runtime.GatewaySnapshot{
		Routes:   routes,
		Spaces:   map[string]config.SpaceConfig{"team": {AllowPublic: true}},
		Platform: config.PlatformConfig{Admins: config.AccessConfig{Users: []string{"root@example.com"}}},
	}}}
	if err := s.refreshRoutes(context.Background()); err != nil {
		t.Fatal(err)
	}
	return s, &seen
}

// machineRequest is a request to the machine's address by one signed-in
// person, with whatever extra headers a client might try.
func machineRequest(t *testing.T, subject, email string, headers map[string]string) *http.Request {
	t.Helper()
	r := httptest.NewRequest("GET", "https://"+testMachineHost+"/api/workspace", nil)
	if subject != "" {
		tok, err := identity.SealToken(testKey, identity.Identity{
			Subject: subject, Email: email, Expires: time.Now().Add(time.Hour).Unix(),
		})
		if err != nil {
			t.Fatal(err)
		}
		r.Header.Set("Authorization", "Bearer "+tok)
	}
	for name, value := range headers {
		r.Header.Set(name, value)
	}
	return r
}

// A machine holds its owner's sign-in and files. Only the owner gets in — a
// platform administrator included, who may manage everything else — and
// nobody else's request ever reaches the machine.
func TestMachineRouteAdmitsOnlyItsOwner(t *testing.T) {
	s, seen := machineGateway(t)
	rt := s.routes[testMachineHost][0]
	if !rt.Login {
		t.Fatal("a machine's route must demand a sign-in")
	}
	guard := config.MachineGuardToken(testKey, testMachineID)

	for name, tc := range map[string]struct {
		subject, email string
		headers        map[string]string
		want           int
	}{
		"owner":          {subject: testMachineOwner, email: "owner@example.com", want: http.StatusOK},
		"stranger":       {subject: "someone-else", email: "else@example.com", want: http.StatusForbidden},
		"platform admin": {subject: "root", email: "root@example.com", want: http.StatusForbidden},
		"anonymous":      {want: http.StatusUnauthorized},
		// Knowing the credential does not help: it is the gateway's to
		// present, after it has decided who is asking.
		"stranger with the credential": {
			subject: "someone-else", email: "else@example.com",
			headers: map[string]string{config.MachineGuardHeader: guard},
			want:    http.StatusForbidden,
		},
	} {
		t.Run(name, func(t *testing.T) {
			before := len(*seen)
			w := httptest.NewRecorder()
			s.serveRoute(w, machineRequest(t, tc.subject, tc.email, tc.headers), rt)
			if w.Code != tc.want {
				t.Fatalf("status = %d, want %d (%s)", w.Code, tc.want, strings.TrimSpace(w.Body.String()))
			}
			if reached := len(*seen) > before; reached != (tc.want == http.StatusOK) {
				t.Fatalf("request reached the machine = %v", reached)
			}
		})
	}
}

// The machine's front door opens for one credential, which only the gateway
// can compute. A copy sent by the visitor is replaced, and the owner's own
// token never travels on.
func TestMachineRouteCarriesTheGatewaysCredential(t *testing.T) {
	s, seen := machineGateway(t)
	rt := s.routes[testMachineHost][0]
	w := httptest.NewRecorder()
	s.serveRoute(w, machineRequest(t, testMachineOwner, "owner@example.com",
		map[string]string{config.MachineGuardHeader: "forged"}), rt)
	if w.Code != http.StatusOK || len(*seen) != 1 {
		t.Fatalf("status = %d, requests = %d", w.Code, len(*seen))
	}
	got := (*seen)[0]
	if want := config.MachineGuardToken(testKey, testMachineID); got.Get(config.MachineGuardHeader) != want {
		t.Errorf("credential = %q, want the gateway's own", got.Get(config.MachineGuardHeader))
	}
	if got.Get("Authorization") != "" || got.Get(accessTokenHeader) != "" || got.Get(idpAccessTokenHeader) != "" {
		t.Errorf("a credential of the owner reached the machine: %v", got)
	}
	// The console may show the machine in a frame, and the machine itself —
	// its agent previews the owner's files in one. Nobody else.
	if policy := w.Header().Values("Content-Security-Policy"); len(policy) != 1 || policy[0] != "frame-ancestors 'self' https://example.com" {
		t.Errorf("frame policy = %q", policy)
	}
	if w.Header().Get("X-Frame-Options") != "" {
		t.Error("the machine's own X-Frame-Options would keep the console from showing it")
	}
}

// An app must never be handed, or be able to forge, a machine's credential.
func TestAppRouteNeverCarriesAMachineCredential(t *testing.T) {
	s, seen := machineGateway(t, config.GatewayRoute{Space: "team", App: "web", Host: "web.team.example.com", Path: "/"})
	rt := s.routes["web.team.example.com"][0]
	r := httptest.NewRequest("GET", "https://web.team.example.com/", nil)
	r.Header.Set(config.MachineGuardHeader, config.MachineGuardToken(testKey, testMachineID))
	w := httptest.NewRecorder()
	s.serveRoute(w, r, rt)
	if w.Code != http.StatusOK || len(*seen) != 1 {
		t.Fatalf("status = %d, requests = %d", w.Code, len(*seen))
	}
	if got := (*seen)[0].Get(config.MachineGuardHeader); got != "" {
		t.Fatalf("an app received a machine credential: %q", got)
	}
	if w.Header().Get("Content-Security-Policy") != "" {
		t.Error("an app's response was given the machine's frame policy")
	}
}

// A space that may take any hostname could name a machine's. The machine
// keeps its address: whatever else claims it is not served there.
func TestMachineRouteKeepsItsHostname(t *testing.T) {
	s, _ := machineGateway(t, config.GatewayRoute{Space: "team", App: "squatter", Host: testMachineHost, Path: "/"})
	routes := s.routes[testMachineHost]
	if len(routes) != 1 || routes[0].Machine != testMachineID {
		t.Fatalf("routes on the machine's hostname: %+v", routes)
	}
}

// A machine route that does not say whose it is, or claims to be an app or
// the console as well, is dropped rather than served to everyone.
func TestMalformedMachineRouteIsNotServed(t *testing.T) {
	for name, rec := range map[string]config.GatewayRoute{
		"no owner":       {Host: "a.platform.example.com", Backend: "http://x:80", Machine: testMachineID},
		"not a machine":  {Host: "b.platform.example.com", Backend: "http://x:80", Machine: "web", Owner: "u"},
		"also an app":    {Host: "c.platform.example.com", Backend: "http://x:80", Machine: testMachineID, Owner: "u", Space: "team", App: "web"},
		"also a console": {Host: "d.platform.example.com", Backend: "http://x:80", Machine: testMachineID, Owner: "u", Token: true},
	} {
		t.Run(name, func(t *testing.T) {
			routes, err := runtimeRoutes{source: &snapshotSource{snapshot: runtime.GatewaySnapshot{
				Routes: []config.GatewayRoute{rec},
			}}}.list(context.Background())
			if err != nil {
				t.Fatal(err)
			}
			if len(routes) != 0 {
				t.Fatalf("served: %+v", routes)
			}
		})
	}
}
