package kubernetes

import (
	"strings"
	"testing"

	"github.com/adrianliechti/donkey/internal/config"
	"github.com/adrianliechti/donkey/internal/servicepolicy"
	rt "github.com/adrianliechti/donkey/pkg/runtime"
	"github.com/adrianliechti/donkey/pkg/runtime/kubernetes/manifests"
)

// Both choices pass in an empty space. Only one may commit.
func TestConcurrentSpaceServicePolicy(t *testing.T) {
	p, err := servicepolicy.Compile(&config.ServicePolicy{Rego: `package donkey.services
deny contains "only one instance in this space" if { count(input.services) > 1 }`}, nil)
	if err != nil {
		t.Fatal(err)
	}
	r := NewRuntime(pluginTestClient(t), Options{SystemNamespace: "main", Placement: manifests.Placement{SharedNamespace: "workloads"}, ServicePolicy: p})
	start, results := make(chan struct{}), make(chan error, 2)
	for _, name := range []string{"first", "second"} {
		go func() {
			<-start
			_, _, err := r.EnsureService(t.Context(), "team", rt.ServiceSpec{Name: name, Type: "postgres", External: true, URL: "postgres://db.example/test"})
			results <- err
		}()
	}
	close(start)
	allowed, denied := 0, 0
	for range 2 {
		switch err := <-results; {
		case err == nil:
			allowed++
		case strings.Contains(err.Error(), "only one instance"):
			denied++
		default:
			t.Fatal(err)
		}
	}
	if allowed != 1 || denied != 1 {
		t.Fatalf("allowed %d, denied %d", allowed, denied)
	}
}
