package gateway

import (
	"context"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/sdk/metric"
	"go.opentelemetry.io/otel/sdk/metric/metricdata"

	"github.com/adrianliechti/donkey/internal/config"
	"github.com/adrianliechti/donkey/internal/identity"
	"github.com/adrianliechti/donkey/internal/telemetry"
)

func TestGatewayTelemetryCoversAuthenticationAndPreflight(t *testing.T) {
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"ok":true}`))
	}))
	t.Cleanup(backend.Close)
	target, _ := url.Parse(backend.URL)
	s := mcpTestServer(t, target)
	s.lastAttempt = time.Now().Add(time.Hour)
	reader := metric.NewManualReader()
	provider := metric.NewMeterProvider(metric.WithReader(reader))
	t.Cleanup(func() { _ = provider.Shutdown(context.Background()) })
	duration, err := provider.Meter("test").Float64Histogram(telemetry.HTTPServerRequestDuration)
	if err != nil {
		t.Fatal(err)
	}
	callers := &batchExporter[telemetry.CallerEvent]{queue: make(chan telemetry.CallerEvent, 16)}
	s.telemetry = &edgeTelemetry{provider: provider, duration: duration, key: testKey, callers: callers}

	grant := func(email, resource string, expiry time.Time) string {
		t.Helper()
		token, err := identity.Seal(testKey, purposeOAuthToken, &oauthAccessToken{
			Identity: identity.Identity{Subject: email, Email: email, Expires: expiry.Unix()}, Resource: resource,
		})
		if err != nil {
			t.Fatal(err)
		}
		return token
	}
	valid := grant("ada@example.com", "https://tools.example/mcp", time.Now().Add(time.Hour))
	for _, tc := range []struct {
		name, method, token string
		preflight           bool
		want                int
	}{
		{"missing bearer", "POST", "", false, 401},
		{"invalid bearer", "POST", "invalid", false, 401},
		{"expired bearer", "POST", grant("ada@example.com", "https://tools.example/mcp", time.Now().Add(-time.Hour)), false, 401},
		{"wrong resource", "POST", grant("ada@example.com", "https://other.example/mcp", time.Now().Add(time.Hour)), false, 401},
		{"access denied", "POST", grant("grace@example.com", "https://tools.example/mcp", time.Now().Add(time.Hour)), false, 403},
		{"success", "POST", valid, false, 200},
		{"preflight", "OPTIONS", "", true, 204},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r := httptest.NewRequest(tc.method, "https://tools.example/mcp", strings.NewReader(`{}`))
			if tc.token != "" {
				r.Header.Set("Authorization", "Bearer "+tc.token)
			}
			if tc.preflight {
				r.Header.Set("Access-Control-Request-Method", "POST")
			}
			w := httptest.NewRecorder()
			s.serveApp(w, r)
			if w.Code != tc.want {
				t.Fatalf("response=%d: %s", w.Code, w.Body.String())
			}
		})
	}
	// Browser authentication uses the same outer capture as OAuth.
	rt := s.routes["tools.example"][0]
	rt.Mode = config.AppModeWeb
	s.routes["tools.example"] = []route{rt}
	r := httptest.NewRequest("GET", "https://tools.example/api", nil)
	w := httptest.NewRecorder()
	s.serveApp(w, r)
	if w.Code != 401 {
		t.Fatalf("web API response=%d", w.Code)
	}
	var metrics metricdata.ResourceMetrics
	if err := reader.Collect(context.Background(), &metrics); err != nil {
		t.Fatal(err)
	}
	counts := map[int]uint64{}
	for _, scope := range metrics.ScopeMetrics {
		for _, m := range scope.Metrics {
			h, ok := m.Data.(metricdata.Histogram[float64])
			if !ok {
				continue
			}
			for _, p := range h.DataPoints {
				status, _ := p.Attributes.Value(attribute.Key("http.response.status_code"))
				counts[int(status.AsInt64())] += p.Count
				for _, a := range p.Attributes.ToSlice() {
					if strings.Contains(string(a.Key), "user") || strings.Contains(string(a.Key), "caller") {
						t.Fatalf("identity metric label: %v", a)
					}
				}
			}
		}
	}
	if counts[401] != 5 || counts[403] != 1 || counts[200] != 1 || counts[204] != 1 {
		t.Fatalf("HTTP counts=%v", counts)
	}
	if len(callers.queue) != 2 {
		t.Fatalf("only verified identities should be observed: %d", len(callers.queue))
	}
	for range 2 {
		e := <-callers.queue
		if len(e.UserHash) != 64 || len(e.SpaceUserHash) != 64 || e.UserHash == e.SpaceUserHash || e.App != "tools" {
			t.Fatalf("caller=%+v", e)
		}
	}
}
