package kubernetes

import (
	"context"
	"fmt"
	"strings"
	"testing"
	"time"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	kruntime "k8s.io/apimachinery/pkg/runtime"
	"k8s.io/client-go/kubernetes/fake"

	"github.com/adrianliechti/donkey/internal/config"
	rt "github.com/adrianliechti/donkey/pkg/runtime"
	"github.com/adrianliechti/donkey/pkg/runtime/kubernetes/manifests"
)

const (
	testMachine      = "m0123456789abcdef"
	testMachineSpace = "donkey-platform"
)

func machineRuntime(objs ...kruntime.Object) *Runtime {
	return NewRuntime(&Client{Clientset: fake.NewClientset(objs...)}, Options{
		Placement:       manifests.Placement{NamespacePrefix: "donkey-"},
		SystemNamespace: "donkey-system",
	})
}

// stamped is how an object looks once donkey applied it.
func stamped(labels map[string]string) map[string]string {
	labels[manifests.LabelInstallation] = "donkey-system"
	return labels
}

func machineDeployment() *appsv1.Deployment {
	return &appsv1.Deployment{ObjectMeta: metav1.ObjectMeta{
		Name: testMachine, Namespace: testMachineSpace,
		Labels: stamped(manifests.MachineLabels(testMachine, manifests.RoleMachine)),
		Annotations: map[string]string{
			manifests.AnnotationImage:         "machine:1",
			manifests.AnnotationHost:          "machine.example.com",
			manifests.AnnotationSignInExpires: "2026-12-01T10:00:00Z",
		},
	}}
}

func machinePod(name string, ready bool, waiting string) *corev1.Pod {
	pod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name: name, Namespace: testMachineSpace,
			Labels: stamped(manifests.MachineLabels(testMachine, manifests.RoleMachine)),
		},
		Status: corev1.PodStatus{StartTime: &metav1.Time{Time: metav1.Now().Time}},
	}
	status := corev1.ConditionFalse
	if ready {
		status = corev1.ConditionTrue
	}
	pod.Status.Conditions = []corev1.PodCondition{{Type: corev1.PodReady, Status: status}}
	if waiting != "" {
		pod.Status.ContainerStatuses = []corev1.ContainerStatus{{
			RestartCount: 4,
			State:        corev1.ContainerState{Waiting: &corev1.ContainerStateWaiting{Reason: waiting}},
		}}
	}
	return pod
}

func machineClaim() *corev1.PersistentVolumeClaim {
	return &corev1.PersistentVolumeClaim{ObjectMeta: metav1.ObjectMeta{
		Name: manifests.MachineDataClaimName(testMachine), Namespace: testMachineSpace,
		Labels: stamped(manifests.MachineLabels(testMachine, manifests.RoleMachineStorage)),
	}}
}

// A machine's state is read off what exists: nothing, a disk alone, a
// workload that is coming up, serving, or stuck.
func TestMachineStatus(t *testing.T) {
	for name, tc := range map[string]struct {
		objs []kruntime.Object
		want rt.MachineState
	}{
		"nothing":      {nil, rt.MachineAbsent},
		"a disk alone": {[]kruntime.Object{machineClaim()}, rt.MachineStopped},
		"no pod yet":   {[]kruntime.Object{machineClaim(), machineDeployment()}, rt.MachineStarting},
		"coming up":    {[]kruntime.Object{machineClaim(), machineDeployment(), machinePod("a", false, "")}, rt.MachineStarting},
		"serving":      {[]kruntime.Object{machineClaim(), machineDeployment(), machinePod("a", true, "")}, rt.MachineRunning},
		"crashing":     {[]kruntime.Object{machineClaim(), machineDeployment(), machinePod("a", false, "CrashLoopBackOff")}, rt.MachineFailed},
		"no image":     {[]kruntime.Object{machineClaim(), machineDeployment(), machinePod("a", false, "ImagePullBackOff")}, rt.MachineFailed},
	} {
		t.Run(name, func(t *testing.T) {
			st, err := machineRuntime(tc.objs...).Machines().Status(context.Background(), testMachine)
			if err != nil {
				t.Fatal(err)
			}
			if st.State != tc.want {
				t.Fatalf("state = %q, want %q (%s)", st.State, tc.want, st.Message)
			}
			if st.Exists() != (tc.want != rt.MachineAbsent) {
				t.Errorf("Exists() = %v for %q", st.Exists(), st.State)
			}
			if tc.want == rt.MachineFailed && (st.Message == "" || st.Restarts != 4) {
				t.Errorf("a failing machine says %q after %d restarts", st.Message, st.Restarts)
			}
			if tc.want == rt.MachineRunning {
				if st.Host != "machine.example.com" || st.Image != "machine:1" || st.SignInExpires.IsZero() || st.StartedAt.IsZero() {
					t.Errorf("status = %+v", st)
				}
			}
		})
	}
}

// A same-named Deployment that is not this machine's — an app's, another
// installation's — is never reported as the machine.
func TestMachineStatusRefusesSomebodyElsesDeployment(t *testing.T) {
	foreign := machineDeployment()
	foreign.Labels = stamped(manifests.Labels("team", testMachine, manifests.RoleWorkload))
	if _, err := machineRuntime(foreign).Machines().Status(context.Background(), testMachine); err == nil {
		t.Fatal("an app's Deployment was read as a machine")
	}
	other := machineDeployment()
	other.Labels[manifests.LabelInstallation] = "another-donkey"
	if _, err := machineRuntime(other).Machines().Status(context.Background(), testMachine); err == nil {
		t.Fatal("another installation's Deployment was read as this one's machine")
	}
}

func TestMachineVerbsRefuseOtherNames(t *testing.T) {
	machines := machineRuntime().Machines()
	ctx := context.Background()
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
	}
}

func machineSecret(id, baseURL, token string, labels map[string]string) *corev1.Secret {
	return &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: id + "-machine", Namespace: testMachineSpace, Labels: labels},
		Data:       map[string][]byte{"OPENAI_BASE_URL": []byte(baseURL), rt.MachineAIKey: []byte(token)},
	}
}

// donkey-ai honours a machine's model token for as long as the machine's
// Secret holds it for this installation's endpoint — and nothing that merely
// looks like one.
func TestMachineAITokens(t *testing.T) {
	const ai = "http://donkey-ai.donkey-system.svc/v1"
	other := "m00000000000000ff"
	r := machineRuntime(
		machineSecret(testMachine, ai, "dkm_running", stamped(manifests.MachineLabels(testMachine, manifests.RoleMachineConfig))),
		// Pointed at somebody else's endpoint: not ours to honour.
		machineSecret(other, "https://api.openai.com/v1", "sk-real", stamped(manifests.MachineLabels(other, manifests.RoleMachineConfig))),
		// An app's Secret dressed up with the key.
		machineSecret("m00000000000000aa", ai, "dkm_forged", stamped(manifests.Labels("team", "web", manifests.RoleConfig))),
		// Another installation's machine.
		machineSecret("m00000000000000bb", ai, "dkm_foreign", func() map[string]string {
			l := manifests.MachineLabels("m00000000000000bb", manifests.RoleMachineConfig)
			l[manifests.LabelInstallation] = "another-donkey"
			return l
		}()),
	)
	tokens, err := r.AIProxyTokens(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(tokens) != 1 || tokens[0] != "dkm_running" {
		t.Fatalf("tokens = %v, want the running machine's alone", tokens)
	}
}

// The gateway takes a machine route's identity from its labels, which have
// to agree with the record; the owner it admits comes with it.
func TestMachineRouteFromConfigMap(t *testing.T) {
	route := config.MachineRoute(testMachine, "owner-subject", "machine.example.com", "http://"+testMachine+".donkey-platform.svc:80")
	cm := func(labels map[string]string, r config.GatewayRoute) corev1.ConfigMap {
		return corev1.ConfigMap{ObjectMeta: metav1.ObjectMeta{Name: "route", Labels: labels}, Data: manifests.RouteData(r)}
	}
	got, ok := routeFromConfigMap(cm(manifests.MachineLabels(testMachine, manifests.RoleMachineRoute), route))
	if !ok || got.Machine != testMachine || got.Owner != "owner-subject" || !got.Login || got.Space != "" || got.App != "" || got.Token {
		t.Fatalf("route = %+v (ok %v)", got, ok)
	}

	stolen := route
	stolen.Owner = ""
	renamed := route
	renamed.Machine = "m00000000000000ff"
	for name, bad := range map[string]corev1.ConfigMap{
		"no owner":                  cm(manifests.MachineLabels(testMachine, manifests.RoleMachineRoute), stolen),
		"label and record disagree": cm(manifests.MachineLabels(testMachine, manifests.RoleMachineRoute), renamed),
		"an app claiming a machine": cm(manifests.Labels("team", "web", manifests.RoleRoute), route),
		"a machine with app labels": cm(func() map[string]string {
			l := manifests.MachineLabels(testMachine, manifests.RoleMachineRoute)
			l[manifests.LabelSpace] = "team"
			return l
		}(), route),
		"a machine label, app route": cm(manifests.MachineLabels(testMachine, manifests.RoleMachineRoute), config.GatewayRoute{Host: "x.example.com", Backend: "http://x.team.svc:80"}),
	} {
		if got, ok := routeFromConfigMap(bad); ok {
			t.Errorf("%s: served as %+v", name, got)
		}
	}
}

// What a machine's disk holds is noted on the disk, the one object that is
// still there when the machine is stopped — so a stopped machine can say
// what it would open.
func TestMachineRemembersItsFolders(t *testing.T) {
	ctx := context.Background()
	r := machineRuntime(machineClaim())
	machines := r.Machines()
	client := r.c.Clientset.(*fake.Clientset)
	patches := func() (n int) {
		for _, action := range client.Actions() {
			if action.GetVerb() == "patch" && action.GetResource().Resource == "persistentvolumeclaims" {
				n++
			}
		}
		return n
	}
	used := time.Date(2026, 11, 13, 9, 30, 0, 0, time.UTC)
	folders := []rt.MachineFolder{{Name: "shop", Modified: used}, {Name: "site", Modified: used.Add(-48 * time.Hour)}}

	if st, _ := machines.Status(ctx, testMachine); st.State != rt.MachineStopped || len(st.Folders) != 0 {
		t.Fatalf("before anything was kept: %+v", st)
	}
	if err := machines.Remember(ctx, testMachine, folders); err != nil {
		t.Fatal(err)
	}
	st, err := machines.Status(ctx, testMachine)
	if err != nil || len(st.Folders) != 2 || st.Folders[0].Name != "shop" || !st.Folders[0].Modified.Equal(used) || st.Folders[1].Name != "site" {
		t.Fatalf("a stopped machine remembers %+v (%v)", st.Folders, err)
	}
	// The same list again is not written again.
	if err := machines.Remember(ctx, testMachine, folders); err != nil || patches() != 1 {
		t.Fatalf("an unchanged list was written %d times (%v)", patches(), err)
	}
	// A workspace of very many folders is remembered by the ones used last.
	var many []rt.MachineFolder
	for i := range rt.MaxMachineFolders + 20 {
		many = append(many, rt.MachineFolder{Name: fmt.Sprintf("f%03d", i), Modified: used})
	}
	if err := machines.Remember(ctx, testMachine, many); err != nil {
		t.Fatal(err)
	}
	if st, _ := machines.Status(ctx, testMachine); len(st.Folders) != rt.MaxMachineFolders || st.Folders[0].Name != "f000" {
		t.Fatalf("kept %d folders", len(st.Folders))
	}

	// A machine without a disk has nothing to keep a note with — and nothing
	// is made for it.
	bare := machineRuntime()
	if err := bare.Machines().Remember(ctx, testMachine, folders); err != nil {
		t.Fatalf("without a disk: %v", err)
	}
	if st, _ := bare.Machines().Status(ctx, testMachine); st.State != rt.MachineAbsent {
		t.Fatalf("remembering made a machine: %+v", st)
	}
	// Somebody else's disk is never written to, and what is not a machine's
	// name is refused.
	foreign := machineClaim()
	foreign.Labels = stamped(manifests.Labels("team", testMachine, manifests.RoleWorkload))
	if err := machineRuntime(foreign).Machines().Remember(ctx, testMachine, folders); err == nil {
		t.Fatal("an app's disk was annotated as a machine's")
	}
	if err := machines.Remember(ctx, "../"+testMachine, folders); err == nil {
		t.Fatal("a name that is not a machine's was accepted")
	}
	// A note that does not read is no note.
	broken := machineClaim()
	broken.Annotations = map[string]string{manifests.AnnotationMachineFolders: "not json"}
	if st, err := machineRuntime(broken).Machines().Status(ctx, testMachine); err != nil || st.State != rt.MachineStopped || len(st.Folders) != 0 {
		t.Fatalf("with an unreadable note: %+v %v", st, err)
	}
}

// A machine made right after one was deleted must not land on the old
// machine's disk: that claim is on its way out, and a start waits for it to
// be gone rather than apply a new machine onto it.
func TestMachineStartWaitsForADeletedDisk(t *testing.T) {
	ctx := context.Background()
	claimName := manifests.MachineDataClaimName(testMachine)

	// Nothing there, or a disk that is simply there: nothing to wait for.
	for name, objs := range map[string][]kruntime.Object{"no disk": nil, "a disk": {machineClaim()}} {
		begun := time.Now()
		if err := machineRuntime(objs...).awaitMachineDisk(ctx, testMachineSpace, claimName); err != nil || time.Since(begun) > time.Second {
			t.Fatalf("%s: %v after %s", name, err, time.Since(begun))
		}
	}

	// A disk that is leaving is waited for, for as long as it takes to go.
	leaving := machineClaim()
	leaving.DeletionTimestamp = &metav1.Time{Time: time.Now()}
	leaving.Finalizers = []string{"kubernetes.io/pvc-protection"}
	r := machineRuntime(leaving)
	done := make(chan error, 1)
	go func() { done <- r.awaitMachineDisk(ctx, testMachineSpace, claimName) }()
	select {
	case err := <-done:
		t.Fatalf("a start went ahead onto a disk that is being deleted (%v)", err)
	case <-time.After(700 * time.Millisecond):
	}
	client := r.c.Clientset.(*fake.Clientset)
	if err := client.Tracker().Delete(corev1.SchemeGroupVersion.WithResource("persistentvolumeclaims"), testMachineSpace, claimName); err != nil {
		t.Fatal(err)
	}
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("once the old disk was gone: %v", err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("the start kept waiting for a disk that is gone")
	}

	// One that does not go in time is said to be in the way.
	stuck := machineRuntime(leaving.DeepCopy())
	short, cancel := context.WithTimeout(ctx, 300*time.Millisecond)
	defer cancel()
	if err := stuck.awaitMachineDisk(short, testMachineSpace, claimName); err == nil || !strings.Contains(err.Error(), "still being removed") {
		t.Fatalf("a disk that does not go: %v", err)
	}
}
