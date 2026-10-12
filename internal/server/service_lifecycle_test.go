package server

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/adrianliechti/donkey/internal/ai"
	"github.com/adrianliechti/donkey/internal/config"
	"github.com/adrianliechti/donkey/pkg/runtime"
)

type serviceLifecycleRuntime struct {
	consoleRuntime
	ensured  *runtime.ServiceSpec
	attached map[string]string
}

func (*serviceLifecycleRuntime) ValidateApp(string, string) error { return nil }
func (r *serviceLifecycleRuntime) EnsureService(_ context.Context, _ string, spec runtime.ServiceSpec) (map[string]string, map[string]any, error) {
	r.ensured = &spec
	return map[string]string{"EXA_BASE_URL": spec.URL}, nil, nil
}
func (r *serviceLifecycleRuntime) AttachService(_ context.Context, _, _, _ string, binding map[string]string) (bool, error) {
	r.attached = binding
	return true, nil
}

func TestAppServiceRejectsTypeMismatchAndAppliesExternalURL(t *testing.T) {
	for _, c := range []struct {
		typ      string
		external bool
		status   int
	}{{"s3", false, 400}, {"exa", true, 200}, {"", false, 200}} {
		rt := &serviceLifecycleRuntime{consoleRuntime: consoleRuntime{connection: runtime.ServiceConnection{Type: "exa", Protocol: "exa", Binding: map[string]string{"EXA_BASE_URL": "https://old-provider"}}}}
		s := planServer(rt, &ai.Analysis{})
		body, _ := json.Marshal(AddonRequest{Type: c.typ, Name: "search", External: c.external, URL: "https://new-provider/exa"})
		req := httptest.NewRequest(http.MethodPost, "/v1/spaces/default/apps/api/services", strings.NewReader(string(body)))
		asAdmin(t, req)
		out := httptest.NewRecorder()
		s.routes().ServeHTTP(out, req)
		if out.Code != c.status {
			t.Fatalf("type=%s external=%t: %d %s", c.typ, c.external, out.Code, out.Body.String())
		}
		if c.status == 400 && (rt.ensured != nil || rt.attached != nil) {
			t.Fatal("mismatched service mutated")
		}
		if c.external && (rt.ensured == nil || rt.attached["EXA_BASE_URL"] != "https://new-provider/exa") {
			t.Fatal("reattaching ignored the new URL")
		}
		if c.external && !strings.Contains(out.Body.String(), `"created":false`) {
			t.Fatal("reconfigured service reported as new")
		}
	}
}

func TestPlanRejectsReusingDifferentServiceType(t *testing.T) {
	rt := consoleRuntime{connection: runtime.ServiceConnection{Type: "postgres", Protocol: "postgres", Binding: map[string]string{"DATABASE_URL": "postgres://db/app"}}}
	s := planServer(rt, &ai.Analysis{})
	app := &config.AppConfig{Name: "api", Space: "default", Addons: []config.AddonConfig{{Name: "objects", Type: "s3"}}}
	if err := s.bindEnvironment(context.Background(), app); err == nil {
		t.Fatal("plan bound PostgreSQL as S3 storage")
	}
}
