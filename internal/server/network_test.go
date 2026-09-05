package server

import (
	"context"
	"net/http/httptest"
	"testing"

	"github.com/adrianliechti/donkey/internal/config"
	"github.com/adrianliechti/donkey/pkg/runtime"
)

type networkRuntime struct {
	runtime.Provider
	status  runtime.AppStatus
	network runtime.NetworkState
}

func (r networkRuntime) Status(context.Context, string, string) (*runtime.AppStatus, error) {
	return &r.status, nil
}

func (r networkRuntime) Network(context.Context, string, string) (*runtime.NetworkState, error) {
	return &r.network, nil
}

func TestNetworkResponseRetainsInternalAPIMode(t *testing.T) {
	s := &Server{
		cfg: &Config{Domain: "apps.example"},
		rt: networkRuntime{status: runtime.AppStatus{
			Found: true, Port: 8000, Mode: config.AppModeAPI, CORS: config.CORSAllowAll,
		}},
	}
	resp, err := s.networkResponse(httptest.NewRequest("GET", "/", nil), "acme", "tools")
	if err != nil {
		t.Fatal(err)
	}
	if resp.Mode != config.AppModeAPI || resp.Exposure != "internal" || resp.CORS != config.CORSAllowAll {
		t.Fatalf("internal API network response = %+v", resp)
	}
}

func TestNetworkResponseUsesPublicRouteMode(t *testing.T) {
	s := &Server{
		cfg: &Config{Domain: "apps.example"},
		rt: networkRuntime{
			status:  runtime.AppStatus{Found: true, Port: 8000, CORS: config.CORSAllowAll},
			network: runtime.NetworkState{RouteFound: true, Host: "tools.acme.apps.example", Mode: config.AppModeAPI},
		},
	}
	resp, err := s.networkResponse(httptest.NewRequest("GET", "/", nil), "acme", "tools")
	if err != nil {
		t.Fatal(err)
	}
	if resp.Mode != config.AppModeAPI || resp.Exposure != "public" || resp.CORS != "" {
		t.Fatalf("public API network response = %+v", resp)
	}
}
