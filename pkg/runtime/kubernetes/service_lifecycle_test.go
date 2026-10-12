package kubernetes

import (
	"context"
	"sync"
	"testing"

	"github.com/adrianliechti/donkey/internal/addons"
	rt "github.com/adrianliechti/donkey/pkg/runtime"
	"github.com/adrianliechti/donkey/pkg/runtime/kubernetes/manifests"
)

func TestSharedProxyCreationAndRemoval(t *testing.T) {
	for _, typ := range []string{"openai", "exa"} {
		t.Run(typ, func(t *testing.T) {
			r := NewRuntime(pluginTestClient(t), Options{Placement: manifests.Placement{SharedNamespace: "workloads"}})
			ctx := context.Background()
			entry, _ := addons.Get(ctx, typ)
			a := entry.(addons.Builtin)
			const requests = 8
			values := make(chan string, requests)
			failures := make(chan error, requests)
			var workers sync.WaitGroup
			for range requests {
				workers.Go(func() {
					binding, _, err := r.EnsureService(ctx, "team", rt.ServiceSpec{Type: typ, Name: "proxy"})
					if err != nil {
						failures <- err
						return
					}
					values <- binding[a.CredentialKey()]
				})
			}
			workers.Wait()
			close(values)
			close(failures)
			for err := range failures {
				t.Fatal(err)
			}
			var credential string
			for v := range values {
				if v == "" || credential != "" && v != credential {
					t.Fatal("apps received different credentials for the same service")
				}
				credential = v
			}
			if _, _, err := r.EnsureService(ctx, "team", rt.ServiceSpec{Type: "s3", Name: "proxy", External: true, URL: "https://store"}); err == nil {
				t.Fatal("replaced proxy with another service type")
			}
			if _, err := r.RemoveService(ctx, "team", "proxy", true, false); err == nil {
				t.Fatal("retained a proxy credential without stored data")
			}
			if _, err := r.RemoveService(ctx, "team", "proxy", false, false); err != nil {
				t.Fatal(err)
			}
			tokens := r.ExaProxyTokens
			if typ == "openai" {
				tokens = r.AIProxyTokens
			}
			got, err := tokens(ctx)
			if err != nil || len(got) != 0 {
				t.Fatalf("removed proxy still authorizes tokens: %v %v", got, err)
			}
		})
	}
}
