package machine

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"net"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/adrianliechti/donkey/internal/config"
	"github.com/adrianliechti/donkey/pkg/runtime"
)

// A machine's disk holds a workspace of folders, one per thing its owner
// works on, and the agent has one of them open at a time. The folder is the
// agent's whole view: its files, its sessions, its terminals start there.
//
// The front door owns the agent's process for that reason. Opening another
// folder ends the agent and starts it again somewhere else, without the
// container, its disk or its address going anywhere — seconds, where a
// restart of the machine is most of a minute.

// Folder is one folder of the workspace. The control plane keeps the same
// list with the disk of a machine that is stopped (runtime.Machines.Remember).
type Folder = runtime.MachineFolder

// Source is one version of an app's source, as the control plane's history
// of that app has it.
type Source struct {
	Space  string `json:"space"`
	App    string `json:"app"`
	Commit string `json:"commit"`
}

// OpenRequest names the folder to open. Create and Source both make it first,
// and refuse a name that is taken: nothing here ever writes into a folder
// that already holds somebody's work.
type OpenRequest struct {
	Folder string `json:"folder"`
	// Create makes an empty folder.
	Create bool `json:"create,omitempty"`
	// Source makes the folder from a version of an app's source.
	Source *Source `json:"source,omitempty"`
}

// WorkspaceStatus is the workspace as the console shows it.
type WorkspaceStatus struct {
	// Open is the folder the agent has open; empty while it has none.
	Open string `json:"open,omitempty"`
	// Ready says the agent is answering in it.
	Ready bool `json:"ready,omitempty"`
	// Message says why nothing is open when something was.
	Message string   `json:"message,omitempty"`
	Folders []Folder `json:"folders"`
}

// Refusal is a request the machine will not carry out, in words for its
// owner. Status is what the control plane answers the console with.
type Refusal struct {
	Status  int
	Message string
}

func (r *Refusal) Error() string { return r.Message }

func refuse(status int, format string, a ...any) error {
	return &Refusal{Status: status, Message: fmt.Sprintf(format, a...)}
}

// partialPrefix marks a folder that is still being fetched. It is hidden, so
// it is never listed or opened, and what a killed machine left behind is
// swept away on the next start.
const partialPrefix = ".fetching-"

// Workspace is the folders of one machine and the agent that has one open.
type Workspace struct {
	// Root is where the folders live.
	Root string
	// Command starts the agent. It runs in the folder that is opened, with
	// Env, and listens on Agent.
	Command []string
	Env     []string
	Agent   *url.URL
	// Fetch fills a new folder with a version of an app's source.
	Fetch func(ctx context.Context, source Source, dir string) error
	// Used is told when the owner does something with the workspace.
	Used func()
	Log  io.Writer
	// Stdout and Stderr are the agent's (default: the machine's own).
	Stdout, Stderr io.Writer

	// changing admits one change at a time; mu guards what Status reads, so a
	// folder that takes a while to fetch never holds up a question.
	changing sync.Mutex
	mu       sync.Mutex
	open     string
	agent    *agentProcess
	message  string
}

type agentProcess struct {
	cmd *exec.Cmd
	// done closes once the process is gone.
	done chan struct{}
}

// Folders lists the workspace, the folder used last first.
func (w *Workspace) Folders() ([]Folder, error) {
	entries, err := os.ReadDir(w.Root)
	if err != nil {
		return nil, err
	}
	folders := []Folder{}
	for _, entry := range entries {
		if !entry.IsDir() || !config.SafeMachineFolder(entry.Name()) {
			continue
		}
		info, err := entry.Info()
		if err != nil {
			continue
		}
		folders = append(folders, Folder{Name: entry.Name(), Modified: info.ModTime().UTC()})
	}
	slices.SortFunc(folders, func(a, b Folder) int {
		if c := b.Modified.Compare(a.Modified); c != 0 {
			return c
		}
		return strings.Compare(a.Name, b.Name)
	})
	return folders, nil
}

// Status is the workspace right now.
func (w *Workspace) Status() WorkspaceStatus {
	w.mu.Lock()
	status := WorkspaceStatus{Open: w.open, Message: w.message}
	w.mu.Unlock()
	if status.Open != "" {
		status.Ready = w.answering()
	}
	folders, err := w.Folders()
	if err != nil {
		folders = []Folder{}
		if status.Message == "" {
			status.Message = "The machine's workspace could not be read: " + err.Error()
		}
	}
	status.Folders = folders
	return status
}

func (w *Workspace) answering() bool {
	conn, err := net.DialTimeout("tcp", w.Agent.Host, time.Second)
	if err != nil {
		return false
	}
	_ = conn.Close()
	return true
}

// Open makes the agent work in a folder: one that is there, a new empty one,
// or a new one made from an app's source. What was open closes — the agent,
// its turns and its terminals — but only once the new folder is settled, so
// a name that is taken or a source that cannot be fetched leaves the owner
// where they were.
func (w *Workspace) Open(ctx context.Context, req OpenRequest) error {
	fresh := req.Create || req.Source != nil
	switch {
	case fresh && !config.ValidMachineFolder(req.Folder):
		return refuse(http.StatusBadRequest,
			"a folder name is made of letters, digits, dots, dashes and underscores, and starts with a letter or a digit — %q is not one", req.Folder)
	case !config.SafeMachineFolder(req.Folder):
		return refuse(http.StatusBadRequest, "%q is not a folder of this machine's workspace", req.Folder)
	}
	w.changing.Lock()
	defer w.changing.Unlock()

	dir := filepath.Join(w.Root, req.Folder)
	info, err := os.Lstat(dir)
	switch {
	case err != nil && !errors.Is(err, fs.ErrNotExist):
		return err
	case fresh && err == nil:
		return refuse(http.StatusConflict, "there already is a folder named %s — open that one, or choose another name", req.Folder)
	case !fresh && (err != nil || !info.IsDir()):
		return refuse(http.StatusNotFound, "there is no folder named %s in this machine", req.Folder)
	}
	if fresh {
		if err := w.create(ctx, dir, req.Source); err != nil {
			return err
		}
	} else if w.running(req.Folder) {
		// Asked twice, or by a console catching up: the agent keeps going.
		return nil
	}
	w.stop()
	return w.start(req.Folder, dir)
}

// create makes a folder that is complete or not there at all: source is
// fetched beside it and moved into place only once all of it has arrived.
func (w *Workspace) create(ctx context.Context, dir string, source *Source) error {
	if source == nil {
		return os.Mkdir(dir, 0o750)
	}
	if w.Fetch == nil {
		return refuse(http.StatusConflict, "this machine cannot fetch an app's source")
	}
	partial, err := os.MkdirTemp(w.Root, partialPrefix)
	if err != nil {
		return err
	}
	defer os.RemoveAll(partial)
	if err := w.Fetch(ctx, *source, partial); err != nil {
		return err
	}
	if err := os.Chmod(partial, 0o750); err != nil {
		return err
	}
	return os.Rename(partial, dir)
}

func (w *Workspace) running(folder string) bool {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.open == folder && w.agent != nil
}

func (w *Workspace) start(folder, dir string) error {
	cmd := exec.Command(w.Command[0], w.Command[1:]...)
	cmd.Dir = dir
	cmd.Env = w.Env
	cmd.Stdout, cmd.Stderr = w.Stdout, w.Stderr
	if cmd.Stdout == nil {
		cmd.Stdout = os.Stdout
	}
	if cmd.Stderr == nil {
		cmd.Stderr = os.Stderr
	}
	ownGroup(cmd)
	if err := cmd.Start(); err != nil {
		return fmt.Errorf("the agent couldn't be started (%s): %w", strings.Join(w.Command, " "), err)
	}
	agent := &agentProcess{cmd: cmd, done: make(chan struct{})}
	w.mu.Lock()
	w.open, w.agent, w.message = folder, agent, ""
	w.mu.Unlock()
	// Opening counts as using: the list shows this folder first next time.
	now := time.Now()
	_ = os.Chtimes(dir, now, now)
	w.logf("donkey machine: %s is open", folder)

	go func() {
		err := cmd.Wait()
		w.mu.Lock()
		// Still the agent of record: nobody asked it to stop. The folder is
		// closed rather than the agent started again — whatever ended it would
		// end the next one too, and the owner reads why instead of a loop.
		if w.agent == agent {
			w.open, w.agent = "", nil
			w.message = fmt.Sprintf("The agent stopped on its own in %s (%s). Opening the folder starts it again.", folder, exitText(err))
			w.logf("donkey machine: the agent stopped on its own in %s: %s", folder, exitText(err))
		}
		w.mu.Unlock()
		close(agent.done)
	}()
	return nil
}

// stop ends the agent and what it started, giving it the chance to save what
// it is writing.
func (w *Workspace) stop() {
	w.mu.Lock()
	agent := w.agent
	w.open, w.agent = "", nil
	w.mu.Unlock()
	if agent == nil {
		return
	}
	signalGroup(agent.cmd, syscall.SIGTERM)
	select {
	case <-agent.done:
	case <-time.After(agentStopGrace):
	}
	// Whether it went by itself or not: nothing of the old folder stays
	// behind to hold a port or a file of the next one.
	signalGroup(agent.cmd, syscall.SIGKILL)
	<-agent.done
}

// Close ends the agent for good, when the machine itself stops.
func (w *Workspace) Close() {
	w.changing.Lock()
	defer w.changing.Unlock()
	w.stop()
}

// sweep removes what an interrupted fetch left in the workspace.
func (w *Workspace) sweep() {
	entries, err := os.ReadDir(w.Root)
	if err != nil {
		return
	}
	for _, entry := range entries {
		if strings.HasPrefix(entry.Name(), partialPrefix) {
			_ = os.RemoveAll(filepath.Join(w.Root, entry.Name()))
		}
	}
}

func (w *Workspace) logf(format string, a ...any) {
	if w.Log != nil {
		fmt.Fprintf(w.Log, format+"\n", a...)
	}
}

func exitText(err error) string {
	if err == nil {
		return "it exited"
	}
	return err.Error()
}

// ControlPrefix is where the front door answers the control plane itself:
// donkey-server, with its own credential, on behalf of the machine's owner in
// the console. Nothing under it is reachable through the machine's address.
const ControlPrefix = "/.donkey/control/"

// Handler is the workspace as donkey-server drives it.
func (w *Workspace) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET "+ControlPrefix+"workspace", func(rw http.ResponseWriter, r *http.Request) {
		writeControl(rw, http.StatusOK, w.Status())
	})
	mux.HandleFunc("POST "+ControlPrefix+"workspace", func(rw http.ResponseWriter, r *http.Request) {
		var req OpenRequest
		if err := json.NewDecoder(io.LimitReader(r.Body, 64<<10)).Decode(&req); err != nil {
			writeControl(rw, http.StatusBadRequest, map[string]string{"error": "the request is not an open request: " + err.Error()})
			return
		}
		// Asking for a folder is using the machine, however long the folder
		// then takes to arrive.
		if w.Used != nil {
			w.Used()
		}
		if err := w.Open(r.Context(), req); err != nil {
			status := http.StatusInternalServerError
			var refusal *Refusal
			if errors.As(err, &refusal) {
				status = refusal.Status
			}
			writeControl(rw, status, map[string]string{"error": err.Error()})
			return
		}
		writeControl(rw, http.StatusOK, w.Status())
	})
	return mux
}

func writeControl(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}
