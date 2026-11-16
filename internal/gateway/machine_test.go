package gateway

import (
	"context"
	"net"
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
	testMachineHost  = "machine.example.com"
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

// machinesRoute is what the gateway finds for the machines' address before
// it knows who is asking.
func machinesRoute(t *testing.T, s *Server) route {
	t.Helper()
	rt, ok := s.lookup(context.Background(), testMachineHost, "/api/workspace")
	if !ok || !rt.Personal || !rt.Login {
		t.Fatalf("the machines' address must demand a sign-in and lead nowhere by itself: %+v", rt)
	}
	return rt
}

// A machine holds its owner's sign-in and files. The machines' address leads
// only the owner to it — a platform administrator, who may manage everything
// else, is told like anybody else that no machine of theirs is running — and
// nobody else's request ever reaches the machine.
func TestMachineAddressLeadsOnlyTheOwnerIn(t *testing.T) {
	s, seen := machineGateway(t)
	rt := machinesRoute(t, s)
	guard := config.MachineGuardToken(testKey, testMachineID)

	for name, tc := range map[string]struct {
		subject, email string
		headers        map[string]string
		want           int
	}{
		"owner":          {subject: testMachineOwner, email: "owner@example.com", want: http.StatusOK},
		"stranger":       {subject: "someone-else", email: "else@example.com", want: http.StatusServiceUnavailable},
		"platform admin": {subject: "root", email: "root@example.com", want: http.StatusServiceUnavailable},
		"anonymous":      {want: http.StatusUnauthorized},
		// Knowing the credential does not help: it is the gateway's to
		// present, after it has decided who is asking.
		"stranger with the credential": {
			subject: "someone-else", email: "else@example.com",
			headers: map[string]string{config.MachineGuardHeader: guard},
			want:    http.StatusServiceUnavailable,
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

	// The route of a machine admits its owner and nobody else, however a
	// request came to be matched with it.
	t.Run("a machine's own route", func(t *testing.T) {
		before := len(*seen)
		w := httptest.NewRecorder()
		s.serveRoute(w, machineRequest(t, "someone-else", "else@example.com", nil), s.machines[testMachineOwner])
		if w.Code != http.StatusForbidden || len(*seen) != before {
			t.Fatalf("status = %d, reached the machine = %v", w.Code, len(*seen) != before)
		}
	})
	if (route{Personal: true}).allows(testMachineOwner, "owner@example.com", nil) {
		t.Error("the machines' address admits somebody without a machine's route taking its place")
	}
}

// Every machine answers on the one address. Which of them a request reaches
// is decided by who is signed in, and by nothing the request says.
func TestMachinesShareOneAddress(t *testing.T) {
	backend := func(name string) string {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			_, _ = w.Write([]byte(name))
		}))
		t.Cleanup(srv.Close)
		return srv.URL
	}
	other := "mfedcba9876543210"
	s := testServer()
	s.cfg.Domain = "example.com"
	s.cfg.OIDCIssuer, s.cfg.OIDCClientID, s.cfg.OIDCClientSecret = "https://idp.example.com", "id", "secret"
	source := &snapshotSource{snapshot: runtime.GatewaySnapshot{Routes: []config.GatewayRoute{
		config.MachineRoute(testMachineID, testMachineOwner, testMachineHost, backend("first")),
		config.MachineRoute(other, "second-subject", testMachineHost, backend("second")),
	}}}
	s.source = runtimeRoutes{source: source}
	if err := s.refreshRoutes(context.Background()); err != nil {
		t.Fatal(err)
	}
	rt := machinesRoute(t, s)
	for subject, want := range map[string]string{testMachineOwner: "first", "second-subject": "second"} {
		w := httptest.NewRecorder()
		// Naming the other machine changes nothing: nothing in a request
		// chooses the machine.
		r := machineRequest(t, subject, subject+"@example.com", map[string]string{"X-Donkey-Machine": other})
		r.URL.RawQuery = "machine=" + other
		s.serveRoute(w, r, rt)
		if w.Code != http.StatusOK || w.Body.String() != want {
			t.Errorf("%s reached %q (%d), want %q", subject, w.Body.String(), w.Code, want)
		}
	}

	// A machine that stopped is gone from the address for its owner alone.
	source.snapshot.Routes = source.snapshot.Routes[1:]
	if err := s.refreshRoutes(context.Background()); err != nil {
		t.Fatal(err)
	}
	w := httptest.NewRecorder()
	s.serveRoute(w, machineRequest(t, testMachineOwner, "owner@example.com", nil), rt)
	if w.Code != http.StatusServiceUnavailable || !strings.Contains(w.Body.String(), "Your machine") {
		t.Errorf("the owner of a stopped machine got %d: %s", w.Code, w.Body.String())
	}
	w = httptest.NewRecorder()
	s.serveRoute(w, machineRequest(t, "second-subject", "second@example.com", nil), rt)
	if w.Code != http.StatusOK || w.Body.String() != "second" {
		t.Errorf("the other machine answered %d %q", w.Code, w.Body.String())
	}
}

// A machine started a moment ago answers on the address right away: the
// gateway reads its routes again when it finds none for the visitor.
func TestMachineStartedJustNowAnswers(t *testing.T) {
	s, seen := machineGateway(t)
	source := s.source.(runtimeRoutes).source.(*snapshotSource)
	started := source.snapshot.Routes
	source.snapshot.Routes = nil
	if err := s.refreshRoutes(context.Background()); err != nil {
		t.Fatal(err)
	}
	source.snapshot.Routes = started
	s.mu.Lock()
	s.lastAttempt = time.Now().Add(-time.Minute)
	s.mu.Unlock()
	w := httptest.NewRecorder()
	s.serveRoute(w, machineRequest(t, testMachineOwner, "owner@example.com", nil), machinesRoute(t, s))
	if w.Code != http.StatusOK || len(*seen) != 1 {
		t.Fatalf("status = %d, requests = %d", w.Code, len(*seen))
	}
}

// machineBrowser is a request from a browser its owner is signed in on, with
// the gateway's note about the machine that browser showed last.
func machineBrowser(t *testing.T, s *Server, subject, showed string) *http.Request {
	t.Helper()
	sealed, err := identity.Seal(testKey, purposeSession, session{Subject: subject, Expires: time.Now().Add(time.Hour).Unix()})
	if err != nil {
		t.Fatal(err)
	}
	r := httptest.NewRequest("GET", "https://"+testMachineHost+"/", nil)
	r.Header.Set("Accept", "text/html")
	r.AddCookie(&http.Cookie{Name: sessionCookie, Value: sealed})
	if showed != "" {
		r.AddCookie(&http.Cookie{Name: machineBrowserCookie, Value: showed})
	}
	return r
}

// One address is one origin, so a browser keeps what every machine leaves
// behind in the same place. A browser that showed somebody else's machine
// last — or none the gateway knows of — starts clean before it shows this
// one; a browser that showed this one keeps what it has.
func TestMachineAddressStartsCleanForAnotherPerson(t *testing.T) {
	s, seen := machineGateway(t)
	rt := machinesRoute(t, s)
	for name, tc := range map[string]struct {
		showed string
		clean  bool
	}{
		"first visit":               {showed: "", clean: true},
		"somebody else's was shown": {showed: "mfedcba9876543210", clean: true},
		"this machine was shown":    {showed: testMachineID, clean: false},
	} {
		t.Run(name, func(t *testing.T) {
			*seen = nil
			w := httptest.NewRecorder()
			s.serveRoute(w, machineBrowser(t, s, testMachineOwner, tc.showed), rt)
			if w.Code != http.StatusOK || len(*seen) != 1 {
				t.Fatalf("status = %d, requests = %d", w.Code, len(*seen))
			}
			if got := w.Header().Get("Clear-Site-Data"); (got == `"storage"`) != tc.clean {
				t.Errorf("Clear-Site-Data = %q, clean = %v", got, tc.clean)
			}
			var note *http.Cookie
			for _, c := range w.Result().Cookies() {
				if c.Name == machineBrowserCookie {
					note = c
				}
			}
			switch {
			case !tc.clean && note != nil:
				t.Errorf("the note was written again: %+v", note)
			case tc.clean && (note == nil || note.Value != testMachineID || !note.HttpOnly || !note.Secure || note.Domain != "" || note.Path != "/"):
				t.Errorf("note = %+v, want this machine, kept from scripts and to this host", note)
			}
			// The note is the gateway's: the machine never sees it.
			if cookie := (*seen)[0].Get("Cookie"); strings.Contains(cookie, machineBrowserCookie) || strings.Contains(cookie, sessionCookie) {
				t.Errorf("the machine received %q", cookie)
			}
			// What a browser cached for one sign-in is not reused for another.
			if !strings.Contains(strings.Join(w.Header().Values("Vary"), ","), "Cookie") {
				t.Errorf("Vary = %q", w.Header().Values("Vary"))
			}
		})
	}
}

// lateMachine is a gateway whose one machine is not taking connections yet,
// and the way to make it. The gateway's patience with machines is cut to
// what a test can wait.
func lateMachine(t *testing.T, patience time.Duration) (*Server, func()) {
	t.Helper()
	reserved, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	address := reserved.Addr().String()
	reserved.Close()

	kept := machineTransport
	machineTransport = &http.Transport{DialContext: runtime.MachineDialer(patience)}
	t.Cleanup(func() { machineTransport = kept })

	s, _ := machineGateway(t)
	source := s.source.(runtimeRoutes).source.(*snapshotSource)
	source.snapshot.Routes = []config.GatewayRoute{
		config.MachineRoute(testMachineID, testMachineOwner, testMachineHost, "http://"+address),
		{Space: "team", App: "web", Host: "web.team.example.com", Path: "/", Backend: "http://" + address},
	}
	if err := s.refreshRoutes(context.Background()); err != nil {
		t.Fatal(err)
	}
	return s, func() {
		door, err := net.Listen("tcp", address)
		if err != nil {
			t.Skipf("the port was taken by somebody else in the meantime: %v", err)
		}
		srv := &http.Server{Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			_, _ = w.Write([]byte("machine"))
		})}
		go func() { _ = srv.Serve(door) }()
		t.Cleanup(func() { _ = srv.Close() })
	}
}

// The console shows a machine the moment it reads "running", which is before
// every runtime can route to it. The gateway waits for it then, so its owner
// gets the machine a moment later instead of an error and a reload button.
func TestGatewayWaitsForAMachineThatJustCameUp(t *testing.T) {
	s, comeUp := lateMachine(t, 10*time.Second)
	go func() {
		time.Sleep(500 * time.Millisecond)
		comeUp()
	}()
	w := httptest.NewRecorder()
	s.serveRoute(w, machineRequest(t, testMachineOwner, "owner@example.com", nil), machinesRoute(t, s))
	if w.Code != http.StatusOK || w.Body.String() != "machine" {
		t.Fatalf("status = %d: %s", w.Code, strings.TrimSpace(w.Body.String()))
	}
}

// A machine that still is not there when the wait ends is, more often than
// not, a moment from being there or from being gone: the page a browser gets
// asks again by itself. Nothing else does — and an app that takes no
// connections is down, and says so at once.
func TestGatewayAsksAgainForAMachineThatDoesNotAnswer(t *testing.T) {
	s, _ := lateMachine(t, 300*time.Millisecond)
	rt := machinesRoute(t, s)

	w := httptest.NewRecorder()
	s.serveRoute(w, machineBrowser(t, s, testMachineOwner, testMachineID), rt)
	if w.Code != http.StatusBadGateway || w.Header().Get("Refresh") == "" || !strings.Contains(w.Body.String(), "Your machine") {
		t.Errorf("a browser got %d, Refresh %q: %s", w.Code, w.Header().Get("Refresh"), w.Body.String())
	}

	w = httptest.NewRecorder()
	s.serveRoute(w, machineRequest(t, testMachineOwner, "owner@example.com", map[string]string{"Accept": "application/json"}), rt)
	if w.Code != http.StatusBadGateway || w.Header().Get("Refresh") != "" {
		t.Errorf("a request of the page got %d, Refresh %q", w.Code, w.Header().Get("Refresh"))
	}

	began := time.Now()
	w = httptest.NewRecorder()
	s.serveRoute(w, httptest.NewRequest("GET", "https://web.team.example.com/", nil), s.routes["web.team.example.com"][0])
	if w.Code != http.StatusBadGateway || w.Header().Get("Refresh") != "" || time.Since(began) > 250*time.Millisecond {
		t.Errorf("an app got %d after %s, Refresh %q", w.Code, time.Since(began), w.Header().Get("Refresh"))
	}
}

// A machine cannot write the gateway's note itself — it could claim the
// browser for another person's machine ahead of time — and cannot install a
// service worker, which would answer for the address without the gateway.
func TestMachineCannotKeepHoldOfTheAddress(t *testing.T) {
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Add("Set-Cookie", machineBrowserCookie+"=mfedcba9876543210; Path=/; Secure")
		w.Header().Add("Set-Cookie", "theme=dark; Path=/")
		_, _ = w.Write([]byte("machine"))
	}))
	t.Cleanup(backend.Close)
	s, _ := machineGateway(t)
	source := s.source.(runtimeRoutes).source.(*snapshotSource)
	source.snapshot.Routes = []config.GatewayRoute{config.MachineRoute(testMachineID, testMachineOwner, testMachineHost, backend.URL)}
	if err := s.refreshRoutes(context.Background()); err != nil {
		t.Fatal(err)
	}
	rt := machinesRoute(t, s)

	w := httptest.NewRecorder()
	s.serveRoute(w, machineBrowser(t, s, testMachineOwner, testMachineID), rt)
	var names []string
	for _, c := range w.Result().Cookies() {
		names = append(names, c.Name)
	}
	if w.Code != http.StatusOK || len(names) != 1 || names[0] != "theme" {
		t.Errorf("status = %d, cookies = %v, want only the machine's own", w.Code, names)
	}

	r := machineBrowser(t, s, testMachineOwner, testMachineID)
	r.Header.Set("Service-Worker", "script")
	w = httptest.NewRecorder()
	s.serveRoute(w, r, rt)
	if w.Code != http.StatusForbidden {
		t.Errorf("a service worker script was served: %d", w.Code)
	}
}

// The machine's front door opens for one credential, which only the gateway
// can compute. A copy sent by the visitor is replaced, and the owner's own
// token never travels on.
func TestMachineRouteCarriesTheGatewaysCredential(t *testing.T) {
	s, seen := machineGateway(t)
	rt := machinesRoute(t, s)
	w := httptest.NewRecorder()
	// The owner can read both of their machine's credentials inside it. The
	// control plane's still never arrives through the gateway: what a browser
	// sends is the owner's, and is never taken for donkey's own.
	s.serveRoute(w, machineRequest(t, testMachineOwner, "owner@example.com", map[string]string{
		config.MachineGuardHeader:   "forged",
		config.MachineControlHeader: config.MachineControlToken(testKey, testMachineID),
	}), rt)
	if w.Code != http.StatusOK || len(*seen) != 1 {
		t.Fatalf("status = %d, requests = %d", w.Code, len(*seen))
	}
	got := (*seen)[0]
	if got.Get(config.MachineControlHeader) != "" {
		t.Errorf("a browser's request reached the machine as the control plane's")
	}
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

// A space that may take any hostname could name the machines'. The address
// stays theirs — also while no machine is running, when it would otherwise
// be free: whatever else claims it is not served there.
func TestMachineAddressIsKeptForMachines(t *testing.T) {
	squatter := config.GatewayRoute{Space: "team", App: "squatter", Host: testMachineHost, Path: "/"}
	s, seen := machineGateway(t, squatter)
	if len(s.routes[testMachineHost]) != 0 || len(s.machines) != 1 {
		t.Fatalf("on the machines' address: %+v, machines: %+v", s.routes[testMachineHost], s.machines)
	}

	source := s.source.(runtimeRoutes).source.(*snapshotSource)
	squatter.Backend = source.snapshot.Routes[0].Backend
	source.snapshot.Routes = []config.GatewayRoute{squatter}
	if err := s.refreshRoutes(context.Background()); err != nil {
		t.Fatal(err)
	}
	w := httptest.NewRecorder()
	s.serveRoute(w, machineRequest(t, "someone-else", "else@example.com", nil), machinesRoute(t, s))
	if w.Code != http.StatusServiceUnavailable || len(*seen) != 0 {
		t.Fatalf("an app answered on the machines' address: %d, requests = %d", w.Code, len(*seen))
	}
	// The address is always entitled to its certificate, so a visitor whose
	// machine is stopped reads that instead of a TLS error.
	if err := s.allowHost(context.Background(), testMachineHost); err != nil {
		t.Errorf("no certificate for the machines' address: %v", err)
	}
}

// No hostname stands for a single machine: a machine route naming an address
// of its own is not served there or anywhere else.
func TestMachineRouteOnAnotherAddressIsNotServed(t *testing.T) {
	own := testMachineID + ".platform.example.com"
	s, seen := machineGateway(t)
	source := s.source.(runtimeRoutes).source.(*snapshotSource)
	source.snapshot.Routes[0].Host = own
	if err := s.refreshRoutes(context.Background()); err != nil {
		t.Fatal(err)
	}
	if len(s.machines) != 0 || len(s.routes[own]) != 0 {
		t.Fatalf("machines: %+v, routes: %+v", s.machines, s.routes[own])
	}
	if _, ok := s.tableLookup(own, "/"); ok || len(*seen) != 0 {
		t.Fatal("a machine answers on a hostname of its own")
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
