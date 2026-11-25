package addons

import (
	"context"
	"testing"

	"github.com/adrianliechti/donkey/internal/config"
	addonv1 "github.com/adrianliechti/donkey/pkg/addons/v1"
)

func TestSelectedServicesReplaceStaleEnvWithReferences(t *testing.T) {
	app := &config.AppConfig{Env: map[string]string{"DATABASE_URL": "postgres://localhost/old", "AWS_ENDPOINT_URL_S3": "http://localhost:9000", "LOG_LEVEL": "debug", "REDIS_URL": "redis://localhost:6379"}, Addons: []config.AddonConfig{{Name: "cache", Type: "redis"}, {Name: "db", Type: "postgres"}, {Name: "objects", Type: "s3"}}}
	for _, service := range app.Addons {
		recipe, _ := Get(context.Background(), service.Type)
		desc, _ := recipe.Describe(context.Background())
		if err := BindEnvironment(app, service.Name, EnvTemplate(desc)); err != nil {
			t.Fatal(err)
		}
	}
	for key, want := range map[string]string{"REDIS_URL": "${cache.REDIS_URL}", "REDIS_PASSWORD": "${cache.REDIS_PASSWORD}", "DATABASE_URL": "${db.DATABASE_URL}", "AWS_ENDPOINT_URL_S3": "${objects.AWS_ENDPOINT_URL_S3}", "AWS_SECRET_ACCESS_KEY": "${objects.AWS_SECRET_ACCESS_KEY}", "LOG_LEVEL": "debug"} {
		if app.Env[key] != want {
			t.Errorf("%s = %q, want %q", key, app.Env[key], want)
		}
	}
	for _, service := range app.Addons {
		recipe, _ := Get(context.Background(), service.Type)
		desc, _ := recipe.Describe(context.Background())
		if err := BindEnvironment(app, service.Name, EnvTemplate(desc)); err != nil {
			t.Fatal(err)
		}
	}
	if app.Env["DATABASE_URL"] != "${db.DATABASE_URL}" {
		t.Fatal("rebinding changed the selected service")
	}
}

func TestOptionalPluginKeyIsBoundOnlyWhenProvisioned(t *testing.T) {
	desc := &addonv1.Offering{Env: []*addonv1.EnvVar{{Name: "OPENAI_BASE_URL"}, {Name: "OPENAI_API_KEY", Optional: true}}}
	app := &config.AppConfig{}
	if err := PlanEnvironment(app, "model", desc); err != nil {
		t.Fatal(err)
	}
	values := map[string]string{"OPENAI_BASE_URL": "https://models.example/v1"}
	if err := BindEnvironment(app, "model", values); err != nil {
		t.Fatal(err)
	}
	if _, ok := app.Env["OPENAI_API_KEY"]; ok {
		t.Fatal("a keyless model service injected an API key reference")
	}
	values["OPENAI_API_KEY"] = "provider-key"
	if err := BindEnvironment(app, "model", values); err != nil || app.Env["OPENAI_API_KEY"] != config.EnvRef("model", "OPENAI_API_KEY") {
		t.Fatal("provisioned key was not bound", err)
	}
}

func TestExternalS3KeepsAppCredentials(t *testing.T) {
	const typ = "s3"
	recipe := builtins[typ]
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
}
