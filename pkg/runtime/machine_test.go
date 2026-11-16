package runtime

import (
	"context"
	"net"
	"strings"
	"testing"
	"time"
)

func machineSpec() MachineSpec {
	return MachineSpec{
		ID: "m0123456789abcdef", Owner: "sub", Email: "ada@example.com", Name: "Ada",
		Host: "machine.example.com", Token: "sealed", GuardToken: "guard", ControlToken: "control",
		AIToken: "dkm_x", Model: "model-1",
	}
}

// A machine's package managers install through the installation's mirror,
// the way a build's do — by the names the tools read themselves, so nothing
// in the machine has to know.
func TestMachineEnvPointsPackageManagersAtTheMirror(t *testing.T) {
	env := MachineEnv(machineSpec(), "http://api", "http://ai/v1", "http://donkey-packages.donkey-system.svc/")
	for name, want := range map[string]string{
		"NPM_CONFIG_REGISTRY": "http://donkey-packages.donkey-system.svc/npm/",
		// Corepack fetches pnpm and yarn themselves, and has to be told too.
		"COREPACK_NPM_REGISTRY": "http://donkey-packages.donkey-system.svc/npm",
		"PIP_INDEX_URL":         "http://donkey-packages.donkey-system.svc/pypi/",
		"UV_INDEX_URL":          "http://donkey-packages.donkey-system.svc/pypi/",
		"GOPROXY":               "http://donkey-packages.donkey-system.svc/go/",
		// Plain HTTP inside the runtime: pip and yarn have to be told.
		"PIP_TRUSTED_HOST":           "donkey-packages.donkey-system.svc",
		"YARN_UNSAFE_HTTP_WHITELIST": "donkey-packages.donkey-system.svc",
		// A machine is where lockfiles are written.
		"NPM_CONFIG_OMIT_LOCKFILE_REGISTRY_RESOLVED": "true",
		// Where only the mirror is reachable, Go's checksum database is not.
		"GOSUMDB": "off",
	} {
		if env[name] != want {
			t.Errorf("%s = %q, want %q", name, env[name], want)
		}
	}
	// `uv add` would write this one into pyproject.toml.
	if _, set := env["UV_DEFAULT_INDEX"]; set {
		t.Error("UV_DEFAULT_INDEX is set")
	}
	// A port in the mirror's address is part of the host pip trusts.
	if got := MachineEnv(machineSpec(), "", "", "http://donkey-packages:8090")["PIP_TRUSTED_HOST"]; got != "donkey-packages:8090" {
		t.Errorf("PIP_TRUSTED_HOST = %q", got)
	}
}

// Without a mirror, a model or an address, the names are left out rather
// than set to nothing: an empty registry setting breaks a package manager
// that would have worked with its default.
func TestMachineEnvLeavesOutWhatIsNotThere(t *testing.T) {
	spec := machineSpec()
	spec.AIToken, spec.Email = "", ""
	env := MachineEnv(spec, "http://api", "http://ai/v1", "")
	for name, value := range env {
		if value == "" {
			t.Errorf("%s is set to nothing", name)
		}
		if strings.HasPrefix(name, "NPM_") || strings.HasPrefix(name, "PIP_") || name == "GOPROXY" || name == "GOSUMDB" ||
			strings.HasPrefix(name, "OPENAI_") || name == "WINGMAN_MODEL" || name == "DONKEY_MACHINE_USER_EMAIL" {
			t.Errorf("%s is set without anything behind it", name)
		}
	}
	if env["DONKEY_TOKEN"] != "sealed" || env["DONKEY_SERVER_URL"] != "http://api" || env["DONKEY_MACHINE_GUARD_TOKEN"] != "guard" {
		t.Errorf("env = %v", env)
	}
}

func TestMachineEnvModel(t *testing.T) {
	env := MachineEnv(machineSpec(), "http://api", "http://ai/v1", "")
	if env["OPENAI_BASE_URL"] != "http://ai/v1" || env[MachineAIKey] != "dkm_x" || env["WINGMAN_MODEL"] != "model-1" {
		t.Errorf("env = %v", env)
	}
}

// The machine is told how long it may go unused, because it is the one that
// knows — and is told nothing where the installation lets machines run.
func TestMachineEnvIdle(t *testing.T) {
	spec := machineSpec()
	env := MachineEnv(spec, "http://api", "", "")
	if env["DONKEY_MACHINE_IDLE"] != "1h0m0s" || env["DONKEY_MACHINE_CONTROL_TOKEN"] != "control" {
		t.Errorf("env = %v", env)
	}
	spec.Settings.Idle = "90m"
	if got := MachineEnv(spec, "http://api", "", "")["DONKEY_MACHINE_IDLE"]; got != "1h30m0s" {
		t.Errorf("idle = %q", got)
	}
	spec.Settings.Idle = "never"
	if _, set := MachineEnv(spec, "http://api", "", "")["DONKEY_MACHINE_IDLE"]; set {
		t.Error("a machine that is never stopped was given an idle time")
	}
}

// A machine reads "running" before every way to it is there. Whoever
// connects right then waits for the door instead of failing.
func TestMachineDialerWaitsForADoorThatIsNotThereYet(t *testing.T) {
	reserved, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	address := reserved.Addr().String()
	reserved.Close()

	opened := make(chan net.Listener, 1)
	go func() {
		time.Sleep(600 * time.Millisecond)
		door, err := net.Listen("tcp", address)
		if err != nil {
			close(opened)
			return
		}
		opened <- door
	}()
	began := time.Now()
	conn, err := MachineDialer(10*time.Second)(context.Background(), "tcp", address)
	door, ok := <-opened
	if !ok {
		t.Skip("the port was taken by somebody else in the meantime")
	}
	defer door.Close()
	if err != nil {
		t.Fatalf("gave up on a door that opened after %s: %v", time.Since(began), err)
	}
	conn.Close()
	if waited := time.Since(began); waited < 500*time.Millisecond {
		t.Errorf("connected after %s, before the door was there", waited)
	}
}

// The wait has an end, and what the caller reads then is why the machine did
// not answer — not that the waiting stopped.
func TestMachineDialerGivesUp(t *testing.T) {
	reserved, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	address := reserved.Addr().String()
	reserved.Close()

	began := time.Now()
	_, err = MachineDialer(700*time.Millisecond)(context.Background(), "tcp", address)
	if err == nil {
		t.Fatal("connected to nothing")
	}
	if waited := time.Since(began); waited < 600*time.Millisecond || waited > 5*time.Second {
		t.Errorf("gave up after %s, want about 700ms", waited)
	}
	if !strings.Contains(err.Error(), "refused") {
		t.Errorf("error = %v, want the refusal", err)
	}

	// Whoever stops waiting first ends it: the caller's own deadline counts.
	ctx, cancel := context.WithTimeout(context.Background(), 300*time.Millisecond)
	defer cancel()
	began = time.Now()
	if _, err := MachineDialer(time.Minute)(ctx, "tcp", address); err == nil || time.Since(began) > 5*time.Second {
		t.Errorf("after the caller gave up: err = %v, waited %s", err, time.Since(began))
	}
}
