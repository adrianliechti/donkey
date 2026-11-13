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
	"github.com/adrianliechti/donkey/internal/machine"
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
	// door is where every running machine's front door answers.
	door string
	// folders is what was kept with each disk, and remembered how often.
	folders    map[string][]runtime.MachineFolder
	remembered int
	// startedAt is when each running machine came up.
	startedAt map[string]time.Time
}

func newFakeMachines() *fakeMachines {
	return &fakeMachines{specs: map[string]runtime.MachineSpec{}, disks: map[string]bool{}}
}

func (m *fakeMachines) Status(_ context.Context, id string) (*runtime.MachineStatus, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if spec, ok := m.specs[id]; ok {
		return &runtime.MachineStatus{
			State: runtime.MachineRunning, Image: spec.Image, Host: spec.Host, Backend: m.door,
			SignInExpires: spec.SignInExpires, StartedAt: m.startedAt[id],
		}, nil
	}
	if m.disks[id] {
		return &runtime.MachineStatus{State: runtime.MachineStopped, Folders: m.folders[id]}, nil
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
	if m.startedAt == nil {
		m.startedAt = map[string]time.Time{}
	}
	m.startedAt[spec.ID] = time.Unix(int64(1700000000+len(m.started)), 0)
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
	delete(m.folders, id)
	return nil
}

func (m *fakeMachines) Remember(_ context.Context, id string, folders []runtime.MachineFolder) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.folders == nil {
		m.folders = map[string][]runtime.MachineFolder{}
	}
	m.folders[id] = folders
	m.remembered++
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
	return machineSend(t, s, id, method, path, nil)
}

func machineSend(t *testing.T, s *Server, id identity.Identity, method, path string, body []byte) *httptest.ResponseRecorder {
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
	req := httptest.NewRequest(method, "https://donkey.example.com"+path, bytes.NewReader(body))
	req.Header.Set("Authorization", "Bearer "+token)
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
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
	// Unless the installation opens them, machines reach the control plane
	// only — and the console is told, so it can say so.
	if got.Egress != config.MachineEgressRestricted {
		t.Errorf("egress = %q, want restricted by default", got.Egress)
	}

	spec := machines.specs[id]
	if spec.Owner != machineOwner.Subject || spec.Email != "owner@example.com" || spec.Name != "Ada Owner" {
		t.Errorf("owner = %q / %q / %q", spec.Owner, spec.Email, spec.Name)
	}
	if spec.Image != s.cfg.Machines.Image || spec.Model != "test-model" || spec.Host != id+".platform.example.com" {
		t.Errorf("spec = %+v", spec)
	}
	if !spec.Settings.RestrictEgress() {
		t.Error("the machine was started open on an installation that did not ask for it")
	}
	if spec.GuardToken != config.MachineGuardToken(testSessionKey, id) {
		t.Error("the machine was given a credential the gateway will not present")
	}
	if spec.ControlToken != config.MachineControlToken(testSessionKey, id) || spec.ControlToken == spec.GuardToken {
		t.Error("the machine was given a credential donkey-server will not present, or the gateway's twice")
	}
	// Unused, a machine stops after an hour unless the installation says
	// otherwise — and the console is told, so it can say so.
	if got.IdleMinutes != 60 || spec.Settings.IdleTimeout() != time.Hour {
		t.Errorf("idle = %d minutes, want 60 by default", got.IdleMinutes)
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

// machineDoor is a machine's front door as far as donkey-server asks it:
// it answers its control path, to the control plane's credential only.
type machineDoor struct {
	*httptest.Server
	mu sync.Mutex
	// seen is every request that carried the right credential.
	seen []string
	// answer, when set, replaces the workspace the door reports.
	answer func(w http.ResponseWriter, r *http.Request)
}

func newMachineDoor(t *testing.T, id string) *machineDoor {
	t.Helper()
	door := &machineDoor{}
	door.Server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get(config.MachineControlHeader) != config.MachineControlToken(testSessionKey, id) {
			http.Error(w, "This is for donkey itself.", http.StatusForbidden)
			return
		}
		body, _ := io.ReadAll(r.Body)
		door.mu.Lock()
		door.seen = append(door.seen, strings.TrimSpace(r.Method+" "+r.URL.Path+" "+string(body)))
		answer := door.answer
		door.mu.Unlock()
		if answer != nil {
			answer(w, r)
			return
		}
		writeJSON(w, http.StatusOK, machine.WorkspaceStatus{Open: "shop", Ready: true, Folders: []machine.Folder{{Name: "shop"}, {Name: "site"}}})
	}))
	t.Cleanup(door.Close)
	return door
}

func workspaceBody(t *testing.T, rec *httptest.ResponseRecorder) machine.WorkspaceStatus {
	t.Helper()
	var out machine.WorkspaceStatus
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatalf("not a workspace: %s", rec.Body.String())
	}
	return out
}

// The folders are on the machine's disk, so the machine answers for them:
// donkey-server asks its front door, as itself, for whoever is signed in.
func TestMachineWorkspace(t *testing.T) {
	s, machines := machineTestServer()
	id := config.MachineID(machineOwner.Subject)
	door := newMachineDoor(t, id)
	machines.door = door.URL

	// Nothing to ask while it is not running.
	rec := machineCall(t, s, machineOwner, "GET", "/v1/machine/workspace")
	if rec.Code != http.StatusConflict || !strings.Contains(rec.Body.String(), "isn't running") {
		t.Fatalf("before a start: %d %s", rec.Code, rec.Body.String())
	}
	machineCall(t, s, machineOwner, "POST", "/v1/machine")

	rec = machineCall(t, s, machineOwner, "GET", "/v1/machine/workspace")
	if got := workspaceBody(t, rec); rec.Code != 200 || got.Open != "shop" || !got.Ready || len(got.Folders) != 2 {
		t.Fatalf("workspace: %d %s", rec.Code, rec.Body.String())
	}

	// Opening hands the request on as it came.
	open := []byte(`{"folder":"api","source":{"space":"team","app":"api","commit":"0123456789abcdef0123456789abcdef01234567"}}`)
	rec = machineSend(t, s, machineOwner, "POST", "/v1/machine/workspace", open)
	if rec.Code != 200 || workspaceBody(t, rec).Open != "shop" {
		t.Fatalf("open: %d %s", rec.Code, rec.Body.String())
	}
	want := []string{
		"GET /.donkey/control/workspace",
		`POST /.donkey/control/workspace {"folder":"api","source":{"space":"team","app":"api","commit":"0123456789abcdef0123456789abcdef01234567"}}`,
	}
	if got := strings.Join(door.seen, "\n"); got != strings.Join(want, "\n") {
		t.Fatalf("the door saw:\n%s", got)
	}

	// What the machine refuses reaches the console in the machine's words —
	// and never as a 401, which the console reads as its own sign-in ending.
	for status, relayed := range map[int]int{
		http.StatusConflict:            http.StatusConflict,
		http.StatusNotFound:            http.StatusNotFound,
		http.StatusForbidden:           http.StatusForbidden,
		http.StatusUnauthorized:        http.StatusBadGateway,
		http.StatusInternalServerError: http.StatusBadGateway,
	} {
		door.answer = func(w http.ResponseWriter, r *http.Request) {
			writeJSON(w, status, map[string]string{"error": "the machine says no"})
		}
		rec = machineSend(t, s, machineOwner, "POST", "/v1/machine/workspace", []byte(`{"folder":"shop","create":true}`))
		if rec.Code != relayed || !strings.Contains(rec.Body.String(), "the machine says no") {
			t.Errorf("a %d from the machine reached the console as %d %s", status, rec.Code, rec.Body.String())
		}
	}
	door.answer = nil

	rec = machineSend(t, s, machineOwner, "POST", "/v1/machine/workspace", []byte(`not json`))
	if rec.Code != http.StatusBadRequest {
		t.Errorf("a request that is not JSON: %d", rec.Code)
	}

	// A machine that is gone, or was started by a donkey that could not do
	// this, is said to be so.
	machines.door = "http://127.0.0.1:1"
	rec = machineCall(t, s, machineOwner, "GET", "/v1/machine/workspace")
	if rec.Code != http.StatusBadGateway || !strings.Contains(rec.Body.String(), "isn't answering") {
		t.Errorf("an unreachable machine: %d %s", rec.Code, rec.Body.String())
	}
	old := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "This machine only answers through its donkey address.", http.StatusForbidden)
	}))
	defer old.Close()
	machines.door = old.URL
	rec = machineCall(t, s, machineOwner, "GET", "/v1/machine/workspace")
	if rec.Code != http.StatusConflict || !strings.Contains(rec.Body.String(), "restart it") {
		t.Errorf("a machine from before: %d %s", rec.Code, rec.Body.String())
	}
}

// Each person asks their own machine, with a credential that opens that one
// alone — and people who may not have a machine ask nothing.
func TestMachineWorkspaceIsOnesOwn(t *testing.T) {
	s, machines := machineTestServer()
	door := newMachineDoor(t, config.MachineID(machineOwner.Subject))
	machines.door = door.URL
	machineCall(t, s, machineOwner, "POST", "/v1/machine")
	machineCall(t, s, machineAdmin, "POST", "/v1/machine")

	// The admin's machine answers at the same address in this test, but it
	// is asked with the admin's machine's credential, which this door — the
	// owner's — does not take.
	rec := machineCall(t, s, machineAdmin, "GET", "/v1/machine/workspace")
	if rec.Code == 200 || len(door.seen) != 0 {
		t.Fatalf("one person's request opened another's machine: %d %s", rec.Code, rec.Body.String())
	}
	rec = machineCall(t, s, machineStranger, "GET", "/v1/machine/workspace")
	if rec.Code != http.StatusForbidden {
		t.Fatalf("someone without a machine: %d", rec.Code)
	}
	rec = machineSend(t, s, machineStranger, "POST", "/v1/machine/workspace", []byte(`{"folder":"shop","create":true}`))
	if rec.Code != http.StatusForbidden || len(door.seen) != 0 {
		t.Fatalf("someone without a machine opened a folder: %d", rec.Code)
	}
}

func TestMachineIdleSetting(t *testing.T) {
	s, _ := machineTestServer()
	s.cfg.Machines.Idle = "never"
	machineCall(t, s, machineOwner, "POST", "/v1/machine")
	if got := machineBody(t, machineCall(t, s, machineOwner, "GET", "/v1/machine")); got.IdleMinutes != 0 {
		t.Fatalf("idle = %d minutes on an installation that never stops machines", got.IdleMinutes)
	}
	s.cfg.Machines.Idle = "90m"
	if got := machineBody(t, machineCall(t, s, machineOwner, "GET", "/v1/machine")); got.IdleMinutes != 90 {
		t.Fatalf("idle = %d minutes, want 90", got.IdleMinutes)
	}
}

// With machines that stop by themselves, a stopped machine is how its owner
// usually finds it. What its disk held is kept with the disk while it runs,
// so the console can offer the folders before anything is started.
func TestMachineRemembersItsFolders(t *testing.T) {
	s, machines := machineTestServer()
	id := config.MachineID(machineOwner.Subject)
	door := newMachineDoor(t, id)
	machines.door = door.URL
	used := time.Date(2026, 11, 13, 9, 30, 0, 0, time.UTC)
	answer := func(folders ...machine.Folder) {
		door.mu.Lock()
		defer door.mu.Unlock()
		door.answer = func(w http.ResponseWriter, r *http.Request) {
			writeJSON(w, http.StatusOK, machine.WorkspaceStatus{Open: folders[0].Name, Ready: true, Folders: folders})
		}
	}
	shop, site := machine.Folder{Name: "shop", Modified: used}, machine.Folder{Name: "site", Modified: used.Add(-48 * time.Hour)}

	machineCall(t, s, machineOwner, "POST", "/v1/machine")
	// A running machine is asked itself; nothing remembered is passed off as
	// what it holds.
	if got := machineBody(t, machineCall(t, s, machineOwner, "GET", "/v1/machine")); len(got.Folders) != 0 {
		t.Fatalf("a running machine reports remembered folders: %+v", got.Folders)
	}

	// The console asks every few seconds: the list is kept once, not each time.
	answer(shop, site)
	for range 3 {
		if rec := machineCall(t, s, machineOwner, "GET", "/v1/machine/workspace"); rec.Code != 200 {
			t.Fatalf("workspace: %d %s", rec.Code, rec.Body.String())
		}
	}
	if machines.remembered != 1 {
		t.Fatalf("an unchanged list was kept %d times", machines.remembered)
	}
	// A minute's difference is the same list; another hour, another order or
	// another name is not.
	shop.Modified = used.Add(time.Minute)
	answer(shop, site)
	machineCall(t, s, machineOwner, "GET", "/v1/machine/workspace")
	if machines.remembered != 1 {
		t.Fatalf("a minute's difference was kept again (%d)", machines.remembered)
	}
	site.Modified = used.Add(3 * time.Hour)
	answer(site, shop)
	machineCall(t, s, machineOwner, "GET", "/v1/machine/workspace")
	if machines.remembered != 2 {
		t.Fatalf("a changed order was kept %d times, want 2", machines.remembered)
	}
	// Opening a folder says what the workspace holds now, too.
	api := machine.Folder{Name: "api", Modified: used.Add(4 * time.Hour)}
	answer(api, site, shop)
	if rec := machineSend(t, s, machineOwner, "POST", "/v1/machine/workspace", []byte(`{"folder":"api","create":true}`)); rec.Code != 200 {
		t.Fatalf("open: %d %s", rec.Code, rec.Body.String())
	}
	if machines.remembered != 3 {
		t.Fatalf("after opening a new folder the list was kept %d times, want 3", machines.remembered)
	}

	// Stopped, the machine offers what it held.
	stopped := machineBody(t, machineCall(t, s, machineOwner, "POST", "/v1/machine/stop"))
	if stopped.State != "stopped" || len(stopped.Folders) != 3 ||
		stopped.Folders[0].Name != "api" || stopped.Folders[1].Name != "site" || stopped.Folders[2].Name != "shop" {
		t.Fatalf("a stopped machine offers %+v", stopped.Folders)
	}
	// A machine deleted and made again stands on a new disk. The same list,
	// said again, is not on it yet — and is kept again.
	machineCall(t, s, machineOwner, "DELETE", "/v1/machine")
	machineCall(t, s, machineOwner, "POST", "/v1/machine")
	machineCall(t, s, machineOwner, "GET", "/v1/machine/workspace")
	if machines.remembered != 4 {
		t.Fatalf("on a new disk the same list was kept %d times in all, want 4", machines.remembered)
	}
	again := machineBody(t, machineCall(t, s, machineOwner, "POST", "/v1/machine/stop"))
	if len(again.Folders) != 3 {
		t.Fatalf("the remade machine offers %+v", again.Folders)
	}
	// Each person's note is their own.
	machineCall(t, s, machineAdmin, "POST", "/v1/machine")
	if got := machineBody(t, machineCall(t, s, machineAdmin, "POST", "/v1/machine/stop")); len(got.Folders) != 0 {
		t.Fatalf("one person was offered another's folders: %+v", got.Folders)
	}
}
