package server

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/adrianliechti/donkey/internal/config"
	"github.com/adrianliechti/donkey/internal/identity"
	"github.com/adrianliechti/donkey/pkg/runtime"
)

// fakeMachines is a runtime's machine half: it keeps what it was started
// with and reports it back.
type fakeMachines struct {
	mu      sync.Mutex
	specs   map[string]runtime.MachineSpec
	disks   map[string]bool
	started []string
	err     error
}

func newFakeMachines() *fakeMachines {
	return &fakeMachines{specs: map[string]runtime.MachineSpec{}, disks: map[string]bool{}}
}

func (m *fakeMachines) Status(_ context.Context, id string) (*runtime.MachineStatus, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if spec, ok := m.specs[id]; ok {
		return &runtime.MachineStatus{State: runtime.MachineRunning, Image: spec.Image, Host: spec.Host, SignInExpires: spec.SignInExpires}, nil
	}
	if m.disks[id] {
		return &runtime.MachineStatus{State: runtime.MachineStopped}, nil
	}
	return &runtime.MachineStatus{State: runtime.MachineAbsent}, nil
}

func (m *fakeMachines) Start(_ context.Context, spec runtime.MachineSpec) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.err != nil {
		return m.err
	}
	m.specs[spec.ID], m.disks[spec.ID] = spec, true
	m.started = append(m.started, spec.ID)
	return nil
}

func (m *fakeMachines) Stop(_ context.Context, id string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	delete(m.specs, id)
	return nil
}

func (m *fakeMachines) Delete(_ context.Context, id string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	delete(m.specs, id)
	delete(m.disks, id)
	return nil
}

func (m *fakeMachines) Logs(_ context.Context, id string, _ runtime.LogOptions, out io.Writer) error {
	_, err := io.WriteString(out, "logs of "+id+"\n")
	return err
}

type machineTestRuntime struct {
	*spaceTestRuntime
	machines *fakeMachines
}

func (r *machineTestRuntime) Machines() runtime.Machines { return r.machines }

// machineTestServer is a control plane with machines turned on, over the
// spaces of spaceTestServer: owner@example.com administers "team", root@ is
// a platform admin, everyone else is nobody.
func machineTestServer() (*Server, *fakeMachines) {
	s, spaces := spaceTestServer()
	machines := newFakeMachines()
	s.rt = &machineTestRuntime{spaceTestRuntime: spaces, machines: machines}
	s.cfg.Machines = config.MachineSettings{Image: "registry.example.com/donkey-machine:1"}
	s.cfg.AIModel = "test-model"
	return s, machines
}

// machineCall drives a request the way callAs does — donkey-server, then
// donkey-orchestrator for what it forwards — as one identity.
func machineCall(t *testing.T, s *Server, id identity.Identity, method, path string) *httptest.ResponseRecorder {
	t.Helper()
	orchestrator := httptest.NewServer(s.orchestratorRoutes())
	defer orchestrator.Close()
	s.cfg.OrchestratorURL = orchestrator.URL
	if id.Expires == 0 {
		id.Expires = time.Now().Add(time.Hour).Unix()
	}
	token, err := identity.SealToken(testSessionKey, id)
	if err != nil {
		t.Fatal(err)
	}
	req := httptest.NewRequest(method, "https://donkey.example.com"+path, bytes.NewReader(nil))
	req.Header.Set("Authorization", "Bearer "+token)
	rec := httptest.NewRecorder()
	s.routes().ServeHTTP(rec, req)
	return rec
}

func machineBody(t *testing.T, rec *httptest.ResponseRecorder) MachineResponse {
	t.Helper()
	var out MachineResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatalf("not a machine: %s", rec.Body.String())
	}
	return out
}

var (
	machineOwner    = identity.Identity{Subject: "sub-owner", Email: "owner@example.com", Name: "Ada Owner"}
	machineStranger = identity.Identity{Subject: "sub-stranger", Email: "stranger@example.com"}
	machineAdmin    = identity.Identity{Subject: "sub-root", Email: "root@example.com"}
)

// The whole life of a machine, as its owner drives it from the console.
func TestMachineLifecycle(t *testing.T) {
	s, machines := machineTestServer()
	id := config.MachineID(machineOwner.Subject)

	rec := machineCall(t, s, machineOwner, "GET", "/v1/machine")
	if got := machineBody(t, rec); rec.Code != 200 || !got.Enabled || !got.Allowed || got.State != "absent" || got.URL != "" {
		t.Fatalf("before: %d %+v", rec.Code, got)
	}

	rec = machineCall(t, s, machineOwner, "POST", "/v1/machine")
	got := machineBody(t, rec)
	if rec.Code != 200 || got.State != "running" {
		t.Fatalf("start: %d %s", rec.Code, rec.Body.String())
	}
	if want := "https://" + id + ".platform.example.com"; got.URL != want {
		t.Errorf("url = %q, want %q", got.URL, want)
	}
	if got.SignInExpires == "" || got.Outdated {
		t.Errorf("started machine = %+v", got)
	}

	spec := machines.specs[id]
	if spec.Owner != machineOwner.Subject || spec.Email != "owner@example.com" || spec.Name != "Ada Owner" {
		t.Errorf("owner = %q / %q / %q", spec.Owner, spec.Email, spec.Name)
	}
	if spec.Image != s.cfg.Machines.Image || spec.Model != "test-model" || spec.Host != id+".platform.example.com" {
		t.Errorf("spec = %+v", spec)
	}
	if spec.GuardToken != config.MachineGuardToken(testSessionKey, id) {
		t.Error("the machine was given a credential the gateway will not present")
	}
	// The CLI inside signs in as the owner, with a token of its own.
	signIn, err := identity.VerifyToken(testSessionKey, spec.Token)
	if err != nil {
		t.Fatalf("the machine's sign-in does not verify: %v", err)
	}
	if signIn.Subject != machineOwner.Subject || signIn.Email != machineOwner.Email {
		t.Errorf("the machine signs in as %+v", signIn)
	}
	if !strings.HasPrefix(spec.AIToken, "dkm_") || len(spec.AIToken) != 68 {
		t.Errorf("model token = %q", spec.AIToken)
	}

	// Stopping keeps the disk; deleting is what removes it.
	rec = machineCall(t, s, machineOwner, "POST", "/v1/machine/stop")
	if got := machineBody(t, rec); rec.Code != 200 || got.State != "stopped" || got.URL != "" || got.SignInExpires != "" {
		t.Fatalf("stop: %d %+v", rec.Code, got)
	}
	rec = machineCall(t, s, machineOwner, "DELETE", "/v1/machine")
	if got := machineBody(t, rec); rec.Code != 200 || got.State != "absent" {
		t.Fatalf("delete: %d %+v", rec.Code, got)
	}
}

// A second start is a restart: the machine gets a model token nobody has
// seen before, so the old one is worth nothing.
func TestMachineRestartRotatesItsModelToken(t *testing.T) {
	s, machines := machineTestServer()
	id := config.MachineID(machineOwner.Subject)
	machineCall(t, s, machineOwner, "POST", "/v1/machine")
	first := machines.specs[id].AIToken
	machineCall(t, s, machineOwner, "POST", "/v1/machine")
	if second := machines.specs[id].AIToken; second == first || second == "" {
		t.Fatalf("model token %q was reused", first)
	}
	if len(machines.started) != 2 {
		t.Fatalf("starts = %v", machines.started)
	}
}

// Everyone acts on their own machine and nobody else's: the name comes from
// who is asking, so there is nothing to point at another person's.
func TestMachinesAreOnePerPerson(t *testing.T) {
	s, machines := machineTestServer()
	machineCall(t, s, machineOwner, "POST", "/v1/machine")
	machineCall(t, s, machineAdmin, "POST", "/v1/machine")
	owner, admin := config.MachineID(machineOwner.Subject), config.MachineID(machineAdmin.Subject)
	if owner == admin || len(machines.specs) != 2 {
		t.Fatalf("machines = %v", machines.started)
	}
	// A platform admin stopping "the machine" stops their own.
	machineCall(t, s, machineAdmin, "POST", "/v1/machine/stop")
	if _, running := machines.specs[owner]; !running {
		t.Fatal("an administrator stopped somebody else's machine")
	}
	if machines.specs[owner].Owner != machineOwner.Subject || machines.specs[owner].GuardToken == config.MachineGuardToken(testSessionKey, admin) {
		t.Fatal("the owner's machine carries somebody else's identity")
	}
}

// A machine costs compute, disk and model calls. It is for the people who
// deploy here; a sign-in alone, or a viewer's role, does not get one.
func TestMachineIsForPeopleWhoBuildHere(t *testing.T) {
	s, machines := machineTestServer()
	viewer := identity.Identity{Subject: "sub-viewer", Email: "viewer@example.com"}
	spaces := s.rt.(*machineTestRuntime).spaceTestRuntime
	team := spaces.settings["team"]
	team.Viewers = config.AccessConfig{Users: []string{"viewer@example.com"}}
	spaces.settings["team"] = team

	for _, who := range []identity.Identity{machineStranger, viewer} {
		rec := machineCall(t, s, who, "GET", "/v1/machine")
		if got := machineBody(t, rec); rec.Code != 200 || !got.Enabled || got.Allowed {
			t.Errorf("%s: GET = %d %+v", who.Email, rec.Code, got)
		}
		for _, call := range [][2]string{{"POST", "/v1/machine"}, {"POST", "/v1/machine/stop"}, {"DELETE", "/v1/machine"}, {"GET", "/v1/machine/logs"}} {
			if rec := machineCall(t, s, who, call[0], call[1]); rec.Code != http.StatusForbidden {
				t.Errorf("%s: %s %s = %d, want 403", who.Email, call[0], call[1], rec.Code)
			}
		}
	}
	if len(machines.started) != 0 {
		t.Fatalf("machines were started: %v", machines.started)
	}
	// A developer by group and a platform admin both qualify.
	developer := identity.Identity{Subject: "sub-dev", Email: "dev@example.com", Groups: []string{"team-dev"}}
	for _, who := range []identity.Identity{developer, machineAdmin} {
		if rec := machineCall(t, s, who, "POST", "/v1/machine"); rec.Code != 200 {
			t.Errorf("%s: start = %d %s", who.Email, rec.Code, rec.Body.String())
		}
	}
}

func TestMachinesAreOffWithoutAnImage(t *testing.T) {
	s, machines := machineTestServer()
	s.cfg.Machines = config.MachineSettings{}
	rec := machineCall(t, s, machineOwner, "GET", "/v1/machine")
	if got := machineBody(t, rec); rec.Code != 200 || got.Enabled || got.Allowed {
		t.Fatalf("GET = %d %+v", rec.Code, got)
	}
	if rec := machineCall(t, s, machineOwner, "POST", "/v1/machine"); rec.Code != http.StatusNotFound {
		t.Fatalf("start = %d, want 404", rec.Code)
	}
	if len(machines.started) != 0 {
		t.Fatal("a machine was started on an installation without machines")
	}
}

func TestMachineRequiresASignIn(t *testing.T) {
	s, _ := machineTestServer()
	for _, call := range [][2]string{{"GET", "/v1/machine"}, {"POST", "/v1/machine"}, {"DELETE", "/v1/machine"}} {
		req := httptest.NewRequest(call[0], "https://donkey.example.com"+call[1], nil)
		rec := httptest.NewRecorder()
		s.routes().ServeHTTP(rec, req)
		if rec.Code != http.StatusUnauthorized {
			t.Errorf("%s %s without a sign-in = %d", call[0], call[1], rec.Code)
		}
	}
}

// The sign-in a machine holds expires when a `donkey login` at the same
// moment as its owner's would have — never later, however often the machine
// is restarted and whichever of its own tokens is used to do it.
func TestMachineSignInDoesNotOutliveItsLogin(t *testing.T) {
	now := time.Now()
	login := now.Add(-10 * 24 * time.Hour)
	owner := identity.Identity{Subject: "u", Email: "u@example.com", Groups: []string{"g"},
		LoginTime: login.Unix(), RefreshToken: "idp-refresh", Grant: "grant", AccessToken: "idp-access"}

	signIn := machineSignIn(&owner, now)
	if want := login.Add(identity.TokenTTL).Unix(); signIn.Expires != want {
		t.Fatalf("expires %s, want %s — 30 days from the login, not from the start",
			time.Unix(signIn.Expires, 0), time.Unix(want, 0))
	}
	// Starting the machine again with the token it was given changes nothing.
	again := machineSignIn(&signIn, now.Add(5*24*time.Hour))
	if again.Expires != signIn.Expires {
		t.Fatalf("a restart moved the expiry from %s to %s", time.Unix(signIn.Expires, 0), time.Unix(again.Expires, 0))
	}
	if signIn.RefreshToken != "" || signIn.Grant != "" || signIn.AccessToken != "" {
		t.Fatalf("an IdP credential went into the machine: %+v", signIn)
	}
	if signIn.Subject != "u" || signIn.Email != "u@example.com" || len(signIn.Groups) != 1 {
		t.Fatalf("identity = %+v", signIn)
	}
	// A sign-in from before the field counts from now.
	legacy := machineSignIn(&identity.Identity{Subject: "u"}, now)
	if legacy.Expires != now.Add(identity.TokenTTL).Unix() {
		t.Fatalf("legacy expiry = %s", time.Unix(legacy.Expires, 0))
	}
}

// A sign-in about to run out cannot start a machine: its CLI would stop
// working within the hour, with nothing to say why.
func TestMachineStartRefusesAnExpiringSignIn(t *testing.T) {
	s, machines := machineTestServer()
	old := machineOwner
	old.LoginTime = time.Now().Add(-identity.TokenTTL).Add(10 * time.Minute).Unix()
	rec := machineCall(t, s, old, "POST", "/v1/machine")
	if rec.Code != http.StatusUnauthorized || len(machines.started) != 0 {
		t.Fatalf("start = %d %s (started %v)", rec.Code, rec.Body.String(), machines.started)
	}
}

func TestMachineReportsANewerImage(t *testing.T) {
	s, _ := machineTestServer()
	machineCall(t, s, machineOwner, "POST", "/v1/machine")
	s.cfg.Machines.Image = "registry.example.com/donkey-machine:2"
	if got := machineBody(t, machineCall(t, s, machineOwner, "GET", "/v1/machine")); !got.Outdated {
		t.Fatalf("machine = %+v, want outdated", got)
	}
}

func TestMachineLogs(t *testing.T) {
	s, _ := machineTestServer()
	rec := machineCall(t, s, machineOwner, "GET", "/v1/machine/logs")
	if want := "logs of " + config.MachineID(machineOwner.Subject) + "\n"; rec.Code != 200 || rec.Body.String() != want {
		t.Fatalf("logs = %d %q", rec.Code, rec.Body.String())
	}
}

// The console may frame a machine's address — and nothing else, and only
// where the installation has machines.
func TestConsoleFramePolicy(t *testing.T) {
	s, _ := machineTestServer()
	policy := func() string {
		rec := httptest.NewRecorder()
		s.routes().ServeHTTP(rec, httptest.NewRequest("GET", "https://example.com/", nil))
		return rec.Header().Get("Content-Security-Policy")
	}
	if got := policy(); !strings.Contains(got, "frame-src https://*.platform.example.com") || !strings.Contains(got, "frame-ancestors 'none'") {
		t.Fatalf("policy = %q", got)
	}
	s.cfg.Machines = config.MachineSettings{}
	if got := policy(); strings.Contains(got, "frame-src") {
		t.Fatalf("an installation without machines allows frames: %q", got)
	}
}
