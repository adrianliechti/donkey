package telemetry

import (
	"crypto/hmac"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/hex"
	"strings"
	"time"

	metricsv1 "go.opentelemetry.io/proto/otlp/collector/metrics/v1"

	"github.com/adrianliechti/donkey/internal/config"
)

// TelemetryTokenHeader is deliberately separate from Authorization. It fits
// OTEL_EXPORTER_OTLP_HEADERS without putting the installation's session key,
// or a credential useful anywhere else, on the wire.
const TelemetryTokenHeader = "X-Donkey-Telemetry-Token"

const (
	gatewayTokenPurpose = "donkey/telemetry/gateway/v1"
	queryTokenPurpose   = "donkey/telemetry/query/v1"
	appTokenPurpose     = "donkey/telemetry/app/v1"
	runtimeTokenPurpose = "donkey/telemetry/runtime/v1"
	appVisitorPurpose   = "donkey/telemetry/app-visitor/v1"
	spaceVisitorPurpose = "donkey/telemetry/space-visitor/v1"
	webVisitorPurpose   = "donkey/telemetry/web-visitor/v1"
)

func GatewayToken(key []byte) string { return scopedToken(key, gatewayTokenPurpose) }

func QueryToken(key []byte) string { return scopedToken(key, queryTokenPurpose) }

// RuntimeToken lets donkey-server submit resource snapshots for every app in
// its installation in one OTLP request. It is distinct from the gateway and
// app tokens, so none of those writers can forge runtime-owned measurements.
func RuntimeToken(key []byte) string { return scopedToken(key, runtimeTokenPurpose) }

// AppToken is valid for exactly one semantic-convention resource scope. An
// app can report its own in-process metrics, but cannot write another app's
// stream or the gateway-owned edge stream.
func AppToken(key []byte, space, app string) string {
	return scopedToken(key, appTokenPurpose, space, app)
}

// VisitorHashes deliberately use different domains. A query for a whole
// space can deduplicate one signed-in person across its apps, while app-level
// hashes cannot be joined across apps. Empty subjects remain anonymous.
func VisitorHashes(key []byte, space, app, subject string) (appHash, spaceHash string) {
	if subject == "" {
		return "", ""
	}
	return scopedToken(key, appVisitorPurpose, space, app, subject),
		scopedToken(key, spaceVisitorPurpose, space, subject)
}

// WebVisitor is the web analytics visitor: for a signed-in visitor a per-app
// hash of the subject, stable across days; for anyone else a per-app hash of
// the address and browser that changes with the UTC day, so it cannot follow
// a person from one day to the next or from one app to another. Nothing is
// stored on the visitor's device either way.
func WebVisitor(key []byte, space, app, subject, address, userAgent string, day time.Time) string {
	if subject != "" {
		return scopedToken(key, webVisitorPurpose, space, app, "subject", subject)
	}
	return scopedToken(key, webVisitorPurpose, space, app, "anonymous", day.UTC().Format(time.DateOnly), address, userAgent)
}

func scopedToken(key []byte, values ...string) string {
	h := hmac.New(sha256.New, key)
	for _, value := range values {
		_, _ = h.Write([]byte(value))
		_, _ = h.Write([]byte{0})
	}
	return hex.EncodeToString(h.Sum(nil))
}

func tokenMatches(got, expected string) bool {
	got = strings.TrimSpace(got)
	return len(got) == len(expected) && subtle.ConstantTimeCompare([]byte(got), []byte(expected)) == 1
}

// metricPrincipal authenticates one whole OTLP request. A standard exporter
// sends one service resource per request; requiring a single app scope keeps
// the scoped token meaningful even if a caller constructs protobuf by hand.
type metricPrincipalKind uint8

const (
	metricPrincipalInvalid metricPrincipalKind = iota
	metricPrincipalApp
	metricPrincipalGateway
	metricPrincipalRuntime
)

func metricPrincipal(req *metricsv1.ExportMetricsServiceRequest, token string, key []byte) (metricPrincipalKind, bool) {
	if tokenMatches(token, GatewayToken(key)) {
		return metricPrincipalGateway, true
	}
	if tokenMatches(token, RuntimeToken(key)) {
		return metricPrincipalRuntime, true
	}
	space, app := "", ""
	found := false
	for _, rm := range req.ResourceMetrics {
		if rm == nil {
			return metricPrincipalInvalid, false
		}
		resources := resourceMap(rm.Resource)
		candidateSpace, candidateApp := resources["service.namespace"], resources["service.name"]
		if !config.ValidName(candidateSpace) || !config.ValidName(candidateApp) {
			return metricPrincipalInvalid, false
		}
		if !found {
			space, app, found = candidateSpace, candidateApp, true
			continue
		}
		if candidateSpace != space || candidateApp != app {
			return metricPrincipalInvalid, false
		}
	}
	if found && tokenMatches(token, AppToken(key, space, app)) {
		return metricPrincipalApp, true
	}
	return metricPrincipalInvalid, false
}
