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

func TestOIDCRegistrationRoutePersistence(t *testing.T) {
	r := &Runtime{state: state{dir: t.TempDir()}, opts: Options{Domain: "example.com"}}
	cli, err := client.NewClientWithOpts(client.WithHost("http://docker.test"), client.WithVersion("1.48"), client.WithHTTPClient(&http.Client{Transport: dockerRoundTripFunc(func(req *http.Request) (*http.Response, error) {
		if strings.TrimPrefix(req.URL.Path, "/v1.48") != "/networks/donkey-team" {
			t.Fatalf("unexpected Docker request: %s", req.URL)
		}
		return dockerJSONResponse(200, network.Inspect{Name: netName("team"), Labels: resourceLabels("team", roleSpace)}), nil
	})}))
	if err != nil {
		t.Fatal(err)
	}
	defer cli.Close()
	r.cli = cli
	if err := r.state.writeRecord(config.DefaultSpaceConfig("team", "example.com"), "spaces", "team", "config"); err != nil {
		t.Fatal(err)
	}
	app := &config.AppConfig{Name: "web", Space: "team", Port: 8080, OIDC: &config.OIDCClient{RedirectURIs: []string{"https://web.team.example.com/callback"}}}
	if err := r.applyRoutes(context.Background(), "team", app, 8080); err != nil {
		t.Fatal(err)
	}
	network, err := r.Network(context.Background(), "team", "web")
	if err != nil || !reflect.DeepEqual(network.OIDC, app.OIDC) {
		t.Fatalf("registration lost in network state: %+v %v", network, err)
	}
	app.Host = "other"
	app.OIDC = network.OIDC
	if _, err := r.ApplyNetwork(context.Background(), app); err != nil {
		t.Fatal(err)
	}
	route, err := r.state.route("team", "web")
	if err != nil || route.Host != "other.team.example.com" || !reflect.DeepEqual(route.OIDC, app.OIDC) {
		t.Fatalf("network edit lost registration: %+v %v", route, err)
	}
}
