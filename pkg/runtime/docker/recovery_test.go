package docker

import (
	"context"
	"net/http"
	"strings"
	"testing"

	"github.com/adrianliechti/donkey/internal/config"
	"github.com/docker/docker/client"
)

func TestRecoveryRestoresExactContainerAndAppRecords(t *testing.T) {
	for _, candidateExists := range []bool{false, true} {
		t.Run(map[bool]string{false: "creation failed", true: "readiness failed"}[candidateExists], func(t *testing.T) {
			parked, removed, restarted, restored := false, false, false, false
			labels := appLabels("team", "app", roleWorkload)
			transport := dockerRoundTripFunc(func(req *http.Request) (*http.Response, error) {
				p := strings.TrimPrefix(req.URL.Path, "/v1.48")
				switch {
				case req.Method == "GET" && strings.HasSuffix(p, "/json"):
					id := "original-id"
					if parked {
						if !candidateExists {
							return dockerJSONResponse(404, map[string]string{"message": "not found"}), nil
						}
						id = "candidate-id"
					}
					return dockerJSONResponse(200, map[string]any{"Id": id, "Name": "/donkey.team.app", "Config": map[string]any{"Labels": labels}, "State": map[string]any{"Running": true}}), nil
				case p == "/containers/original-id/stop":
					return dockerJSONResponse(204, nil), nil
				case p == "/containers/original-id/rename":
					if !parked {
						if !strings.Contains(req.URL.Query().Get("name"), ".previous-") {
							t.Fatal("missing backup name")
						}
						parked = true
					} else {
						restored = true
					}
					return dockerJSONResponse(204, nil), nil
				case p == "/containers/candidate-id" && req.Method == "DELETE":
					removed = true
					return dockerJSONResponse(204, nil), nil
				case p == "/containers/original-id/start":
					restarted = true
					return dockerJSONResponse(204, nil), nil
				default:
					t.Fatalf("unexpected Docker request: %s %s", req.Method, req.URL)
					return nil, nil
				}
			})
			cli, err := client.NewClientWithOpts(client.WithHost("http://docker.test"), client.WithVersion("1.48"), client.WithHTTPClient(&http.Client{Transport: transport}))
			if err != nil {
				t.Fatal(err)
			}
			defer cli.Close()
			r := &Runtime{cli: cli, state: state{dir: t.TempDir()}}
			if err := r.state.setEnv("team", "app", map[string]string{"MODE": "original"}); err != nil {
				t.Fatal(err)
			}
			if err := r.setRunRecord("team", "app", runSpec{Image: "original:tag", Command: []string{"original"}}); err != nil {
				t.Fatal(err)
			}
			app := &config.AppConfig{Name: "app", Addons: []config.AddonConfig{{Name: "postgres"}}}
			recovery, err := r.prepareRecovery(context.Background(), "team", app)
			if err != nil {
				t.Fatal(err)
			}
			if err := r.state.setEnv("team", "app", map[string]string{"MODE": "candidate"}); err != nil {
				t.Fatal(err)
			}
			if err := r.state.setBinding("team", "app", "postgres", map[string]string{"URL": "new"}); err != nil {
				t.Fatal(err)
			}
			if err := r.setRunRecord("team", "app", runSpec{Image: "candidate:tag"}); err != nil {
				t.Fatal(err)
			}
			if err := recovery.park(context.Background()); err != nil {
				t.Fatal(err)
			}
			if err := recovery.restore(context.Background()); err != nil {
				t.Fatal(err)
			}
			if removed != candidateExists || !restarted || !restored {
				t.Fatalf("recovery incomplete: removed=%v restarted=%v restored=%v", removed, restarted, restored)
			}
			env, _ := r.state.env("team", "app")
			if env["MODE"] != "original" {
				t.Fatal(env)
			}
			run, _ := r.runRecord("team", "app")
			if run.Image != "original:tag" || len(run.Command) != 1 {
				t.Fatal(run)
			}
			binding, _ := r.state.binding("team", "app", "postgres")
			if len(binding) != 0 {
				t.Fatal("new binding survived rollback")
			}
		})
	}
}
