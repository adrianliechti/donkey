package server

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/adrianliechti/donkey/internal/config"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

func TestPortalFleetKubernetesRequestBudget(t *testing.T) {
	for _, shared := range []bool{false, true} {
		for _, member := range []bool{false, true} {
			for _, size := range []struct{ apps, spaces int }{{1, 1}, {100, 10}} {
				t.Run(fmt.Sprintf("shared=%t/member=%t/apps=%d", shared, member, size.apps), func(t *testing.T) {
					f := newKubernetesLoadFixture(t, shared, member, size.apps, size.spaces)
					var fleet kubernetesFleetResponse
					f.get(t, "/v1/fleet", &fleet)
					if len(fleet.Apps) != size.apps || len(fleet.Spaces) != size.spaces {
						t.Fatalf("incomplete fleet: %+v", fleet)
					}
					if reads := f.takeReads(t); reads > 8 {
						t.Fatalf("cold fleet needs %d Kubernetes reads; budget is 8 independent of app count", reads)
					}
					f.get(t, "/v1/fleet", &fleet)
					if reads := f.takeReads(t); reads > 3 {
						t.Fatalf("another portal tab needs %d Kubernetes reads; budget is 3", reads)
					}
				})
			}
		}
	}
}

func TestConcurrentPortalTabsShareFleetReads(t *testing.T) {
	for _, shared := range []bool{false, true} {
		t.Run(fmt.Sprintf("shared=%t", shared), func(t *testing.T) {
			f := newKubernetesLoadFixture(t, shared, true, 100, 10)
			const tabs = 8
			var wg sync.WaitGroup
			for i := 0; i < tabs; i++ {
				wg.Go(func() {
					var fleet kubernetesFleetResponse
					f.get(t, "/v1/fleet", &fleet)
					if len(fleet.Apps) != 100 || len(fleet.Spaces) != 10 {
						t.Errorf("incomplete concurrent fleet: %+v", fleet)
					}
				})
			}
			wg.Wait()
			var deploymentLists int
			for _, action := range f.typed.Actions() {
				if action.GetResource().Resource == "deployments" {
					deploymentLists++
				}
			}
			if deploymentLists != 1 {
				t.Fatalf("concurrent tabs repeated the workload sweep %d times", deploymentLists)
			}
			if reads := f.takeReads(t); reads > 5+3*tabs {
				t.Fatalf("concurrent tabs exceeded the shared snapshot budget: %d reads", reads)
			}
		})
	}
}

func TestFleetCacheDoesNotDelaySpaceRevocation(t *testing.T) {
	for _, shared := range []bool{false, true} {
		t.Run(fmt.Sprintf("shared=%t", shared), func(t *testing.T) {
			f := newKubernetesLoadFixture(t, shared, true, 10, 1)
			var fleet kubernetesFleetResponse
			f.get(t, "/v1/fleet", &fleet)
			if len(fleet.Apps) != 10 || fleet.Roles["team-0"] != config.SpaceDeveloper {
				t.Fatalf("developer cannot read initial fleet: %+v", fleet)
			}
			f.takeReads(t)
			// A change outside this server leaves the display cache warm. Roles
			// must still be resolved against the updated policy immediately.
			if err := f.runtime.SetSpaceConfig(context.Background(), "team-0", config.SpaceConfig{}); err != nil {
				t.Fatal(err)
			}
			f.typed.ClearActions()
			for _, path := range []string{"/v1/fleet", "/v1/apps", "/v1/spaces"} {
				var response map[string]any
				f.get(t, path, &response)
				for _, key := range []string{"apps", "spaces"} {
					if values, ok := response[key].([]any); ok && len(values) != 0 {
						t.Fatalf("revoked developer still sees %s through %s: %+v", key, path, response)
					}
				}
			}
			for _, action := range f.typed.Actions() {
				if action.GetResource().Resource == "deployments" {
					t.Fatal("revocation test unexpectedly refreshed workload data instead of checking a cached fleet")
				}
			}
		})
	}
}

func TestFleetCacheIsSharedAcrossUsersAndInvalidatedByWrites(t *testing.T) {
	f := newKubernetesLoadFixture(t, false, true, 1, 1)
	s := &Server{cfg: &Config{Domain: "example.com", SessionKey: testSessionKey}, rt: f.runtime}
	orchestrator := httptest.NewServer(s.orchestratorRoutes())
	defer orchestrator.Close()
	s.cfg.OrchestratorURL = orchestrator.URL
	f.handler = s.routes()
	var fleet kubernetesFleetResponse
	f.get(t, "/v1/fleet", &fleet)
	f.takeReads(t)
	// The same snapshot is safe for an administrator: visibility and roles
	// belong to the request, rather than to whichever user populated it.
	req := httptest.NewRequest(http.MethodGet, "/v1/fleet", nil)
	asAdmin(t, req)
	rec := httptest.NewRecorder()
	f.handler.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), `"platformAdmin":true`) {
		t.Fatalf("administrator reused another user's authorization: %d %s", rec.Code, rec.Body.String())
	}
	if reads := f.takeReads(t); reads > 3 {
		t.Fatalf("administrator did not share workload snapshot: %d reads", reads)
	}
	dep, err := f.typed.AppsV1().Deployments("donkey-team-0").Get(context.Background(), "app-0", metav1.GetOptions{})
	if err != nil {
		t.Fatal(err)
	}
	dep.Spec.Template.Spec.Containers[0].Image = "example/app:v2"
	if _, err := f.typed.AppsV1().Deployments(dep.Namespace).Update(context.Background(), dep, metav1.UpdateOptions{}); err != nil {
		t.Fatal(err)
	}
	f.get(t, "/v1/fleet", &fleet)
	if fleet.Apps[0].Image != "example/app:v1" {
		t.Fatal("fixture did not exercise a warm cache")
	}
	req = httptest.NewRequest(http.MethodPost, "/v1/spaces/team-0/apps/app-0/restart", nil)
	asAdmin(t, req)
	rec = httptest.NewRecorder()
	f.handler.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("restart: %d %s", rec.Code, rec.Body.String())
	}
	f.get(t, "/v1/fleet", &fleet)
	if fleet.Apps[0].Image != "example/app:v2" {
		t.Fatal("completed write did not invalidate the fleet snapshot")
	}
}
