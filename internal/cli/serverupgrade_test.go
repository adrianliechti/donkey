package cli

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/adrianliechti/donkey/internal/ui"
	"github.com/docker/docker/client"
)

func TestDockerUpgradeDrainsTheProcessThatOwnsBuilds(t *testing.T) {
	for _, tc := range []struct {
		name, orchestrator, server, stopped string
	}{
		{"from main", "missing", "running", dockerServerName},
		{"from vnext", "running", "running", dockerOrchestratorName},
		{"fresh install", "missing", "missing", ""},
		{"already stopped", "stopped", "running", ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			stopped := ""
			api := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				p := strings.TrimPrefix(r.URL.Path, "/v1.48/containers/")
				if r.Method == http.MethodPost && strings.HasSuffix(p, "/stop") {
					stopped = strings.TrimSuffix(p, "/stop")
					if r.URL.Query().Get("t") != "660" {
						t.Errorf("builds got the wrong drain timeout: %s", r.URL)
					}
					w.WriteHeader(http.StatusNoContent)
					return
				}
				state := tc.orchestrator
				if p == dockerServerName+"/json" {
					state = tc.server
				} else if p != dockerOrchestratorName+"/json" {
					t.Errorf("unexpected Docker request: %s", r.URL)
				}
				w.Header().Set("Content-Type", "application/json")
				if state == "missing" {
					w.WriteHeader(http.StatusNotFound)
					fmt.Fprint(w, `{"message":"no such container"}`)
					return
				}
				fmt.Fprintf(w, `{"State":{"Running":%t}}`, state == "running")
			}))
			defer api.Close()
			cli, err := client.NewClientWithOpts(client.WithHost(api.URL), client.WithVersion("1.48"))
			if err != nil {
				t.Fatal(err)
			}
			defer cli.Close()
			if err := drainDockerDeploys(context.Background(), cli, &ui.UI{Out: io.Discard}); err != nil {
				t.Fatal(err)
			}
			if stopped != tc.stopped {
				t.Fatalf("stopped %q, want %q", stopped, tc.stopped)
			}
		})
	}
}
