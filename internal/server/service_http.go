package server

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/adrianliechti/donkey/internal/addons"
	"github.com/adrianliechti/donkey/internal/config"
	"github.com/adrianliechti/donkey/internal/serviceproxy"
	"golang.org/x/net/http/httpguts"
)

const httpTestBodyLimit = 1 << 20

var httpTestClient = func() *http.Client {
	transport := http.DefaultTransport.(*http.Transport).Clone()
	transport.Proxy = nil // the shared endpoint is always internal
	return &http.Client{Transport: transport, Timeout: 30 * time.Second,
		CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
}()

type serviceHTTPRequest struct {
	Method  string            `json:"method"`
	URL     string            `json:"url"`
	Headers map[string]string `json:"headers"`
	Body    string            `json:"body"`
}

func (s *Server) handleServiceHTTPRequest(w http.ResponseWriter, r *http.Request) {
	space, err := s.space(r)
	if err != nil {
		writeErr(w, http.StatusBadRequest, err)
		return
	}
	name := r.PathValue("service")
	if !config.ValidName(name) {
		writeErr(w, http.StatusBadRequest, fmt.Errorf("invalid service name"))
		return
	}
	var input serviceHTTPRequest
	decoder := json.NewDecoder(http.MaxBytesReader(w, r.Body, 2*httpTestBodyLimit))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&input); err != nil {
		writeErr(w, http.StatusBadRequest, fmt.Errorf("invalid HTTP request settings"))
		return
	}
	switch input.Method {
	case "GET", "HEAD", "POST", "PUT", "PATCH", "DELETE", "OPTIONS":
	default:
		writeErr(w, http.StatusBadRequest, fmt.Errorf("unsupported HTTP method"))
		return
	}
	if len(input.Body) > httpTestBodyLimit || len(input.Headers) > 64 {
		writeErr(w, http.StatusBadRequest, fmt.Errorf("request exceeds the test client limits"))
		return
	}
	connection, err := s.rt.ServiceConnection(r.Context(), space, name)
	if err != nil {
		writeErr(w, http.StatusBadGateway, err)
		return
	}
	configured := false
	for _, service := range s.cfg.ProxyServices {
		configured = configured || service.Name == connection.Type
	}
	if connection.Protocol != "http" || !configured {
		writeErr(w, http.StatusBadRequest, fmt.Errorf("this service is not a configured HTTP proxy"))
		return
	}
	base, err := url.Parse(connection.Binding[addons.ProxyEnvKey(connection.Type)])
	if err != nil || base.Scheme != "http" || base.Host == "" || base.User != nil ||
		!addons.IsProxyBinding("http://"+base.Host, connection.Type, connection.Binding) {
		writeErr(w, http.StatusBadRequest, fmt.Errorf("the service has no proxy binding"))
		return
	}
	target, err := httpTestURL(base, input.URL)
	if err != nil {
		writeErr(w, http.StatusBadRequest, err)
		return
	}
	out, err := http.NewRequestWithContext(r.Context(), input.Method, target.String(), strings.NewReader(input.Body))
	if err != nil {
		writeErr(w, http.StatusBadRequest, fmt.Errorf("invalid request URL"))
		return
	}
	for name, value := range input.Headers {
		if !httpguts.ValidHeaderFieldName(name) || !httpguts.ValidHeaderFieldValue(value) ||
			strings.EqualFold(name, serviceproxy.TestRequestHeader) || strings.EqualFold(name, "Host") ||
			strings.EqualFold(name, "Content-Length") || strings.EqualFold(name, "Proxy-Authorization") {
			writeErr(w, http.StatusBadRequest, fmt.Errorf("invalid or reserved request header"))
			return
		}
		out.Header.Set(name, value)
	}
	grant, err := serviceproxy.SealTestRequest(s.cfg.TelemetryKey, space, connection.Type, out)
	if err != nil {
		writeErr(w, http.StatusServiceUnavailable, err)
		return
	}
	out.Header.Set(serviceproxy.TestRequestHeader, grant)
	started := time.Now()
	response, err := httpTestClient.Do(out)
	if err != nil {
		writeErr(w, http.StatusBadGateway, fmt.Errorf("the shared proxy did not answer"))
		return
	}
	defer response.Body.Close()
	body, err := io.ReadAll(io.LimitReader(response.Body, httpTestBodyLimit+1))
	if err != nil {
		writeErr(w, http.StatusBadGateway, fmt.Errorf("reading the proxy response failed"))
		return
	}
	truncated := len(body) > httpTestBodyLimit
	if truncated {
		body = body[:httpTestBodyLimit]
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"status": response.StatusCode, "statusText": http.StatusText(response.StatusCode),
		"headers": response.Header, "body": string(body), "truncated": truncated,
		"durationMs": time.Since(started).Milliseconds(),
	})
}

// Only the selected binding's route is usable; neither full URLs nor relative
// references may turn the console into an arbitrary internal HTTP client.
func httpTestURL(base *url.URL, raw string) (*url.URL, error) {
	requested, err := url.Parse(raw)
	if err != nil || requested.User != nil || requested.Fragment != "" || requested.Opaque != "" {
		return nil, fmt.Errorf("enter a path or a URL under this service's base URL")
	}
	if requested.IsAbs() || requested.Host != "" {
		if requested.Scheme != base.Scheme || requested.Host != base.Host || !strings.HasPrefix(requested.EscapedPath(), base.EscapedPath()) {
			return nil, fmt.Errorf("the URL must belong to this service's base URL")
		}
		return requested, nil
	}
	target := *base
	target.RawPath = base.EscapedPath() + strings.TrimPrefix(requested.EscapedPath(), "/")
	target.Path, _ = url.PathUnescape(target.RawPath)
	target.RawQuery, target.ForceQuery = requested.RawQuery, requested.ForceQuery
	return &target, nil
}
