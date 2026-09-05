package docker

import (
	"context"
	"testing"

	"github.com/adrianliechti/donkey/internal/config"
)

func TestEnvResolvesLiveServiceReferences(t *testing.T) {
	r := &Runtime{state: state{dir: t.TempDir()}}
	if err := r.state.setEnv("team", "api", map[string]string{"DB_URL": "${db.DATABASE_URL}", "DATABASE_URL": "${db.DATABASE_URL}", "MODE": "production"}); err != nil {
		t.Fatal(err)
	}
	for _, value := range []string{"postgres://first", "postgres://rotated"} {
		if err := r.state.setBinding("team", "api", "db", map[string]string{"DATABASE_URL": value}); err != nil {
			t.Fatal(err)
		}
		env, err := r.appEnv("team", "api", 8080)
		if err != nil {
			t.Fatal(err)
		}
		if err := r.resolveEnv("team", "api", env); err != nil {
			t.Fatal(err)
		}
		if env["DB_URL"] != value || env["DATABASE_URL"] != value || env["MODE"] != "production" {
			t.Fatalf("wrong runtime env: %v", env)
		}
		stored, _ := r.state.env("team", "api")
		if stored["DB_URL"] != "${db.DATABASE_URL}" {
			t.Fatal("resolved credential was written over the reference")
		}
	}
	if err := r.resolveEnv("team", "api", map[string]string{"DB_URL": "${db.MISSING}"}); err == nil {
		t.Fatal("missing binding key silently accepted")
	}
}

func TestDetachServiceRemovesOnlyItsReferences(t *testing.T) {
	r := &Runtime{state: state{dir: t.TempDir()}}
	if err := r.state.setEnv("team", "api", map[string]string{
		"DB_URL": "${db.DATABASE_URL}", "DATABASE_URL": "${db.DATABASE_URL}",
		"MODEL": "${ai.OPENAI_MODEL}", "MODE": "production",
	}); err != nil {
		t.Fatal(err)
	}
	if err := r.state.setBinding("team", "api", "db", map[string]string{"DATABASE_URL": "postgres://db/app"}); err != nil {
		t.Fatal(err)
	}
	if err := r.state.setBinding("team", "api", "ai", map[string]string{"OPENAI_MODEL": "model"}); err != nil {
		t.Fatal(err)
	}
	if err := r.DetachService(context.Background(), "team", "api", "db"); err != nil {
		t.Fatal(err)
	}
	stored, _ := r.state.env("team", "api")
	if stored["DB_URL"] != "" || stored["DATABASE_URL"] != "" || stored["MODEL"] != "${ai.OPENAI_MODEL}" || stored["MODE"] != "production" {
		t.Fatalf("detach removed the wrong environment: %v", stored)
	}
	env, err := r.appEnv("team", "api", 8080)
	if err != nil {
		t.Fatal(err)
	}
	if err := r.resolveEnv("team", "api", env); err != nil {
		t.Fatalf("the next restart still references the removed binding: %v", err)
	}
}

func TestProjectRouteUsesReviewedHostBeforeFrontendExists(t *testing.T) {
	r := &Runtime{state: state{dir: t.TempDir()}, opts: Options{Domain: "example.com"}}
	app := &config.AppConfig{Name: "api", Internal: true, Routes: []config.RouteConfig{{App: "web", Path: "/_donkey/api", StripPrefix: true}}}
	if err := r.applyRoutes(context.Background(), "team", app, 8080, map[string]string{"web": "custom.example.com"}); err != nil {
		t.Fatal(err)
	}
	route, err := r.state.route("team", pathRouteName("api", "web", "/_donkey/api"))
	if err != nil || route == nil {
		t.Fatalf("route missing: %v", err)
	}
	if route.Host != "custom.example.com" || !route.StripPrefix || !route.Login {
		t.Fatalf("wrong route: %+v", route)
	}
}
