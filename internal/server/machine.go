package server

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"net/http"
	"strconv"
	"time"

	"github.com/adrianliechti/donkey/internal/config"
	"github.com/adrianliechti/donkey/internal/identity"
	"github.com/adrianliechti/donkey/pkg/runtime"
)

// A machine is one person's development environment: a container with the
// usual toolchains, the donkey CLI signed in as its owner and a coding agent,
// with a disk that outlives it. Everything here acts on the caller's own
// machine — its name is derived from who is asking, so there is no path
// value to get wrong and no way to address somebody else's.

// MachineResponse is a person's machine as the console and CLI see it.
type MachineResponse struct {
	// Enabled says the installation offers machines at all; Allowed that
	// this person may have one.
	Enabled bool `json:"enabled"`
	Allowed bool `json:"allowed"`
	// State is absent, stopped, starting, running or failed.
	State string `json:"state"`
	// URL is where the machine's own interface answers while it runs: the
	// installation's one address for machines, which leads each person to
	// their own.
	URL string `json:"url,omitempty"`
	// StartedAt is when the running container came up (RFC 3339).
	StartedAt string `json:"startedAt,omitempty"`
	Restarts  int32  `json:"restarts,omitempty"`
	// Message explains a machine that is slow to start or failing.
	Message string `json:"message,omitempty"`
	// SignInExpires is when the donkey sign-in inside the machine runs out
	// (RFC 3339). Starting the machine again renews it.
	SignInExpires string `json:"signInExpires,omitempty"`
	// Outdated says the installation's machine image — or the address
	// machines answer on — changed since this one started; a restart picks
	// the new one up.
	Outdated bool `json:"outdated,omitempty"`
	// Egress is restricted or open: whether a machine reaches the control
	// plane only, or the network at large.
	Egress string `json:"egress,omitempty"`
	// IdleMinutes is how long a machine may go unused before it is stopped;
	// absent where machines run until their owners stop them.
	IdleMinutes int `json:"idleMinutes,omitempty"`
	// Folders is what a stopped machine's disk held when it last ran: what
	// it can offer to open before it is started. A running machine is asked
	// itself (GET /v1/machine/workspace), so it is absent then.
	Folders []runtime.MachineFolder `json:"folders,omitempty"`
	// What every machine of this installation gets.
	Storage string `json:"storage,omitempty"`
	CPU     string `json:"cpu,omitempty"`
	Memory  string `json:"memory,omitempty"`
}

// errMachinesOff and errMachineForbidden are the two reasons a person has no
// machine to act on.
var (
	errMachinesOff      = errors.New("this installation has no machines — an administrator turns them on with machines.image in the install settings")
	errMachineForbidden = errors.New("machines are for people who build here — ask a space admin to make you a developer of a space")
)

// machineLockSpace keys the per-machine lock. It is not a space name anybody
// can have, so it never contends with an app's lock.
const machineLockSpace = "@machine"

// machineStartTimeout bounds one start. On Docker it includes pulling the
// image, which is a few gigabytes of toolchains the first time.
const machineStartTimeout = 15 * time.Minute

func (s *Server) machines() (runtime.Machines, error) {
	if !s.cfg.Machines.Enabled() {
		return nil, errMachinesOff
	}
	provider, ok := s.rt.(runtime.MachineProvider)
	if !ok {
		return nil, fmt.Errorf("this installation's runtime cannot run machines")
	}
	return provider.Machines(), nil
}

// mayUseMachines: platform administrators, and anyone who is an admin or
// developer of at least one space. A machine costs compute, disk and model
// calls, so it is for the people who deploy here — not for everyone the
// identity provider admits, and not for those who may only look.
func (s *Server) mayUseMachines(ctx context.Context) (bool, error) {
	if s.platformAdmin(ctx) {
		return true, nil
	}
	id := requestIdentity(ctx)
	if id == nil {
		return false, nil
	}
	spaces, err := s.listSpaceConfigs(ctx)
	if err != nil {
		return false, err
	}
	for _, policy := range spaces {
		if role := policy.Role(id); role == config.SpaceAdmin || role == config.SpaceDeveloper {
			return true, nil
		}
	}
	return false, nil
}

// machine resolves the caller's machine, or answers why there is none.
func (s *Server) machine(w http.ResponseWriter, r *http.Request) (runtime.Machines, string, bool) {
	machines, err := s.machines()
	if err != nil {
		writeErr(w, http.StatusNotFound, err)
		return nil, "", false
	}
	allowed, err := s.mayUseMachines(r.Context())
	if err != nil {
		writeErr(w, http.StatusBadGateway, err)
		return nil, "", false
	}
	if !allowed {
		writeErr(w, http.StatusForbidden, errMachineForbidden)
		return nil, "", false
	}
	return machines, config.MachineID(requestIdentity(r.Context()).Subject), true
}

func (s *Server) machineResponse(st *runtime.MachineStatus) MachineResponse {
	settings := s.cfg.Machines
	resp := MachineResponse{
		Enabled: true, Allowed: true,
		State:       string(st.State),
		Egress:      settings.EgressMode(),
		IdleMinutes: int(settings.IdleTimeout().Minutes()),
		Storage:     settings.StorageSize(), CPU: settings.CPULimit(), Memory: settings.MemoryLimit(),
		Restarts: st.Restarts, Message: st.Message,
	}
	if resp.State == "" {
		resp.State = string(runtime.MachineAbsent)
	}
	switch st.State {
	case runtime.MachineRunning, runtime.MachineStarting, runtime.MachineFailed:
		// The address is the installation's, not the machine's: one started
		// when each machine still had a hostname of its own is not reachable
		// there, and says so the way one on an old image does.
		host := config.MachineHost(s.cfg.Domain)
		if st.Host != "" {
			resp.URL = s.cfg.Scheme() + "://" + host
		}
		resp.Outdated = st.Image != "" && st.Image != settings.Image || st.Host != "" && st.Host != host
		if !st.SignInExpires.IsZero() {
			resp.SignInExpires = st.SignInExpires.UTC().Format(time.RFC3339)
		}
	}
	if !st.StartedAt.IsZero() {
		resp.StartedAt = st.StartedAt.UTC().Format(time.RFC3339)
	}
	if st.State == runtime.MachineStopped {
		resp.Folders = st.Folders
	}
	return resp
}

// handleGetMachine answers for everyone who is signed in, including those
// with no machine to have: the console asks it to decide what to show.
func (s *Server) handleGetMachine(w http.ResponseWriter, r *http.Request) {
	machines, err := s.machines()
	if err != nil {
		writeJSON(w, http.StatusOK, MachineResponse{State: string(runtime.MachineAbsent)})
		return
	}
	allowed, err := s.mayUseMachines(r.Context())
	if err != nil {
		writeErr(w, http.StatusBadGateway, err)
		return
	}
	if !allowed {
		writeJSON(w, http.StatusOK, MachineResponse{Enabled: true, State: string(runtime.MachineAbsent)})
		return
	}
	st, err := machines.Status(r.Context(), config.MachineID(requestIdentity(r.Context()).Subject))
	if err != nil {
		writeErr(w, http.StatusBadGateway, err)
		return
	}
	writeJSON(w, http.StatusOK, s.machineResponse(st))
}

// handleStartMachine creates the caller's machine if there is none and starts
// it — or starts it again, which is how a running machine gets a fresh
// sign-in and a newer image. It answers once the runtime has taken the
// machine; the console follows its state from there.
func (s *Server) handleStartMachine(w http.ResponseWriter, r *http.Request) {
	machines, id, ok := s.machine(w, r)
	if !ok {
		return
	}
	owner := requestIdentity(r.Context())
	now := time.Now()
	signIn := machineSignIn(owner, now)
	if time.Until(time.Unix(signIn.Expires, 0)) < time.Hour {
		writeErr(w, http.StatusUnauthorized,
			fmt.Errorf("your own sign-in is about to expire, so the machine would lose its sign-in right away — sign in again, then start it"))
		return
	}
	token, err := identity.SealToken(s.cfg.SessionKey, signIn)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err)
		return
	}
	aiToken, err := machineSecret()
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err)
		return
	}
	spec := runtime.MachineSpec{
		ID:            id,
		Owner:         owner.Subject,
		Email:         owner.Email,
		Name:          owner.Name,
		Image:         s.cfg.Machines.Image,
		Host:          config.MachineHost(s.cfg.Domain),
		Token:         token,
		SignInExpires: time.Unix(signIn.Expires, 0),
		GuardToken:    config.MachineGuardToken(s.cfg.SessionKey, id),
		ControlToken:  config.MachineControlToken(s.cfg.SessionKey, id),
		AIToken:       aiToken,
		Model:         s.cfg.AIModel,
		Settings:      s.cfg.Machines,
	}
	defer s.lockApp(machineLockSpace, id)()
	// A start outlives the request that asked for it: closing the tab while
	// the image is still being pulled must not leave half a machine.
	ctx, cancel := context.WithTimeout(context.WithoutCancel(r.Context()), machineStartTimeout)
	defer cancel()
	if err := machines.Start(ctx, spec); err != nil {
		writeErr(w, http.StatusBadGateway, fmt.Errorf("the machine couldn't be started: %w", err))
		return
	}
	s.writeMachine(w, r, machines, id)
}

func (s *Server) handleStopMachine(w http.ResponseWriter, r *http.Request) {
	machines, id, ok := s.machine(w, r)
	if !ok {
		return
	}
	defer s.lockApp(machineLockSpace, id)()
	ctx, cancel := context.WithTimeout(context.WithoutCancel(r.Context()), time.Minute)
	defer cancel()
	if err := machines.Stop(ctx, id); err != nil {
		writeErr(w, http.StatusBadGateway, fmt.Errorf("the machine couldn't be stopped: %w", err))
		return
	}
	s.writeMachine(w, r, machines, id)
}

// handleDeleteMachine removes the machine and its disk. There is no keeping
// the disk: stopping is what keeps it.
func (s *Server) handleDeleteMachine(w http.ResponseWriter, r *http.Request) {
	machines, id, ok := s.machine(w, r)
	if !ok {
		return
	}
	defer s.lockApp(machineLockSpace, id)()
	ctx, cancel := context.WithTimeout(context.WithoutCancel(r.Context()), time.Minute)
	defer cancel()
	if err := machines.Delete(ctx, id); err != nil {
		writeErr(w, http.StatusBadGateway, fmt.Errorf("the machine couldn't be deleted: %w", err))
		return
	}
	s.writeMachine(w, r, machines, id)
}

func (s *Server) writeMachine(w http.ResponseWriter, r *http.Request, machines runtime.Machines, id string) {
	st, err := machines.Status(context.WithoutCancel(r.Context()), id)
	if err != nil {
		writeErr(w, http.StatusBadGateway, err)
		return
	}
	writeJSON(w, http.StatusOK, s.machineResponse(st))
}

func (s *Server) handleMachineLogs(w http.ResponseWriter, r *http.Request) {
	machines, id, ok := s.machine(w, r)
	if !ok {
		return
	}
	q := r.URL.Query()
	// The same reading of the query as an app's logs.
	tail, perr := strconv.ParseInt(q.Get("tail"), 10, 64)
	if q.Get("tail") == "" || perr != nil || tail < 0 {
		tail = 200
	}
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	w.WriteHeader(http.StatusOK)
	err := machines.Logs(r.Context(), id, runtime.LogOptions{
		Follow:    q.Get("follow") == "true",
		TailLines: tail,
	}, flushWriter{w})
	if err != nil && r.Context().Err() == nil {
		fmt.Fprintf(w, "\nerror: %v\n", err)
	}
}

// machineSignIn is the identity a machine's CLI signs in with: its owner's,
// as the IdP described them at the login this request descends from.
//
// It expires when a `donkey login` at that same moment would have. Counting
// from now instead would let a sign-in renew itself without the IdP — start a
// machine, read its token, start it again with that — and sealed tokens
// cannot be revoked one by one. A sign-in older than the field counts from
// now; those are gone within a token's lifetime.
//
// The IdP's own tokens stay out: the machine runs what an agent is told to
// run, and gets what it needs to deploy, nothing to call the IdP with.
func machineSignIn(owner *identity.Identity, now time.Time) identity.Identity {
	from := now
	if owner.LoginTime > 0 && owner.LoginTime <= now.Unix() {
		from = time.Unix(owner.LoginTime, 0)
	}
	return identity.Identity{
		Subject:           owner.Subject,
		Email:             owner.Email,
		EmailVerified:     owner.EmailVerified,
		Name:              owner.Name,
		PreferredUsername: owner.PreferredUsername,
		ObjectID:          owner.ObjectID,
		Groups:            owner.Groups,
		AuthTime:          owner.AuthTime,
		LoginTime:         owner.LoginTime,
		Expires:           from.Add(identity.TokenTTL).Unix(),
	}
}

// machineSecret is a fresh stand-in token: random, meaningless anywhere but
// at this installation's AI endpoint, and gone when the machine stops.
func machineSecret() (string, error) {
	buf := make([]byte, 32)
	if _, err := rand.Read(buf); err != nil {
		return "", err
	}
	return "dkm_" + hex.EncodeToString(buf), nil
}
