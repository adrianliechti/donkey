package gateway

import (
	"context"
	"net/http"
	"net/http/httputil"
	"time"

	"github.com/adrianliechti/donkey/internal/analytics"
)

// telemetryPlugin is the gateway's internal extension point. Plugins observe
// one routed exchange; exporting and app identity stay with the gateway.
// HTTP covers every app/API, while web and MCP add their protocol semantics.
// Hooks must preserve streaming, bound their memory and never reject traffic.
type telemetryPlugin interface {
	Observe(*http.Request, route) *telemetryObservation
}

type telemetryObservation struct {
	rewrite  func(*httputil.ProxyRequest)
	response func(*http.Response)
	finish   func(*session, int, time.Duration, string)
}

type telemetryObservations []*telemetryObservation
type telemetryContextKey struct{}

func (s *Server) observeTraffic(r *http.Request, rt route) *http.Request {
	plugins := []telemetryPlugin{
		httpTelemetryPlugin{s.telemetry},
		webTelemetryPlugin{s},
		mcpTelemetryPlugin{s.telemetry},
	}
	var observations telemetryObservations
	for _, plugin := range plugins {
		if observation := plugin.Observe(r, rt); observation != nil {
			observations = append(observations, observation)
		}
	}
	return r.WithContext(context.WithValue(r.Context(), telemetryContextKey{}, observations))
}

func trafficObservations(r *http.Request) telemetryObservations {
	observations, _ := r.Context().Value(telemetryContextKey{}).(telemetryObservations)
	return observations
}

type httpTelemetryPlugin struct{ telemetry *edgeTelemetry }

func (p httpTelemetryPlugin) Observe(r *http.Request, rt route) *telemetryObservation {
	return &telemetryObservation{finish: func(sess *session, status int, elapsed time.Duration, contentType string) {
		p.telemetry.RecordHTTP(r, rt, sess, status, elapsed)
	}}
}

type webTelemetryPlugin struct{ server *Server }

func (p webTelemetryPlugin) Observe(r *http.Request, rt route) *telemetryObservation {
	acceptsGzip := false
	inject := p.server.countsVisits(rt) && wantsDocument(r)
	return &telemetryObservation{
		rewrite: func(pr *httputil.ProxyRequest) {
			if inject {
				acceptsGzip = analytics.AcceptsGzip(pr.In)
				// The transport can decode gzip before the web plugin edits HTML.
				pr.Out.Header.Del("Accept-Encoding")
			}
		},
		response: func(res *http.Response) {
			if inject {
				injectScript(res, acceptsGzip)
			}
		},
		finish: func(sess *session, status int, elapsed time.Duration, contentType string) {
			p.server.telemetry.RecordView(r, rt, sess, status, contentType)
		},
	}
}
