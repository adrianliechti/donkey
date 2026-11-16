package server

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/adrianliechti/donkey/pkg/runtime"
)

// systemRuntime answers the system questions with whatever a test gives it,
// and counts how often it was asked.
type systemRuntime struct {
	planRuntime
	info       runtime.Info
	nodes      []runtime.Node
	nodesErr   error
	components []runtime.Component
	partsErr   error
	asked      map[string]int
}

func (r *systemRuntime) count(what string) {
	if r.asked == nil {
		r.asked = map[string]int{}
	}
	r.asked[what]++
}

func (r *systemRuntime) Info(context.Context) (runtime.Info, error) {
	r.count("info")
	return r.info, nil
}
func (r *systemRuntime) Nodes(context.Context) ([]runtime.Node, error) {
	r.count("nodes")
	return r.nodes, r.nodesErr
}
func (r *systemRuntime) Components(context.Context) ([]runtime.Component, error) {
	r.count("components")
	return r.components, r.partsErr
}

func systemServer(rt runtime.Provider) *Server {
	return &Server{cfg: &Config{Domain: "example.com", SessionKey: testSessionKey}, rt: rt}
}

func getAs(t *testing.T, handler http.Handler, path, email string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodGet, path, nil)
	if email != "" {
		req.Header.Set("X-Access-Token", sealedToken(t, testSessionKey, email))
	}
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	return rec
}

func TestSystemDescribesTheControlPlaneInTheRuntimesWords(t *testing.T) {
	started := time.Date(2026, 11, 1, 8, 0, 0, 0, time.UTC)
	rt := &systemRuntime{
		info: runtime.Info{Name: "Kubernetes", Version: "v1.31.2"},
		components: []runtime.Component{
			{Name: "donkey-server", Role: "server", Image: "ghcr.io/acme/donkey-server:0.4.0", Ready: 2, Want: 2, StartedAt: started},
			{Name: "donkey-gateway", Role: "gateway", Image: "ghcr.io/acme/donkey-server@sha256:abc", Ready: 0, Want: 1, Restarts: 7, Message: "CrashLoopBackOff"},
		},
	}
	handler := systemServer(rt).routes()
	rec := getAs(t, handler, "/v1/system", "u1@example.com")
	if rec.Code != http.StatusOK {
		t.Fatalf("system: %d %s", rec.Code, rec.Body.String())
	}
	var system SystemResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &system); err != nil {
		t.Fatal(err)
	}
	if system.Runtime.Name != "Kubernetes" || system.Runtime.Version != "v1.31.2" || system.Domain != "example.com" ||
		system.Version != Version || len(system.Components) != 2 {
		t.Fatalf("system: %+v", system)
	}
	server, gateway := system.Components[0], system.Components[1]
	if !server.Healthy || server.Version != "0.4.0" || server.Ready != 2 || server.Want != 2 || server.StartedAt != "2026-11-01T08:00:00Z" {
		t.Fatalf("server: %+v", server)
	}
	// An image pinned by digest alone has no version to show.
	if gateway.Healthy || gateway.Version != "" || gateway.Restarts != 7 || gateway.Message != "CrashLoopBackOff" || gateway.StartedAt != "" {
		t.Fatalf("gateway: %+v", gateway)
	}

	// Another open console a moment later costs the runtime nothing.
	getAs(t, handler, "/v1/system", "u1@example.com")
	if rt.asked["components"] != 1 || rt.asked["info"] != 1 {
		t.Fatalf("the runtime was asked again within the moment: %+v", rt.asked)
	}
}

// A runtime that cannot list donkey's workloads is still named, and the page
// says what went wrong instead of failing whole.
func TestSystemSaysWhyTheComponentsAreMissing(t *testing.T) {
	rt := &systemRuntime{info: runtime.Info{Name: "Docker", Version: "29.9.3"}, partsErr: errors.New("the engine is not answering")}
	rec := getAs(t, systemServer(rt).routes(), "/v1/system", "u1@example.com")
	var system SystemResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &system); err != nil || rec.Code != http.StatusOK {
		t.Fatalf("system: %d %v", rec.Code, err)
	}
	if system.Runtime.Name != "Docker" || system.ComponentsError != "the engine is not answering" || system.Components == nil || len(system.Components) != 0 {
		t.Fatalf("system: %+v", system)
	}
}

func TestNodesListWhatTheRuntimeMeasures(t *testing.T) {
	used, reserved := 1.25, 2.0
	rt := &systemRuntime{nodes: []runtime.Node{
		{
			Name: "node-a", Ready: true, Schedulable: true, Roles: []string{"control-plane"}, Version: "v1.31.2",
			OS: "Ubuntu 24.04", Arch: "arm64", CreatedAt: time.Date(2026, 10, 1, 0, 0, 0, 0, time.UTC),
			CPU:       runtime.NodeResource{Capacity: 4, Used: &used, Reserved: &reserved},
			Memory:    runtime.NodeResource{Capacity: 8 << 30},
			Instances: 23, InstanceCapacity: 110,
		},
		{Name: "node-b", Problems: []string{"NotReady: kubelet stopped"}, CPU: runtime.NodeResource{Capacity: 2}},
	}}
	handler := systemServer(rt).routes()
	rec := getAs(t, handler, "/v1/nodes", "u1@example.com")
	if rec.Code != http.StatusOK {
		t.Fatalf("nodes: %d %s", rec.Code, rec.Body.String())
	}
	var list NodesResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &list); err != nil {
		t.Fatal(err)
	}
	if len(list.Nodes) != 2 || list.Note != "" {
		t.Fatalf("nodes: %+v", list)
	}
	a := list.Nodes[0]
	if a.Name != "node-a" || !a.Ready || !a.Schedulable || a.Roles[0] != "control-plane" || a.CreatedAt != "2026-10-01T00:00:00Z" ||
		a.CPU.Capacity != 4 || *a.CPU.Used != 1.25 || *a.CPU.Reserved != 2 || a.Instances != 23 || a.InstanceCapacity != 110 {
		t.Fatalf("node-a: %+v", a)
	}
	// What the runtime did not measure is absent, not zero.
	if body := rec.Body.String(); strings.Contains(body, `"memory":{"capacity":8589934592,"used"`) ||
		!strings.Contains(body, `"memory":{"capacity":8589934592}`) {
		t.Fatalf("an unmeasured resource was given a value: %s", body)
	}
	if b := list.Nodes[1]; b.Ready || b.Problems[0] != "NotReady: kubelet stopped" || b.CreatedAt != "" {
		t.Fatalf("node-b: %+v", b)
	}
	getAs(t, handler, "/v1/nodes", "u1@example.com")
	if rt.asked["nodes"] != 1 {
		t.Fatalf("the runtime was asked for its nodes %d times within the moment", rt.asked["nodes"])
	}
}

// Not being allowed to look at the machines is a fact about the installation:
// the page gets the reason, not an error.
func TestNodesTheRuntimeMayNotSeeAreANote(t *testing.T) {
	rt := &systemRuntime{nodesErr: runtime.Unavailable{Reason: "the nodes are not this installation's to see"}}
	rec := getAs(t, systemServer(rt).routes(), "/v1/nodes", "u1@example.com")
	var list NodesResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &list); err != nil || rec.Code != http.StatusOK {
		t.Fatalf("nodes: %d %v %s", rec.Code, err, rec.Body.String())
	}
	if list.Note != "the nodes are not this installation's to see" || list.Nodes == nil || len(list.Nodes) != 0 {
		t.Fatalf("nodes: %+v", list)
	}

	// A runtime that fails is a failure.
	failing := &systemRuntime{nodesErr: errors.New("the API server is not answering")}
	if rec := getAs(t, systemServer(failing).routes(), "/v1/nodes", "u1@example.com"); rec.Code != http.StatusBadGateway ||
		!strings.Contains(rec.Body.String(), "not answering") {
		t.Fatalf("a failing runtime: %d %s", rec.Code, rec.Body.String())
	}
}

// What the installation runs on is its administrators' to see.
func TestSystemAndNodesAreForPlatformAdmins(t *testing.T) {
	rt := &systemRuntime{info: runtime.Info{Name: "Kubernetes"}}
	handler := systemServer(rt).routes()
	for _, path := range []string{"/v1/system", "/v1/nodes"} {
		if rec := getAs(t, handler, path, "dev@example.com"); rec.Code != http.StatusForbidden {
			t.Errorf("%s as a signed-in person who is no platform admin: %d", path, rec.Code)
		}
		if rec := getAs(t, handler, path, ""); rec.Code != http.StatusUnauthorized {
			t.Errorf("%s without a token: %d", path, rec.Code)
		}
	}
	if len(rt.asked) != 0 {
		t.Fatalf("the runtime was asked on behalf of somebody not allowed to: %+v", rt.asked)
	}
}

func TestImageTag(t *testing.T) {
	for image, want := range map[string]string{
		"ghcr.io/acme/donkey-server:0.4.0":            "0.4.0",
		"registry.platform/donkey/donkey-server:dev":  "dev",
		"localhost:5000/donkey/server":                "",
		"localhost:5000/donkey/server:1.2":            "1.2",
		"ghcr.io/acme/donkey-server@sha256:abc":       "",
		"ghcr.io/acme/donkey-server:0.4.0@sha256:abc": "0.4.0",
		"busybox": "",
		"":        "",
	} {
		if got := imageTag(image); got != want {
			t.Errorf("imageTag(%q) = %q, want %q", image, got, want)
		}
	}
}
