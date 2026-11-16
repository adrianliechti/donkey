//go:build e2e

package docker

import (
	"bytes"
	"context"
	"crypto/rand"
	"os"
	"slices"
	"strings"
	"testing"
	"time"

	cerrdefs "github.com/containerd/errdefs"
	"github.com/docker/docker/api/types/container"
	"github.com/docker/docker/api/types/network"
	"github.com/docker/docker/pkg/stdcopy"

	"github.com/adrianliechti/donkey/internal/config"
	rt "github.com/adrianliechti/donkey/pkg/runtime"
)

// TestMachineDocker runs one machine's whole life on a real Docker engine,
// with the real machine image:
//
//	DONKEY_E2E_MACHINE_IMAGE=donkey/donkey-machine:dev go test -tags e2e ./pkg/runtime/docker -run TestMachineDocker -v
//
// No control plane is needed: the containers a machine shares its network
// with are simply absent, which the runtime tolerates outside an installation.
func TestMachineDocker(t *testing.T) {
	image := os.Getenv("DONKEY_E2E_MACHINE_IMAGE")
	if image == "" {
		t.Skip("set DONKEY_E2E_MACHINE_IMAGE to a machine image the engine has (task machine-image)")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()
	r, err := New(Options{StateDir: t.TempDir()})
	if err != nil {
		t.Fatal(err)
	}
	defer r.cli.Close()
	machines := r.Machines()
	id := config.MachineID("e2e-" + rand.Text())
	name := machineName(id)
	t.Cleanup(func() {
		cleanup, cancel := context.WithTimeout(context.Background(), time.Minute)
		defer cancel()
		if err := machines.Delete(cleanup, id); err != nil {
			t.Error(err)
		}
	})
	spec := rt.MachineSpec{
		ID: id, Owner: "owner-subject", Email: "ada@example.com", Name: "Ada Owner",
		Image: image, Host: config.MachineHost("example.com"),
		Token: "sealed-sign-in", SignInExpires: time.Now().Add(time.Hour),
		GuardToken: "the-gateways-credential", ControlToken: "the-control-planes-credential",
		AIToken: "dkm_e2e", Model: "test-model",
		Settings: config.MachineSettings{Image: image, CPU: "1", Memory: "1Gi"},
	}

	// run executes a command inside the machine, as the machine's own user.
	run := func(t *testing.T, command string) (string, int) {
		t.Helper()
		exec, err := r.cli.ContainerExecCreate(ctx, name, container.ExecOptions{
			Cmd: []string{"sh", "-c", command}, AttachStdout: true, AttachStderr: true,
		})
		if err != nil {
			t.Fatal(err)
		}
		attached, err := r.cli.ContainerExecAttach(ctx, exec.ID, container.ExecAttachOptions{})
		if err != nil {
			t.Fatal(err)
		}
		defer attached.Close()
		var out bytes.Buffer
		_, _ = stdcopy.StdCopy(&out, &out, attached.Reader)
		info, err := r.cli.ContainerExecInspect(ctx, exec.ID)
		if err != nil {
			t.Fatal(err)
		}
		return strings.TrimSpace(out.String()), info.ExitCode
	}
	// serving waits for the front door to say the machine is up.
	serving := func(t *testing.T) {
		t.Helper()
		for deadline := time.Now().Add(90 * time.Second); ; time.Sleep(time.Second) {
			if _, code := run(t, "curl -fsS http://127.0.0.1:8080"+rt.MachineHealthPath); code == 0 {
				return
			}
			if time.Now().After(deadline) {
				t.Fatalf("the machine never served: %s", r.logsTail(ctx, name, 40))
			}
		}
	}
	// control asks the front door the way donkey-server does.
	control := func(t *testing.T, credential, body string) (string, string) {
		t.Helper()
		request := "curl -s -w '\\n%{http_code}' -H '" + config.MachineControlHeader + ": " + credential + "'"
		if body != "" {
			request += " -H 'Content-Type: application/json' -d '" + body + "'"
		}
		out, _ := run(t, request+" http://127.0.0.1:8080/.donkey/control/workspace")
		// The status is the last line; the answer is whatever came before it.
		at := strings.LastIndex(out, "\n")
		return strings.TrimSpace(out[:max(at, 0)]), out[at+1:]
	}
	// open opens a folder and waits for the agent to answer in it.
	open := func(t *testing.T, body, folder string) {
		t.Helper()
		if answer, code := control(t, spec.ControlToken, body); code != "200" || !strings.Contains(answer, `"open":"`+folder+`"`) {
			t.Fatalf("opening %s: %s %s", folder, code, answer)
		}
		for deadline := time.Now().Add(60 * time.Second); ; time.Sleep(time.Second) {
			if answer, _ := control(t, spec.ControlToken, ""); strings.Contains(answer, `"ready":true`) {
				return
			}
			if time.Now().After(deadline) {
				t.Fatalf("the agent never answered in %s: %s", folder, r.logsTail(ctx, name, 40))
			}
		}
	}
	// agent asks the agent what it has open, as the gateway would for the owner.
	agent := func(t *testing.T) string {
		t.Helper()
		out, _ := run(t, `curl -s -w ' %{http_code}' -H '`+config.MachineGuardHeader+`: `+spec.GuardToken+`' -H 'Host: `+spec.Host+`' http://127.0.0.1:8080/api/workspace`)
		return out
	}

	if st, err := machines.Status(ctx, id); err != nil || st.State != rt.MachineAbsent {
		t.Fatalf("before: %+v %v", st, err)
	}
	if err := machines.Start(ctx, spec); err != nil {
		t.Fatal(err)
	}
	if st, err := machines.Status(ctx, id); err != nil || st.Backend != "http://"+name+":8080" {
		t.Fatalf("the control plane would reach the machine at %q (%v)", st.Backend, err)
	}

	t.Run("runs locked down, on a network of its own", func(t *testing.T) {
		info, err := r.cli.ContainerInspect(ctx, name)
		if err != nil {
			t.Fatal(err)
		}
		if info.Config.User != "1000:1000" {
			t.Errorf("user = %q", info.Config.User)
		}
		if out, _ := run(t, "hostname; id -un"); out != "donkey\ndev" {
			t.Errorf("the prompt would read %q, want dev@donkey", strings.ReplaceAll(out, "\n", " / "))
		}
		host := info.HostConfig
		if !slices.Contains([]string(host.CapDrop), "ALL") || !slices.Contains(host.SecurityOpt, "no-new-privileges:true") {
			t.Errorf("capabilities dropped %v, security options %v", host.CapDrop, host.SecurityOpt)
		}
		if host.PidsLimit == nil || *host.PidsLimit != 4096 || host.Memory != 1<<30 || host.NanoCPUs != 1e9 {
			t.Errorf("limits: pids %v, memory %d, cpus %d", host.PidsLimit, host.Memory, host.NanoCPUs)
		}
		if host.RestartPolicy.Name != container.RestartPolicyUnlessStopped {
			t.Errorf("restart policy = %q", host.RestartPolicy.Name)
		}
		networks := info.NetworkSettings.Networks
		if len(networks) != 1 || networks[name] == nil {
			t.Errorf("networks = %v, want its own alone", networks)
		}
		if len(info.Mounts) != 1 || info.Mounts[0].Destination != config.MachineDataPath || info.Mounts[0].Name != name+".data" {
			t.Errorf("mounts = %+v", info.Mounts)
		}
		// Restricted by default: a network with no way out of the engine.
		nw, err := r.cli.NetworkInspect(ctx, name, network.InspectOptions{})
		if err != nil || !nw.Internal {
			t.Errorf("the machine's network is not internal (%v)", err)
		}
		serving(t)
		if out, code := run(t, "curl -s -m 5 -o /dev/null -w '%{http_code}' https://example.com/"); code == 0 {
			t.Errorf("a restricted machine reached the internet (%s)", out)
		}
		if out, _ := run(t, "id -u; stat -c %u /data; grep NoNewPrivs /proc/self/status; grep CapEff /proc/self/status"); !strings.Contains(out, "1000\n1000\n") ||
			!strings.Contains(out, "NoNewPrivs:\t1") || !strings.Contains(out, "CapEff:\t0000000000000000") {
			t.Errorf("inside the machine:\n%s", out)
		}
	})

	t.Run("opens one folder at a time, for the control plane", func(t *testing.T) {
		// Up, with nothing open: no agent runs, and the owner is told so.
		if out, _ := run(t, "pgrep -c wingman"); out != "0" {
			t.Errorf("an agent runs before any folder is open (%s)", out)
		}
		if got := agent(t); !strings.HasSuffix(got, " 502") || !strings.Contains(got, "Nothing is open") {
			t.Errorf("with nothing open the owner got %q", got)
		}
		// Only the control plane's own credential opens the control path.
		for who, credential := range map[string]string{"nobody": "", "a guess": "guess", "the gateway": spec.GuardToken} {
			if _, code := control(t, credential, `{"folder":"project","create":true}`); code != "403" {
				t.Errorf("%s got %s at the control path", who, code)
			}
		}
		if answer, code := control(t, spec.ControlToken, ""); code != "200" || !strings.Contains(answer, `"folders":[]`) {
			t.Fatalf("a fresh workspace: %s %s", code, answer)
		}

		open(t, `{"folder":"project","create":true}`, "project")
		if got := agent(t); !strings.Contains(got, `"path":"/data/workspace/project"`) || !strings.HasSuffix(got, " 200") {
			t.Fatalf("the agent has open: %s", got)
		}
		open(t, `{"folder":"site","create":true}`, "site")
		if got := agent(t); !strings.Contains(got, `"path":"/data/workspace/site"`) {
			t.Fatalf("after opening another folder the agent has open: %s", got)
		}
		if out, _ := run(t, "pgrep -c -x wingman"); out != "1" {
			t.Errorf("%s agents run after a folder was switched, want one", out)
		}
		// A name that is taken is refused, and what is open stays open.
		if answer, code := control(t, spec.ControlToken, `{"folder":"project","create":true}`); code != "409" || !strings.Contains(answer, "already is a folder named project") {
			t.Errorf("a taken name: %s %s", code, answer)
		}
		open(t, `{"folder":"project"}`, "project")
		if answer, _ := control(t, spec.ControlToken, ""); !strings.Contains(answer, `"folders":[{"name":"project"`) {
			t.Errorf("the folder used last is not listed first: %s", answer)
		}
	})

	t.Run("is its owner's environment", func(t *testing.T) {
		out, _ := run(t, `cat ~/.config/donkey/config.yaml; git config --global user.name; git config --global user.email; echo "$DONKEY_TOKEN|$OPENAI_BASE_URL|$OPENAI_API_KEY|$WINGMAN_MODEL"; echo "$NPM_CONFIG_REGISTRY $PIP_INDEX_URL $GOPROXY"; head -3 ~/.agents/skills/donkey/SKILL.md`)
		for _, want := range []string{
			"url: " + r.machineAPIURL(), "Ada Owner", "ada@example.com", "sealed-sign-in|" + r.aiProxyURL() + "|dkm_e2e|test-model",
			// Package managers are pointed at the mirror before anybody asks.
			r.machinePackagesURL() + "/npm/ " + r.machinePackagesURL() + "/pypi/ " + r.machinePackagesURL() + "/go/",
			// The agent finds the donkey skill.
			"name: donkey",
		} {
			if !strings.Contains(out, want) {
				t.Errorf("missing %q in:\n%s", want, out)
			}
		}
		// The agent and everything it starts never see the door's credentials.
		if out, _ := run(t, `tr '\0' '\n' < /proc/$(pgrep -o -x wingman)/environ | grep -c -e DONKEY_MACHINE_GUARD_TOKEN -e DONKEY_MACHINE_CONTROL_TOKEN`); out != "0" {
			t.Errorf("the agent holds a credential of the front door (%s)", out)
		}
		// It is told how long it may go unused, and reaches its model through
		// the door, which is how a working agent counts as use.
		if out, _ := run(t, `echo "$DONKEY_MACHINE_IDLE"; tr '\0' '\n' < /proc/$(pgrep -o -x wingman)/environ | grep '^OPENAI_BASE_URL='`); !strings.HasPrefix(out, "1h0m0s\nOPENAI_BASE_URL=http://127.0.0.1:9001") {
			t.Errorf("idle time and the agent's model address:\n%s", out)
		}
	})

	t.Run("answers the gateway's credential and nothing else", func(t *testing.T) {
		code := func(headers string) string {
			out, _ := run(t, `curl -s -o /dev/null -w '%{http_code}' `+headers+` http://127.0.0.1:8080/api/workspace`)
			return out
		}
		if got := code(""); got != "403" {
			t.Errorf("without the credential: %s", got)
		}
		if got := code("-H '" + config.MachineGuardHeader + ": guess'"); got != "403" {
			t.Errorf("with a wrong credential: %s", got)
		}
		if got := code("-H '" + config.MachineGuardHeader + ": the-gateways-credential' -H 'Host: " + spec.Host + "'"); got != "200" {
			t.Errorf("with the gateway's credential: %s", got)
		}
	})

	t.Run("is known to the gateway and the AI endpoint while it runs", func(t *testing.T) {
		snapshot, err := (&GatewayAdapter{StateDir: r.opts.StateDir}).GatewaySnapshot(ctx)
		if err != nil || len(snapshot.Routes) != 1 {
			t.Fatalf("routes = %+v %v", snapshot.Routes, err)
		}
		if route := snapshot.Routes[0]; route.Machine != id || route.Owner != "owner-subject" || route.Host != spec.Host || route.Backend != "http://"+name+":8080" {
			t.Errorf("route = %+v", route)
		}
		if tokens, err := r.AIProxyTokens(ctx); err != nil || !slices.Equal(tokens, []string{"dkm_e2e"}) {
			t.Errorf("model tokens = %v %v", tokens, err)
		}
	})

	t.Run("stop keeps the disk and withdraws everything else", func(t *testing.T) {
		if out, code := run(t, "echo kept > /data/home/note && echo mine > /data/workspace/project/file"); code != 0 {
			t.Fatalf("the disk is not writable: %s", out)
		}
		// What the control plane last saw on the disk stays with it.
		if err := machines.Remember(ctx, id, []rt.MachineFolder{{Name: "project"}, {Name: "site"}}); err != nil {
			t.Fatal(err)
		}
		if err := machines.Stop(ctx, id); err != nil {
			t.Fatal(err)
		}
		st, err := machines.Status(ctx, id)
		if err != nil || st.State != rt.MachineStopped {
			t.Fatalf("after stop: %+v %v", st, err)
		}
		if len(st.Folders) != 2 || st.Folders[0].Name != "project" || st.Folders[1].Name != "site" {
			t.Errorf("a stopped machine remembers %+v", st.Folders)
		}
		if _, err := r.cli.ContainerInspect(ctx, name); !cerrdefs.IsNotFound(err) {
			t.Errorf("the container is still there: %v", err)
		}
		if _, err := r.cli.NetworkInspect(ctx, name, network.InspectOptions{}); !cerrdefs.IsNotFound(err) {
			t.Errorf("the network is still there: %v", err)
		}
		if snapshot, _ := (&GatewayAdapter{StateDir: r.opts.StateDir}).GatewaySnapshot(ctx); len(snapshot.Routes) != 0 {
			t.Errorf("still routed: %+v", snapshot.Routes)
		}
		if tokens, _ := r.AIProxyTokens(ctx); len(tokens) != 0 {
			t.Errorf("model token still honoured: %v", tokens)
		}

		// Started again, it is the same home — with a new model token, and
		// this time on an installation that opened its machines.
		spec.AIToken = "dkm_second"
		spec.Settings.Egress = config.MachineEgressOpen
		if err := machines.Start(ctx, spec); err != nil {
			t.Fatal(err)
		}
		serving(t)
		if nw, err := r.cli.NetworkInspect(ctx, name, network.InspectOptions{}); err != nil || nw.Internal {
			t.Errorf("an opened machine is still on an internal network (%v)", err)
		}
		// Restricted again takes effect on the next start too: the network
		// is made again rather than kept as it was.
		spec.Settings.Egress = ""
		if err := machines.Start(ctx, spec); err != nil {
			t.Fatal(err)
		}
		serving(t)
		if nw, err := r.cli.NetworkInspect(ctx, name, network.InspectOptions{}); err != nil || !nw.Internal {
			t.Errorf("a machine restricted again kept its open network (%v)", err)
		}
		if out, _ := run(t, "cat /data/home/note; cat /data/workspace/project/file; ls /data/workspace"); out != "kept\nmine\nproject\nsite" {
			t.Errorf("the disk after a restart:\n%s", out)
		}
		// A machine that starts has nothing open: its owner says where.
		if answer, code := control(t, spec.ControlToken, ""); code != "200" || strings.Contains(answer, `"open"`) || !strings.Contains(answer, `"name":"project"`) {
			t.Errorf("the workspace after a restart: %s %s", code, answer)
		}
		if tokens, _ := r.AIProxyTokens(ctx); !slices.Equal(tokens, []string{"dkm_second"}) {
			t.Errorf("model tokens = %v", tokens)
		}
	})

	t.Run("delete removes the disk", func(t *testing.T) {
		if err := machines.Delete(ctx, id); err != nil {
			t.Fatal(err)
		}
		if st, err := machines.Status(ctx, id); err != nil || st.State != rt.MachineAbsent {
			t.Fatalf("after delete: %+v %v", st, err)
		}
		if _, err := r.cli.VolumeInspect(ctx, name+".data"); !cerrdefs.IsNotFound(err) {
			t.Errorf("the disk is still there: %v", err)
		}
		var note machineFolderNote
		if found, _ := r.state.readRecord(&note, machineFolderNotes, id); found {
			t.Errorf("the note of its folders outlived the disk: %+v", note)
		}
	})
}
