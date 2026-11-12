package docker

import (
	"context"
	"errors"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	rt "github.com/adrianliechti/donkey/pkg/runtime"
	"github.com/docker/docker/client"
)

func TestReadinessChecksHTTPStatusAndHost(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Host != "app.example.com" {
			http.Error(w, "unknown host", 400)
			return
		}
		switch r.URL.Path {
		case "/ready":
			w.WriteHeader(http.StatusNoContent)
		case "/redirect":
			w.Header().Set("Location", "https://unreachable.invalid/")
			w.WriteHeader(http.StatusFound)
		default:
			http.Error(w, "not ready", 503)
		}
	}))
	defer srv.Close()
	addr := strings.TrimPrefix(srv.URL, "http://")
	for _, tc := range []struct {
		host, path string
		ok         bool
	}{
		{"app.example.com", "/ready", true}, {"app.example.com", "/redirect", true},
		{"app.example.com", "/failed", false}, {"wrong.example.com", "/ready", false},
	} {
		err := probeEndpoint(context.Background(), addr, tc.host, tc.path)
		if (err == nil) != tc.ok {
			t.Fatalf("%+v: %v", tc, err)
		}
	}
}

func TestReadinessTCPRequiresListener(t *testing.T) {
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	address := l.Addr().String()
	if err := probeEndpoint(context.Background(), address, "", ""); err != nil {
		t.Fatal(err)
	}
	l.Close()
	if err := probeEndpoint(context.Background(), address, "", ""); err == nil {
		t.Fatal("closed port passed readiness")
	}
}

func TestRunningContainerWithoutListenerFailsRollout(t *testing.T) {
	transport := dockerRoundTripFunc(func(request *http.Request) (*http.Response, error) {
		if strings.HasSuffix(request.URL.Path, "/json") {
			return dockerJSONResponse(http.StatusOK, map[string]any{
				"Id": "app-id", "Name": "/127.0.0.1", "Config": map[string]any{"Labels": map[string]string{labelApp: "app", labelRole: roleWorkload, labelPort: "1"}},
				"State": map[string]any{"Running": true, "Status": "running"},
			}), nil
		}
		if strings.HasSuffix(request.URL.Path, "/logs") {
			return dockerJSONResponse(http.StatusOK, nil), nil
		}
		t.Fatalf("unexpected request: %s", request.URL)
		return nil, nil
	})
	cli, err := client.NewClientWithOpts(client.WithHost("http://docker.test"), client.WithVersion("1.48"), client.WithHTTPClient(&http.Client{Transport: transport}))
	if err != nil {
		t.Fatal(err)
	}
	defer cli.Close()
	r := &Runtime{cli: cli}
	err = r.waitRunning(context.Background(), "app", 30*time.Millisecond)
	var rollout *rt.RolloutError
	if !errors.As(err, &rollout) || !strings.Contains(rollout.Reason, "not listening") {
		t.Fatalf("not a repairable readiness failure: %v", err)
	}
}
