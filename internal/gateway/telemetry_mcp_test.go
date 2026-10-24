package gateway

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	"go.opentelemetry.io/otel/sdk/metric"
	"go.opentelemetry.io/otel/sdk/metric/metricdata"

	"github.com/adrianliechti/donkey/internal/telemetry"
)

func mcpTelemetryServer(t *testing.T, handler http.HandlerFunc) (*Server, *metric.ManualReader) {
	t.Helper()
	backend := httptest.NewServer(handler)
	t.Cleanup(backend.Close)
	target, _ := url.Parse(backend.URL)
	s := mcpTestServer(t, target)
	rt := s.routes["tools.example"][0]
	rt.Login = false
	s.routes["tools.example"] = []route{rt}
	reader := metric.NewManualReader()
	provider := metric.NewMeterProvider(metric.WithReader(reader))
	t.Cleanup(func() { _ = provider.Shutdown(context.Background()) })
	httpDuration, err := provider.Meter("test").Float64Histogram(telemetry.HTTPServerRequestDuration)
	if err != nil {
		t.Fatal(err)
	}
	mcpDuration, err := provider.Meter("test").Float64Histogram(telemetry.MCPServerOperationDuration)
	if err != nil {
		t.Fatal(err)
	}
	s.telemetry = &edgeTelemetry{provider: provider, duration: httpDuration, mcpDuration: mcpDuration}
	return s, reader
}

func mcpObservedRequest(s *Server, method, body, session, event string) *httptest.ResponseRecorder {
	r := httptest.NewRequest(method, "https://tools.example/mcp", strings.NewReader(body))
	r.Header.Set("Content-Type", "application/json")
	r.Header.Set("Accept", "application/json, text/event-stream")
	if session != "" {
		r.Header.Set("MCP-Session-Id", session)
	}
	if event != "" {
		r.Header.Set("Last-Event-ID", event)
	}
	w := httptest.NewRecorder()
	s.serveApp(w, r)
	return w
}

func mcpObservedPoints(t *testing.T, reader *metric.ManualReader) []metricdata.HistogramDataPoint[float64] {
	t.Helper()
	var result metricdata.ResourceMetrics
	if err := reader.Collect(context.Background(), &result); err != nil {
		t.Fatal(err)
	}
	var points []metricdata.HistogramDataPoint[float64]
	for _, scope := range result.ScopeMetrics {
		for _, metric := range scope.Metrics {
			if metric.Name == telemetry.MCPServerOperationDuration {
				points = append(points, metric.Data.(metricdata.Histogram[float64]).DataPoints...)
			}
		}
	}
	return points
}

func mcpObservedCount(t *testing.T, reader *metric.ManualReader) uint64 {
	t.Helper()
	var count uint64
	for _, point := range mcpObservedPoints(t, reader) {
		count += point.Count
	}
	return count
}

func TestMCPObserverJSONAndPrivacy(t *testing.T) {
	request := `{"jsonrpc":"2.0","id":"private-id","method":"tools/call","params":{"name":"add","arguments":{"secret":"private-argument"}}}`
	for _, test := range []struct {
		name, response, code, failure string
		status                        int
	}{
		{"success", `{"jsonrpc":"2.0","id":"private-id","result":{"content":[{"type":"text","text":"private-result"}],"isError":false}}`, "", "", 200},
		{"tool failure", `{"jsonrpc":"2.0","id":"private-id","result":{"isError":true}}`, "", "tool_error", 200},
		{"caller error", `{"jsonrpc":"2.0","id":"private-id","error":{"code":-32602,"message":"invalid arguments"}}`, "-32602", "", 200},
		{"server error", `{"jsonrpc":"2.0","id":"private-id","error":{"code":-32603}}`, "-32603", "", 500},
		{"HTTP failure", `unavailable`, "", "503", 503},
	} {
		t.Run(test.name, func(t *testing.T) {
			s, reader := mcpTelemetryServer(t, func(w http.ResponseWriter, r *http.Request) {
				body, _ := io.ReadAll(r.Body)
				if string(body) != request {
					t.Errorf("request changed: %s", body)
				}
				w.Header().Set("Content-Type", "application/json")
				w.WriteHeader(test.status)
				_, _ = io.WriteString(w, test.response)
			})
			result := mcpObservedRequest(s, "POST", request, "private-session", "")
			if result.Code != test.status || result.Body.String() != test.response {
				t.Fatalf("response changed: %d %s", result.Code, result.Body.String())
			}
			points := mcpObservedPoints(t, reader)
			if len(points) != 1 || points[0].Count != 1 {
				t.Fatalf("points=%+v", points)
			}
			values := map[string]string{}
			for _, attr := range points[0].Attributes.ToSlice() {
				values[string(attr.Key)] = attr.Value.AsString()
			}
			if values["mcp.method.name"] != "tools/call" || values["gen_ai.tool.name"] != "add" || values["error.type"] != test.failure || values["rpc.response.status_code"] != test.code {
				t.Fatalf("attributes=%v", values)
			}
			for key, value := range values {
				if strings.Contains(value, "private") || strings.Contains(key, "session") || strings.Contains(key, "request.id") {
					t.Fatalf("private metadata escaped: %v", values)
				}
			}
		})
	}
}

func TestMCPObserverStreamsAndResumesOnce(t *testing.T) {
	initial := "id: event-1\r\ndata: \r\n\r\ndata: {\"jsonrpc\":\"2.0\",\"method\":\"notifications/progress\",\"params\":{}}\r\n\r\n"
	completed := "id: event-2\r\ndata: {\"jsonrpc\":\"2.0\",\r\ndata: \"id\":7,\"result\":{\"isError\":false}}\r\n\r\n"
	for _, session := range []string{"session-1", ""} {
		t.Run("session="+session, func(t *testing.T) {
			s, reader := mcpTelemetryServer(t, func(w http.ResponseWriter, r *http.Request) {
				_, _ = io.Copy(io.Discard, r.Body)
				w.Header().Set("Content-Type", "text/event-stream")
				if session != "" {
					w.Header().Set("MCP-Session-Id", session)
				}
				data := initial
				if r.Method == "GET" {
					data = completed
				}
				for _, b := range []byte(data) {
					_, _ = w.Write([]byte{b})
					w.(http.Flusher).Flush()
				}
			})
			request := `{"jsonrpc":"2.0","id":7,"method":"tools/call","params":{"name":"slow"}}`
			result := mcpObservedRequest(s, "POST", request, session, "")
			if result.Body.String() != initial || mcpObservedCount(t, reader) != 0 {
				t.Fatal("partial stream was modified or counted as complete")
			}
			for range 2 {
				result = mcpObservedRequest(s, "GET", "", session, "event-1")
				if result.Body.String() != completed {
					t.Fatal("resumed response changed")
				}
			}
			if count := mcpObservedCount(t, reader); count != 1 {
				t.Fatalf("resumed/replayed count=%d", count)
			}
		})
	}
}

func TestMCPObserverDoesNotDelaySSE(t *testing.T) {
	release := make(chan struct{})
	s, reader := mcpTelemetryServer(t, func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.Copy(io.Discard, r.Body)
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = io.WriteString(w, "data: {\"jsonrpc\":\"2.0\",\"method\":\"notifications/progress\"}\n\n")
		w.(http.Flusher).Flush()
		<-release
		_, _ = io.WriteString(w, "data: {\"jsonrpc\":\"2.0\",\"id\":1,\"result\":{}}\n\n")
	})
	edge := httptest.NewServer(s.handler())
	defer edge.Close()
	defer close(release)
	r, _ := http.NewRequest("POST", edge.URL+"/mcp", strings.NewReader(`{"jsonrpc":"2.0","id":1,"method":"ping"}`))
	r.Host = "tools.example"
	r.Header.Set("Content-Type", "application/json")
	r.Header.Set("Accept", "text/event-stream")
	client := &http.Client{Timeout: 2 * time.Second}
	res, err := client.Do(r)
	if err != nil {
		t.Fatal(err)
	}
	defer res.Body.Close()
	buf := make([]byte, 6)
	if _, err := io.ReadFull(res.Body, buf); err != nil || string(buf) != "data: " {
		t.Fatalf("first chunk=%q %v", buf, err)
	}
	if mcpObservedCount(t, reader) != 0 {
		t.Fatal("progress counted as completion")
	}
}

func TestMCPObserverSkipsUnobservableMessages(t *testing.T) {
	valid := `{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"add"}}`
	for _, test := range []struct{ name, request, response, encoding string }{
		{"notification", `{"jsonrpc":"2.0","method":"notifications/initialized"}`, "", ""},
		{"ordinary API", `{"method":"tools/call","id":1}`, `{"result":{}}`, ""},
		{"wrong ID", valid, `{"jsonrpc":"2.0","id":2,"result":{}}`, ""},
		{"truncated", valid, `{"jsonrpc":"2.0","id":1,"result":`, ""},
		{"oversized response", valid, `{"jsonrpc":"2.0","id":1,"result":{"text":"` + strings.Repeat("x", mcpCaptureLimit) + `"}}`, ""},
		{"oversized request", `{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"add","arguments":{"x":"` + strings.Repeat("x", mcpCaptureLimit) + `"}}}`, `{"jsonrpc":"2.0","id":1,"result":{}}`, ""},
		{"encoded", valid, "encoded bytes", "gzip"},
	} {
		t.Run(test.name, func(t *testing.T) {
			s, reader := mcpTelemetryServer(t, func(w http.ResponseWriter, r *http.Request) {
				body, _ := io.ReadAll(r.Body)
				if !bytes.Equal(body, []byte(test.request)) {
					t.Error("request changed")
				}
				w.Header().Set("Content-Type", "application/json")
				if test.encoding != "" {
					w.Header().Set("Content-Encoding", test.encoding)
				}
				_, _ = io.WriteString(w, test.response)
			})
			r := httptest.NewRequest("POST", "https://tools.example/mcp", strings.NewReader(test.request))
			r.Header.Set("Content-Type", "application/json")
			r.Header.Set("Accept", "text/event-stream")
			r.Header.Set("Accept-Encoding", "gzip") // Preserve the upstream encoding byte for byte.
			w := httptest.NewRecorder()
			s.serveApp(w, r)
			if w.Body.String() != test.response {
				t.Fatal("response changed")
			}
			if count := mcpObservedCount(t, reader); count != 0 {
				t.Fatalf("unexpected count=%d", count)
			}
		})
	}
}

func TestMCPTrackerBoundsAndScope(t *testing.T) {
	tracker := &mcpTracker{}
	scope := mcpHash("team", "app", "/mcp")
	for i := range mcpPendingLimit + 10 {
		op := &mcpOperation{scope: scope, id: mcpHash(fmt.Sprint(i)), started: time.Now()}
		tracker.register(op, "session")
		tracker.event(op, "session", fmt.Sprint(i))
	}
	if len(tracker.operations) != mcpPendingLimit || len(tracker.events) != mcpPendingLimit {
		t.Fatal("unbounded correlation")
	}
	if tracker.find(mcpHash("other-app"), "session", mcpHash("1")) != nil {
		t.Fatal("cross-app correlation")
	}
	if tracker.find(scope, "another-session", mcpHash("1")) != nil {
		t.Fatal("cross-session correlation")
	}
	for _, op := range tracker.operations {
		op.started = time.Now().Add(-2 * mcpPendingTTL)
	}
	tracker.cleaned = time.Time{}
	tracker.clean()
	if len(tracker.operations) != 0 || len(tracker.events) != 0 {
		t.Fatal("expired correlation retained")
	}
}

func TestMCPObserverUnknownMethodsAndEquivalentIDs(t *testing.T) {
	s, reader := mcpTelemetryServer(t, func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.Copy(io.Discard, r.Body)
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"jsonrpc":"2.0","id":"id-a","error":{"code":-32601}}`)
	})
	mcpObservedRequest(s, "POST", `{"jsonrpc":"2.0","id":"id-\u0061","method":"unbounded-private-method"}`, "", "")
	points := mcpObservedPoints(t, reader)
	if len(points) != 1 || points[0].Count != 1 {
		t.Fatalf("points=%+v", points)
	}
	for _, attr := range points[0].Attributes.ToSlice() {
		if string(attr.Key) == "mcp.method.name" && attr.Value.AsString() != "_OTHER" {
			t.Fatalf("unbounded method=%v", attr)
		}
	}
}

func TestMCPSSEParserRecoversAfterOversizedEvent(t *testing.T) {
	var messages []string
	parser := mcpSSEParser{message: func(data []byte) { messages = append(messages, string(data)) }, eventID: func(string) {}}
	parser.write([]byte("data: " + strings.Repeat("x", mcpCaptureLimit+1) + "\n\n"))
	parser.write([]byte("data: {\"ok\":true}\n\n"))
	if len(messages) != 1 || messages[0] != `{"ok":true}` {
		t.Fatalf("messages=%v", messages)
	}
	if len(parser.data.data) != 0 || len(parser.line.data) != 0 {
		t.Fatal("event payload retained")
	}
}
