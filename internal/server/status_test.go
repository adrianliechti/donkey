package server

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	rt "github.com/adrianliechti/donkey/pkg/runtime"
)

type endpointStatusRuntime struct {
	rt.Provider
	status        *rt.AppStatus
	endpoint      rt.EndpointStatus
	endpointCalls int
}

func (*endpointStatusRuntime) ValidateSpace(string) error { return nil }

func (r *endpointStatusRuntime) Status(context.Context, string, string) (*rt.AppStatus, error) {
	return r.status, nil
}

func (*endpointStatusRuntime) HasData(context.Context, string, string) bool { return false }

func (r *endpointStatusRuntime) EndpointStatus(_ context.Context, _, _, host string) rt.EndpointStatus {
	r.endpointCalls++
	return r.endpoint
}

func TestStatusIncludesEndpointReadinessForExposedApp(t *testing.T) {
	expires := time.Date(2027, time.January, 2, 3, 4, 5, 0, time.FixedZone("test", 2*60*60))
	provider := &endpointStatusRuntime{
		status: &rt.AppStatus{
			Found: true, UpToDate: true, WantReplica: 1, ReadyReplica: 1,
			PublicHost: "shop.acme.example.com",
		},
		endpoint: rt.EndpointStatus{
			Certificate: rt.CertificateStatus{
				State: rt.EndpointReady, Mode: "cert-manager", Issuer: "ClusterIssuer platform-ca",
				ExpiresAt: expires,
			},
			DNS: rt.DNSStatus{
				State: rt.EndpointReady, Observed: true, RecordType: "A",
				Expected: []string{"10.204.166.5"},
			},
		},
	}
	s := &Server{cfg: &Config{Domain: "example.com"}, rt: provider}
	req := httptest.NewRequest(http.MethodGet, "/v1/spaces/acme/apps/shop/status", nil)
	req.SetPathValue("space", "acme")
	req.SetPathValue("app", "shop")
	rec := httptest.NewRecorder()

	s.handleStatus(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("status code = %d, body %s", rec.Code, rec.Body.String())
	}
	var response StatusResponse
	if err := json.NewDecoder(rec.Body).Decode(&response); err != nil {
		t.Fatal(err)
	}
	if provider.endpointCalls != 1 || response.Endpoint == nil {
		t.Fatalf("endpoint calls = %d, response = %+v", provider.endpointCalls, response.Endpoint)
	}
	if response.Endpoint.Host != "shop.acme.example.com" ||
		response.Endpoint.Certificate.State != "ready" || response.Endpoint.DNS.State != "ready" {
		t.Fatalf("endpoint response = %+v", response.Endpoint)
	}
	if want := expires.UTC().Format(time.RFC3339); response.Endpoint.Certificate.ExpiresAt != want {
		t.Errorf("expiresAt = %q, want %q", response.Endpoint.Certificate.ExpiresAt, want)
	}
}

func TestStatusSkipsEndpointChecksForInternalApp(t *testing.T) {
	provider := &endpointStatusRuntime{status: &rt.AppStatus{
		Found: true, UpToDate: true, WantReplica: 1, ReadyReplica: 1,
	}}
	s := &Server{cfg: &Config{Domain: "example.com"}, rt: provider}
	req := httptest.NewRequest(http.MethodGet, "/v1/spaces/acme/apps/worker/status", nil)
	req.SetPathValue("space", "acme")
	req.SetPathValue("app", "worker")
	rec := httptest.NewRecorder()

	s.handleStatus(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("status code = %d, body %s", rec.Code, rec.Body.String())
	}
	if provider.endpointCalls != 0 {
		t.Fatalf("internal app triggered %d endpoint checks", provider.endpointCalls)
	}
}
