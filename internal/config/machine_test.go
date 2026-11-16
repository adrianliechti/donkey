package config

import (
	"bytes"
	"strings"
	"testing"
	"time"
)

// A machine's name ends up in a hostname, so it has to be a DNS label that is
// the same for the same person every time and says nothing about them.
func TestMachineID(t *testing.T) {
	a, b := MachineID("auth0|ada@example.com"), MachineID("auth0|grace@example.com")
	if a != MachineID("auth0|ada@example.com") {
		t.Fatal("the same subject produced two names")
	}
	if a == b {
		t.Fatal("two subjects share a machine")
	}
	for _, id := range []string{a, b, MachineID("")} {
		if !ValidMachineID(id) || !ValidName(id) {
			t.Errorf("%q is not a usable name", id)
		}
		if strings.Contains(id, "ada") || strings.Contains(id, "grace") {
			t.Errorf("%q gives its owner away", id)
		}
	}
	for _, id := range []string{"", "web", "m0123", "M0123456789ABCDEF", "m0123456789abcdeg", "m0123456789abcdef0", "../m0123456789abcdef"} {
		if ValidMachineID(id) {
			t.Errorf("%q passes as a machine", id)
		}
	}
}

// Machines answer on one address for the whole installation: nothing about a
// machine or its owner is in it, so a start has no name to set up. It has one
// label under the domain, where the rules that hand out app addresses always
// write two, and machines live under the one name no space can take.
func TestMachineHostIsOneForTheInstallation(t *testing.T) {
	if got, want := MachineHost("example.com"), "machine.example.com"; got != want {
		t.Fatalf("host = %q, want %q", got, want)
	}
	if strings.Contains(MachineHost("example.com"), MachineID("u1")) {
		t.Fatal("the address names a machine")
	}
	if err := ValidateSpaceName(MachineSpace); err == nil {
		t.Fatalf("%q can be taken as a space name — an app there would sit beside the machines", MachineSpace)
	}
}

// One credential opens one machine, under one session key.
func TestMachineGuardToken(t *testing.T) {
	key, other := bytes.Repeat([]byte{1}, 32), bytes.Repeat([]byte{2}, 32)
	a := MachineGuardToken(key, "m0000000000000001")
	if a != MachineGuardToken(key, "m0000000000000001") {
		t.Fatal("the credential is not stable")
	}
	if a == MachineGuardToken(key, "m0000000000000002") {
		t.Fatal("two machines share a credential")
	}
	if a == MachineGuardToken(other, "m0000000000000001") {
		t.Fatal("the credential survives a new session key")
	}
	if len(a) != 64 {
		t.Fatalf("credential length = %d", len(a))
	}
}

func TestMachineSettings(t *testing.T) {
	if (MachineSettings{}).Enabled() {
		t.Fatal("machines are on without an image")
	}
	if err := (MachineSettings{}).Validate(); err != nil {
		t.Fatal(err)
	}
	on := MachineSettings{Image: "ghcr.io/acme/donkey-machine:1.2.3"}
	if err := on.Validate(); err != nil {
		t.Fatal(err)
	}
	if on.StorageSize() != DefaultMachineStorage || on.CPULimit() != DefaultMachineCPU || on.MemoryLimit() != DefaultMachineMemory {
		t.Fatalf("defaults: %s %s %s", on.StorageSize(), on.CPULimit(), on.MemoryLimit())
	}
	for name, bad := range map[string]MachineSettings{
		"settings without an image": {Storage: "20Gi"},
		"image":                     {Image: "Not An Image"},
		"storage":                   {Image: "machine:1", Storage: "lots"},
		"memory":                    {Image: "machine:1", Memory: "4 gigs"},
		"cpu":                       {Image: "machine:1", CPU: "two"},
		"egress":                    {Image: "machine:1", Egress: "internet"},
		"egress without an image":   {Egress: "open"},
		"idle":                      {Image: "machine:1", Idle: "an hour"},
		"idle in seconds":           {Image: "machine:1", Idle: "30s"},
		"idle as a bare number":     {Image: "machine:1", Idle: "60"},
		"idle without an image":     {Idle: "2h"},
	} {
		if err := bad.Validate(); err == nil {
			t.Errorf("%s: accepted %+v", name, bad)
		}
	}
	// A machine reaches the control plane and nothing beyond it unless the
	// installation says otherwise — and says so in so many words.
	if !on.RestrictEgress() || on.EgressMode() != MachineEgressRestricted {
		t.Fatalf("a machine is open by default: %q", on.EgressMode())
	}
	for egress, restricted := range map[string]bool{"": true, "restricted": true, "open": false} {
		m := MachineSettings{Image: "machine:1", Egress: egress}
		if err := m.Validate(); err != nil || m.RestrictEgress() != restricted {
			t.Errorf("egress %q: restricted = %v (%v)", egress, m.RestrictEgress(), err)
		}
	}
	// What the installer writes is what the control plane reads.
	// A machine nobody uses stops after an hour unless the installation
	// names another time — or says, in so many words, never.
	for idle, want := range map[string]time.Duration{"": time.Hour, "60m": time.Hour, "2h": 2 * time.Hour, "90m": 90 * time.Minute, "never": 0} {
		m := MachineSettings{Image: "machine:1", Idle: idle}
		if err := m.Validate(); err != nil || m.IdleTimeout() != want {
			t.Errorf("idle %q: %s (%v), want %s", idle, m.IdleTimeout(), err, want)
		}
	}
	full := MachineSettings{Image: "machine:1", Storage: "20Gi", StorageClass: "fast", CPU: "500m", Memory: "2Gi", Egress: "open", Idle: "2h"}
	if err := full.Validate(); err != nil {
		t.Fatal(err)
	}
	env := full.Env()
	if got := MachineSettingsFromEnv(func(key string) string { return env[key] }); got != full {
		t.Fatalf("round trip = %+v, want %+v", got, full)
	}
}

// The control plane's credential at a machine is its own: neither derivable
// from the gateway's nor the same for two machines.
func TestMachineControlToken(t *testing.T) {
	key := []byte("0123456789abcdef0123456789abcdef")
	control := MachineControlToken(key, "m0000000000000001")
	if control != MachineControlToken(key, "m0000000000000001") || len(control) != 64 {
		t.Fatalf("credential = %q", control)
	}
	if control == MachineGuardToken(key, "m0000000000000001") {
		t.Fatal("the control plane and the gateway present the same credential")
	}
	if control == MachineControlToken(key, "m0000000000000002") {
		t.Fatal("two machines share a credential")
	}
}

func TestMachineFolderNames(t *testing.T) {
	for _, name := range []string{"shop", "shop-2", "My.App_v2", "0", strings.Repeat("a", 63)} {
		if !ValidMachineFolder(name) || !SafeMachineFolder(name) {
			t.Errorf("%q refused", name)
		}
	}
	for _, name := range []string{"", ".", "..", ".git", "-shop", "my shop", "shop/src", `shop\src`, "söp", strings.Repeat("a", 64)} {
		if ValidMachineFolder(name) {
			t.Errorf("%q accepted for a new folder", name)
		}
	}
	// A folder that is already there only has to stay inside the workspace
	// and in sight.
	for _, name := range []string{"my shop", "söp", "Shop (old)", "-shop"} {
		if !SafeMachineFolder(name) {
			t.Errorf("%q refused for a folder that exists", name)
		}
	}
	for _, name := range []string{"", ".", "..", ".git", "shop/src", `shop\src`, "a\x00b", "a\nb", strings.Repeat("a", 256)} {
		if SafeMachineFolder(name) {
			t.Errorf("%q accepted", name)
		}
	}
}
