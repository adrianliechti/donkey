package docker

import (
	"context"
	"net/http"
	"reflect"
	"strings"
	"testing"

	"github.com/adrianliechti/donkey/internal/config"
	"github.com/docker/docker/api/types/network"
	"github.com/docker/docker/client"
)

func TestExistingSpacesMigrateWithoutResettingSettings(t *testing.T) {
	ctx := context.Background()
	nw := network.Inspect{Name: netName("old"), Labels: resourceLabels("old", roleSpace)}
	transport := dockerRoundTripFunc(func(req *http.Request) (*http.Response, error) {
		switch strings.TrimPrefix(req.URL.Path, "/v1.48") {
		case "/networks":
			// A legacy network with a now-reserved space name must not block startup.
			reserved := network.Inspect{Name: netName("donkey"), Labels: resourceLabels("donkey", roleSpace)}
			return dockerJSONResponse(200, []network.Inspect{nw, reserved}), nil
		case "/networks/donkey-old":
			return dockerJSONResponse(200, nw), nil
		default:
			t.Fatalf("unexpected request: %s", req.URL)
			return nil, nil
		}
	})
	cli, err := client.NewClientWithOpts(client.WithHost("http://docker.test"), client.WithVersion("1.48"), client.WithHTTPClient(&http.Client{Transport: transport}))
	if err != nil {
		t.Fatal(err)
	}
	r := &Runtime{cli: cli, state: state{dir: t.TempDir()}}
	if err := r.EnsureSpaceConfigs(ctx); err != nil {
		t.Fatal(err)
	}
	settings, err := r.SpaceConfig(ctx, "old")
	if err != nil || settings.Domains == nil || len(settings.Domains) != 0 || !settings.Admins.Empty() || !settings.Developers.Empty() {
		t.Fatalf("unexpected migration: %+v, %v", settings, err)
	}
	settings.Admins.Users = []string{"owner@example.com"}
	settings.Domains = []string{"custom.example.org"}
	settings.Offerings = map[string]config.SpaceOffering{"remote-db": {Defaults: map[string]any{"version": "11.4"}, Config: map[string]any{"tenant": "finance"}}}
	if err := r.SetSpaceConfig(ctx, "old", settings); err != nil {
		t.Fatal(err)
	}
	if err := r.EnsureSpaceConfigs(ctx); err != nil {
		t.Fatal(err)
	}
	got, err := r.SpaceConfig(ctx, "old")
	if err != nil || !reflect.DeepEqual(got, settings) {
		t.Fatalf("migration reset settings: %+v, %v", got, err)
	}
}

func TestSharedRouteCannotEscapeSpaceDomains(t *testing.T) {
	r := &Runtime{state: state{dir: t.TempDir()}, opts: Options{Domain: "example.com"}}
	if err := r.state.writeRecord(config.DefaultSpaceConfig("team", "example.com"), "spaces", "team", "config"); err != nil {
		t.Fatal(err)
	}
	app := &config.AppConfig{Name: "api", Routes: []config.RouteConfig{{App: "web", Path: "/api"}}}
	if err := r.applyRoutes(context.Background(), "team", app, 8080, map[string]string{"web": "outside.example.org"}); err == nil {
		t.Fatal("shared route bypassed domain settings")
	}
	routes, err := r.state.listRoutes("team")
	if err != nil || len(routes) != 0 {
		t.Fatalf("rejected route was published: %v %v", routes, err)
	}
}
