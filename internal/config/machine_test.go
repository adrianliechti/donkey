package config

import (
	"bytes"
	"strings"
	"testing"
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

// The address sits under the one name no space can take, so no app can ever
// be given a machine's hostname by the rules that hand out app addresses.
func TestMachineHostIsOutOfReachOfSpaces(t *testing.T) {
	id := MachineID("u1")
	if got, want := MachineHost(id, "example.com"), id+".platform.example.com"; got != want {
		t.Fatalf("host = %q, want %q", got, want)
	}
	if err := ValidateSpaceName(MachineSpace); err == nil {
		t.Fatalf("%q can be taken as a space name — an app there would get machine addresses", MachineSpace)
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
	full := MachineSettings{Image: "machine:1", Storage: "20Gi", StorageClass: "fast", CPU: "500m", Memory: "2Gi", Egress: "open"}
	if err := full.Validate(); err != nil {
		t.Fatal(err)
	}
	env := full.Env()
	if got := MachineSettingsFromEnv(func(key string) string { return env[key] }); got != full {
		t.Fatalf("round trip = %+v, want %+v", got, full)
	}
}
