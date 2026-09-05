package manifests

import (
	"testing"

	"github.com/adrianliechti/donkey/internal/config"
)

func TestEnvReferencesUseBindingSecrets(t *testing.T) {
	in := testInput(t)
	in.ObjectName = "team-api"
	in.App.Env = map[string]string{"DB_URL": "${db.DATABASE_URL}", "MODE": "production"}
	env, _ := appEnvironment(in)
	for _, variable := range env {
		switch variable.Name {
		case "DB_URL":
			if variable.Value != "" || variable.ValueFrom == nil || variable.ValueFrom.SecretKeyRef == nil {
				t.Fatalf("credential must stay in Secret: %+v", variable)
			}
			ref := variable.ValueFrom.SecretKeyRef
			if ref.Name != "team-api-addon-db" || ref.Key != "DATABASE_URL" {
				t.Fatalf("wrong secret reference: %+v", ref)
			}
		case "MODE":
			if variable.Value != "production" || variable.ValueFrom != nil {
				t.Fatal("literal env changed")
			}
		}
	}
}

func TestBrowserRouteKeepsLoginAndStripsOnlyProxyPrefix(t *testing.T) {
	in := testInput(t)
	in.App.Routes = []config.RouteConfig{{App: "web", Path: "/_donkey/api", StripPrefix: true}}
	route := PathRoute(in, RouteTarget{App: "web", Host: "web.example.com", Path: "/_donkey/api", StripPrefix: true})
	if route.Annotations[AnnotationStripPrefix] != "true" || route.Annotations[AnnotationLogin] != LoginRequired || route.Annotations[AnnotationHost] != "web.example.com" {
		t.Fatalf("wrong route annotations: %+v", route.Annotations)
	}
}
