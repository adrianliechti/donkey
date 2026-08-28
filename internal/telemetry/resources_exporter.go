package telemetry

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"math"
	"net/http"
	"strings"
	"time"

	collectormetricspb "go.opentelemetry.io/proto/otlp/collector/metrics/v1"
	commonpb "go.opentelemetry.io/proto/otlp/common/v1"
	metricspb "go.opentelemetry.io/proto/otlp/metrics/v1"
	resourcepb "go.opentelemetry.io/proto/otlp/resource/v1"
	"google.golang.org/protobuf/proto"

	"github.com/adrianliechti/donkey/internal/config"
)

// ResourceExporter is donkey-server's small OTLP/HTTP writer. It constructs
// the convention's Gauge and UpDownCounter data directly rather than starting
// a second OpenTelemetry SDK and periodic reader inside the control plane;
// the server already owns the sampling cadence.
type ResourceExporter struct {
	endpoint string
	token    string
	http     *http.Client
}

func NewResourceExporter(endpoint string, key []byte) *ResourceExporter {
	endpoint = strings.TrimSuffix(endpoint, "/")
	if endpoint != "" && !strings.HasSuffix(endpoint, "/v1/metrics") {
		endpoint += "/v1/metrics"
	}
	return &ResourceExporter{
		endpoint: endpoint,
		token:    RuntimeToken(key),
		http:     &http.Client{Timeout: 10 * time.Second},
	}
}

func (e *ResourceExporter) Export(ctx context.Context, observations []ResourceObservation) error {
	if len(observations) == 0 {
		return nil
	}
	if e == nil || e.endpoint == "" {
		return errors.New("telemetry OTLP endpoint is not configured")
	}
	request := &collectormetricspb.ExportMetricsServiceRequest{
		ResourceMetrics: make([]*metricspb.ResourceMetrics, 0, len(observations)),
	}
	for _, observation := range observations {
		metrics, err := resourceMetrics(observation)
		if err != nil {
			return err
		}
		request.ResourceMetrics = append(request.ResourceMetrics, metrics)
	}
	body, err := proto.Marshal(request)
	if err != nil {
		return err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, e.endpoint, bytes.NewReader(body))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/x-protobuf")
	req.Header.Set(TelemetryTokenHeader, e.token)
	response, err := e.http.Do(req)
	if err != nil {
		return err
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		return fmt.Errorf("telemetry OTLP endpoint returned %s", response.Status)
	}
	return nil
}

func resourceMetrics(observation ResourceObservation) (*metricspb.ResourceMetrics, error) {
	if !config.ValidName(observation.Space) || !config.ValidName(observation.App) {
		return nil, fmt.Errorf("invalid resource telemetry scope %q/%q", observation.Space, observation.App)
	}
	if observation.Instance == "" || observation.MemoryBytes < 0 ||
		math.IsNaN(observation.CPUCores) || math.IsInf(observation.CPUCores, 0) || observation.CPUCores < 0 {
		return nil, fmt.Errorf("invalid resource telemetry values for %s/%s", observation.Space, observation.App)
	}
	if observation.CPUModesAvailable &&
		(!validCPUUsage(observation.CPUUserCores) || !validCPUUsage(observation.CPUSystemCores)) {
		return nil, fmt.Errorf("invalid resource telemetry CPU modes for %s/%s", observation.Space, observation.App)
	}
	at := observation.SampledAt.UTC()
	if at.IsZero() {
		at = time.Now().UTC()
	}
	timestamp := uint64(at.UnixNano())
	cpuPoints := []*metricspb.NumberDataPoint{doublePoint(timestamp, observation.CPUCores)}
	if observation.CPUModesAvailable {
		cpuPoints = []*metricspb.NumberDataPoint{
			doublePointWithAttribute(timestamp, observation.CPUUserCores, "cpu.mode", "user"),
			doublePointWithAttribute(timestamp, observation.CPUSystemCores, "cpu.mode", "system"),
		}
	}
	metrics := []*metricspb.Metric{
		doubleGauge(ContainerCPUUsage, "{cpu}", cpuPoints...),
		intUpDownCounter(ContainerMemoryWorkingSet, "By", timestamp, observation.MemoryBytes),
	}
	attributes := []*commonpb.KeyValue{
		otlpStringAttribute("service.namespace", observation.Space),
		otlpStringAttribute("service.name", observation.App),
		otlpStringAttribute("service.instance.id", observation.Instance),
	}
	if observation.ContainerID != "" {
		attributes = append(attributes, otlpStringAttribute("container.id", observation.ContainerID))
	}
	if observation.KubeNamespace == "" && observation.ContainerName != "" {
		attributes = append(attributes, otlpStringAttribute("container.name", observation.ContainerName))
	}
	if observation.KubeNamespace != "" {
		for _, attribute := range []struct{ key, value string }{
			{"k8s.namespace.name", observation.KubeNamespace},
			{"k8s.pod.name", observation.KubePodName},
			{"k8s.pod.uid", observation.KubePodUID},
			{"k8s.container.name", observation.ContainerName},
		} {
			if attribute.value != "" {
				attributes = append(attributes, otlpStringAttribute(attribute.key, attribute.value))
			}
		}
	}
	return &metricspb.ResourceMetrics{
		Resource: &resourcepb.Resource{Attributes: attributes},
		ScopeMetrics: []*metricspb.ScopeMetrics{{
			Scope:   &commonpb.InstrumentationScope{Name: "donkey.runtime.resources"},
			Metrics: metrics,
		}},
	}, nil
}

func validCPUUsage(value float64) bool {
	return !math.IsNaN(value) && !math.IsInf(value, 0) && value >= 0
}

func doubleGauge(name, unit string, points ...*metricspb.NumberDataPoint) *metricspb.Metric {
	return &metricspb.Metric{
		Name: name, Unit: unit,
		Data: &metricspb.Metric_Gauge{Gauge: &metricspb.Gauge{DataPoints: points}},
	}
}

func doublePoint(at uint64, value float64) *metricspb.NumberDataPoint {
	return &metricspb.NumberDataPoint{TimeUnixNano: at, Value: &metricspb.NumberDataPoint_AsDouble{AsDouble: value}}
}

func doublePointWithAttribute(at uint64, value float64, key, attribute string) *metricspb.NumberDataPoint {
	point := doublePoint(at, value)
	point.Attributes = []*commonpb.KeyValue{otlpStringAttribute(key, attribute)}
	return point
}

func intUpDownCounter(name, unit string, at uint64, value int64) *metricspb.Metric {
	return upDownCounter(name, unit, &metricspb.NumberDataPoint{
		TimeUnixNano: at,
		Value:        &metricspb.NumberDataPoint_AsInt{AsInt: value},
	})
}

func upDownCounter(name, unit string, point *metricspb.NumberDataPoint) *metricspb.Metric {
	return &metricspb.Metric{
		Name: name, Unit: unit,
		Data: &metricspb.Metric_Sum{Sum: &metricspb.Sum{
			AggregationTemporality: metricspb.AggregationTemporality_AGGREGATION_TEMPORALITY_CUMULATIVE,
			IsMonotonic:            false,
			DataPoints:             []*metricspb.NumberDataPoint{point},
		}},
	}
}

func otlpStringAttribute(key, value string) *commonpb.KeyValue {
	return &commonpb.KeyValue{
		Key:   key,
		Value: &commonpb.AnyValue{Value: &commonpb.AnyValue_StringValue{StringValue: value}},
	}
}
