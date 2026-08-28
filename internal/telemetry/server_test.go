package telemetry

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	commonpb "go.opentelemetry.io/proto/otlp/common/v1"
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
