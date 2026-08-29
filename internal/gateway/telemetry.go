package gateway

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"strconv"
	"strings"
	"sync"
	"time"

	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/exporters/otlp/otlpmetric/otlpmetrichttp"
	otelmetric "go.opentelemetry.io/otel/metric"
	"go.opentelemetry.io/otel/sdk/metric"
	"go.opentelemetry.io/otel/sdk/resource"

	"github.com/adrianliechti/donkey/internal/telemetry"
)

const (
	telemetryExportInterval = 10 * time.Second
	viewFlushInterval       = 2 * time.Second
	viewBatchSize           = 100
	viewQueueSize           = 2048
)

type edgeTelemetry struct {
	provider  *metric.MeterProvider
	duration  otelmetric.Float64Histogram
	viewsURL  string
	viewToken string
	key       []byte
	client    *http.Client
	// views is never closed: request handlers send to it concurrently, and a
	// send on a closed channel panics even inside a select with a default.
	// Shutdown closes closing instead; the exporter drains what is buffered
	// and signals done. Events sent after that are simply never read.
	views     chan telemetry.ViewEvent
	closing   chan struct{}
	done      chan struct{}
	closeOnce sync.Once
}

func newEdgeTelemetry(cfg *Config) (*edgeTelemetry, error) {
	t := &edgeTelemetry{
		key:       cfg.TelemetryKey,
		views:     make(chan telemetry.ViewEvent, viewQueueSize),
		closing:   make(chan struct{}),
		done:      make(chan struct{}),
		client:    &http.Client{Timeout: 5 * time.Second},
		viewToken: telemetry.GatewayToken(cfg.TelemetryKey),
	}
	if cfg.TelemetryEndpoint == "" {
		close(t.done)
		return t, nil
	}
	if len(cfg.TelemetryKey) != 32 {
		return nil, errors.New("gateway telemetry requires a 32-byte telemetry key")
	}
	base := strings.TrimSuffix(cfg.TelemetryEndpoint, "/")
	base = strings.TrimSuffix(base, "/v1/metrics")
	t.viewsURL = base + "/v1/donkey/views"

	exporter, err := otlpmetrichttp.New(context.Background(),
		otlpmetrichttp.WithEndpointURL(base+"/v1/metrics"),
		otlpmetrichttp.WithHeaders(map[string]string{telemetry.TelemetryTokenHeader: t.viewToken}),
		otlpmetrichttp.WithCompression(otlpmetrichttp.GzipCompression),
		otlpmetrichttp.WithTemporalitySelector(metric.DeltaTemporalitySelector),
	)
	if err != nil {
		return nil, err
	}
	reader := metric.NewPeriodicReader(exporter,
		metric.WithInterval(telemetryExportInterval),
		metric.WithTimeout(5*time.Second),
	)
	hostname, _ := os.Hostname()
	// Merged over the SDK default so the required telemetry.sdk.* resource
	// attributes survive; the schemaless overlay wins for service identity.
	res, err := resource.Merge(resource.Default(), resource.NewSchemaless(
		attribute.String("service.name", "donkey-gateway"),
		attribute.String("service.namespace", cfg.SystemNamespace),
		attribute.String("service.instance.id", hostname),
	))
	if err != nil {
		return nil, err
	}
	t.provider = metric.NewMeterProvider(metric.WithReader(reader), metric.WithResource(res))
	meter := t.provider.Meter("github.com/adrianliechti/donkey/internal/gateway")
	t.duration, err = meter.Float64Histogram(telemetry.HTTPServerRequestDuration,
		otelmetric.WithDescription("Duration of inbound HTTP requests at the donkey app gateway."),
		otelmetric.WithUnit("s"),
		otelmetric.WithExplicitBucketBoundaries(telemetry.HTTPDurationBounds...),
	)
	if err != nil {
		_ = t.provider.Shutdown(context.Background())
		return nil, err
	}
	go t.runViewExporter()
	return t, nil
}

func (t *edgeTelemetry) Record(r *http.Request, rt route, sess *session, status int, elapsed time.Duration, contentType string) {
	if t == nil || t.provider == nil || rt.Space == "" || rt.App == "" {
		return
	}
	method := canonicalHTTPMethod(r.Method)
	attrs := []attribute.KeyValue{
		attribute.String("http.request.method", method),
		attribute.Int("http.response.status_code", status),
		attribute.String("url.scheme", requestScheme(r)),
		attribute.String("http.route", rt.Path),
		attribute.String("network.protocol.version", protocolVersion(r)),
		attribute.String("donkey.space.name", rt.Space),
		attribute.String("donkey.app.name", rt.App),
	}
	// server.address is opt-in for HTTP server metrics and derives from a
	// request-controlled authority. The logical route already supplies the
	// bounded app/space dimensions, so recording it would add cardinality
	// without adding a report dimension.
	// http.request.method_original is deliberately absent: the semconv MUST
	// that records it applies to spans, not this metric — and its value is
	// the client's own bytes, so every garbage method would mint a fresh
	// metric stream. Collapsing to _OTHER is the whole point of _OTHER.
	if status >= 500 {
		attrs = append(attrs, attribute.String("error.type", strconv.Itoa(status)))
	}
	t.duration.Record(context.WithoutCancel(r.Context()), elapsed.Seconds(), otelmetric.WithAttributes(attrs...))

	if r.Method != http.MethodGet || status < 200 || status >= 300 || !wantsHTML(r) ||
		!strings.HasPrefix(strings.ToLower(contentType), "text/html") {
		return
	}
	subject := ""
	if sess != nil {
		subject = sess.Subject
	}
	appHash, spaceHash := telemetry.VisitorHashes(t.key, rt.Space, rt.App, subject)
	event := telemetry.ViewEvent{
		TimeUnixNano:     uint64(time.Now().UnixNano()),
		Space:            rt.Space,
		App:              rt.App,
		AppVisitorHash:   appHash,
		SpaceVisitorHash: spaceHash,
	}
	select {
	case t.views <- event:
	default:
		// Traffic metrics remain complete; under sustained backpressure views
		// are sampled by queue admission instead of delaying app responses.
	}
}

func canonicalHTTPMethod(method string) string {
	switch method {
	case http.MethodConnect, http.MethodDelete, http.MethodGet, http.MethodHead,
		http.MethodOptions, http.MethodPatch, http.MethodPost, http.MethodPut, "QUERY", http.MethodTrace:
		return method
	default:
		return "_OTHER"
	}
}

func requestScheme(r *http.Request) string {
	if r.TLS != nil || strings.EqualFold(r.Header.Get("X-Forwarded-Proto"), "https") {
		return "https"
	}
	return "http"
}

func protocolVersion(r *http.Request) string {
	if r.ProtoMajor == 0 {
		return ""
	}
	if r.ProtoMajor > 1 {
		return strconv.Itoa(r.ProtoMajor)
	}
	return fmt.Sprintf("%d.%d", r.ProtoMajor, r.ProtoMinor)
}

func (t *edgeTelemetry) runViewExporter() {
	defer close(t.done)
	ticker := time.NewTicker(viewFlushInterval)
	defer ticker.Stop()
	batch := make([]telemetry.ViewEvent, 0, viewBatchSize)
	flush := func() {
		if len(batch) == 0 {
			return
		}
		if err := t.sendViews(batch); err != nil {
			log.Printf("exporting gateway views: %v", err)
			// Keep one bounded batch for the next tick. New view events are
			// sampled until the service recovers rather than delaying requests.
			return
		}
		batch = batch[:0]
	}
	for {
		select {
		case event := <-t.views:
			if len(batch) < viewBatchSize {
				batch = append(batch, event)
				if len(batch) >= viewBatchSize {
					flush()
				}
			}
		case <-ticker.C:
			flush()
		case <-t.closing:
			// Drain what handlers already queued, then stop. The channel
			// stays open, so a request finishing mid-shutdown cannot panic.
			for {
				select {
				case event := <-t.views:
					if len(batch) < viewBatchSize {
						batch = append(batch, event)
						if len(batch) >= viewBatchSize {
							flush()
						}
					}
				default:
					flush()
					return
				}
			}
		}
	}
}

func (t *edgeTelemetry) sendViews(events []telemetry.ViewEvent) error {
	var body bytes.Buffer
	if err := json.NewEncoder(&body).Encode(telemetry.ViewBatch{Events: events}); err != nil {
		return err
	}
	req, err := http.NewRequest(http.MethodPost, t.viewsURL, &body)
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set(telemetry.TelemetryTokenHeader, t.viewToken)
	response, err := t.client.Do(req)
	if err != nil {
		return err
	}
	defer response.Body.Close()
	_, _ = io.Copy(io.Discard, io.LimitReader(response.Body, 4096))
	if response.StatusCode != http.StatusOK {
		return fmt.Errorf("telemetry service returned %s", response.Status)
	}
	return nil
}

func (t *edgeTelemetry) Shutdown(ctx context.Context) error {
	if t == nil {
		return nil
	}
	t.closeOnce.Do(func() { close(t.closing) })
	if t.provider == nil {
		return nil
	}
	select {
	case <-t.done:
	case <-ctx.Done():
		return ctx.Err()
	}
	if err := t.provider.ForceFlush(ctx); err != nil && !errors.Is(err, context.Canceled) {
		return err
	}
	return t.provider.Shutdown(ctx)
}
