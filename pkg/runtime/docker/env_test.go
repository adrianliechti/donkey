package docker

import (
	"context"
	"encoding/json"
	"net/http"
	"os"
	"reflect"
	"strings"
	"testing"

	"github.com/docker/docker/api/types/container"
	"github.com/docker/docker/api/types/network"
	"github.com/docker/docker/client"

	"github.com/adrianliechti/donkey/internal/config"
	rt "github.com/adrianliechti/donkey/pkg/runtime"
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
		env, err := r.appEnv("team", "api", runSpec{Port: 8080})
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
	env, err := r.appEnv("team", "api", runSpec{Port: 8080})
	if err != nil {
		t.Fatal(err)
	}
	if err := r.resolveEnv("team", "api", env); err != nil {
		t.Fatalf("the next restart still references the removed binding: %v", err)
	}
}

// As on Kubernetes, donkey.yaml's env is the deploy's — a key dropped from it
// is gone with the next deploy — while `donkey env` values persist, and
// donkey.yaml wins where both set a key.
func TestDeployEnvIsReplacedAndStoredEnvPersists(t *testing.T) {
	r, created := fakeAppEngine(t)
	ctx := context.Background()
	if err := r.state.writeRecord(config.DefaultSpaceConfig("team", "example.com"), "spaces", "team", "config"); err != nil {
		t.Fatal(err)
	}
	stored := map[string]string{"API_KEY": "from-donkey-env", "SHARED": "from-donkey-env"}
	if err := r.SetEnv(ctx, "team", "api", stored); err != nil {
		t.Fatal(err)
	}
	before, err := os.ReadFile(r.state.path("env", "team", "api.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	deploy := func(env map[string]string) map[string]string {
		t.Helper()
		app := &config.AppConfig{Name: "api", Port: 8080, Internal: true, Env: env}
		if err := r.Deploy(ctx, rt.DeploySpec{App: app, Space: "team", ImageRef: "api:1", NoWait: true}, discardProgress{}); err != nil {
			t.Fatal(err)
		}
		return (*created)[len(*created)-1]
	}

	first := deploy(map[string]string{"GREETING": "hello", "SHARED": "from-donkey-yaml", "PORT": "9090"})
	if first["GREETING"] != "hello" || first["SHARED"] != "from-donkey-yaml" || first["API_KEY"] != "from-donkey-env" || first["PORT"] != "9090" {
		t.Fatalf("first deploy env = %v", first)
	}
	second := deploy(map[string]string{})
	if _, ok := second["GREETING"]; ok || second["SHARED"] != "from-donkey-env" || second["API_KEY"] != "from-donkey-env" || second["PORT"] != "8080" {
		t.Fatalf("second deploy env = %v", second)
	}
	if restarted, err := r.Restart(ctx, "team", "api"); err != nil || !restarted || !reflect.DeepEqual((*created)[len(*created)-1], second) {
		t.Fatalf("restart env = %v (%v, %v), want %v", (*created)[len(*created)-1], restarted, err, second)
	}
	if after, err := os.ReadFile(r.state.path("env", "team", "api.yaml")); err != nil || string(after) != string(before) {
		t.Fatalf("deploys rewrote the stored env: %q, %v", after, err)
	}
	if got, err := r.GetEnv(ctx, "team", "api"); err != nil || !reflect.DeepEqual(got, stored) {
		t.Fatalf("donkey env lists %v, %v", got, err)
	}
}

// fakeAppEngine is a Docker engine with space team and nothing else running,
// recording the env of every container created.
func fakeAppEngine(t *testing.T) (*Runtime, *[]map[string]string) {
	t.Helper()
	var created []map[string]string
	transport := dockerRoundTripFunc(func(req *http.Request) (*http.Response, error) {
		path := strings.TrimPrefix(req.URL.Path, "/v1.48")
		switch {
		case req.Method == http.MethodGet && path == "/networks/donkey-team":
			return dockerJSONResponse(http.StatusOK, network.Inspect{Name: "donkey-team", Labels: resourceLabels("team", roleSpace)}), nil
		case req.Method == http.MethodGet && (strings.HasPrefix(path, "/containers/") || strings.HasPrefix(path, "/volumes/")):
			return dockerJSONResponse(http.StatusNotFound, map[string]string{"message": "not found"}), nil
		case req.Method == http.MethodPost && path == "/images/create":
			return dockerJSONResponse(http.StatusOK, map[string]string{"status": "done"}), nil
		case req.Method == http.MethodPost && path == "/containers/create":
			var cfg container.Config
			if err := json.NewDecoder(req.Body).Decode(&cfg); err != nil {
				t.Fatal(err)
			}
			env := map[string]string{}
			for _, kv := range cfg.Env {
				k, v, _ := strings.Cut(kv, "=")
				env[k] = v
			}
			created = append(created, env)
			return dockerJSONResponse(http.StatusCreated, map[string]string{"Id": "api-id"}), nil
		case req.Method == http.MethodPost && path == "/containers/donkey.team.api/start":
			return dockerJSONResponse(http.StatusNoContent, nil), nil
		default:
			t.Fatalf("unexpected Docker request: %s %s", req.Method, req.URL)
			return nil, nil
		}
	})
	cli, err := client.NewClientWithOpts(client.WithHost("http://docker.test"), client.WithVersion("1.48"), client.WithHTTPClient(&http.Client{Transport: transport}))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { cli.Close() })
	return &Runtime{cli: cli, state: state{dir: t.TempDir()}, opts: Options{Domain: "example.com",
		ServerContainer: "donkey-server", GatewayContainer: "donkey-gateway", AIContainer: "donkey-ai", TelemetryContainer: "donkey-telemetry", ProxyContainer: "donkey-proxy"}}, &created
}

type discardProgress struct{}

func (discardProgress) Line(string, ...any) {}

func TestGatewayTelemetryDoesNotConfigureAppSDK(t *testing.T) {
	r := &Runtime{state: state{dir: t.TempDir()}, opts: Options{TelemetryKey: []byte("configured"), TelemetryContainer: "donkey-telemetry"}}
	env, err := r.appEnv("team", "api", runSpec{Port: 8080})
	if err != nil {
		t.Fatal(err)
	}
	for key := range env {
		if strings.HasPrefix(key, "OTEL_") {
			t.Fatalf("injected application telemetry: %s", key)
		}
	}
	env, err = r.appEnv("team", "api", runSpec{Port: 8080, Env: map[string]string{"OTEL_EXPORTER_OTLP_ENDPOINT": "https://collector.example"}})
	if err != nil || env["OTEL_EXPORTER_OTLP_ENDPOINT"] != "https://collector.example" {
		t.Fatalf("explicit user configuration lost: %v %v", env, err)
	}
}

// donkey.yaml's env wins over the bindings as well, its references resolve,
// and a detach takes them out of the run record as Kubernetes takes them out
// of the Deployment — so the restart that follows does not trip over them.
func TestRunRecordEnvResolvesAndDetaches(t *testing.T) {
	r, created := fakeAppEngine(t)
	if err := r.state.setBinding("team", "api", "db", map[string]string{"DATABASE_URL": "postgres://db/app", "PGHOST": "db"}); err != nil {
		t.Fatal(err)
	}
	spec := runSpec{Image: "api:1", Port: 8080, Env: map[string]string{"DB": "${db.DATABASE_URL}", "PGHOST": "override", "MODE": "production"}}
	if err := r.setRunRecord("team", "api", spec); err != nil {
		t.Fatal(err)
	}
	if _, err := r.Restart(context.Background(), "team", "api"); err != nil {
		t.Fatal(err)
	}
	if env := (*created)[0]; env["DB"] != "postgres://db/app" || env["DATABASE_URL"] != "postgres://db/app" || env["PGHOST"] != "override" || env["MODE"] != "production" {
		t.Fatalf("wrong runtime env: %v", env)
	}
	if err := r.DetachService(context.Background(), "team", "api", "db"); err != nil {
		t.Fatal(err)
	}
	run, err := r.runRecord("team", "api")
	if err != nil || !reflect.DeepEqual(run.Env, map[string]string{"PGHOST": "override", "MODE": "production"}) {
		t.Fatalf("run record env after detach = %+v, %v", run, err)
	}
	if env := (*created)[1]; env["DB"] != "" || env["DATABASE_URL"] != "" || env["MODE"] != "production" {
		t.Fatalf("env after detach: %v", env)
	}
	if _, err := os.Stat(r.state.path("env", "team", "api.yaml")); !os.IsNotExist(err) {
		t.Fatal("detach created a stored env it had nothing to change in")
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
