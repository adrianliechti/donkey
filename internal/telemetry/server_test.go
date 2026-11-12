package telemetry

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	metricsv1 "go.opentelemetry.io/proto/otlp/collector/metrics/v1"
	commonpb "go.opentelemetry.io/proto/otlp/common/v1"
	metricspb "go.opentelemetry.io/proto/otlp/metrics/v1"
	codepb "google.golang.org/genproto/googleapis/rpc/code"
	statuspb "google.golang.org/genproto/googleapis/rpc/status"
	"google.golang.org/protobuf/proto"
)

func TestOTLPHTTPRequiresScopeTokenAndServesAuthenticatedReport(t *testing.T) {
	key := bytes.Repeat([]byte{7}, 32)
	service, err := NewService(&Config{DatabasePath: ":memory:", RetentionDays: 30, Key: key})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = service.store.Close() })

	now := time.Now().UTC()
	export := exportRequest(now, []histogramPoint{{status: 200, count: 4, sum: 0.2, bucket: 5}})
	export.ResourceMetrics[0].Resource.Attributes = []*commonpb.KeyValue{
		stringAttribute("service.namespace", "team"),
		stringAttribute("service.name", "site"),
	}
	body, err := proto.Marshal(export)
	if err != nil {
		t.Fatal(err)
	}

	unauthorized := httptest.NewRequest(http.MethodPost, "/v1/metrics", bytes.NewReader(body))
	unauthorized.Header.Set("Content-Type", "application/x-protobuf")
	unauthorized.Header.Set(TelemetryTokenHeader, AppToken(key, "team", "other"))
	unauthorizedResponse := httptest.NewRecorder()
	service.ingestHandler().ServeHTTP(unauthorizedResponse, unauthorized)
	if unauthorizedResponse.Code != http.StatusUnauthorized {
		t.Fatalf("wrong scope status=%d body=%s", unauthorizedResponse.Code, unauthorizedResponse.Body.String())
	}

	request := httptest.NewRequest(http.MethodPost, "/v1/metrics", bytes.NewReader(body))
	request.Header.Set("Content-Type", "application/x-protobuf")
	request.Header.Set(TelemetryTokenHeader, AppToken(key, "team", "site"))
	response := httptest.NewRecorder()
	service.ingestHandler().ServeHTTP(response, request)
	if response.Code != http.StatusOK {
		t.Fatalf("ingest status=%d body=%s", response.Code, response.Body.String())
	}

	query := httptest.NewRequest(http.MethodGet, "/v1/traffic?space=team&app=site&source=app&range=48h", nil)
	query.Header.Set("Authorization", "Bearer "+QueryToken(key))
	queryResponse := httptest.NewRecorder()
	service.apiHandler().ServeHTTP(queryResponse, query)
	if queryResponse.Code != http.StatusOK {
		t.Fatalf("query status=%d body=%s", queryResponse.Code, queryResponse.Body.String())
	}
	var report TrafficReport
	if err := json.NewDecoder(queryResponse.Body).Decode(&report); err != nil {
		t.Fatal(err)
	}
	if report.Totals.Requests != 4 || report.Totals.Successful != 4 {
		t.Fatalf("unexpected report: %+v", report.Totals)
	}
}

func TestRuntimeCredentialIngestsResourcesButNotTraffic(t *testing.T) {
	key := bytes.Repeat([]byte{5}, 32)
	service, err := NewService(&Config{DatabasePath: ":memory:", RetentionDays: 30, Key: key})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = service.store.Close() })

	rm, err := resourceMetrics(ResourceObservation{
		Space: "team", App: "site", Instance: "one", SampledAt: time.Now().UTC(),
		CPUCores: 0.25, MemoryBytes: 64 * 1024 * 1024,
	})
	if err != nil {
		t.Fatal(err)
	}
	body, err := proto.Marshal(&metricsv1.ExportMetricsServiceRequest{
		ResourceMetrics: []*metricspb.ResourceMetrics{rm},
	})
	if err != nil {
		t.Fatal(err)
	}
	request := httptest.NewRequest(http.MethodPost, "/v1/metrics", bytes.NewReader(body))
	request.Header.Set("Content-Type", "application/x-protobuf")
	request.Header.Set(TelemetryTokenHeader, RuntimeToken(key))
	response := httptest.NewRecorder()
	service.ingestHandler().ServeHTTP(response, request)
	if response.Code != http.StatusOK {
		t.Fatalf("ingest status=%d body=%s", response.Code, response.Body.String())
	}

	query := httptest.NewRequest(http.MethodGet, "/v1/resources?space=team&app=site&range=48h", nil)
	query.Header.Set("Authorization", "Bearer "+QueryToken(key))
	queryResponse := httptest.NewRecorder()
	service.apiHandler().ServeHTTP(queryResponse, query)
	if queryResponse.Code != http.StatusOK {
		t.Fatalf("query status=%d body=%s", queryResponse.Code, queryResponse.Body.String())
	}
	var report ResourceReport
	if err := json.NewDecoder(queryResponse.Body).Decode(&report); err != nil {
		t.Fatal(err)
	}
	if report.Current == nil || report.Current.CPUCores != 0.25 || report.Current.MemoryBytes != 64*1024*1024 {
		t.Fatalf("resource report=%+v", report)
	}
}

func TestDeleteSpaceEndpointRequiresQueryCredentialAndPurgesHistory(t *testing.T) {
	key := bytes.Repeat([]byte{6}, 32)
	service, err := NewService(&Config{DatabasePath: ":memory:", RetentionDays: 30, Key: key})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = service.store.Close() })

	now := time.Now().UTC()
	export := exportRequest(now, []histogramPoint{{status: 200, count: 3, sum: 0.15, bucket: 5}})
	export.ResourceMetrics[0].Resource.Attributes = []*commonpb.KeyValue{
		stringAttribute("service.namespace", "team"),
		stringAttribute("service.name", "site"),
	}
	if _, _, err := service.store.IngestMetrics(t.Context(), export, false); err != nil {
		t.Fatal(err)
	}

	unauthorized := httptest.NewRequest(http.MethodDelete, "/v1/spaces/team", nil)
	unauthorizedResponse := httptest.NewRecorder()
	service.apiHandler().ServeHTTP(unauthorizedResponse, unauthorized)
	if unauthorizedResponse.Code != http.StatusUnauthorized {
		t.Fatalf("unauthorized purge status=%d", unauthorizedResponse.Code)
	}

	request := httptest.NewRequest(http.MethodDelete, "/v1/spaces/team", nil)
	request.Header.Set("Authorization", "Bearer "+QueryToken(key))
	response := httptest.NewRecorder()
	service.apiHandler().ServeHTTP(response, request)
	if response.Code != http.StatusOK {
		t.Fatalf("purge status=%d body=%s", response.Code, response.Body.String())
	}
	report, err := service.store.Traffic(t.Context(), "team", "site", SourceApp, Range48Hours, now)
	if err != nil {
		t.Fatal(err)
	}
	if report.Totals.Requests != 0 {
		t.Fatalf("deleted space retained traffic: %+v", report.Totals)
	}
}

func TestViewsRequireGatewayCredentialAndNeverStoreRawIdentity(t *testing.T) {
	key := bytes.Repeat([]byte{9}, 32)
	service, err := NewService(&Config{DatabasePath: ":memory:", RetentionDays: 30, Key: key})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = service.store.Close() })

	appHash, spaceHash := VisitorHashes(key, "team", "site", "the-raw-subject")
	if strings.Contains(appHash, "the-raw-subject") || strings.Contains(spaceHash, "the-raw-subject") {
		t.Fatal("visitor hash contains the raw subject")
	}
	body, _ := json.Marshal(ViewBatch{Events: []ViewEvent{{
		TimeUnixNano: uint64(time.Now().UnixNano()), Space: "team", App: "site",
		AppVisitorHash: appHash, SpaceVisitorHash: spaceHash,
	}}})
	request := httptest.NewRequest(http.MethodPost, "/v1/donkey/views", bytes.NewReader(body))
	request.Header.Set(TelemetryTokenHeader, GatewayToken(key))
	response := httptest.NewRecorder()
	service.ingestHandler().ServeHTTP(response, request)
	if response.Code != http.StatusOK {
		t.Fatalf("view status=%d body=%s", response.Code, response.Body.String())
	}

	var rawSubjectRows int
	if err := service.store.db.QueryRow(`
SELECT COUNT(*) FROM view_visitors
WHERE app_visitor_hash = ? OR space_visitor_hash = ?`, "the-raw-subject", "the-raw-subject").Scan(&rawSubjectRows); err != nil {
		t.Fatal(err)
	}
	if rawSubjectRows != 0 {
		t.Fatal("raw subject reached DuckDB")
	}
}

// OTLP/HTTP requires every 4xx/5xx body to be a google.rpc.Status in the
// request's encoding, and the response Content-Type to mirror the request.
func TestOTLPErrorsAreStatusMessages(t *testing.T) {
	key := bytes.Repeat([]byte{7}, 32)
	service, err := NewService(&Config{DatabasePath: ":memory:", RetentionDays: 30, Key: key})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = service.store.Close() })

	// Protobuf request, bad payload → protobuf-encoded Status.
	request := httptest.NewRequest(http.MethodPost, "/v1/metrics", strings.NewReader("not protobuf"))
	request.Header.Set("Content-Type", "application/x-protobuf")
	request.Header.Set(TelemetryTokenHeader, GatewayToken(key))
	response := httptest.NewRecorder()
	service.ingestHandler().ServeHTTP(response, request)
	if response.Code != http.StatusBadRequest {
		t.Fatalf("status=%d, want 400", response.Code)
	}
	if got := response.Header().Get("Content-Type"); got != "application/x-protobuf" {
		t.Fatalf("error Content-Type=%q, want application/x-protobuf", got)
	}
	status := new(statuspb.Status)
	if err := proto.Unmarshal(response.Body.Bytes(), status); err != nil {
		t.Fatalf("error body is not a google.rpc.Status: %v", err)
	}
	if status.Code != int32(codepb.Code_INVALID_ARGUMENT) || status.Message == "" {
		t.Fatalf("status = %d %q, want INVALID_ARGUMENT with a message", status.Code, status.Message)
	}

	// JSON request, bad token → JSON-encoded Status.
	request = httptest.NewRequest(http.MethodPost, "/v1/metrics", strings.NewReader("{}"))
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set(TelemetryTokenHeader, "bogus")
	response = httptest.NewRecorder()
	service.ingestHandler().ServeHTTP(response, request)
	if response.Code != http.StatusUnauthorized {
		t.Fatalf("status=%d, want 401", response.Code)
	}
	if got := response.Header().Get("Content-Type"); got != "application/json" {
		t.Fatalf("error Content-Type=%q, want application/json", got)
	}
	var decoded struct {
		Code    int32  `json:"code"`
		Message string `json:"message"`
	}
	if err := json.Unmarshal(response.Body.Bytes(), &decoded); err != nil {
		t.Fatalf("error body is not JSON: %v", err)
	}
	if decoded.Code != int32(codepb.Code_UNAUTHENTICATED) || decoded.Message == "" {
		t.Fatalf("status = %d %q, want UNAUTHENTICATED with a message", decoded.Code, decoded.Message)
	}
}
