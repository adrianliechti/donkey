package server

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"strings"
	"time"

	"github.com/adrianliechti/donkey/internal/config"
	"github.com/adrianliechti/donkey/internal/machine"
	"github.com/adrianliechti/donkey/pkg/runtime"
)

// A machine's workspace: the folders on its disk, and which of them its
// agent has open. The disk is the machine's, so the machine answers — its
// front door, asked by this process with a credential of its own
// (config.MachineControlToken) on behalf of the owner who is signed in to
// the console. Nothing here goes through the orchestrator: a folder opens in
// seconds, and the machine is the one doing the work.

const (
	// machineAskTimeout bounds a question the door answers from memory.
	machineAskTimeout = 5 * time.Second
	// machineOpenTimeout bounds opening a folder, which for an app's source
	// includes fetching and unpacking it.
	machineOpenTimeout = 3 * time.Minute
)

// machineTransport keeps the hop to a machine off any forward proxy the
// installation configured, like every other hop inside the control plane.
var machineTransport http.RoundTripper = &http.Transport{
	Proxy:               nil,
	DialContext:         (&net.Dialer{Timeout: 5 * time.Second, KeepAlive: 30 * time.Second}).DialContext,
	MaxIdleConnsPerHost: 2,
	IdleConnTimeout:     30 * time.Second,
}

// machineError is the machine's own answer to a request it will not carry
// out, relayed to the console as it is.
type machineError struct {
	status  int
	message string
}

func (e *machineError) Error() string { return e.message }

func writeMachineErr(w http.ResponseWriter, err error) {
	var refused *machineError
	if errors.As(err, &refused) {
		writeErr(w, refused.status, refused)
		return
	}
	writeErr(w, http.StatusBadGateway, err)
}

// runningMachine resolves the caller's machine for a request that needs it
// to be up, and answers when it is not.
func (s *Server) runningMachine(w http.ResponseWriter, r *http.Request) (machines runtime.Machines, id string, st *runtime.MachineStatus, ok bool) {
	machines, id, ok = s.machine(w, r)
	if !ok {
		return nil, "", nil, false
	}
	st, err := machines.Status(r.Context(), id)
	if err != nil {
		writeErr(w, http.StatusBadGateway, err)
		return nil, "", nil, false
	}
	if st.State != runtime.MachineRunning || st.Backend == "" {
		writeErr(w, http.StatusConflict, fmt.Errorf("your machine isn't running — start it first"))
		return nil, "", nil, false
	}
	return machines, id, st, true
}

// rememberMachineFolders keeps what a running machine just said about its
// folders with its disk, so the console can offer them once the machine is
// stopped — which, with machines that stop by themselves, is how their owner
// usually finds them.
//
// The console asks for the folders every few seconds while its page is open,
// so only a list that differs from the one this process kept last is
// written: other names, another order, or a time that moved into another
// hour. What was kept counts for the container that said it only — a
// machine started again may stand on another disk, a deleted and remade
// one certainly does, and a list that reads the same is still not on it.
// Keeping the note is a convenience and never fails the request.
func (s *Server) rememberMachineFolders(ctx context.Context, machines runtime.Machines, id string, started time.Time, folders []runtime.MachineFolder) {
	var digest strings.Builder
	fmt.Fprintf(&digest, "%d\x00", started.UnixNano())
	for _, folder := range folders {
		fmt.Fprintf(&digest, "%s\x00%d\x00", folder.Name, folder.Modified.Truncate(time.Hour).Unix())
	}
	if last, ok := s.machineFolders.Load(id); ok && last == digest.String() {
		return
	}
	ctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), machineAskTimeout)
	defer cancel()
	if err := machines.Remember(ctx, id, folders); err != nil {
		log.Printf("machine %s: its folders could not be kept with its disk: %v", id, err)
		return
	}
	s.machineFolders.Store(id, digest.String())
}

// askMachine sends one control request to a machine's front door.
func (s *Server) askMachine(ctx context.Context, id, door, method string, in, out any) error {
	var body io.Reader
	if in != nil {
		data, err := json.Marshal(in)
		if err != nil {
			return err
		}
		body = bytes.NewReader(data)
	}
	req, err := http.NewRequestWithContext(ctx, method, strings.TrimSuffix(door, "/")+machine.ControlPrefix+"workspace", body)
	if err != nil {
		return err
	}
	req.Header.Set(config.MachineControlHeader, config.MachineControlToken(s.cfg.SessionKey, id))
	if in != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	res, err := (&http.Client{Transport: machineTransport}).Do(req)
	if err != nil {
		return fmt.Errorf("the machine isn't answering — it may still be starting, or just have stopped")
	}
	defer res.Body.Close()
	if res.StatusCode == http.StatusOK {
		return json.NewDecoder(io.LimitReader(res.Body, 4<<20)).Decode(out)
	}
	var answer struct {
		Error string `json:"error"`
	}
	_ = json.NewDecoder(io.LimitReader(res.Body, 64<<10)).Decode(&answer)
	switch {
	case answer.Error != "" && res.StatusCode >= 400 && res.StatusCode < 500 && res.StatusCode != http.StatusUnauthorized:
		return &machineError{status: res.StatusCode, message: answer.Error}
	case answer.Error != "":
		return &machineError{status: http.StatusBadGateway, message: answer.Error}
	case res.StatusCode == http.StatusForbidden || res.StatusCode == http.StatusNotFound:
		// A front door that does not know the control plane: the machine was
		// started before its image or this installation could do this.
		return &machineError{status: http.StatusConflict, message: "this machine was started before folders could be opened from here — restart it"}
	}
	return fmt.Errorf("the machine answered %s", res.Status)
}

// handleMachineWorkspace: GET /v1/machine/workspace — the folders of the
// caller's machine and the one that is open.
func (s *Server) handleMachineWorkspace(w http.ResponseWriter, r *http.Request) {
	machines, id, st, ok := s.runningMachine(w, r)
	if !ok {
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), machineAskTimeout)
	defer cancel()
	var status machine.WorkspaceStatus
	if err := s.askMachine(ctx, id, st.Backend, http.MethodGet, nil, &status); err != nil {
		writeMachineErr(w, err)
		return
	}
	s.rememberMachineFolders(r.Context(), machines, id, st.StartedAt, status.Folders)
	writeJSON(w, http.StatusOK, status)
}

// handleOpenMachineFolder: POST /v1/machine/workspace — open a folder of the
// caller's machine: one that is there, a new empty one, or a new one made
// from a version of an app's source.
//
// The source is fetched by the machine, with the sign-in it holds for its
// owner. So it is the API that decides, as for the owner's CLI, whether they
// may read that app — not this handler, and not the machine.
func (s *Server) handleOpenMachineFolder(w http.ResponseWriter, r *http.Request) {
	var req machine.OpenRequest
	if err := json.NewDecoder(io.LimitReader(r.Body, 64<<10)).Decode(&req); err != nil {
		writeErr(w, http.StatusBadRequest, fmt.Errorf("the request is not valid JSON: %w", err))
		return
	}
	machines, id, st, ok := s.runningMachine(w, r)
	if !ok {
		return
	}
	// Opening is quick to ask for and may take a moment to do; it is the
	// owner's request either way, so it ends when they stop waiting.
	ctx, cancel := context.WithTimeout(r.Context(), machineOpenTimeout)
	defer cancel()
	var status machine.WorkspaceStatus
	if err := s.askMachine(ctx, id, st.Backend, http.MethodPost, req, &status); err != nil {
		writeMachineErr(w, err)
		return
	}
	s.rememberMachineFolders(r.Context(), machines, id, st.StartedAt, status.Folders)
	writeJSON(w, http.StatusOK, status)
}
