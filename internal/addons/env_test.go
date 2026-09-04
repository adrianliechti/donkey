package addons

import (
	"testing"

	"github.com/adrianliechti/donkey/internal/config"
)

func TestSelectedServicesReplaceStaleEnvWithReferences(t *testing.T) {
	app := &config.AppConfig{Env: map[string]string{"DATABASE_URL": "postgres://localhost/old", "OPENAI_BASE_URL": "http://localhost:1234", "AWS_ENDPOINT_URL_S3": "http://localhost:9000", "LOG_LEVEL": "debug"}, Addons: []config.AddonConfig{{Name: "db", Type: "postgres"}, {Name: "ai", Type: "openai"}, {Name: "objects", Type: "rustfs"}}}
	for _, service := range app.Addons {
		recipe, _ := Get(service.Type)
		if err := BindEnvironment(app, service.Name, recipe.Binding(Input{})); err != nil {
			t.Fatal(err)
		}
	}
	for key, want := range map[string]string{"DATABASE_URL": "${db.DATABASE_URL}", "OPENAI_BASE_URL": "${ai.OPENAI_BASE_URL}", "OPENAI_MODEL": "${ai.OPENAI_MODEL}", "OPENAI_API_KEY": "${ai.OPENAI_API_KEY}", "AWS_ENDPOINT_URL_S3": "${objects.AWS_ENDPOINT_URL_S3}", "AWS_SECRET_ACCESS_KEY": "${objects.AWS_SECRET_ACCESS_KEY}", "LOG_LEVEL": "debug"} {
		if app.Env[key] != want {
			t.Errorf("%s = %q, want %q", key, app.Env[key], want)
		}
	}
	for _, service := range app.Addons {
		recipe, _ := Get(service.Type)
		if err := BindEnvironment(app, service.Name, recipe.Binding(Input{})); err != nil {
			t.Fatal(err)
		}
	}
	if app.Env["DATABASE_URL"] != "${db.DATABASE_URL}" {
		t.Fatal("rebinding changed the selected service")
	}
}

func TestExternalServiceKeepsAppCredentials(t *testing.T) {
	for _, typ := range []string{"openai", "rustfs"} {
		t.Run(typ, func(t *testing.T) {
			recipe, _ := Get(typ)
			values, err := recipe.ExternalBinding("https://service.example.com")
			if err != nil {
				t.Fatal(err)
			}
			app := &config.AppConfig{Env: map[string]string{recipe.CredentialKey(): "app-owned"}}
			if err := BindEnvironment(app, typ, values); err != nil {
				t.Fatal(err)
			}
			if app.Env[recipe.CredentialKey()] != "app-owned" {
				t.Fatal("an endpoint-only binding replaced the app's credential")
			}
			for key := range values {
				if app.Env[key] != config.EnvRef(typ, key) {
					t.Fatalf("endpoint %s was not wired", key)
				}
			}
			app.Env["CUSTOM_KEY"] = config.EnvRef(typ, recipe.CredentialKey())
			if err := BindEnvironment(app, typ, values); err == nil {
				t.Fatal("a reference to a key the service does not supply was accepted")
			}
		})
	}
}
