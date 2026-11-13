package machine

import (
	"archive/zip"
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/adrianliechti/donkey/internal/config"
)

const (
	testControlToken = "the-control-planes-credential"
	testCommit       = "0123456789abcdef0123456789abcdef01234567"
)

// TestHelperAgent is not a test: it is the agent the tests below start, this
// test binary run again. It answers on the port it is given, from the folder
// it was started in, until it is told to stop.
func TestHelperAgent(t *testing.T) {
	port := os.Getenv("DONKEY_TEST_AGENT_PORT")
	if port == "" {
		return
	}
	if os.Getenv("DONKEY_TEST_AGENT_EXITS") != "" {
		os.Exit(3)
	}
	folder, _ := os.Getwd()
	listener, err := net.Listen("tcp", "127.0.0.1:"+port)
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(2)
	}
	_ = http.Serve(listener, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/env" {
			_, _ = w.Write([]byte(strings.Join(os.Environ(), "\n")))
			return
		}
		_, _ = w.Write([]byte(folder))
	}))
}

func freePort(t *testing.T) string {
	t.Helper()
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer listener.Close()
	return strconv.Itoa(listener.Addr().(*net.TCPAddr).Port)
}

// testWorkspace is a workspace on a temporary disk whose agent is the helper.
func testWorkspace(t *testing.T, env ...string) *Workspace {
	t.Helper()
	// The folder a temporary directory really is: the agent reports the one
	// it runs in, and on macOS /var is a link.
	root, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	port := freePort(t)
	w := &Workspace{
		Root:    root,
		Command: []string{os.Args[0], "-test.run=^TestHelperAgent$"},
		Env:     append(append(os.Environ(), "DONKEY_TEST_AGENT_PORT="+port), env...),
		Agent:   &url.URL{Scheme: "http", Host: "127.0.0.1:" + port},
		Log:     io.Discard,
		Stdout:  io.Discard,
		Stderr:  io.Discard,
	}
	t.Cleanup(w.Close)
	return w
}

// agentFolder waits for the agent to answer and says where it runs.
func agentFolder(t *testing.T, w *Workspace) string {
	t.Helper()
	for deadline := time.Now().Add(15 * time.Second); ; time.Sleep(20 * time.Millisecond) {
		if w.Status().Ready {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("the agent never answered: %+v", w.Status())
		}
	}
	res, err := http.Get(w.Agent.String())
	if err != nil {
		t.Fatal(err)
	}
	defer res.Body.Close()
	body, _ := io.ReadAll(res.Body)
	return string(body)
}

func refusal(t *testing.T, err error) *Refusal {
	t.Helper()
	var r *Refusal
	if !errors.As(err, &r) {
		t.Fatalf("want a refusal, got %v", err)
	}
	return r
}

// The agent works in one folder at a time, and opening another moves it —
// the machine around it stays as it is.
func TestWorkspaceOpensOneFolderAtATime(t *testing.T) {
	w := testWorkspace(t)
	ctx := context.Background()
	if st := w.Status(); st.Open != "" || st.Ready || len(st.Folders) != 0 {
		t.Fatalf("a fresh workspace: %+v", st)
	}

	if err := w.Open(ctx, OpenRequest{Folder: "shop", Create: true}); err != nil {
		t.Fatal(err)
	}
	if got := agentFolder(t, w); got != filepath.Join(w.Root, "shop") {
		t.Fatalf("the agent runs in %s", got)
	}
	first := w.agent

	// Asked again for what is open, the agent keeps going.
	if err := w.Open(ctx, OpenRequest{Folder: "shop"}); err != nil || w.agent != first {
		t.Fatalf("opening the open folder restarted the agent (%v)", err)
	}

	if err := w.Open(ctx, OpenRequest{Folder: "site", Create: true}); err != nil {
		t.Fatal(err)
	}
	if got := agentFolder(t, w); got != filepath.Join(w.Root, "site") {
		t.Fatalf("the agent runs in %s", got)
	}
	select {
	case <-first.done:
	default:
		t.Fatal("the agent of the folder that was open is still running")
	}
	if st := w.Status(); st.Open != "site" || len(st.Folders) != 2 {
		t.Fatalf("status = %+v", st)
	}

	// Back to the first: it is there, with what was in it.
	if err := os.WriteFile(filepath.Join(w.Root, "shop", "note"), []byte("mine"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := w.Open(ctx, OpenRequest{Folder: "shop"}); err != nil {
		t.Fatal(err)
	}
	if got := agentFolder(t, w); got != filepath.Join(w.Root, "shop") {
		t.Fatalf("the agent runs in %s", got)
	}
	if kept, _ := os.ReadFile(filepath.Join(w.Root, "shop", "note")); string(kept) != "mine" {
		t.Fatal("opening a folder touched what is in it")
	}

	w.Close()
	if st := w.Status(); st.Open != "" || st.Ready {
		t.Fatalf("after close: %+v", st)
	}
}

// What cannot be opened leaves the owner where they were.
func TestWorkspaceRefusals(t *testing.T) {
	w := testWorkspace(t)
	ctx := context.Background()
	if err := w.Open(ctx, OpenRequest{Folder: "shop", Create: true}); err != nil {
		t.Fatal(err)
	}
	agentFolder(t, w)
	running := w.agent
	if err := os.WriteFile(filepath.Join(w.Root, "a-file"), nil, 0o600); err != nil {
		t.Fatal(err)
	}

	for name, tc := range map[string]struct {
		req    OpenRequest
		status int
	}{
		"a folder that is not there":         {OpenRequest{Folder: "nowhere"}, http.StatusNotFound},
		"a file":                             {OpenRequest{Folder: "a-file"}, http.StatusNotFound},
		"a new folder whose name is taken":   {OpenRequest{Folder: "shop", Create: true}, http.StatusConflict},
		"source into a folder that is there": {OpenRequest{Folder: "shop", Source: &Source{Space: "team", App: "shop", Commit: testCommit}}, http.StatusConflict},
		"a way out of the workspace":         {OpenRequest{Folder: "../shop"}, http.StatusBadRequest},
		"a path":                             {OpenRequest{Folder: "shop/src"}, http.StatusBadRequest},
		"a hidden folder":                    {OpenRequest{Folder: ".git"}, http.StatusBadRequest},
		"no name":                            {OpenRequest{}, http.StatusBadRequest},
		"a new folder with a space":          {OpenRequest{Folder: "my shop", Create: true}, http.StatusBadRequest},
		"a new folder that is all dots":      {OpenRequest{Folder: "..", Create: true}, http.StatusBadRequest},
	} {
		if got := refusal(t, w.Open(ctx, tc.req)); got.Status != tc.status {
			t.Errorf("%s: %d %s, want %d", name, got.Status, got.Message, tc.status)
		}
	}
	if w.agent != running || w.Status().Open != "shop" {
		t.Fatal("a refused request closed the folder that was open")
	}

	// A folder the owner made in a terminal, with a name nobody checked, is
	// theirs to open all the same.
	if err := os.Mkdir(filepath.Join(w.Root, "My Shop (old)"), 0o750); err != nil {
		t.Fatal(err)
	}
	if err := w.Open(ctx, OpenRequest{Folder: "My Shop (old)"}); err != nil {
		t.Fatal(err)
	}
	if got := agentFolder(t, w); got != filepath.Join(w.Root, "My Shop (old)") {
		t.Fatalf("the agent runs in %s", got)
	}
}

// The list is the workspace's folders — not its files, not what is hidden —
// with the one used last first.
func TestWorkspaceFolders(t *testing.T) {
	w := testWorkspace(t)
	for _, name := range []string{"old", "new", ".cache", partialPrefix + "123"} {
		if err := os.Mkdir(filepath.Join(w.Root, name), 0o750); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.WriteFile(filepath.Join(w.Root, "notes.txt"), nil, 0o600); err != nil {
		t.Fatal(err)
	}
	past := time.Now().Add(-48 * time.Hour)
	if err := os.Chtimes(filepath.Join(w.Root, "old"), past, past); err != nil {
		t.Fatal(err)
	}
	names := func() string {
		folders, err := w.Folders()
		if err != nil {
			t.Fatal(err)
		}
		var out []string
		for _, f := range folders {
			out = append(out, f.Name)
		}
		return strings.Join(out, ",")
	}
	if got := names(); got != "new,old" {
		t.Fatalf("folders = %s", got)
	}
	// Opening one is using it.
	if err := w.Open(context.Background(), OpenRequest{Folder: "old"}); err != nil {
		t.Fatal(err)
	}
	if got := names(); got != "old,new" {
		t.Fatalf("after opening old: %s", got)
	}
	// What an interrupted fetch left behind goes with the next start.
	w.sweep()
	if _, err := os.Stat(filepath.Join(w.Root, partialPrefix+"123")); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("a half-fetched folder survived the sweep")
	}
}

// An agent that ends by itself is not started again in a loop: the folder
// closes and the owner reads why.
func TestWorkspaceSaysWhenTheAgentStops(t *testing.T) {
	w := testWorkspace(t, "DONKEY_TEST_AGENT_EXITS=1")
	if err := w.Open(context.Background(), OpenRequest{Folder: "shop", Create: true}); err != nil {
		t.Fatal(err)
	}
	for deadline := time.Now().Add(15 * time.Second); ; time.Sleep(20 * time.Millisecond) {
		st := w.Status()
		if st.Open == "" {
			if !strings.Contains(st.Message, "stopped on its own in shop") || !strings.Contains(st.Message, "exit status 3") {
				t.Fatalf("message = %q", st.Message)
			}
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("the folder is still open behind an agent that is gone")
		}
	}
	// Opening anything clears it.
	w.Env = append(w.Env, "DONKEY_TEST_AGENT_EXITS=")
	if err := w.Open(context.Background(), OpenRequest{Folder: "shop"}); err != nil {
		t.Fatal(err)
	}
	if st := w.Status(); st.Message != "" || st.Open != "shop" {
		t.Fatalf("status = %+v", st)
	}
}

// sourceArchive is an archive the way the source history writes one: every
// file inside one folder named after the app and the version.
func sourceArchive(t *testing.T, files map[string]string, modes map[string]os.FileMode) []byte {
	t.Helper()
	var buf bytes.Buffer
	zw := zip.NewWriter(&buf)
	for name, content := range files {
		header := &zip.FileHeader{Name: name, Method: zip.Deflate}
		mode := os.FileMode(0o644)
		if m, ok := modes[name]; ok {
			mode = m
		}
		header.SetMode(mode)
		out, err := zw.CreateHeader(header)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := out.Write([]byte(content)); err != nil {
			t.Fatal(err)
		}
	}
	if err := zw.Close(); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

// controlPlane is the API a machine calls, as far as a machine calls it.
type controlPlane struct {
	*httptest.Server
	archive []byte
	// refuse, when set, answers every request.
	refuse int
	stops  atomic.Int32
	mu     sync.Mutex
	paths  []string
}

func newControlPlane(t *testing.T) *controlPlane {
	t.Helper()
	cp := &controlPlane{}
	cp.Server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		cp.mu.Lock()
		cp.paths = append(cp.paths, r.Method+" "+r.URL.Path)
		cp.mu.Unlock()
		if r.Header.Get("Authorization") != "Bearer sealed-sign-in" {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		if cp.refuse != 0 {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(cp.refuse)
			_, _ = w.Write([]byte(`{"error":"you are not a member of team"}`))
			return
		}
		switch r.Method + " " + r.URL.Path {
		case "GET /v1/spaces/team/apps/shop/commits/" + testCommit + "/archive":
			_, _ = w.Write(cp.archive)
		case "POST /v1/machine/stop":
			cp.stops.Add(1)
			_, _ = w.Write([]byte(`{"state":"stopped"}`))
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	t.Cleanup(cp.Close)
	return cp
}

func (cp *controlPlane) api() API {
	return API{Server: cp.URL + "/", Token: "sealed-sign-in", Log: io.Discard}
}

// A version of an app's source becomes a folder: its files as they were, as
// the first commit of a repository — and never half of one.
func TestWorkspaceOpensAnAppsSource(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	cp := newControlPlane(t)
	cp.archive = sourceArchive(t, map[string]string{
		"shop-0123456789ab/donkey.yaml":      "name: shop\n",
		"shop-0123456789ab/src/main.py":      "print('hi')\n",
		"shop-0123456789ab/scripts/run.sh":   "#!/bin/sh\n",
		"shop-0123456789ab/link":             "/etc/passwd",
		"README-outside-the-versions-folder": "ignored",
	}, map[string]os.FileMode{
		"shop-0123456789ab/scripts/run.sh": 0o755,
		"shop-0123456789ab/link":           os.ModeSymlink | 0o777,
	})
	w := testWorkspace(t)
	w.Fetch = cp.api().FetchSource
	source := &Source{Space: "team", App: "shop", Commit: testCommit}
	if err := w.Open(context.Background(), OpenRequest{Folder: "shop", Source: source}); err != nil {
		t.Fatal(err)
	}
	dir := filepath.Join(w.Root, "shop")
	if got := agentFolder(t, w); got != dir {
		t.Fatalf("the agent runs in %s", got)
	}
	if content, err := os.ReadFile(filepath.Join(dir, "src", "main.py")); err != nil || string(content) != "print('hi')\n" {
		t.Fatalf("src/main.py = %q %v", content, err)
	}
	if info, err := os.Stat(filepath.Join(dir, "scripts", "run.sh")); err != nil || info.Mode()&0o111 == 0 {
		t.Fatalf("a script lost its executable bit: %v", err)
	}
	// The history holds plain files; nothing else is made of what it sends.
	if _, err := os.Lstat(filepath.Join(dir, "link")); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("a link was unpacked")
	}
	entries, _ := os.ReadDir(w.Root)
	if len(entries) != 1 {
		t.Fatalf("the workspace holds %d entries, want the folder alone", len(entries))
	}
	if _, err := exec.LookPath("git"); err == nil {
		log, err := exec.Command("git", "-C", dir, "log", "--format=%s", "--name-only").Output()
		if err != nil || !strings.HasPrefix(string(log), "shop as deployed to team\n") || !strings.Contains(string(log), "src/main.py") {
			t.Fatalf("the folder's history: %q %v", log, err)
		}
		if status, _ := exec.Command("git", "-C", dir, "status", "--porcelain").Output(); len(status) != 0 {
			t.Fatalf("the fetched version reads as changed:\n%s", status)
		}
	}
}

func TestWorkspaceSourceThatCannotBeFetched(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	cp := newControlPlane(t)
	w := testWorkspace(t)
	w.Fetch = cp.api().FetchSource
	ctx := context.Background()
	if err := w.Open(ctx, OpenRequest{Folder: "site", Create: true}); err != nil {
		t.Fatal(err)
	}
	running := w.agent
	source := &Source{Space: "team", App: "shop", Commit: testCommit}

	check := func(name string, err error, status int, message string) {
		t.Helper()
		got := refusal(t, err)
		if got.Status != status || !strings.Contains(got.Message, message) {
			t.Errorf("%s: %d %q, want %d %q", name, got.Status, got.Message, status, message)
		}
		if entries, _ := os.ReadDir(w.Root); len(entries) != 1 {
			t.Errorf("%s left something in the workspace", name)
		}
		if w.agent != running {
			t.Errorf("%s closed the folder that was open", name)
		}
	}

	// The API says no in its own words: the owner is refused as their CLI
	// would be.
	cp.refuse = http.StatusForbidden
	check("no access", w.Open(ctx, OpenRequest{Folder: "shop", Source: source}), http.StatusForbidden, "you are not a member of team")
	cp.refuse = 0

	// A sign-in that ended is the machine's, not the console's: it must not
	// read as the console's own having ended.
	expired := cp.api()
	expired.Token = "expired"
	w.Fetch = expired.FetchSource
	check("expired sign-in", w.Open(ctx, OpenRequest{Folder: "shop", Source: source}), http.StatusConflict, "restart the machine")

	w.Fetch = cp.api().FetchSource
	check("not a version", w.Open(ctx, OpenRequest{Folder: "shop", Source: &Source{Space: "team", App: "shop", Commit: "main"}}),
		http.StatusBadRequest, "not a version")
	check("not an app", w.Open(ctx, OpenRequest{Folder: "shop", Source: &Source{Space: "team", App: "../other", Commit: testCommit}}),
		http.StatusBadRequest, "not an app")

	// An archive that names a file outside the folder is refused whole.
	cp.archive = sourceArchive(t, map[string]string{"shop-0123456789ab/../../escaped": "x"}, nil)
	if err := w.Open(ctx, OpenRequest{Folder: "shop", Source: source}); err == nil || !strings.Contains(err.Error(), "outside the folder") {
		t.Fatalf("an archive that leaves the folder: %v", err)
	}
	if entries, _ := os.ReadDir(w.Root); len(entries) != 1 {
		t.Fatal("a refused archive left something in the workspace")
	}
	if _, err := os.Stat(filepath.Join(filepath.Dir(w.Root), "escaped")); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("a file was written outside the workspace")
	}
}

// controlled is a front door with a workspace behind its control paths.
func controlled(t *testing.T, w *Workspace, used func()) *httptest.Server {
	t.Helper()
	door := httptest.NewServer((&Guard{
		Token: testToken, Host: testHost, Upstream: w.Agent,
		ControlToken: testControlToken, Control: w.Handler(), Used: used,
	}).Handler())
	t.Cleanup(door.Close)
	return door
}

func control(t *testing.T, door *httptest.Server, method string, body any, headers map[string]string) (*http.Response, WorkspaceStatus, string) {
	t.Helper()
	var reader io.Reader
	if body != nil {
		data, _ := json.Marshal(body)
		reader = bytes.NewReader(data)
	}
	req, err := http.NewRequest(method, door.URL+ControlPrefix+"workspace", reader)
	if err != nil {
		t.Fatal(err)
	}
	for name, value := range headers {
		req.Header.Set(name, value)
	}
	res, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer res.Body.Close()
	raw, _ := io.ReadAll(res.Body)
	var status WorkspaceStatus
	var refused struct {
		Error string `json:"error"`
	}
	_ = json.Unmarshal(raw, &status)
	_ = json.Unmarshal(raw, &refused)
	return res, status, refused.Error
}

// The control plane drives the workspace with a credential of its own. The
// gateway's — which is all a browser's request ever carries — does not open
// it, so the owner's own pages cannot reach it either.
func TestControlIsForTheControlPlane(t *testing.T) {
	w := testWorkspace(t)
	var used atomic.Int32
	w.Used = func() { used.Add(1) }
	door := controlled(t, w, func() { used.Add(1) })
	asControl := map[string]string{config.MachineControlHeader: testControlToken}

	for name, headers := range map[string]map[string]string{
		"nobody":                   nil,
		"the gateway":              {config.MachineGuardHeader: testToken},
		"a guess":                  {config.MachineControlHeader: "guess"},
		"the gateway's as its own": {config.MachineControlHeader: testToken},
	} {
		if res, _, _ := control(t, door, "GET", nil, headers); res.StatusCode != http.StatusForbidden {
			t.Errorf("%s got %d at the control path", name, res.StatusCode)
		}
		if res, _, _ := control(t, door, "POST", OpenRequest{Folder: "shop", Create: true}, headers); res.StatusCode != http.StatusForbidden {
			t.Errorf("%s got %d opening a folder", name, res.StatusCode)
		}
	}
	if entries, _ := os.ReadDir(w.Root); len(entries) != 0 {
		t.Fatal("a refused request made a folder")
	}
	// And the control plane's credential is not a way to the agent.
	if res := doorRequest(t, door, "GET", "/api/workspace", asControl); res.StatusCode != http.StatusForbidden {
		t.Errorf("the control plane's credential reached the agent: %d", res.StatusCode)
	}

	// Asking is not using: the console asks for as long as its page is open.
	res, status, _ := control(t, door, "GET", nil, asControl)
	if res.StatusCode != http.StatusOK || status.Open != "" || status.Folders == nil || len(status.Folders) != 0 {
		t.Fatalf("workspace = %d %+v", res.StatusCode, status)
	}
	if used.Load() != 0 {
		t.Fatal("a question from the control plane counted as use")
	}

	// Opening is.
	res, status, _ = control(t, door, "POST", OpenRequest{Folder: "shop", Create: true}, asControl)
	if res.StatusCode != http.StatusOK || status.Open != "shop" || len(status.Folders) != 1 || status.Folders[0].Name != "shop" {
		t.Fatalf("open = %d %+v", res.StatusCode, status)
	}
	if used.Load() == 0 {
		t.Fatal("opening a folder did not count as use")
	}
	agentFolder(t, w)

	// A refusal arrives with its status and its words.
	res, _, message := control(t, door, "POST", OpenRequest{Folder: "shop", Create: true}, asControl)
	if res.StatusCode != http.StatusConflict || !strings.Contains(message, "already is a folder named shop") {
		t.Fatalf("refusal = %d %q", res.StatusCode, message)
	}
}

// What the owner does through the gateway is use; so is every byte on a
// connection that stays open, which is all a terminal ever sends.
func TestGuardCountsUse(t *testing.T) {
	agent := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Upgrade") == "" {
			_, _ = w.Write([]byte("agent"))
			return
		}
		conn, rw, err := http.NewResponseController(w).Hijack()
		if err != nil {
			return
		}
		defer conn.Close()
		_, _ = rw.WriteString("HTTP/1.1 101 Switching Protocols\r\nConnection: Upgrade\r\nUpgrade: echo\r\n\r\n")
		_ = rw.Flush()
		_, _ = io.Copy(conn, rw)
	}))
	defer agent.Close()
	upstream, _ := url.Parse(agent.URL)
	var used atomic.Int32
	door := httptest.NewServer((&Guard{Token: testToken, Host: testHost, Upstream: upstream, Used: func() { used.Add(1) }}).Handler())
	defer door.Close()

	// Refused requests are nobody's use of the machine.
	doorRequest(t, door, "GET", "/", nil)
	doorRequest(t, door, "GET", "/.donkey/healthz", nil)
	doorRequest(t, door, "POST", "/", map[string]string{config.MachineGuardHeader: testToken, "Origin": "https://evil.example.com"})
	if used.Load() != 0 {
		t.Fatalf("refused requests counted as use (%d)", used.Load())
	}
	doorRequest(t, door, "GET", "/", map[string]string{config.MachineGuardHeader: testToken})
	if used.Load() != 1 {
		t.Fatalf("the owner's request counted %d times", used.Load())
	}

	conn, err := net.Dial("tcp", strings.TrimPrefix(door.URL, "http://"))
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	fmt.Fprintf(conn, "GET /ahp HTTP/1.1\r\nHost: %s\r\nConnection: Upgrade\r\nUpgrade: echo\r\n%s: %s\r\n\r\n", testHost, config.MachineGuardHeader, testToken)
	reader := bufio.NewReader(conn)
	res, err := http.ReadResponse(reader, nil)
	if err != nil || res.StatusCode != http.StatusSwitchingProtocols {
		t.Fatalf("upgrade: %v %v", res, err)
	}
	// An open connection that says nothing is not use…
	opened := used.Load()
	time.Sleep(50 * time.Millisecond)
	if used.Load() != opened {
		t.Fatal("a silent connection counted as use")
	}
	// …and one that carries a keystroke and its echo is, both ways.
	if _, err := conn.Write([]byte("ls\n")); err != nil {
		t.Fatal(err)
	}
	echo := make([]byte, 3)
	if _, err := io.ReadFull(reader, echo); err != nil || string(echo) != "ls\n" {
		t.Fatalf("echo = %q %v", echo, err)
	}
	if used.Load() < opened+2 {
		t.Fatalf("bytes on an open connection counted %d times, want both directions", used.Load()-opened)
	}
}

// A machine left alone asks to be stopped; one in use does not.
func TestWatchIdle(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	activity := &Activity{}
	activity.Mark()
	stops := make(chan struct{}, 8)
	const timeout = 200 * time.Millisecond
	go watchIdle(ctx, activity, timeout, func(context.Context) error {
		stops <- struct{}{}
		return nil
	}, io.Discard)

	// In use for well over the timeout: nothing happens.
	for until := time.Now().Add(3 * timeout); time.Now().Before(until); time.Sleep(10 * time.Millisecond) {
		activity.Mark()
	}
	select {
	case <-stops:
		t.Fatal("a machine in use asked to be stopped")
	default:
	}
	// Left alone, it asks — once, not on every look.
	select {
	case <-stops:
	case <-time.After(10 * time.Second):
		t.Fatal("an unused machine never asked to be stopped")
	}
	select {
	case <-stops:
		t.Fatal("it asked again right away")
	case <-time.After(2 * timeout):
	}
}

// The machine reaches its model through the door, which changes nothing on
// the way and learns one thing: that the machine is working.
func TestModelRelay(t *testing.T) {
	model := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = fmt.Fprintf(w, "%s %s %s %s", r.Method, r.URL.RequestURI(), r.Header.Get("Authorization"), r.Header.Get("X-Forwarded-For"))
	}))
	defer model.Close()
	target, _ := url.Parse(model.URL + "/v1")
	var used atomic.Int32
	relay := httptest.NewServer(modelRelay(target, func() { used.Add(1) }))
	defer relay.Close()

	req, _ := http.NewRequest("POST", relay.URL+"/v1/chat/completions?stream=true", strings.NewReader("{}"))
	req.Header.Set("Authorization", "Bearer dkm_machine")
	res, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer res.Body.Close()
	body, _ := io.ReadAll(res.Body)
	if string(body) != "POST /v1/chat/completions?stream=true Bearer dkm_machine " {
		t.Fatalf("the model saw %q", body)
	}
	if used.Load() < 2 {
		t.Fatalf("a model call counted %d times, want the question and its answer", used.Load())
	}
}

func TestStopMachine(t *testing.T) {
	cp := newControlPlane(t)
	if err := cp.api().StopMachine(context.Background()); err != nil || cp.stops.Load() != 1 {
		t.Fatalf("stop: %v (%d requests)", err, cp.stops.Load())
	}
	expired := cp.api()
	expired.Token = "expired"
	if err := expired.StopMachine(context.Background()); err == nil || !strings.Contains(err.Error(), "sign-in inside this machine has ended") {
		t.Fatalf("with an expired sign-in: %v", err)
	}
	if err := (API{}).StopMachine(context.Background()); err == nil {
		t.Fatal("a machine without a sign-in claimed to have asked")
	}
}

// The whole machine, as the image starts it: nothing runs until a folder is
// opened; the agent gets the owner's environment without the door's
// credentials and with the door between it and its model; and left alone,
// the machine asks to be stopped.
func TestServe(t *testing.T) {
	home, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	t.Setenv("HOME", home)
	cp := newControlPlane(t)
	model := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte("model:" + r.URL.Path))
	}))
	defer model.Close()

	agentPort, doorPort := freePort(t), freePort(t)
	env := map[string]string{
		"DONKEY_MACHINE_GUARD_TOKEN":   testToken,
		"DONKEY_MACHINE_CONTROL_TOKEN": testControlToken,
		"DONKEY_MACHINE_HOST":          testHost,
		"DONKEY_MACHINE_IDLE":          "1s",
		"DONKEY_SERVER_URL":            cp.URL,
		"DONKEY_TOKEN":                 "sealed-sign-in",
		"OPENAI_BASE_URL":              model.URL + "/v1",
		"DONKEY_TEST_AGENT_PORT":       agentPort,
	}
	// The agent's environment is the machine's own.
	for name, value := range env {
		t.Setenv(name, value)
	}
	ctx, cancel := context.WithCancel(context.Background())
	served := make(chan error, 1)
	go func() {
		served <- Serve(ctx, Options{
			Listen:    "127.0.0.1:" + doorPort,
			Upstream:  "http://127.0.0.1:" + agentPort,
			Relay:     "127.0.0.1:" + freePort(t),
			Workspace: filepath.Join(home, "workspace"),
			Command:   []string{os.Args[0], "-test.run=^TestHelperAgent$"},
			Getenv:    func(name string) string { return env[name] },
			Log:       io.Discard,
		})
	}()
	door := &httptest.Server{URL: "http://127.0.0.1:" + doorPort}
	asControl := map[string]string{config.MachineControlHeader: testControlToken}
	asOwner := map[string]string{config.MachineGuardHeader: testToken}

	// Up before anything is open.
	for deadline := time.Now().Add(15 * time.Second); ; time.Sleep(20 * time.Millisecond) {
		if res, err := http.Get(door.URL + "/.donkey/healthz"); err == nil {
			res.Body.Close()
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("the door never opened")
		}
	}
	if res := doorRequest(t, door, "GET", "/", asOwner); res.StatusCode != http.StatusBadGateway {
		t.Fatalf("with nothing open the owner got %d", res.StatusCode)
	}

	res, status, message := control(t, door, "POST", OpenRequest{Folder: "shop", Create: true}, asControl)
	if res.StatusCode != http.StatusOK || status.Open != "shop" {
		t.Fatalf("open = %d %+v %s", res.StatusCode, status, message)
	}
	var agentEnv string
	for deadline := time.Now().Add(15 * time.Second); ; time.Sleep(20 * time.Millisecond) {
		res := doorRequest(t, door, "GET", "/env", asOwner)
		body, _ := io.ReadAll(res.Body)
		if res.StatusCode == http.StatusOK {
			agentEnv = string(body)
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("the agent never answered through the door: %d", res.StatusCode)
		}
	}
	if strings.Contains(agentEnv, testToken) || strings.Contains(agentEnv, testControlToken) {
		t.Error("the agent holds one of the door's credentials")
	}
	if !strings.Contains(agentEnv, "DONKEY_TOKEN=sealed-sign-in") || !strings.Contains(agentEnv, "WINGMAN_SANDBOX=off") {
		t.Errorf("the agent's environment:\n%s", agentEnv)
	}
	// The model's address, with the door in front — reachable from inside,
	// and not through the machine's own address.
	var relay string
	for _, line := range strings.Split(agentEnv, "\n") {
		if value, ok := strings.CutPrefix(line, "OPENAI_BASE_URL="); ok {
			relay = value
		}
	}
	if !strings.HasPrefix(relay, "http://127.0.0.1:") || !strings.HasSuffix(relay, "/v1") || strings.Contains(relay, model.URL) {
		t.Fatalf("the agent reaches its model at %q", relay)
	}
	answer, err := http.Get(relay + "/models")
	if err != nil {
		t.Fatal(err)
	}
	body, _ := io.ReadAll(answer.Body)
	answer.Body.Close()
	if string(body) != "model:/v1/models" {
		t.Fatalf("through the relay: %q", body)
	}
	relayURL, _ := url.Parse(relay)
	if res := doorRequest(t, door, "GET", PortPrefix+relayURL.Port()+"/v1/models", asOwner); res.StatusCode != http.StatusNotFound {
		t.Errorf("the model relay answers under the machine's address: %d", res.StatusCode)
	}

	// Left alone for its idle time, it asks to be stopped, as its owner.
	for deadline := time.Now().Add(20 * time.Second); cp.stops.Load() == 0; time.Sleep(50 * time.Millisecond) {
		if time.Now().After(deadline) {
			t.Fatal("an unused machine never asked to be stopped")
		}
	}

	cancel()
	select {
	case err := <-served:
		if err != nil {
			t.Fatalf("serve: %v", err)
		}
	case <-time.After(30 * time.Second):
		t.Fatal("the machine did not stop")
	}
	if conn, err := net.DialTimeout("tcp", "127.0.0.1:"+agentPort, time.Second); err == nil {
		conn.Close()
		t.Fatal("the agent outlived its machine")
	}
}
