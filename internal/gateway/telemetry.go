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
	provider    *metric.MeterProvider
	duration    otelmetric.Float64Histogram
	mcpDuration otelmetric.Float64Histogram
	mcp         mcpTracker
	key         []byte
	// views are document loads, web are browser events, callers are verified
	// identities across all requests. All are nil while telemetry is disabled.
	views   *batchExporter[telemetry.ViewEvent]
	web     *batchExporter[telemetry.WebEvent]
	callers *batchExporter[telemetry.CallerEvent]
}

func newEdgeTelemetry(cfg *Config) (*edgeTelemetry, error) {
	t := &edgeTelemetry{key: cfg.TelemetryKey}
	if cfg.TelemetryEndpoint == "" {
		return t, nil
	}
	if len(cfg.TelemetryKey) != 32 {
		return nil, errors.New("gateway telemetry requires a 32-byte telemetry key")
	}
	base := strings.TrimSuffix(cfg.TelemetryEndpoint, "/")
	base = strings.TrimSuffix(base, "/v1/metrics")
	token := telemetry.GatewayToken(cfg.TelemetryKey)
	client := &http.Client{Timeout: 5 * time.Second}

	exporter, err := otlpmetrichttp.New(context.Background(),
		otlpmetrichttp.WithEndpointURL(base+"/v1/metrics"),
		otlpmetrichttp.WithHeaders(map[string]string{telemetry.TelemetryTokenHeader: token}),
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
	t.mcpDuration, err = meter.Float64Histogram(telemetry.MCPServerOperationDuration,
		otelmetric.WithDescription("Duration of MCP requests observed by the donkey gateway."),
		otelmetric.WithUnit("s"),
		otelmetric.WithExplicitBucketBoundaries(telemetry.MCPDurationBounds...),
	)
	if err != nil {
		_ = t.provider.Shutdown(context.Background())
		return nil, err
	}
	t.views = newBatchExporter(base+"/v1/donkey/views", token, client, func(events []telemetry.ViewEvent) any {
		return telemetry.ViewBatch{Events: events}
	})
	t.web = newBatchExporter(base+"/v1/donkey/web", token, client, func(events []telemetry.WebEvent) any {
		return telemetry.WebBatch{Events: events}
	})
	t.callers = newBatchExporter(base+"/v1/donkey/callers", token, client, func(events []telemetry.CallerEvent) any {
		return telemetry.CallerBatch{Events: events}
	})
	return t, nil
}

func (t *edgeTelemetry) RecordHTTP(r *http.Request, rt route, sess *session, status int, elapsed time.Duration) {
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
	if sess != nil && sess.Subject != "" && t.callers != nil {
		appHash, spaceHash := telemetry.VisitorHashes(t.key, rt.Space, rt.App, sess.Subject)
		t.callers.offer(telemetry.CallerEvent{
			TimeUnixNano: uint64(time.Now().UnixNano()), Space: rt.Space, App: rt.App,
			UserHash: appHash, SpaceUserHash: spaceHash,
		})
	}
}

func (t *edgeTelemetry) RecordView(r *http.Request, rt route, sess *session, status int, contentType string) {
	if t == nil || t.views == nil || rt.Space == "" || rt.App == "" ||
		r.Method != http.MethodGet || status < 200 || status >= 300 || !wantsHTML(r) ||
		!strings.HasPrefix(strings.ToLower(contentType), "text/html") {
		return
	}
	subject := ""
	if sess != nil {
		subject = sess.Subject
	}
	appHash, spaceHash := telemetry.VisitorHashes(t.key, rt.Space, rt.App, subject)
	t.views.offer(telemetry.ViewEvent{
		TimeUnixNano:     uint64(time.Now().UnixNano()),
		Space:            rt.Space,
		App:              rt.App,
		AppVisitorHash:   appHash,
		SpaceVisitorHash: spaceHash,
	})
}

// collectsWeb says the analytics script has somewhere to send to.
func (t *edgeTelemetry) collectsWeb() bool { return t != nil && t.web != nil }

func (t *edgeTelemetry) RecordWeb(event telemetry.WebEvent) {
	if t.collectsWeb() {
		t.web.offer(event)
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

func (t *edgeTelemetry) Shutdown(ctx context.Context) error {
	if t == nil || t.provider == nil {
		return nil
	}
	t.views.close()
	t.web.close()
	t.callers.close()
	for _, done := range []chan struct{}{t.views.done, t.web.done, t.callers.done} {
		select {
		case <-done:
		case <-ctx.Done():
			return ctx.Err()
		}
	}
	if err := t.provider.ForceFlush(ctx); err != nil && !errors.Is(err, context.Canceled) {
		return err
	}
	return t.provider.Shutdown(ctx)
}

// batchExporter posts events to donkey-telemetry in small JSON batches, off
// the request path. Under sustained backpressure events are sampled by queue
// admission instead of delaying app responses.
type batchExporter[T any] struct {
	url    string
	token  string
	client *http.Client
	wrap   func([]T) any
	// queue is never closed: request handlers send to it concurrently, and a
	// send on a closed channel panics even inside a select with a default.
	// close closes closing instead; the exporter drains what is buffered and
	// signals done. Events sent after that are simply never read.
	queue     chan T
	closing   chan struct{}
	done      chan struct{}
	closeOnce sync.Once
}

func newBatchExporter[T any](url, token string, client *http.Client, wrap func([]T) any) *batchExporter[T] {
	b := &batchExporter[T]{
		url: url, token: token, client: client, wrap: wrap,
		queue:   make(chan T, viewQueueSize),
		closing: make(chan struct{}),
		done:    make(chan struct{}),
	}
	go b.run()
	return b
}

func (b *batchExporter[T]) offer(event T) {
	select {
	case b.queue <- event:
	default:
	}
}

func (b *batchExporter[T]) close() { b.closeOnce.Do(func() { close(b.closing) }) }

func (b *batchExporter[T]) run() {
	defer close(b.done)
	ticker := time.NewTicker(viewFlushInterval)
	defer ticker.Stop()
	batch := make([]T, 0, viewBatchSize)
	flush := func() {
		if len(batch) == 0 {
			return
		}
		if err := b.send(batch); err != nil {
			log.Printf("exporting gateway telemetry to %s: %v", b.url, err)
			// Keep one bounded batch for the next tick. New events are
			// sampled until the service recovers rather than delaying requests.
			return
		}
		batch = batch[:0]
	}
	add := func(event T) {
		if len(batch) < viewBatchSize {
			batch = append(batch, event)
			if len(batch) >= viewBatchSize {
				flush()
			}
		}
	}
	for {
		select {
		case event := <-b.queue:
			add(event)
		case <-ticker.C:
			flush()
		case <-b.closing:
			// Drain what handlers already queued, then stop. The channel
			// stays open, so a request finishing mid-shutdown cannot panic.
			for {
				select {
				case event := <-b.queue:
					add(event)
				default:
					flush()
					return
				}
			}
		}
	}
}

func (b *batchExporter[T]) send(events []T) error {
	var body bytes.Buffer
	if err := json.NewEncoder(&body).Encode(b.wrap(events)); err != nil {
		return err
	}
	req, err := http.NewRequest(http.MethodPost, b.url, &body)
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set(telemetry.TelemetryTokenHeader, b.token)
	response, err := b.client.Do(req)
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
