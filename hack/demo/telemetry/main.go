// Command telemetry seeds a local donkey-telemetry process with a small,
// deterministic OTLP data set and verifies its 48h, 7d and 30d reports.
package main

import (
	"bytes"
	"context"
	"encoding/hex"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"io/fs"
	"net/http"
	"net/url"
	"os"
	"strings"
	"time"

	collectormetricspb "go.opentelemetry.io/proto/otlp/collector/metrics/v1"
	commonpb "go.opentelemetry.io/proto/otlp/common/v1"
	metricspb "go.opentelemetry.io/proto/otlp/metrics/v1"
	resourcepb "go.opentelemetry.io/proto/otlp/resource/v1"
	"google.golang.org/protobuf/proto"

	"github.com/adrianliechti/donkey/internal/telemetry"
	"github.com/adrianliechti/donkey/web"
)

type sample struct {
	at     time.Time
	status int64
	count  uint64
	sum    float64
	bucket int
}

func main() {
	if err := run(); err != nil {
		fmt.Fprintln(os.Stderr, "telemetry demo:", err)
		os.Exit(1)
	}
}

func run() error {
	otlpURL := flag.String("otlp", "http://127.0.0.1:14318", "donkey-telemetry OTLP/HTTP base URL")
	apiURL := flag.String("api", "http://127.0.0.1:19090", "donkey-telemetry private API base URL")
	keyHex := flag.String("key", "", "32-byte telemetry key as hex")
	serveUI := flag.Bool("serve-ui", false, "serve a local console populated by the demo reports")
	seed := flag.Bool("seed", true, "write the deterministic telemetry sample before querying or serving")
	listen := flag.String("listen", "127.0.0.1:18088", "local console listen address")
	flag.Parse()
	key, err := hex.DecodeString(*keyHex)
	if err != nil || len(key) != 32 {
		return errors.New("-key must contain 32 bytes of hex")
	}
	if !*seed {
		if !*serveUI {
			return errors.New("-seed=false requires -serve-ui")
		}
		return serveConsole(*listen, *apiURL, key)
	}

	base := time.Now().UTC().Add(-time.Minute)
	edge := []sample{
		{at: base.AddDate(0, 0, -10), status: 200, count: 5, sum: .5, bucket: 6},
		{at: base.AddDate(0, 0, -6), status: 200, count: 10, sum: .8, bucket: 5},
		{at: base.Add(-26 * time.Hour), status: 503, count: 2, sum: .7, bucket: 8},
		{at: base.Add(-6 * time.Hour), status: 404, count: 3, sum: .3, bucket: 6},
		{at: base, status: 200, count: 8, sum: .4, bucket: 5},
	}
	if err := postMetrics(*otlpURL, telemetry.GatewayToken(key), metricRequest("donkey-gateway", "", edge, true)); err != nil {
		return err
	}
	app := []sample{
		{at: base.AddDate(0, 0, -10), status: 200, count: 1, sum: .05, bucket: 5},
		{at: base.Add(-6 * time.Hour), status: 200, count: 4, sum: .12, bucket: 4},
	}
	if err := postMetrics(*otlpURL, telemetry.AppToken(key, "demo", "website"), metricRequest("website", "demo", app, false)); err != nil {
		return err
	}

	userAApp, userASpace := telemetry.VisitorHashes(key, "demo", "website", "demo-user-a")
	userBApp, userBSpace := telemetry.VisitorHashes(key, "demo", "website", "demo-user-b")
	views := telemetry.ViewBatch{Events: []telemetry.ViewEvent{
		view(base.AddDate(0, 0, -10), userAApp, userASpace),
		view(base.AddDate(0, 0, -6), userAApp, userASpace),
		view(base.Add(-26*time.Hour), userAApp, userASpace),
		view(base.Add(-6*time.Hour), userBApp, userBSpace),
		view(base, "", ""),
	}}
	if err := postJSON(strings.TrimSuffix(*otlpURL, "/")+"/v1/donkey/views", telemetry.GatewayToken(key), views); err != nil {
		return err
	}

	for _, selected := range []telemetry.TrafficRange{telemetry.Range48Hours, telemetry.Range7Days, telemetry.Range30Days} {
		edgeReport, err := query(*apiURL, key, "website", telemetry.SourceEdge, selected)
		if err != nil {
			return err
		}
		appReport, err := query(*apiURL, key, "website", telemetry.SourceApp, selected)
		if err != nil {
			return err
		}
		fmt.Printf("%-3s  %-4s buckets=%2d  gateway requests=%2d errors=%d views=%d unique=%d  app requests=%d\n",
			edgeReport.Range, edgeReport.Granularity, len(edgeReport.Series), edgeReport.Totals.Requests,
			edgeReport.Totals.ClientErrors+edgeReport.Totals.ServerErrors, edgeReport.Totals.Views,
			edgeReport.Totals.UniqueVisitors, appReport.Totals.Requests)
	}
	if *serveUI {
		return serveConsole(*listen, *apiURL, key)
	}
	return nil
}

func view(at time.Time, appHash, spaceHash string) telemetry.ViewEvent {
	return telemetry.ViewEvent{
		TimeUnixNano: uint64(at.UnixNano()), Space: "demo", App: "website",
		AppVisitorHash: appHash, SpaceVisitorHash: spaceHash,
	}
}

func metricRequest(service, namespace string, samples []sample, edge bool) *collectormetricspb.ExportMetricsServiceRequest {
	resourceAttrs := []*commonpb.KeyValue{stringAttr("service.name", service)}
	if namespace != "" {
		resourceAttrs = append(resourceAttrs, stringAttr("service.namespace", namespace))
	}
	points := make([]*metricspb.HistogramDataPoint, 0, len(samples))
	for _, item := range samples {
		counts := make([]uint64, len(telemetry.HTTPDurationBounds)+1)
		counts[item.bucket] = item.count
		attrs := []*commonpb.KeyValue{
			stringAttr("http.request.method", "GET"),
			intAttr("http.response.status_code", item.status),
		}
		if edge {
			attrs = append(attrs, stringAttr("donkey.space.name", "demo"), stringAttr("donkey.app.name", "website"))
		}
		sum := item.sum
		points = append(points, &metricspb.HistogramDataPoint{
			Attributes: attrs, StartTimeUnixNano: uint64(item.at.Add(-time.Minute).UnixNano()),
			TimeUnixNano: uint64(item.at.UnixNano()), Count: item.count, Sum: &sum,
			BucketCounts: counts, ExplicitBounds: append([]float64(nil), telemetry.HTTPDurationBounds...),
		})
	}
	return &collectormetricspb.ExportMetricsServiceRequest{ResourceMetrics: []*metricspb.ResourceMetrics{{
		Resource: &resourcepb.Resource{Attributes: resourceAttrs},
		ScopeMetrics: []*metricspb.ScopeMetrics{{Metrics: []*metricspb.Metric{{
			Name: telemetry.HTTPServerRequestDuration, Unit: "s",
			Data: &metricspb.Metric_Histogram{Histogram: &metricspb.Histogram{
				AggregationTemporality: metricspb.AggregationTemporality_AGGREGATION_TEMPORALITY_DELTA,
				DataPoints:             points,
			}},
		}}}},
	}}}
}

func postMetrics(baseURL, token string, request *collectormetricspb.ExportMetricsServiceRequest) error {
	body, err := proto.Marshal(request)
	if err != nil {
		return err
	}
	return post(strings.TrimSuffix(baseURL, "/")+"/v1/metrics", token, "application/x-protobuf", body)
}

func postJSON(endpoint, token string, value any) error {
	body, err := json.Marshal(value)
	if err != nil {
		return err
	}
	return post(endpoint, token, "application/json", body)
}

func post(endpoint, token, contentType string, body []byte) error {
	request, err := http.NewRequestWithContext(context.Background(), http.MethodPost, endpoint, bytes.NewReader(body))
	if err != nil {
		return err
	}
	request.Header.Set("Content-Type", contentType)
	request.Header.Set(telemetry.TelemetryTokenHeader, token)
	response, err := http.DefaultClient.Do(request)
	if err != nil {
		return err
	}
	defer response.Body.Close()
	raw, _ := io.ReadAll(io.LimitReader(response.Body, 4096))
	if response.StatusCode != http.StatusOK {
		return fmt.Errorf("POST %s returned %s: %s", endpoint, response.Status, strings.TrimSpace(string(raw)))
	}
	return nil
}

func query(baseURL string, key []byte, app, source string, selected telemetry.TrafficRange) (telemetry.TrafficReport, error) {
	values := url.Values{"space": {"demo"}, "source": {source}, "range": {string(selected)}}
	if app != "" {
		values.Set("app", app)
	}
	endpoint := strings.TrimSuffix(baseURL, "/") + "/v1/traffic?" + values.Encode()
	request, err := http.NewRequestWithContext(context.Background(), http.MethodGet, endpoint, nil)
	if err != nil {
		return telemetry.TrafficReport{}, err
	}
	request.Header.Set("Authorization", "Bearer "+telemetry.QueryToken(key))
	response, err := http.DefaultClient.Do(request)
	if err != nil {
		return telemetry.TrafficReport{}, err
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		raw, _ := io.ReadAll(io.LimitReader(response.Body, 4096))
		return telemetry.TrafficReport{}, fmt.Errorf("GET %s returned %s: %s", endpoint, response.Status, strings.TrimSpace(string(raw)))
	}
	var report telemetry.TrafficReport
	if err := json.NewDecoder(response.Body).Decode(&report); err != nil {
		return telemetry.TrafficReport{}, err
	}
	return report, nil
}

func serveConsole(listen, apiURL string, key []byte) error {
	mux := http.NewServeMux()
	writeJSON := func(w http.ResponseWriter, value any) {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(value)
	}
	writeAPIError := func(w http.ResponseWriter, status int, message string) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(status)
		_ = json.NewEncoder(w).Encode(map[string]string{"error": message})
	}
	mux.HandleFunc("GET /v1/ping", func(w http.ResponseWriter, _ *http.Request) {
		writeJSON(w, map[string]any{"ok": true, "version": "telemetry-demo"})
	})
	mux.HandleFunc("GET /v1/doctor", func(w http.ResponseWriter, _ *http.Request) {
		writeJSON(w, map[string]any{
			"domain": "demo.local",
			"checks": []map[string]any{
				{"name": "Control plane", "ok": true, "detail": "local telemetry demo answering"},
				{"name": "Gateway", "ok": true, "detail": "exporting HTTP telemetry over OTLP/HTTP"},
				{"name": "Telemetry (OTLP + DuckDB)", "ok": true, "detail": "30-day traffic store answering"},
			},
		})
	})
	mux.HandleFunc("GET /v1/catalog", func(w http.ResponseWriter, _ *http.Request) {
		writeJSON(w, map[string]any{"types": []map[string]any{
			{
				"type": "postgres", "version": "18", "storage": "2Gi",
				"description": "A PostgreSQL database shared by apps in this space.",
				"keys":        []string{"DATABASE_URL", "PGHOST", "PGPORT", "PGDATABASE", "PGUSER", "PGPASSWORD"},
			},
			{
				"type": "openai", "description": "An OpenAI-compatible endpoint managed for this space.",
				"keys": []string{"OPENAI_BASE_URL", "OPENAI_API_KEY"},
			},
		}})
	})
	mux.HandleFunc("GET /v1/apps", func(w http.ResponseWriter, _ *http.Request) {
		writeJSON(w, map[string]any{"apps": []map[string]any{{
			"name": "website", "space": "demo", "healthy": true, "ready": "1/1",
			"image": "demo/website:telemetry", "url": "#", "exposure": "public",
			"startedAt": time.Now().UTC().Add(-6 * time.Hour).Format(time.RFC3339),
		}}})
	})
	mux.HandleFunc("GET /v1/spaces", func(w http.ResponseWriter, _ *http.Request) {
		writeJSON(w, map[string]any{"spaces": []string{"demo"}})
	})
	mux.HandleFunc("GET /v1/spaces/demo/services", func(w http.ResponseWriter, _ *http.Request) {
		writeJSON(w, map[string]any{"addons": []any{}})
	})
	mux.HandleFunc("GET /v1/spaces/demo/apps/website/services", func(w http.ResponseWriter, _ *http.Request) {
		writeJSON(w, map[string]any{"addons": []any{}})
	})
	mux.HandleFunc("GET /v1/spaces/demo/apps/website/status", func(w http.ResponseWriter, _ *http.Request) {
		writeJSON(w, map[string]any{
			"found": true, "healthy": true, "ready": "1/1", "image": "demo/website:telemetry",
			"url": "#", "pods": []string{"website-demo: Running"}, "exposure": "public",
			"startedAt": time.Now().UTC().Add(-6 * time.Hour).Format(time.RFC3339),
		})
	})
	mux.HandleFunc("GET /v1/spaces/demo/apps/website/network", func(w http.ResponseWriter, _ *http.Request) {
		writeJSON(w, map[string]any{
			"exposure": "public", "url": "https://website.demo.local", "host": "website.demo.local",
			"port": 8080, "routes": []any{},
		})
	})
	mux.HandleFunc("GET /v1/spaces/demo/apps/website/env", func(w http.ResponseWriter, _ *http.Request) {
		writeJSON(w, map[string]any{"env": map[string]string{
			"OTEL_SERVICE_NAME":      "website",
			"OTEL_SERVICE_NAMESPACE": "demo",
		}})
	})
	mux.HandleFunc("GET /v1/spaces/demo/apps/website/logs", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/plain; charset=utf-8")
		_, _ = io.WriteString(w, "website: listening on :8080\nwebsite: telemetry exporter connected\n")
	})
	mux.HandleFunc("GET /v1/spaces/demo/apps/website/traffic", func(w http.ResponseWriter, r *http.Request) {
		serveReport(w, r, apiURL, key, "website", writeJSON)
	})
	mux.HandleFunc("GET /v1/spaces/demo/traffic", func(w http.ResponseWriter, r *http.Request) {
		serveReport(w, r, apiURL, key, "", writeJSON)
	})
	// The console is a real production build, but this demo intentionally
	// implements only a safe, read-only slice of its API. Keep an unsupported
	// API request out of the SPA fallback: a 200 index.html response makes the
	// browser's JSON parser surface a cryptic error unrelated to the real cause.
	unavailableAPI := func(w http.ResponseWriter, r *http.Request) {
		writeAPIError(w, http.StatusNotFound, fmt.Sprintf("%s %s is not available in the read-only telemetry demo", r.Method, r.URL.Path))
	}
	for _, method := range []string{http.MethodGet, http.MethodPost, http.MethodPut, http.MethodPatch, http.MethodDelete} {
		mux.HandleFunc(method+" /v1/", unavailableAPI)
	}

	assets, err := fs.Sub(web.Dist, "dist")
	if err != nil {
		return err
	}
	files := http.FileServer(http.FS(assets))
	mux.HandleFunc("GET /", func(w http.ResponseWriter, r *http.Request) {
		path := strings.TrimPrefix(r.URL.Path, "/")
		if path != "" {
			if file, err := assets.Open(path); err == nil {
				file.Close()
				files.ServeHTTP(w, r)
				return
			}
		}
		http.ServeFileFS(w, r, assets, "index.html")
	})
	fmt.Printf("Console: http://%s/#/app/demo/website/traffic\n", listen)
	return http.ListenAndServe(listen, mux)
}

func serveReport(
	w http.ResponseWriter,
	r *http.Request,
	apiURL string,
	key []byte,
	app string,
	writeJSON func(http.ResponseWriter, any),
) {
	selected, err := telemetry.ParseTrafficRange(r.URL.Query().Get("range"))
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	report, err := query(apiURL, key, app, r.URL.Query().Get("source"), selected)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadGateway)
		return
	}
	writeJSON(w, report)
}

func stringAttr(key, value string) *commonpb.KeyValue {
	return &commonpb.KeyValue{Key: key, Value: &commonpb.AnyValue{Value: &commonpb.AnyValue_StringValue{StringValue: value}}}
}

func intAttr(key string, value int64) *commonpb.KeyValue {
	return &commonpb.KeyValue{Key: key, Value: &commonpb.AnyValue{Value: &commonpb.AnyValue_IntValue{IntValue: value}}}
}
