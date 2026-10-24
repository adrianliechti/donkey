package gateway

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/json"
	"io"
	"mime"
	"net/http"
	"net/http/httputil"
	"strconv"
	"strings"
	"sync"
	"time"

	"go.opentelemetry.io/otel/attribute"
	otelmetric "go.opentelemetry.io/otel/metric"
)

const (
	mcpCaptureLimit = 1 << 20 // Per JSON body or SSE event; traffic itself is never limited.
	mcpPendingLimit = 4096
	mcpPendingTTL   = 10 * time.Minute
)

type mcpTelemetryPlugin struct{ telemetry *edgeTelemetry }

func (p mcpTelemetryPlugin) Observe(r *http.Request, rt route) *telemetryObservation {
	if p.telemetry == nil || p.telemetry.mcpDuration == nil || rt.Token || rt.Space == "" || rt.App == "" {
		return nil
	}
	// Streamable HTTP clients advertise SSE, including on the initialize POST.
	// Never classify an ordinary JSON API solely from fields in its body.
	if r.Header.Get("MCP-Protocol-Version") == "" && !strings.Contains(r.Header.Get("Accept"), "text/event-stream") {
		return nil
	}
	tracker := &p.telemetry.mcp
	scope := mcpHash(rt.Space, rt.App, requestHost(r), r.URL.Path)
	requestSession := r.Header.Get("MCP-Session-Id")
	started := time.Now()
	var mu sync.Mutex
	var operation *mcpOperation
	var requestBody io.ReadCloser
	if r.Method == http.MethodPost && mediaType(r.Header.Get("Content-Type")) == "application/json" && r.Header.Get("Content-Encoding") == "" && r.Body != nil {
		capture := &boundedCapture{}
		requestBody = observeBody(r.Body, r.ContentLength, capture.write, func(complete bool) {
			defer func() { capture.data = nil }()
			if !complete || capture.overflow {
				return
			}
			var request struct {
				Version string          `json:"jsonrpc"`
				ID      json.RawMessage `json:"id"`
				Method  string          `json:"method"`
				Params  struct {
					Name string `json:"name"`
				} `json:"params"`
			}
			if json.Unmarshal(capture.data, &request) != nil || request.Version != "2.0" || request.Method == "" {
				return
			}
			id, valid := mcpID(request.ID)
			if !valid {
				return
			}
			method := request.Method
			if !mcpMethod(method) {
				method = "_OTHER"
			}
			name := ""
			if request.Method == "tools/call" || request.Method == "prompts/get" {
				name = request.Params.Name
				if len(name) > 128 || strings.ContainsAny(name, "\r\n\t") {
					name = "_OTHER"
				}
			}
			op := &mcpOperation{route: rt, scope: scope, id: id, method: method, target: name, started: started}
			mu.Lock()
			operation = tracker.register(op, requestSession)
			mu.Unlock()
		})
	} else if r.Method != http.MethodGet || r.Header.Get("Last-Event-ID") == "" {
		return nil
	}
	return &telemetryObservation{rewrite: func(pr *httputil.ProxyRequest) {
		// Only tap traffic authorized for forwarding. net/http may drain a
		// rejected request's original body after the handler has returned.
		if requestBody != nil {
			pr.Out.Body = requestBody
		}
	}, response: func(res *http.Response) {
		// A response from the backend, never an auth rejection generated here.
		mu.Lock()
		op := operation
		mu.Unlock()
		session := requestSession
		if newSession := res.Header.Get("MCP-Session-Id"); newSession != "" {
			session = newSession
		}
		if op != nil {
			op = tracker.register(op, session)
			mu.Lock()
			operation = op
			mu.Unlock()
		} else if r.Method == http.MethodGet {
			op = tracker.resume(scope, session, r.Header.Get("Last-Event-ID"))
		}
		if res.Body == nil || res.Header.Get("Content-Encoding") != "" {
			return
		}
		consume := func(data []byte) {
			var response struct {
				Version string          `json:"jsonrpc"`
				ID      json.RawMessage `json:"id"`
				Result  json.RawMessage `json:"result"`
				Error   *struct {
					Code *int64 `json:"code"`
				} `json:"error"`
			}
			if json.Unmarshal(data, &response) != nil || response.Version != "2.0" {
				return
			}
			matched := op
			id, valid := mcpID(response.ID)
			if !valid {
				return
			}
			if matched == nil || matched.id != id {
				// GET streams can replay a response after an earlier POST disconnected.
				if r.Method != http.MethodGet || session == "" {
					return
				}
				matched = tracker.find(scope, session, id)
			}
			if matched == nil {
				return
			}
			code, errorType := "", ""
			if response.Error != nil {
				if response.Error.Code == nil || response.Result != nil {
					return
				}
				code = strconv.FormatInt(*response.Error.Code, 10)
			} else {
				var result struct {
					IsError bool `json:"isError"`
				}
				if response.Result == nil || bytes.Equal(response.Result, []byte("null")) || json.Unmarshal(response.Result, &result) != nil {
					return
				}
				if matched.method == "tools/call" && result.IsError {
					errorType = "tool_error"
				}
			}
			matched.once.Do(func() { p.telemetry.recordMCP(matched, code, errorType) })
		}
		switch mediaType(res.Header.Get("Content-Type")) {
		case "application/json":
			capture := &boundedCapture{}
			res.Body = observeBody(res.Body, res.ContentLength, capture.write, func(complete bool) {
				defer func() { capture.data = nil }()
				if complete && !capture.overflow {
					consume(capture.data)
				}
			})
		case "text/event-stream":
			parser := &mcpSSEParser{message: consume, eventID: func(id string) {
				if op != nil {
					tracker.event(op, session, id)
				}
			}}
			res.Body = observeBody(res.Body, res.ContentLength, parser.write, func(bool) {})
		}
	}, finish: func(_ *session, status int, _ time.Duration, _ string) {
		mu.Lock()
		op := operation
		mu.Unlock()
		if op != nil && status >= 400 {
			op.once.Do(func() { p.telemetry.recordMCP(op, "", strconv.Itoa(status)) })
		}
	}}
}

func mediaType(value string) string {
	media, _, _ := mime.ParseMediaType(value)
	return media
}

func mcpMethod(method string) bool {
	switch method {
	case "initialize", "ping", "tools/list", "tools/call", "prompts/list", "prompts/get",
		"resources/list", "resources/templates/list", "resources/read", "resources/subscribe", "resources/unsubscribe",
		"logging/setLevel", "completion/complete", "tasks/get", "tasks/result", "tasks/list", "tasks/cancel", "server/discover":
		return true
	}
	return false
}

func mcpID(raw json.RawMessage) ([32]byte, bool) {
	if len(raw) == 0 || len(raw) > 256 {
		return [32]byte{}, false
	}
	var value any
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.UseNumber()
	if decoder.Decode(&value) != nil {
		return [32]byte{}, false
	}
	switch id := value.(type) {
	case string:
		return mcpHash("string", id), true
	case json.Number:
		// MCP IDs are strings or integers. Preserve large integer IDs exactly.
		if strings.ContainsAny(string(id), ".eE") {
			return [32]byte{}, false
		}
		if id == "-0" {
			id = "0"
		}
		return mcpHash("number", string(id)), true
	}
	return [32]byte{}, false
}

func mcpHash(parts ...string) [32]byte { return sha256.Sum256([]byte(strings.Join(parts, "\x00"))) }

type mcpOperation struct {
	route          route
	scope, id      [32]byte
	method, target string
	started        time.Time
	once           sync.Once
}

// Correlation is ephemeral and bounded. Raw IDs, sessions, paths and bodies
// never leave the observer. Completed entries deduplicate replayed responses.
// Losing state (restart, another gateway, TTL/capacity) omits the operation;
// a disconnected stream is never guessed to be a successful or failed tool.
type mcpTracker struct {
	mu         sync.Mutex
	operations map[[32]byte]*mcpOperation
	events     map[[32]byte]*mcpOperation
	cleaned    time.Time
}

func (t *mcpTracker) clean() {
	if t.operations == nil {
		t.operations = make(map[[32]byte]*mcpOperation)
		t.events = make(map[[32]byte]*mcpOperation)
	}
	now := time.Now()
	if now.Sub(t.cleaned) < time.Minute {
		return
	}
	for _, entries := range []map[[32]byte]*mcpOperation{t.operations, t.events} {
		for key, op := range entries {
			if now.Sub(op.started) > mcpPendingTTL {
				delete(entries, key)
			}
		}
	}
	t.cleaned = now
}

func mcpOperationKey(scope [32]byte, session string, id [32]byte) [32]byte {
	return mcpHash(string(scope[:]), session, string(id[:]))
}

func (t *mcpTracker) register(op *mcpOperation, session string) *mcpOperation {
	if session == "" {
		return op
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	t.clean()
	key := mcpOperationKey(op.scope, session, op.id)
	if old := t.operations[key]; old != nil {
		return old
	}
	if len(t.operations) < mcpPendingLimit {
		t.operations[key] = op
	}
	return op
}

func (t *mcpTracker) find(scope [32]byte, session string, id [32]byte) *mcpOperation {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.clean()
	return t.operations[mcpOperationKey(scope, session, id)]
}

func (t *mcpTracker) event(op *mcpOperation, session, id string) {
	if id == "" || len(id) > 1024 {
		return
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	t.clean()
	key := mcpHash(string(op.scope[:]), session, id)
	if len(t.events) < mcpPendingLimit {
		t.events[key] = op
	}
}

func (t *mcpTracker) resume(scope [32]byte, session, event string) *mcpOperation {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.clean()
	return t.events[mcpHash(string(scope[:]), session, event)]
}

func (t *edgeTelemetry) recordMCP(op *mcpOperation, code, errorType string) {
	attrs := []attribute.KeyValue{
		attribute.String("donkey.space.name", op.route.Space), attribute.String("donkey.app.name", op.route.App),
		attribute.String("mcp.method.name", op.method),
	}
	if op.target != "" {
		key := "gen_ai.tool.name"
		if op.method == "prompts/get" {
			key = "gen_ai.prompt.name"
		}
		attrs = append(attrs, attribute.String(key, op.target))
	}
	if code != "" {
		attrs = append(attrs, attribute.String("rpc.response.status_code", code))
	}
	if errorType != "" {
		attrs = append(attrs, attribute.String("error.type", errorType))
	}
	t.mcpDuration.Record(context.Background(), time.Since(op.started).Seconds(), otelmetric.WithAttributes(attrs...))
}

type boundedCapture struct {
	data     []byte
	overflow bool
}

func (b *boundedCapture) write(p []byte) {
	if b.overflow {
		return
	}
	if len(b.data)+len(p) > mcpCaptureLimit {
		b.data = nil
		b.overflow = true
		return
	}
	b.data = append(b.data, p...)
}

// A read-through tap: no extra reads, buffering delay, goroutines, or changes
// to EOF/errors. Request and response ownership stays with net/http.
type observedBody struct {
	io.ReadCloser
	mu           sync.Mutex
	read, length int64
	done         bool
	data         func([]byte)
	end          func(bool)
}

func observeBody(body io.ReadCloser, length int64, data func([]byte), end func(bool)) io.ReadCloser {
	return &observedBody{ReadCloser: body, length: length, data: data, end: end}
}

func (b *observedBody) Read(p []byte) (int, error) {
	n, err := b.ReadCloser.Read(p)
	b.mu.Lock()
	defer b.mu.Unlock()
	if !b.done {
		b.read += int64(n)
		b.data(p[:n])
		if err != nil || b.length >= 0 && b.read == b.length {
			b.done = true
			b.end(err == nil || err == io.EOF)
		}
	}
	return n, err
}

func (b *observedBody) Close() error {
	err := b.ReadCloser.Close()
	b.mu.Lock()
	defer b.mu.Unlock()
	if !b.done {
		b.done = true
		b.end(b.length >= 0 && b.read == b.length)
	}
	return err
}

// SSE events can span reads and contain multiple data lines. Comments,
// progress and server-initiated requests never finish a client operation.
type mcpSSEParser struct {
	line, data boundedCapture
	id         string
	cr         bool
	message    func([]byte)
	eventID    func(string)
}

func (p *mcpSSEParser) write(chunk []byte) {
	for _, ch := range chunk {
		if ch == '\n' && p.cr {
			p.cr = false
			continue
		}
		p.cr = ch == '\r'
		if ch == '\r' || ch == '\n' {
			p.endLine()
			continue
		}
		p.line.write([]byte{ch})
	}
}

func (p *mcpSSEParser) endLine() {
	line, oversized := p.line.data, p.line.overflow
	p.line = boundedCapture{}
	if oversized {
		p.data.overflow = true
		p.data.data = nil
		return
	}
	if len(line) == 0 {
		if p.id != "" {
			p.eventID(p.id)
		}
		if !p.data.overflow && len(p.data.data) != 0 {
			p.message(bytes.TrimSuffix(p.data.data, []byte{'\n'}))
		}
		p.data, p.id = boundedCapture{}, ""
		return
	}
	field, value, _ := bytes.Cut(line, []byte{':'})
	value = bytes.TrimPrefix(value, []byte{' '})
	switch string(field) {
	case "data":
		p.data.write(value)
		p.data.write([]byte{'\n'})
	case "id":
		if len(value) <= 1024 && !bytes.ContainsRune(value, 0) {
			p.id = string(value)
		}
	}
}
