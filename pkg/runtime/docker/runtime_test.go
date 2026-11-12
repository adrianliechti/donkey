package docker

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"os"
	"strings"
	"testing"

	"github.com/docker/docker/api/types/container"
	"github.com/docker/docker/api/types/network"
	"github.com/docker/docker/api/types/volume"
	"github.com/docker/docker/client"
)

type dockerRoundTripFunc func(*http.Request) (*http.Response, error)

func (f dockerRoundTripFunc) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

func TestRuntimeLabelsSeparateAppsAddonsAndInstallation(t *testing.T) {
	for name, labels := range map[string]map[string]string{
		"app":   appLabels("team", "site", roleWorkload),
		"addon": addonLabels("team", "postgres", roleAddon),
		"space": resourceLabels("team", roleSpace),
	} {
		if labels[labelManaged] != "true" || labels[labelInstallation] != dockerInstallation ||
			labels[labelSpace] != "team" {
			t.Errorf("%s ownership labels=%v", name, labels)
		}
	}
	if labels := appLabels("team", "site", roleWorkload); labels[labelApp] != "site" || labels[labelAddon] != "" {
		t.Errorf("app identity=%v", labels)
	}
	if labels := addonLabels("team", "postgres", roleAddon); labels[labelAddon] != "postgres" || labels[labelApp] != "" {
		t.Errorf("addon identity=%v", labels)
	}
	for key := range SystemLabels("server") {
		if !strings.HasPrefix(key, "local.donkey.") {
			t.Errorf("Docker label %q is not reverse-DNS namespaced", key)
		}
	}
	if !MatchesSystemLabels(SystemLabels("server"), "server") {
		t.Fatal("the canonical system identity did not match itself")
	}
	wrongRole := SystemLabels("gateway")
	if MatchesSystemLabels(wrongRole, "server") {
		t.Fatal("a gateway matched the server identity")
	}
	tenant := SystemLabels("server")
	tenant[labelSpace] = "team"
	if MatchesSystemLabels(tenant, "server") {
		t.Fatal("a tenant-labelled resource matched control-plane identity")
	}
}

func TestRemoveOwnedContainerRefusesSameNamedForeignResource(t *testing.T) {
	deleted := false
	transport := dockerRoundTripFunc(func(request *http.Request) (*http.Response, error) {
		path := strings.TrimPrefix(request.URL.Path, "/v1.48")
		switch {
		case request.Method == http.MethodGet && path == "/containers/donkey.team.site/json":
			return dockerJSONResponse(http.StatusOK, map[string]any{
				"Id": "foreign",
				"Config": map[string]any{"Labels": map[string]string{
					labelManaged: "true", labelInstallation: dockerInstallation,
					labelRole: roleWorkload,
				}},
			}), nil
		case request.Method == http.MethodDelete:
			deleted = true
			return dockerJSONResponse(http.StatusNoContent, nil), nil
		default:
			t.Fatalf("unexpected Docker request: %s %s", request.Method, request.URL.String())
			return nil, nil
		}
	})
	cli, err := client.NewClientWithOpts(
		client.WithHost("http://docker.test"), client.WithVersion("1.48"),
		client.WithHTTPClient(&http.Client{Transport: transport}),
	)
	if err != nil {
		t.Fatal(err)
	}
	runtime := &Runtime{cli: cli}
	if err := runtime.removeOwnedContainer(context.Background(), "donkey.team.site",
		appLabels("team", "site", roleWorkload)); err == nil {
		t.Fatal("same-named foreign container was accepted")
	}
	if deleted {
		t.Fatal("same-named foreign container was deleted")
	}
}

func TestRemoveOwnedContainerDeletesTheInspectedID(t *testing.T) {
	deleted := ""
	transport := dockerRoundTripFunc(func(request *http.Request) (*http.Response, error) {
		path := strings.TrimPrefix(request.URL.Path, "/v1.48")
		switch {
		case request.Method == http.MethodGet && path == "/containers/donkey.team.site/json":
			return dockerJSONResponse(http.StatusOK, map[string]any{
				"Id":     "immutable-container-id",
				"Config": map[string]any{"Labels": appLabels("team", "site", roleWorkload)},
			}), nil
		case request.Method == http.MethodDelete && strings.HasPrefix(path, "/containers/"):
			deleted = strings.TrimPrefix(path, "/containers/")
			return dockerJSONResponse(http.StatusNoContent, nil), nil
		default:
			t.Fatalf("unexpected Docker request: %s %s", request.Method, request.URL.String())
			return nil, nil
		}
	})
	cli, err := client.NewClientWithOpts(
		client.WithHost("http://docker.test"), client.WithVersion("1.48"),
		client.WithHTTPClient(&http.Client{Transport: transport}),
	)
	if err != nil {
		t.Fatal(err)
	}
	runtime := &Runtime{cli: cli}
	if err := runtime.removeOwnedContainer(context.Background(), "donkey.team.site",
		appLabels("team", "site", roleWorkload)); err != nil {
		t.Fatal(err)
	}
	if deleted != "immutable-container-id" {
		t.Fatalf("deleted %q, want the inspected immutable ID", deleted)
	}
}

func TestDeleteSpaceCascadesAllOwnedDockerResources(t *testing.T) {
	var removedContainers, removedVolumes []string
	var disconnected int
	networkRemoved := false
	transport := dockerRoundTripFunc(func(request *http.Request) (*http.Response, error) {
		path := strings.TrimPrefix(request.URL.Path, "/v1.48")
		switch {
		case request.Method == http.MethodGet && path == "/networks/donkey-team":
			return dockerJSONResponse(http.StatusOK, network.Inspect{
				Name: "donkey-team", Labels: resourceLabels("team", roleSpace),
			}), nil
		case request.Method == http.MethodGet && strings.HasPrefix(path, "/containers/donkey-") && strings.HasSuffix(path, "/json"):
			name := strings.TrimSuffix(strings.TrimPrefix(path, "/containers/"), "/json")
			return dockerJSONResponse(http.StatusOK, map[string]any{
				"Id": name,
				"Config": map[string]any{
					"Labels": SystemLabels(strings.TrimPrefix(name, "donkey-")),
				},
			}), nil
		case request.Method == http.MethodGet && path == "/containers/json":
			assertDockerOwnershipFilter(t, request)
			return dockerJSONResponse(http.StatusOK, []container.Summary{
				{ID: "app"}, {ID: "addon"}, {ID: "helper"},
			}), nil
		case request.Method == http.MethodDelete && strings.HasPrefix(path, "/containers/"):
			removedContainers = append(removedContainers, strings.TrimPrefix(path, "/containers/"))
			return dockerJSONResponse(http.StatusNoContent, nil), nil
		case request.Method == http.MethodGet && path == "/volumes":
			assertDockerOwnershipFilter(t, request)
			return dockerJSONResponse(http.StatusOK, volume.ListResponse{Volumes: []*volume.Volume{
				{Name: "donkey.team.site.data"}, {Name: "donkey.team.postgres.data"},
			}}), nil
		case request.Method == http.MethodDelete && strings.HasPrefix(path, "/volumes/"):
			removedVolumes = append(removedVolumes, strings.TrimPrefix(path, "/volumes/"))
			return dockerJSONResponse(http.StatusNoContent, nil), nil
		case request.Method == http.MethodPost && path == "/networks/donkey-team/disconnect":
			disconnected++
			return dockerJSONResponse(http.StatusOK, nil), nil
		case request.Method == http.MethodDelete && path == "/networks/donkey-team":
			networkRemoved = true
			return dockerJSONResponse(http.StatusNoContent, nil), nil
		default:
			t.Fatalf("unexpected Docker request: %s %s", request.Method, request.URL.String())
			return nil, nil
		}
	})
	httpClient := &http.Client{Transport: transport}
	cli, err := client.NewClientWithOpts(
		client.WithHost("http://docker.test"), client.WithVersion("1.48"), client.WithHTTPClient(httpClient),
	)
	if err != nil {
		t.Fatal(err)
	}
	runtime := &Runtime{
		cli: cli,
		opts: Options{
			ServerContainer: "donkey-server", OrchestratorContainer: "donkey-orchestrator",
			GatewayContainer: "donkey-gateway", AIContainer: "donkey-ai",
			TelemetryContainer: "donkey-telemetry", ProxyContainer: "donkey-proxy",
		},
		state: state{dir: t.TempDir()},
	}
	if err := runtime.DeleteSpace(context.Background(), "team"); err != nil {
		t.Fatal(err)
	}
	if strings.Join(removedContainers, ",") != "app,addon,helper" {
		t.Errorf("removed containers=%v", removedContainers)
	}
	if strings.Join(removedVolumes, ",") != "donkey.team.site.data,donkey.team.postgres.data" {
		t.Errorf("removed volumes=%v", removedVolumes)
	}
	// Every one of donkey's own containers leaves the network first, the
	// orchestrator (which probes its deploys there) included.
	if disconnected != 6 || !networkRemoved {
		t.Errorf("disconnected=%d networkRemoved=%v", disconnected, networkRemoved)
	}
}

func TestDestroyAppRemovesEveryHelperAndItsVolume(t *testing.T) {
	wantRoles := map[string]string{
		"donkey.team.site":           roleWorkload,
		"donkey.team.site.files":     roleFiles,
		"donkey.team.site.data.init": roleInit,
	}
	var removed []string
	transport := dockerRoundTripFunc(func(request *http.Request) (*http.Response, error) {
		path := strings.TrimPrefix(request.URL.Path, "/v1.48")
		if request.Method == http.MethodGet && strings.HasPrefix(path, "/containers/") && strings.HasSuffix(path, "/json") {
			name := strings.TrimSuffix(strings.TrimPrefix(path, "/containers/"), "/json")
			role, ok := wantRoles[name]
			if !ok {
				t.Fatalf("unexpected container inspect: %s", name)
			}
			return dockerJSONResponse(http.StatusOK, map[string]any{
				"Id": name, "Config": map[string]any{"Labels": appLabels("team", "site", role)},
			}), nil
		}
		if request.Method == http.MethodDelete && strings.HasPrefix(path, "/containers/") {
			removed = append(removed, strings.TrimPrefix(path, "/containers/"))
			return dockerJSONResponse(http.StatusNoContent, nil), nil
		}
		if request.Method == http.MethodGet && path == "/volumes/donkey.team.site.data" {
			return dockerJSONResponse(http.StatusOK, volume.Volume{
				Name: "donkey.team.site.data", Labels: appLabels("team", "site", roleStorage),
			}), nil
		}
		if request.Method == http.MethodDelete && path == "/volumes/donkey.team.site.data" {
			removed = append(removed, "donkey.team.site.data")
			return dockerJSONResponse(http.StatusNoContent, nil), nil
		}
		t.Fatalf("unexpected Docker request: %s %s", request.Method, request.URL.String())
		return nil, nil
	})
	cli, err := client.NewClientWithOpts(
		client.WithHost("http://docker.test"), client.WithVersion("1.48"),
		client.WithHTTPClient(&http.Client{Transport: transport}),
	)
	if err != nil {
		t.Fatal(err)
	}
	runtime := &Runtime{cli: cli, state: state{dir: t.TempDir()}}
	if err := runtime.Destroy(context.Background(), "team", "site", false); err != nil {
		t.Fatal(err)
	}
	if got := strings.Join(removed, ","); got !=
		"donkey.team.site,donkey.team.site.files,donkey.team.site.data.init,donkey.team.site.data" {
		t.Errorf("removed=%s", got)
	}
}

// --keep-data spares what Kubernetes spares: the env Secret and the binding
// Secrets. Metadata, routes and the run record go with the Deployment and
// route ConfigMaps either way.
func TestDestroyKeepDataKeepsEnvAndBindings(t *testing.T) {
	for _, keepData := range []bool{true, false} {
		t.Run(map[bool]string{true: "keep data", false: "remove data"}[keepData], func(t *testing.T) {
			cli, err := client.NewClientWithOpts(
				client.WithHost("http://docker.test"), client.WithVersion("1.48"),
				client.WithHTTPClient(&http.Client{Transport: dockerRoundTripFunc(func(*http.Request) (*http.Response, error) {
					return dockerJSONResponse(http.StatusNotFound, map[string]string{"message": "not found"}), nil
				})}),
			)
			if err != nil {
				t.Fatal(err)
			}
			defer cli.Close()
			r := &Runtime{cli: cli, state: state{dir: t.TempDir()}}
			// The env as an older version left it: a legacy JSON record.
			if err := os.MkdirAll(r.state.path("env", "team"), 0o755); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(r.state.path("env", "team", "site.json"), []byte(`{"API_KEY":"set-by-donkey-env"}`), 0o600); err != nil {
				t.Fatal(err)
			}
			for owner, url := range map[string]string{"site": "postgres://site", sharedOwner: "postgres://space"} {
				if err := r.state.setBinding("team", owner, "postgres", map[string]string{"DATABASE_URL": url}); err != nil {
					t.Fatal(err)
				}
			}
			if err := r.state.setMeta("team", "site", map[string]string{"key": "value"}); err != nil {
				t.Fatal(err)
			}
			if err := r.setRunRecord("team", "site", runSpec{Image: "site:1", Port: 8080}); err != nil {
				t.Fatal(err)
			}
			for _, name := range []string{"site", pathRouteName("site", "web", "/api")} {
				if err := r.state.setRoute("team", name, Route{Space: "team", App: "site", Host: "web.team.example.com", Path: "/", Backend: "http://site:8080"}); err != nil {
					t.Fatal(err)
				}
			}

			if err := r.Destroy(context.Background(), "team", "site", keepData); err != nil {
				t.Fatal(err)
			}

			env, err := r.state.env("team", "site")
			if err != nil || (env["API_KEY"] == "set-by-donkey-env") != keepData {
				t.Fatalf("env after destroy = %v, %v", env, err)
			}
			bindings, err := r.state.listBindings("team", "site")
			if err != nil || (len(bindings) == 1) != keepData {
				t.Fatalf("binding copies after destroy = %v, %v", bindings, err)
			}
			if !keepData {
				for _, file := range []string{"env/team/site.json", "env/team/site.yaml", "bindings/team/site.postgres.yaml"} {
					if _, err := os.Stat(r.state.path(file)); !os.IsNotExist(err) {
						t.Errorf("%s survived the destroy", file)
					}
				}
			}
			if shared, err := r.state.binding("team", sharedOwner, "postgres"); err != nil || shared["DATABASE_URL"] != "postgres://space" {
				t.Fatalf("the space's own binding was touched: %v, %v", shared, err)
			}
			if meta, err := r.state.meta("team", "site"); err != nil || len(meta) != 0 {
				t.Fatalf("metadata survived: %v, %v", meta, err)
			}
			if run, err := r.runRecord("team", "site"); err != nil || run != nil {
				t.Fatalf("run record survived: %+v, %v", run, err)
			}
			if routes, err := r.state.listRoutes("team"); err != nil || len(routes) != 0 {
				t.Fatalf("routes survived: %v, %v", routes, err)
			}
		})
	}
}

func assertDockerOwnershipFilter(t *testing.T, request *http.Request) {
	t.Helper()
	filter := request.URL.Query().Get("filters")
	for _, want := range []string{labelManaged, labelInstallation, labelSpace} {
		if !strings.Contains(filter, want) {
			t.Errorf("%s filter %q lacks %q", request.URL.Path, filter, want)
		}
	}
}

func dockerJSONResponse(status int, value any) *http.Response {
	var body io.Reader = strings.NewReader("")
	if value != nil {
		data, _ := json.Marshal(value)
		body = strings.NewReader(string(data))
	}
	return &http.Response{
		StatusCode: status, Status: http.StatusText(status),
		Header: http.Header{"Content-Type": []string{"application/json"}},
		Body:   io.NopCloser(body),
	}
}
