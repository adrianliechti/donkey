package server

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"testing"

	"github.com/adrianliechti/donkey/internal/addons"
	"github.com/adrianliechti/donkey/internal/ai"
	"github.com/adrianliechti/donkey/internal/config"
	"github.com/adrianliechti/donkey/internal/servicepolicy"
	addonv1 "github.com/adrianliechti/donkey/pkg/addons/v1"
	"github.com/adrianliechti/donkey/pkg/runtime"
)

type selectionRuntime struct {
	serviceLifecycleRuntime
	inventory map[string][]runtime.ServiceBindingInfo
}

func (r *selectionRuntime) ListServices(_ context.Context, space, owner string) ([]runtime.ServiceBindingInfo, error) {
	if owner == addons.AllOwners {
		return r.inventory[space], nil
	}
	return nil, nil
}

func TestServiceSelectionAcrossEntryPoints(t *testing.T) {
	rt := &selectionRuntime{inventory: map[string][]runtime.ServiceBindingInfo{
		"default": {{Name: "customer", Type: "s3"}, {Name: "customer"}},
	}}
	s := planServer(rt, &ai.Analysis{})
	p, err := servicepolicy.Compile(&config.ServicePolicy{
		Rego: `package donkey.services
deny contains "customer data requires a delegated destination" if {
	some source in input.services
	source.id == "s3"
	"read" in source.tags
	some sink in input.services
	"write" in sink.tags
	sink.authorization != "delegated"
}`,
		Offerings: map[string]config.OfferingPolicyMetadata{"s3": {Tags: []string{"read"}}, "postgres": {Tags: []string{"write"}}},
	}, nil)
	if err != nil {
		t.Fatal(err)
	}
	s.cfg.ServicePolicy = p
	request := func(handler http.Handler, path, body string) *httptest.ResponseRecorder {
		t.Helper()
		req := httptest.NewRequest("POST", "/v1/spaces/"+path, strings.NewReader(body))
		asForwarded(t, req)
		rec := httptest.NewRecorder()
		handler.ServeHTTP(rec, req)
		return rec
	}
	preview := request(s.routes(), "default/service-options", `{"candidates":[{"name":"customer"},{"name":"report","type":"postgres"},{"name":"cache","type":"redis"}]}`)
	var result struct{ Blocked map[string]string }
	if err := json.Unmarshal(preview.Body.Bytes(), &result); err != nil || preview.Code != 200 || len(result.Blocked) != 1 || result.Blocked["report"] == "" {
		t.Fatalf("preview: %d %s", preview.Code, preview.Body)
	}
	for _, path := range []string{"default/services", "default/apps/another-app/services"} {
		denied := request(s.orchestratorRoutes(), path, `{"name":"new-db","type":"postgres","tags":["read"],"authorization":"delegated"}`)
		if denied.Code != 400 || !strings.Contains(denied.Body.String(), "requires a delegated") || rt.ensured != nil || rt.attached != nil {
			t.Fatalf("denied %s: %d %s", path, denied.Code, denied.Body)
		}
	}
	allowed := request(s.orchestratorRoutes(), "other/services", `{"type":"postgres","name":"report"}`)
	if allowed.Code != 200 || rt.ensured == nil {
		t.Fatalf("independent space: %d %s", allowed.Code, allowed.Body)
	}
	rt.inventory["default"] = append(rt.inventory["default"], runtime.ServiceBindingInfo{Name: "report", Type: "postgres"})
	rt.connection = runtime.ServiceConnection{Type: "postgres", Binding: map[string]string{"DATABASE_URL": "private"}}
	denied := request(s.orchestratorRoutes(), "default/apps/existing/services", `{"name":"report"}`)
	if denied.Code != 400 || rt.attached != nil {
		t.Fatalf("denied name-only attach: %d %s", denied.Code, denied.Body)
	}
	rt.connection = runtime.ServiceConnection{}
	delete(rt.inventory, "default")
	read := config.AddonConfig{Type: "rustfs", Name: "input"}
	write := config.AddonConfig{Type: "postgres", Name: "output"}
	plan := func(name string, services ...config.AddonConfig) *storedPlan {
		return &storedPlan{app: &config.AppConfig{Name: name, Space: "default", Addons: services}, root: name, analysis: &ai.Analysis{}}
	}
	checkDenied := func(ready bool, findings []Finding) {
		t.Helper()
		for _, f := range findings {
			if !ready && f.Code == "service-policy" && strings.Contains(f.Title, "requires a delegated") {
				return
			}
		}
		t.Fatalf("expected blocked review, got %v", findings)
	}
	one := s.renderPlan(t.Context(), "one", plan("api", read, write))
	checkDenied(one.Ready, one.Findings)
	project, _, _ := s.resolveProject(t.Context(), "project", &projectState{
		apps: []*storedPlan{plan("reader", read), plan("writer", write)}, reading: &ai.Project{},
	})
	checkDenied(project.Ready, project.Findings)
	rt.inventory["default"] = []runtime.ServiceBindingInfo{{Name: "unknown"}}
	if err := runtime.CheckServiceSelection(t.Context(), rt, p, "default", []runtime.ServiceChoice{{Name: "another", Type: "redis"}}); err == nil {
		t.Fatal("unidentifiable service was silently excluded")
	}
}

func TestInstanceDisplaysMetadataForRetiredOffering(t *testing.T) {
	// This existing instance is named db. Its deprecated offering is no
	// longer selectable, but its descriptor still supplies the read tag.
	registerTestCatalog(t, "vendor", &addonv1.Offering{
		Id: "external-vendor", Title: "External database", Protocol: "postgres", Tags: []string{"read"}, Deprecated: true,
		Env: []*addonv1.EnvVar{{Name: "DATABASE_URL"}},
	})
	s := planServer(databaseTestRuntime{protocol: "postgres"}, &ai.Analysis{})
	policy, err := servicepolicy.Compile(&config.ServicePolicy{
		Rego:      "package donkey.services\ndeny := []",
		Offerings: map[string]config.OfferingPolicyMetadata{"external-vendor": {Tags: []string{"C4"}, Authorization: "delegated"}},
	}, nil)
	if err != nil {
		t.Fatal(err)
	}
	s.cfg.ServicePolicy = policy
	req := httptest.NewRequest("GET", "/v1/spaces/default/services", nil)
	asAdmin(t, req)
	rec := httptest.NewRecorder()
	s.routes().ServeHTTP(rec, req)
	var response struct {
		Addons []struct {
			Name string
			config.OfferingPolicyMetadata
		}
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &response); rec.Code != http.StatusOK || err != nil || len(response.Addons) != 1 {
		t.Fatalf("service list: %d %s", rec.Code, rec.Body)
	}
	entry := response.Addons[0]
	if entry.Name != "db" || !slices.Equal(entry.Tags, []string{"C4", "read"}) || entry.Authorization != "delegated" {
		t.Fatalf("unexpected instance metadata: %+v", entry)
	}
}

func TestTightenedPolicyReportsExistingSetupAndAllowsUnchangedApp(t *testing.T) {
	rt := &selectionRuntime{inventory: map[string][]runtime.ServiceBindingInfo{"default": {
		{Name: "old", Type: "retired", Offering: &addonv1.Offering{Id: "retired", Tags: []string{"legacy"}}},
		{Name: "old", Owner: "api"},
		{Name: "newer", Type: "retired", Offering: &addonv1.Offering{Id: "retired", Tags: []string{"current"}}},
	}}}
	s := planServer(rt, &ai.Analysis{})
	p, err := servicepolicy.Compile(&config.ServicePolicy{Rego: `package donkey.services
deny contains sprintf("retire %s", [service.name]) if {
	some service in input.services
	"legacy" in service.tags
}`}, nil)
	if err != nil {
		t.Fatal(err)
	}
	s.cfg.ServicePolicy = p
	list := func(want string) string {
		t.Helper()
		req := httptest.NewRequest("GET", "/v1/spaces/default/services", nil)
		asAdmin(t, req)
		w := httptest.NewRecorder()
		s.routes().ServeHTTP(w, req)
		var body struct {
			ServicePolicy struct{ Status, Reason string }
		}
		if err := json.Unmarshal(w.Body.Bytes(), &body); err != nil || w.Code != 200 || body.ServicePolicy.Status != want {
			t.Fatalf("service inventory: %d %s", w.Code, w.Body)
		}
		return body.ServicePolicy.Reason
	}
	if got := list("noncompliant"); got != "retire old" {
		t.Fatalf("pinned offering metadata lost: %q", got)
	}
	existing := runtime.ServiceChoice{Name: "old", App: "api"}
	if err := runtime.CheckServiceSelection(t.Context(), rt, p, "default", []runtime.ServiceChoice{existing}); err != nil {
		t.Fatal(err)
	}
	for _, choice := range []runtime.ServiceChoice{{Name: "old", App: "another-app"}, {Name: "old", Type: "retired"}, {Name: "cache", Type: "redis", App: "api"}} {
		if err := runtime.CheckServiceSelection(t.Context(), rt, p, "default", []runtime.ServiceChoice{existing, choice}); err == nil || !strings.Contains(err.Error(), "retire old") {
			t.Fatalf("new selection %+v was not denied: %v", choice, err)
		}
	}
	rt.inventory["default"] = []runtime.ServiceBindingInfo{{Name: "unknown"}}
	list("unknown")
	delete(rt.inventory, "default")
	list("compliant")
}
