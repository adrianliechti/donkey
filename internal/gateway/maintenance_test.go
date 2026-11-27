package gateway

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/adrianliechti/donkey/internal/config"
	"github.com/adrianliechti/donkey/internal/identity"
	"github.com/adrianliechti/donkey/pkg/runtime"
)

type countedSnapshot struct {
	snapshot runtime.GatewaySnapshot
	reads    int
}

func (c *countedSnapshot) GatewaySnapshot(context.Context) (runtime.GatewaySnapshot, error) {
	c.reads++
	return c.snapshot, nil
}

func TestGatewayServesMaintenanceFromMemoryAfterAccessCheck(t *testing.T) {
	var forwarded atomic.Int64
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		forwarded.Add(1)
		w.WriteHeader(http.StatusNoContent)
	}))
	defer upstream.Close()
	s := testServer()
	s.cfg.OIDCIssuer, s.cfg.OIDCClientID, s.cfg.OIDCClientSecret = "https://idp.example", "client", "secret"
	now := time.Now()
	source := &countedSnapshot{snapshot: runtime.GatewaySnapshot{
		Routes: []config.GatewayRoute{
			{Space: "team", App: "healthy", Host: "web.example", Path: "/", Backend: upstream.URL},
			{Space: "team", App: "web", Host: "web.example", Path: "/shop", Backend: upstream.URL},
			{Space: "team", App: "web", Host: "private.example", Login: true, Backend: upstream.URL},
			{Space: "team", App: "web", Host: "api.example", Login: true, Mode: config.AppModeAPI, Backend: upstream.URL},
		},
		Platform: config.PlatformConfig{GatewayMaintenance: config.GatewayMaintenance{CriticalDays: 1}},
		Spaces:   map[string]config.SpaceConfig{"team": {AllowPublic: true, Viewers: config.AccessConfig{Users: []string{"owner@example.com"}}}},
		Maintenance: map[string]config.SpaceMaintenance{"team": {Apps: map[string]config.MaintenanceTimes{
			"web": {CriticalSince: now.Add(-24 * time.Hour).Unix()},
		}}},
	}}
	s.source = runtimeRoutes{source: source}
	refresh := func() {
		t.Helper()
		if err := s.refreshRoutes(t.Context()); err != nil {
			t.Fatal(err)
		}
	}
	refresh()
	handler := s.handler()
	request := func(target, accept, token string, want int) *httptest.ResponseRecorder {
		t.Helper()
		r := httptest.NewRequest("GET", "https://"+target, nil)
		r.Header.Set("Accept", accept)
		if token != "" {
			r.Header.Set("Authorization", "Bearer "+token)
		}
		w := httptest.NewRecorder()
		handler.ServeHTTP(w, r)
		if w.Code != want {
			t.Fatalf("%s: got %d, want %d: %s", target, w.Code, want, w.Body)
		}
		return w
	}
	id := identity.Identity{Subject: "u1", Email: "owner@example.com", Expires: now.Add(time.Hour).Unix()}
	allowed, err := identity.SealToken(testKey, id)
	if err != nil {
		t.Fatal(err)
	}
	apiToken, err := identity.Seal(testKey, purposeOAuthToken, oauthAccessToken{Identity: id, Resource: "https://api.example"})
	if err != nil {
		t.Fatal(err)
	}
	id.Email = "visitor@example.com"
	denied, err := identity.SealToken(testKey, id)
	if err != nil {
		t.Fatal(err)
	}
	request("private.example/", "application/json", "", 401)
	request("private.example/", "application/json", denied, 403)
	request("api.example/", "application/json", "", 401)
	for range 100 {
		w := request("web.example/shop/item", "text/html", "", 503)
		if w.Header().Get("Cache-Control") != "no-store" || !strings.Contains(w.Body.String(), "owner") {
			t.Fatal("missing owner notice or freshness headers")
		}
	}
	for _, w := range []*httptest.ResponseRecorder{
		request("private.example/", "application/json", allowed, 503),
		request("api.example/", "application/json", apiToken, 503),
	} {
		if !strings.Contains(w.Body.String(), `"maintenance_required"`) {
			t.Fatal(w.Body)
		}
	}
	if source.reads != 1 || forwarded.Load() != 0 {
		t.Fatalf("maintenance requests reached storage or upstream: %d snapshot reads, %d upstream calls", source.reads, forwarded.Load())
	}
	request("web.example/", "text/html", "", 204)
	source.snapshot.Platform.GatewayMaintenance.CriticalDays = 2
	refresh()
	request("web.example/shop", "text/html", "", 204)
	delete(source.snapshot.Maintenance, "team")
	source.snapshot.Platform.GatewayMaintenance.CriticalDays = 1
	refresh()
	request("web.example/shop", "text/html", "", 204)
}
