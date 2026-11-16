package docker

import (
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/adrianliechti/donkey/internal/config"
	rt "github.com/adrianliechti/donkey/pkg/runtime"
)

const testMachine = "m0123456789abcdef"

// A machine's names sit under "platform", which no space can be called — so
// its container, volume and network can never be an app's, and its labels
// carry no space or app for an app sweep to find.
func TestMachineNamesAndLabels(t *testing.T) {
	if got := machineName(testMachine); got != "donkey.platform."+testMachine {
		t.Fatalf("name = %q", got)
	}
	if err := config.ValidateSpaceName(config.MachineSpace); err == nil {
		t.Fatal("the machines' name can be taken by a space")
	}
	labels := machineLabels(testMachine, roleMachine)
	if labels[labelMachine] != testMachine || labels[labelRole] != roleMachine || labels[labelSpace] != "" || labels[labelApp] != "" {
		t.Fatalf("labels = %v", labels)
	}
	// Identity includes the machine: one machine's container is not another's,
	// and neither is an app's workload.
	if sameIdentity(labels, machineLabels("m00000000000000ff", roleMachine)) {
		t.Error("two machines share an identity")
	}
	if sameIdentity(labels, appLabels(config.MachineSpace, testMachine, roleWorkload)) {
		t.Error("a machine and an app share an identity")
	}
	if MatchesSystemLabels(labels, roleMachine) {
		t.Error("a machine passes as part of the control plane")
	}
}

// The gateway reads a machine's route from the state volume like any other,
// with the owner it admits.
func TestGatewaySnapshotLoadsMachineRoutes(t *testing.T) {
	dir := t.TempDir()
	s := state{dir: dir}
	route := config.MachineRoute(testMachine, "owner-subject", "machine.example.com", "http://donkey.platform."+testMachine+":8080")
	if err := s.setRoute(config.MachineSpace, testMachine, route); err != nil {
		t.Fatal(err)
	}
	snapshot, err := (&GatewayAdapter{StateDir: dir}).GatewaySnapshot(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(snapshot.Routes) != 1 {
		t.Fatalf("routes = %+v", snapshot.Routes)
	}
	got := snapshot.Routes[0]
	if got.Machine != testMachine || got.Owner != "owner-subject" || !got.Login || got.Space != "" || got.App != "" {
		t.Fatalf("route = %+v", got)
	}
	if err := s.removeRoute(config.MachineSpace, testMachine); err != nil {
		t.Fatal(err)
	}
	if snapshot, _ := (&GatewayAdapter{StateDir: dir}).GatewaySnapshot(context.Background()); len(snapshot.Routes) != 0 {
		t.Fatalf("a stopped machine is still routed: %+v", snapshot.Routes)
	}
}

// donkey-ai honours a machine's model token while the machine's record
// exists and names this installation's endpoint — the record goes when the
// machine stops.
func TestMachineAITokens(t *testing.T) {
	r := &Runtime{state: state{dir: t.TempDir()}, opts: Options{AIContainer: "donkey-ai"}}
	write := func(id, baseURL, token string) {
		t.Helper()
		record := machineRecord{Owner: "u", Env: map[string]string{"OPENAI_BASE_URL": baseURL, rt.MachineAIKey: token}}
		if err := r.state.writeRecord(record, machineRecords, id); err != nil {
			t.Fatal(err)
		}
	}
	write(testMachine, r.aiProxyURL(), "dkm_running")
	write("m00000000000000ff", "https://api.openai.com/v1", "sk-real")
	write("not-a-machine", r.aiProxyURL(), "dkm_forged")

	tokens, err := r.AIProxyTokens(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(tokens) != 1 || tokens[0] != "dkm_running" {
		t.Fatalf("tokens = %v, want the running machine's alone", tokens)
	}
	if err := r.state.remove(machineRecords, testMachine); err != nil {
		t.Fatal(err)
	}
	if tokens, _ := r.AIProxyTokens(context.Background()); len(tokens) != 0 {
		t.Fatalf("a stopped machine's token is still honoured: %v", tokens)
	}
}

func TestMachineVerbsRefuseOtherNames(t *testing.T) {
	machines := (&Runtime{state: state{dir: t.TempDir()}}).Machines()
	ctx := context.Background()
	// Each of these would become part of a container name, which the engine
	// client joins into its request path.
	for _, id := range []string{"", "web", "../" + testMachine, "m0123"} {
		if _, err := machines.Status(ctx, id); err == nil {
			t.Errorf("Status(%q) accepted", id)
		}
		if err := machines.Start(ctx, rt.MachineSpec{ID: id}); err == nil {
			t.Errorf("Start(%q) accepted", id)
		}
		if err := machines.Stop(ctx, id); err == nil {
			t.Errorf("Stop(%q) accepted", id)
		}
		if err := machines.Delete(ctx, id); err == nil {
			t.Errorf("Delete(%q) accepted", id)
		}
		if err := machines.Logs(ctx, id, rt.LogOptions{}, nil); err == nil {
			t.Errorf("Logs(%q) accepted", id)
		}
	}
}

// What a machine's disk holds is kept in a note beside its records. The note
// is not one of them: those hold credentials and go when the machine stops,
// and donkey-ai reads them for the tokens it honours.
func TestMachineRemembersItsFolders(t *testing.T) {
	r := &Runtime{state: state{dir: t.TempDir()}, opts: Options{AIContainer: "donkey-ai"}}
	machines := r.Machines()
	ctx := context.Background()
	used := time.Date(2026, 11, 13, 9, 30, 0, 0, time.UTC)
	folders := []rt.MachineFolder{{Name: "shop", Modified: used}, {Name: "site", Modified: used.Add(-48 * time.Hour)}}
	if err := machines.Remember(ctx, testMachine, folders); err != nil {
		t.Fatal(err)
	}
	var note machineFolderNote
	if found, err := r.state.readRecord(&note, machineFolderNotes, testMachine); !found || err != nil {
		t.Fatalf("no note was kept: %v", err)
	}
	if len(note.Folders) != 2 || note.Folders[0].Name != "shop" || !note.Folders[0].Modified.Equal(used) {
		t.Fatalf("note = %+v", note)
	}
	if tokens, err := r.AIProxyTokens(ctx); err != nil || len(tokens) != 0 {
		t.Fatalf("a note of folders was read as a running machine: %v %v", tokens, err)
	}
	var many []rt.MachineFolder
	for i := range rt.MaxMachineFolders + 20 {
		many = append(many, rt.MachineFolder{Name: fmt.Sprintf("f%03d", i), Modified: used})
	}
	if err := machines.Remember(ctx, testMachine, many); err != nil {
		t.Fatal(err)
	}
	if _, err := r.state.readRecord(&note, machineFolderNotes, testMachine); err != nil || len(note.Folders) != rt.MaxMachineFolders {
		t.Fatalf("kept %d folders (%v)", len(note.Folders), err)
	}
	for _, id := range []string{"", "web", "../" + testMachine} {
		if err := machines.Remember(ctx, id, folders); err == nil {
			t.Errorf("Remember(%q) accepted", id)
		}
	}
}
