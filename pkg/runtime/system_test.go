package runtime

import (
	"strings"
	"testing"
)

// Every runtime lists the control plane in the same order, whatever order
// its own API handed the parts over in.
func TestSortComponentsReadsTheControlPlaneFrontToBack(t *testing.T) {
	components := []Component{
		{Name: "donkey-registry", Role: "registry"},
		{Name: "zeta", Role: "something-new"},
		{Name: "donkey-gateway", Role: "gateway"},
		{Name: "alpha", Role: "something-else"},
		{Name: "donkey-buildkitd", Role: "buildkitd"},
		{Name: "donkey-server", Role: "server"},
		{Name: "donkey-orchestrator", Role: "orchestrator"},
	}
	SortComponents(components)
	var names []string
	for _, component := range components {
		names = append(names, component.Name)
	}
	// Parts this list has not heard of follow, by name.
	want := "donkey-server donkey-orchestrator donkey-gateway donkey-buildkitd donkey-registry alpha zeta"
	if got := strings.Join(names, " "); got != want {
		t.Fatalf("order: %s", got)
	}
}

func TestComponentIsHealthyWhenEveryInstanceItShouldHaveIsReady(t *testing.T) {
	for _, tc := range []struct {
		ready, want int32
		healthy     bool
	}{{1, 1, true}, {2, 1, true}, {1, 2, false}, {0, 1, false}, {0, 0, false}} {
		if got := (Component{Ready: tc.ready, Want: tc.want}).Healthy(); got != tc.healthy {
			t.Errorf("%d of %d: healthy=%t", tc.ready, tc.want, got)
		}
	}
}
