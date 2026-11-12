package docker

import (
	"context"
	"net/http"
	"reflect"
	"strings"
	"testing"

	"github.com/adrianliechti/donkey/internal/config"
	rt "github.com/adrianliechti/donkey/pkg/runtime"
	"github.com/docker/docker/api/types/container"
	"github.com/docker/docker/api/types/network"
	"github.com/docker/docker/client"
)

func TestReconcileAppRoutesProtectsOwnAndSharedAddresses(t *testing.T) {
	r := &Runtime{state: state{dir: t.TempDir()}}
	for _, name := range []string{"api", "api.on.web.api", "unrelated"} {
		if err := r.state.setRoute("team", name, Route{Host: "example.com", Backend: "http://api:8000", Access: &config.AccessConfig{Users: []string{"old@example.com"}}}); err != nil {
			t.Fatal(err)
		}
	}
	for range 2 {
		if err := r.ReconcileAppRoutes(context.Background(), "team", "api", true); err != nil {
			t.Fatal(err)
		}
	}
	for _, name := range []string{"api", "api.on.web.api", "unrelated"} {
		route, err := r.state.route("team", name)
		if err != nil {
			t.Fatal(err)
		}
		want := name != "unrelated"
		if route.Login != want || (route.Access == nil) != want || route.Backend != "http://api:8000" {
			t.Fatalf("%s: %+v", name, route)
		}
	}
}

func TestAccessMigrationDeduplicatesAcrossAppsAndKeepsSpacesSeparate(t *testing.T) {
	ctx := context.Background()
	transport := dockerRoundTripFunc(func(req *http.Request) (*http.Response, error) {
		switch path := strings.TrimPrefix(req.URL.Path, "/v1.48"); path {
		case "/containers/json":
			return dockerJSONResponse(200, []container.Summary{
				{Labels: appLabels("team", "web", roleWorkload)},
				{Labels: appLabels("team", "api", roleWorkload)},
				{Labels: appLabels("other", "web", roleWorkload)},
			}), nil
		case "/networks/donkey-team", "/networks/donkey-other":
			space := strings.TrimPrefix(path, "/networks/donkey-")
			return dockerJSONResponse(200, network.Inspect{Name: netName(space), Labels: resourceLabels(space, roleSpace)}), nil
		default:
			t.Fatalf("unexpected request: %s", req.URL)
			return nil, nil
		}
	})
	cli, err := client.NewClientWithOpts(client.WithHost("http://docker.test"), client.WithVersion("1.48"), client.WithHTTPClient(&http.Client{Transport: transport}))
	if err != nil {
		t.Fatal(err)
	}
	defer cli.Close()
	r := &Runtime{cli: cli, state: state{dir: t.TempDir()}}
	policy := config.SpaceConfig{
		Admins:     config.AccessConfig{Users: []string{"owner@example.com"}},
		Developers: config.AccessConfig{Groups: []string{"builders"}},
		Viewers:    config.AccessConfig{Users: []string{"existing@example.com"}},
		Domains:    []string{"team.example.com"},
	}
	other := config.SpaceConfig{AllowPublic: true, Domains: []string{}}
	for space, settings := range map[string]config.SpaceConfig{"team": policy, "other": other} {
		if err := r.state.writeRecord(settings, "spaces", space, "config"); err != nil {
			t.Fatal(err)
		}
	}
	for _, record := range []struct {
		space, name string
		access      config.AccessConfig
	}{
		{"team", "api", config.AccessConfig{Users: []string{"reader@example.com", "OWNER@example.com"}}},
		{"team", "api.on.web.api", config.AccessConfig{Groups: []string{"builders", "readers"}}},
		{"team", "web", config.AccessConfig{Users: []string{"READER@example.com", "EXISTING@example.com"}, Groups: []string{"readers"}}},
		{"other", "web", config.AccessConfig{Users: []string{"other@example.com"}}},
	} {
		if err := r.state.setRoute(record.space, record.name, Route{Host: "web." + record.space + ".example.com", Backend: "http://api:8000", Access: &record.access}); err != nil {
			t.Fatal(err)
		}
	}
	console := `{"exposure":"public","host":"api.team.example.com","access":{"users":["READER@example.com","console@example.com"],"groups":["readers"]}}`
	if err := r.SetAppMeta(ctx, "team", "api", rt.MetaConsole, &console); err != nil {
		t.Fatal(err)
	}
	for range 2 {
		if err := rt.ReconcileAccess(ctx, r, "team"); err != nil {
			t.Fatal(err)
		}
	}
	policy.Viewers = config.AccessConfig{Users: []string{"existing@example.com", "reader@example.com", "console@example.com"}, Groups: []string{"readers"}}
	got, err := r.SpaceConfig(ctx, "team")
	if err != nil || !reflect.DeepEqual(got, policy) {
		t.Fatalf("migration duplicated entries or changed existing roles: %+v, %v", got, err)
	}
	for _, name := range []string{"api", "api.on.web.api", "web"} {
		route, err := r.state.route("team", name)
		if err != nil || route == nil || route.Access != nil || !route.Login {
			t.Fatalf("legacy route not cleaned up: %+v, %v", route, err)
		}
	}
	saved, err := r.AppMeta(ctx, "team", "api", rt.MetaConsole)
	if err != nil || strings.Contains(saved, "access") || !strings.Contains(saved, `"exposure":"protected"`) || !strings.Contains(saved, "api.team.example.com") {
		t.Fatalf("console choices not migrated: %s, %v", saved, err)
	}
	got, err = r.SpaceConfig(ctx, "other")
	if err != nil || !reflect.DeepEqual(got, other) {
		t.Fatalf("targeted migration changed another space: %+v, %v", got, err)
	}
	otherRoute, err := r.state.route("other", "web")
	if err != nil || otherRoute == nil || otherRoute.Access == nil || otherRoute.Login {
		t.Fatalf("targeted migration changed another route: %+v, %v", otherRoute, err)
	}
	if err := rt.ReconcileAccess(ctx, r, ""); err != nil {
		t.Fatal(err)
	}
	other.Viewers.Users = []string{"other@example.com"}
	got, err = r.SpaceConfig(ctx, "other")
	if err != nil || !reflect.DeepEqual(got, other) {
		t.Fatalf("startup migration mixed space membership: %+v, %v", got, err)
	}
}
