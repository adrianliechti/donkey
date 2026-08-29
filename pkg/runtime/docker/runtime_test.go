package docker

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
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
		"app":   appLabels("team", "site", componentWeb),
		"addon": addonLabels("team", "postgres", componentAddon),
		"space": resourceLabels("team", componentSpace),
	} {
		if labels[labelManaged] != "true" || labels[labelInstallation] != dockerInstallation ||
			labels[labelSpace] != "team" {
			t.Errorf("%s ownership labels=%v", name, labels)
		}
	}
	if labels := appLabels("team", "site", componentWeb); labels[labelApp] != "site" || labels[labelAddon] != "" {
		t.Errorf("app identity=%v", labels)
	}
	if labels := addonLabels("team", "postgres", componentAddon); labels[labelAddon] != "postgres" || labels[labelApp] != "" {
		t.Errorf("addon identity=%v", labels)
	}
	for key := range SystemLabels("server") {
		if !strings.HasPrefix(key, "dev.donkey.") {
			t.Errorf("Docker label %q is not reverse-DNS namespaced", key)
		}
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
				Name: "donkey-team", Labels: resourceLabels("team", componentSpace),
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
			ServerContainer: "donkey-server", GatewayContainer: "donkey-gateway",
			AIContainer: "donkey-ai", TelemetryContainer: "donkey-telemetry",
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
	if disconnected != 4 || !networkRemoved {
		t.Errorf("disconnected=%d networkRemoved=%v", disconnected, networkRemoved)
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
