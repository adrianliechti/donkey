// Package telemetry is donkey's small, purpose-built traffic telemetry
// backend. It accepts OTLP/HTTP metrics, keeps 30 days of bounded rollups and
// serves the reports used by the admin console.
package telemetry

import (
	"fmt"
	"math"
	"time"
)

const (
	// HTTPServerRequestDuration is the stable OpenTelemetry HTTP server
	// duration metric. Counts come from the histogram's count, so one
	// instrument supplies request volume and latency without a second stream.
	HTTPServerRequestDuration  = "http.server.request.duration"
	MCPServerOperationDuration = "mcp.server.operation.duration"

	// Runtime resource snapshots use only OpenTelemetry's container metrics.
	ContainerCPUUsage         = "container.cpu.usage"
	ContainerMemoryWorkingSet = "container.memory.working_set"

	SourceEdge = "edge"
	SourceApp  = "app"

	DefaultRetentionDays = 30
)

type TrafficRange string

const (
	Range48Hours TrafficRange = "48h"
	Range7Days   TrafficRange = "7d"
	Range30Days  TrafficRange = "30d"

	DefaultTrafficRange = Range30Days
)

func ParseTrafficRange(value string) (TrafficRange, error) {
	if value == "" {
		return DefaultTrafficRange, nil
	}
	selected := TrafficRange(value)
	switch selected {
	case Range48Hours, Range7Days, Range30Days:
		return selected, nil
	default:
		return "", fmt.Errorf("range must be one of 48h, 7d or 30d")
	}
}

// ResourceRange is separate from TrafficRange because recent CPU and memory
// snapshots have a finer retained resolution than HTTP traffic rollups.
type ResourceRange string

const (
	ResourceRange1Hour   ResourceRange = "1h"
	ResourceRange4Hours  ResourceRange = "4h"
	ResourceRange48Hours ResourceRange = "48h"
	ResourceRange7Days   ResourceRange = "7d"
	ResourceRange30Days  ResourceRange = "30d"

	DefaultResourceRange = ResourceRange4Hours
)

func ParseResourceRange(value string) (ResourceRange, error) {
	if value == "" {
		return DefaultResourceRange, nil
	}
	selected := ResourceRange(value)
	switch selected {
	case ResourceRange1Hour, ResourceRange4Hours, ResourceRange48Hours, ResourceRange7Days, ResourceRange30Days:
		return selected, nil
	default:
		return "", fmt.Errorf("range must be one of 1h, 4h, 48h, 7d or 30d")
	}
}

// HTTPDurationBounds are the explicit bucket boundaries recommended by the
// OpenTelemetry HTTP semantic conventions. Values and stored sums are seconds.
var HTTPDurationBounds = []float64{
	0.005, 0.01, 0.025, 0.05, 0.075, 0.1, 0.25,
	0.5, 0.75, 1, 2.5, 5, 7.5, 10,
}

const httpDurationBucketCount = 15

// TrafficReport is one app or one whole space over a bounded UTC window.
// Edge and in-app telemetry are deliberately queried separately: adding them
// would count the same public request twice.
type TrafficReport struct {
	Space       string            `json:"space"`
	App         string            `json:"app,omitempty"`
	Source      string            `json:"source"`
	Range       TrafficRange      `json:"range"`
	Granularity string            `json:"granularity"`
	From        time.Time         `json:"from"`
	To          time.Time         `json:"to"`
	Totals      TrafficTotals     `json:"totals"`
	Series      []TrafficPoint    `json:"series"`
	Definitions Definitions       `json:"definitions"`
	Endpoints   []TrafficEndpoint `json:"endpoints"`
	MCP         MCPReport         `json:"mcp"`
}

// Definitions keeps dashboard words honest. In particular, a document load
// observed at the gateway is not every SPA route transition, and public
// anonymous visitors are intentionally not assigned a tracking cookie.
type Definitions struct {
	Success              string `json:"success"`
	View                 string `json:"view"`
	UniqueVisitors       string `json:"uniqueVisitors"`
	AuthenticatedCallers string `json:"authenticatedCallers"`
}

type TrafficTotals struct {
	AuthenticatedCallers int64   `json:"authenticatedCallers"`
	Requests             int64   `json:"requests"`
	Successful           int64   `json:"successful"`
	ClientErrors         int64   `json:"clientErrors"`
	ServerErrors         int64   `json:"serverErrors"`
	OtherResponses       int64   `json:"otherResponses,omitempty"`
	AverageLatencyMS     float64 `json:"averageLatencyMs"`
	P50LatencyMS         float64 `json:"p50LatencyMs"`
	P95LatencyMS         float64 `json:"p95LatencyMs"`
	Views                int64   `json:"views"`
	UniqueVisitors       int64   `json:"uniqueVisitors"`
	IdentifiedViews      int64   `json:"identifiedViews"`
	VisitorCoveragePC    float64 `json:"visitorCoveragePercent"`
}

type TrafficPoint struct {
	AuthenticatedCallers int64   `json:"authenticatedCallers"`
	Bucket               string  `json:"bucket"`
	Requests             int64   `json:"requests"`
	Successful           int64   `json:"successful"`
	ClientErrors         int64   `json:"clientErrors"`
	ServerErrors         int64   `json:"serverErrors"`
	OtherResponses       int64   `json:"otherResponses,omitempty"`
	AverageLatencyMS     float64 `json:"averageLatencyMs"`
	P50LatencyMS         float64 `json:"p50LatencyMs"`
	P95LatencyMS         float64 `json:"p95LatencyMs"`
	Views                int64   `json:"views"`
	UniqueVisitors       int64   `json:"uniqueVisitors"`
	IdentifiedViews      int64   `json:"identifiedViews"`
	VisitorCoveragePC    float64 `json:"visitorCoveragePercent"`
}

// ResourceObservation is one container snapshot exported by donkey-server as
// OTLP after sampling Docker or Kubernetes. App totals are calculated only
// when reports are read, preserving the standard container entity boundary.
type ResourceObservation struct {
	Space         string
	App           string
	Instance      string
	ContainerID   string
	ContainerName string
	KubeNamespace string
	KubePodName   string
	KubePodUID    string
	SampledAt     time.Time

	CPUCores          float64
	CPUUserCores      float64
	CPUSystemCores    float64
	CPUModesAvailable bool
	MemoryBytes       int64
}

// ResourceReport is retained CPU and memory usage for one app. Current is the
// most recent observation even when it is stale; Fresh tells the console
// whether it is recent enough to describe the app now.
type ResourceReport struct {
	Space       string           `json:"space"`
	App         string           `json:"app"`
	Range       ResourceRange    `json:"range"`
	Granularity string           `json:"granularity"`
	From        time.Time        `json:"from"`
	To          time.Time        `json:"to"`
	Available   bool             `json:"available"`
	Current     *ResourceCurrent `json:"current,omitempty"`
	Series      []ResourcePoint  `json:"series"`
}

type ResourceCurrent struct {
	SampledAt time.Time `json:"sampledAt"`
	Fresh     bool      `json:"fresh"`
	Instances int64     `json:"instances"`

	CPUCores    float64 `json:"cpuCores"`
	MemoryBytes int64   `json:"memoryBytes"`
}

type ResourcePoint struct {
	Bucket             string  `json:"bucket"`
	Samples            int64   `json:"samples"`
	AverageCPUCores    float64 `json:"averageCpuCores"`
	PeakCPUCores       float64 `json:"peakCpuCores"`
	AverageMemoryBytes int64   `json:"averageMemoryBytes"`
	PeakMemoryBytes    int64   `json:"peakMemoryBytes"`
}

// ViewEvent is the gateway's privacy-bounded document-navigation event.
// Hashes are HMACs made by the gateway; the subject, email, IP, user agent,
// path and query string never enter the telemetry service.
type ViewEvent struct {
	TimeUnixNano     uint64 `json:"timeUnixNano"`
	Space            string `json:"space"`
	App              string `json:"app"`
	AppVisitorHash   string `json:"appVisitorHash,omitempty"`
	SpaceVisitorHash string `json:"spaceVisitorHash,omitempty"`
}

type ViewBatch struct {
	Events []ViewEvent `json:"events"`
}

type trafficAggregate struct {
	TrafficTotals
	durationSum float64
	buckets     [httpDurationBucketCount]uint64
}

func (a *trafficAggregate) addHTTP(status int, count uint64, sum float64, buckets [httpDurationBucketCount]uint64) {
	n := int64(count)
	a.Requests += n
	switch {
	case status >= 200 && status < 400:
		a.Successful += n
	case status >= 400 && status < 500:
		a.ClientErrors += n
	case status >= 500 && status < 600:
		a.ServerErrors += n
	default:
		a.OtherResponses += n
	}
	a.durationSum += sum
	for i := range a.buckets {
		a.buckets[i] += buckets[i]
	}
}

func (a *trafficAggregate) finish() {
	if a.Requests > 0 {
		a.AverageLatencyMS = rounded(a.durationSum/float64(a.Requests)*1000, 1)
		a.P50LatencyMS = histogramQuantileMS(a.buckets, 0.50)
		a.P95LatencyMS = histogramQuantileMS(a.buckets, 0.95)
	}
	if a.Views > 0 {
		a.VisitorCoveragePC = rounded(float64(a.IdentifiedViews)/float64(a.Views)*100, 1)
	}
}

func histogramQuantileMS(buckets [httpDurationBucketCount]uint64, quantile float64) float64 {
	return durationQuantileMS(buckets, HTTPDurationBounds, quantile)
}

func durationQuantileMS(buckets [httpDurationBucketCount]uint64, bounds []float64, quantile float64) float64 {
	var total uint64
	for _, n := range buckets {
		total += n
	}
	if total == 0 {
		return 0
	}
	target := uint64(math.Ceil(float64(total) * quantile))
	if target == 0 {
		target = 1
	}
	var seen uint64
	for i, n := range buckets {
		seen += n
		if seen < target {
			continue
		}
		// The final bucket is +Inf. Returning the last finite boundary is a
		// useful, explicitly bounded approximation rather than JSON infinity.
		if i >= len(bounds) {
			return bounds[len(bounds)-1] * 1000
		}
		return bounds[i] * 1000
	}
	return 0
}

func rounded(value float64, places int) float64 {
	scale := math.Pow10(places)
	return math.Round(value*scale) / scale
}
