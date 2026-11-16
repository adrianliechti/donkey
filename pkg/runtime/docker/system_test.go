package docker

import (
	"context"
	"fmt"
	"math"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/docker/docker/client"
)

// systemEngine is a Docker engine that knows a few containers and its host.
func systemEngine(t *testing.T, containers []map[string]any, inspect map[string]map[string]any) *client.Client {
	t.Helper()
	transport := dockerRoundTripFunc(func(req *http.Request) (*http.Response, error) {
		path := strings.TrimPrefix(req.URL.Path, "/v1.48")
		switch {
		case path == "/containers/json":
			if req.URL.Query().Get("all") != "1" {
				t.Errorf("stopped containers were left out of the listing: %s", req.URL.RawQuery)
			}
			return dockerJSONResponse(http.StatusOK, containers), nil
		case path == "/info":
			return dockerJSONResponse(http.StatusOK, map[string]any{
				"Name": "build-host", "NCPU": 4, "MemTotal": int64(8 << 30), "ServerVersion": "29.9.3",
				"OperatingSystem": "Ubuntu 24.04", "Architecture": "aarch64", "ContainersRunning": 7,
			}), nil
		case path == "/version":
			return dockerJSONResponse(http.StatusOK, map[string]any{"Version": "29.9.3", "ApiVersion": "1.48"}), nil
		case strings.HasPrefix(path, "/containers/") && strings.HasSuffix(path, "/json"):
			id := strings.TrimSuffix(strings.TrimPrefix(path, "/containers/"), "/json")
			if info, ok := inspect[id]; ok {
				return dockerJSONResponse(http.StatusOK, info), nil
			}
			return dockerJSONResponse(http.StatusNotFound, map[string]string{"message": "no such container"}), nil
		}
		return dockerJSONResponse(http.StatusNotFound, map[string]string{"message": "not found: " + path}), nil
	})
	cli, err := client.NewClientWithOpts(client.WithHost("http://docker.test"), client.WithVersion("1.48"), client.WithHTTPClient(&http.Client{Transport: transport}))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { cli.Close() })
	return cli
}

func TestComponentsAreDonkeysOwnContainers(t *testing.T) {
	machine := SystemLabels(roleMachine)
	machine[labelMachine] = "u-abc"
	containers := []map[string]any{
		{"Id": "gw", "Names": []string{"/donkey-gateway"}, "Image": "donkey/donkey-server:0.4.0", "Labels": SystemLabels(RoleGateway)},
		{"Id": "srv", "Names": []string{"/donkey-server"}, "Image": "donkey/donkey-server:0.4.0", "Labels": SystemLabels(RoleServer)},
		{"Id": "bk", "Names": []string{"/donkey-buildkitd"}, "Image": "moby/buildkit:v0.26", "Labels": SystemLabels(RoleBuildkit)},
		// An app, a service and a machine are somebody's, not the control plane.
		{"Id": "app", "Names": []string{"/donkey.team.site"}, "Labels": appLabels("team", "site", roleWorkload)},
		{"Id": "db", "Names": []string{"/donkey.team.postgres"}, "Labels": addonLabels("team", "postgres", roleAddon)},
		{"Id": "m", "Names": []string{"/donkey-machine-u-abc"}, "Labels": machine},
	}
	inspect := map[string]map[string]any{
		"srv": {"RestartCount": 2, "State": map[string]any{"Running": true, "Status": "running", "StartedAt": "2026-11-01T08:00:00.5Z"}},
		"gw":  {"RestartCount": 9, "State": map[string]any{"Running": true, "Restarting": true, "Status": "restarting", "ExitCode": 1}},
		"bk": {"State": map[string]any{
			"Running": true, "Status": "running", "StartedAt": "2026-11-01T08:00:00Z",
			"Health": map[string]any{"Status": "unhealthy"},
		}},
	}
	r := &Runtime{cli: systemEngine(t, containers, inspect), state: state{dir: t.TempDir()}}
	components, err := r.Components(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(components) != 3 {
		t.Fatalf("components: %+v", components)
	}
	// The same order on every runtime: server, gateway, build engine.
	server, gateway, buildkit := components[0], components[1], components[2]
	if server.Name != "donkey-server" || server.Role != RoleServer || !server.Healthy() || server.Restarts != 2 ||
		server.Image != "donkey/donkey-server:0.4.0" || !server.StartedAt.Equal(time.Date(2026, 11, 1, 8, 0, 0, 500_000_000, time.UTC)) {
		t.Fatalf("server: %+v", server)
	}
	if gateway.Role != RoleGateway || gateway.Healthy() || gateway.Restarts != 9 || gateway.Message != "restarting (exit code 1)" {
		t.Fatalf("gateway: %+v", gateway)
	}
	if buildkit.Role != RoleBuildkit || buildkit.Healthy() || !strings.Contains(buildkit.Message, "health check") {
		t.Fatalf("build engine: %+v", buildkit)
	}
}

func TestInfoNamesTheEngine(t *testing.T) {
	r := &Runtime{cli: systemEngine(t, nil, nil)}
	info, err := r.Info(context.Background())
	if err != nil || info.Name != "Docker" || info.Version != "29.9.3" {
		t.Fatalf("info: %+v %v", info, err)
	}
}

// procFiles writes the two files the host meter reads.
func procFiles(t *testing.T, dir string, memTotalKB, memAvailableKB int64, cores int, busy, idle uint64) {
	t.Helper()
	meminfo := fmt.Sprintf("MemTotal:       %d kB\nMemFree:          1024 kB\nMemAvailable:   %d kB\n", memTotalKB, memAvailableKB)
	if err := os.WriteFile(filepath.Join(dir, "meminfo"), []byte(meminfo), 0600); err != nil {
		t.Fatal(err)
	}
	// user nice system idle iowait irq softirq steal guest guest_nice
	stat := fmt.Sprintf("cpu  %d 0 0 %d 0 0 0 0 99 99\n", busy, idle)
	for i := range cores {
		stat += fmt.Sprintf("cpu%d 1 0 0 1 0 0 0 0 0 0\n", i)
	}
	stat += "intr 12345\nctxt 67890\n"
	if err := os.WriteFile(filepath.Join(dir, "stat"), []byte(stat), 0600); err != nil {
		t.Fatal(err)
	}
}

func TestNodeIsTheEnginesHostWithWhatItUses(t *testing.T) {
	proc := t.TempDir()
	// The engine's host has 8 GiB of which 5 GiB are still to be had, and
	// since the last reading its four processors were busy a quarter of
	// the time.
	procFiles(t, proc, 8<<20, 5<<20, 4, 2000, 6000)
	r := &Runtime{cli: systemEngine(t, nil, nil), host: hostMeter{
		proc: proc,
		from: cpuSample{at: time.Now().Add(-10 * time.Second), busy: 1000, total: 4000},
	}}
	nodes, err := r.Nodes(context.Background())
	if err != nil || len(nodes) != 1 {
		t.Fatalf("nodes: %+v %v", nodes, err)
	}
	node := nodes[0]
	if node.Name != "build-host" || !node.Ready || !node.Schedulable || node.Version != "29.9.3" ||
		node.OS != "Ubuntu 24.04" || node.Arch != "aarch64" || node.Instances != 7 {
		t.Fatalf("node: %+v", node)
	}
	if node.CPU.Capacity != 4 || node.Memory.Capacity != 8<<30 {
		t.Fatalf("capacity: %+v %+v", node.CPU, node.Memory)
	}
	if node.CPU.Used == nil || math.Abs(*node.CPU.Used-1) > 1e-9 || node.Memory.Used == nil || *node.Memory.Used != 3<<30 {
		t.Fatalf("usage: %+v %+v", node.CPU, node.Memory)
	}
	// Reservations are a scheduler's notion; an engine has none.
	if node.CPU.Reserved != nil || node.Memory.Reserved != nil {
		t.Fatalf("an engine does not reserve: %+v %+v", node.CPU, node.Memory)
	}
}

// The reading only counts when it describes the engine's machine. A server
// started somewhere else must not present its own machine's load as the
// engine's.
func TestNodeUsageIsLeftOutWhenThisIsNotTheEnginesMachine(t *testing.T) {
	for name, write := range map[string]func(dir string){
		"other memory":     func(dir string) { procFiles(t, dir, 16<<20, 5<<20, 4, 2000, 6000) },
		"other processors": func(dir string) { procFiles(t, dir, 8<<20, 5<<20, 12, 2000, 6000) },
		"no proc":          func(string) {},
	} {
		t.Run(name, func(t *testing.T) {
			proc := t.TempDir()
			write(proc)
			r := &Runtime{cli: systemEngine(t, nil, nil), host: hostMeter{
				proc: proc,
				from: cpuSample{at: time.Now().Add(-10 * time.Second), busy: 1000, total: 4000},
			}}
			nodes, err := r.Nodes(context.Background())
			if err != nil || len(nodes) != 1 {
				t.Fatalf("nodes: %+v %v", nodes, err)
			}
			if nodes[0].CPU.Used != nil || nodes[0].Memory.Used != nil || nodes[0].CPU.Capacity != 4 {
				t.Fatalf("usage of another machine was reported: %+v %+v", nodes[0].CPU, nodes[0].Memory)
			}
		})
	}
}

func TestHostMeterReadsAgainstItsLastReading(t *testing.T) {
	proc := t.TempDir()
	meter := &hostMeter{proc: proc}
	ctx := context.Background()

	// The first look has nothing to compare with: it reads twice, and the
	// counters must have moved in between.
	procFiles(t, proc, 8<<20, 4<<20, 2, 100, 300)
	go func() {
		time.Sleep(cpuFirstWindow / 3)
		procFiles(t, proc, 8<<20, 4<<20, 2, 150, 350)
	}()
	cpu, memory, ok := meter.usage(ctx, 2, 8<<30)
	if !ok || math.Abs(cpu-1) > 1e-9 || memory != 4<<30 {
		t.Fatalf("first reading: cpu=%v memory=%v ok=%v", cpu, memory, ok)
	}

	// A second look a blink later gets the same answer, not the blink.
	procFiles(t, proc, 8<<20, 4<<20, 2, 151, 350)
	if again, _, ok := meter.usage(ctx, 2, 8<<30); !ok || again != cpu {
		t.Fatalf("an answer did not stand: %v then %v", cpu, again)
	}

	// Later, the window is the time since the kept reading.
	meter.shareAt = time.Now().Add(-time.Minute)
	meter.from.at = time.Now().Add(-time.Minute)
	procFiles(t, proc, 8<<20, 4<<20, 2, 250, 650)
	if cpu, _, ok := meter.usage(ctx, 2, 8<<30); !ok || math.Abs(cpu-0.5) > 1e-9 {
		t.Fatalf("windowed reading: cpu=%v ok=%v", cpu, ok)
	}

	// Counters that started over (a reboot) are not a negative load.
	meter.shareAt = time.Time{}
	procFiles(t, proc, 8<<20, 4<<20, 2, 10, 20)
	if _, _, ok := meter.usage(ctx, 2, 8<<30); ok {
		t.Fatal("counters that went backwards produced a reading")
	}
}
