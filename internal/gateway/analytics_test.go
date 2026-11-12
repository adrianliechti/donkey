package gateway

import (
	"compress/gzip"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/adrianliechti/donkey/internal/analytics"
	"github.com/adrianliechti/donkey/internal/config"
	"github.com/adrianliechti/donkey/internal/telemetry"
)

const chromeUA = "Mozilla/5.0 (Macintosh; Intel Mac OS X 10_15_7) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/141.0.0.0 Safari/537.36"

// analyticsServer is a gateway with telemetry pointed at a recorder, and
// routes for shop (web, counted), api (an API on the same host) and quiet
// (opted out) in front of one backend.
type analyticsServer struct {
	*Server
	mu      sync.Mutex
	events  []telemetry.WebEvent
	backend *httptest.Server
	seen    chan *http.Request
}

func newAnalyticsServer(t *testing.T, page string) *analyticsServer {
	t.Helper()
	a := &analyticsServer{seen: make(chan *http.Request, 16)}
	collector := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/v1/donkey/web" {
			var batch telemetry.WebBatch
			_ = json.NewDecoder(r.Body).Decode(&batch)
			a.mu.Lock()
			a.events = append(a.events, batch.Events...)
			a.mu.Unlock()
		}
		w.WriteHeader(http.StatusOK)
	}))
	t.Cleanup(collector.Close)
	a.backend = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		a.seen <- r.Clone(context.Background())
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		w.Header().Set("ETag", `"v1"`)
		if strings.Contains(r.Header.Get("Accept-Encoding"), "gzip") {
			w.Header().Set("Content-Encoding", "gzip")
			gz := gzip.NewWriter(w)
			_, _ = io.WriteString(gz, page)
			_ = gz.Close()
			return
		}
		_, _ = io.WriteString(w, page)
	}))
	t.Cleanup(a.backend.Close)
	tel, err := newEdgeTelemetry(&Config{TelemetryEndpoint: collector.URL, TelemetryKey: testKey, SystemNamespace: "donkey-system"})
	if err != nil {
		t.Fatal(err)
	}
	backend, _ := url.Parse(a.backend.URL)
	policy := &config.SpaceConfig{Viewers: config.AccessConfig{Users: []string{"ada@example.com"}}}
	a.Server = &Server{
		cfg:       &Config{SessionKey: testKey, TelemetryKey: testKey},
		telemetry: tel,
		routes: map[string][]route{
			"shop.acme.example.com": {
				{Space: "acme", App: "api", Host: "shop.acme.example.com", Path: "/api", Backend: backend, Mode: config.AppModeAPI, Policy: policy},
				{Space: "acme", App: "shop", Host: "shop.acme.example.com", Path: "/", Backend: backend, Mode: config.AppModeWeb, Policy: policy},
			},
			"quiet.acme.example.com": {
				{Space: "acme", App: "quiet", Host: "quiet.acme.example.com", Path: "/", Backend: backend, Analytics: config.AnalyticsOff, Policy: policy},
			},
			"private.acme.example.com": {
				{Space: "acme", App: "private", Host: "private.acme.example.com", Path: "/", Backend: backend, Login: true, Policy: policy},
			},
		},
		lastAttempt: time.Now().Add(time.Hour), // never refresh: there is no route source
	}
	return a
}

// recorded flushes the exporter and returns what reached telemetry.
func (a *analyticsServer) recorded(t *testing.T) []telemetry.WebEvent {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := a.telemetry.Shutdown(ctx); err != nil {
		t.Fatal(err)
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.events
}

func navigate(host, path string) *http.Request {
	r := httptest.NewRequest(http.MethodGet, "https://"+host+path, nil)
	r.Header.Set("Accept", "text/html,application/xhtml+xml")
	r.Header.Set("Sec-Fetch-Dest", "document")
	r.Header.Set("Accept-Encoding", "gzip, deflate, br")
	r.Header.Set("User-Agent", chromeUA)
	return r
}

func TestGatewayAddsTheScriptToWebPages(t *testing.T) {
	page := `<!doctype html><html><head><title>Shop</title></head><body>hi</body></html>`
	a := newAnalyticsServer(t, page)

	rec := httptest.NewRecorder()
	a.serveApp(rec, navigate("shop.acme.example.com", "/"))
	upstream := <-a.seen
	if upstream.Header.Get("Accept-Encoding") != "gzip" {
		t.Errorf("the app should be asked for gzip only, got %q", upstream.Header.Get("Accept-Encoding"))
	}
	res := rec.Result()
	if res.Header.Get("Content-Encoding") != "gzip" || res.Header.Get("Content-Length") != "" || res.Header.Get("ETag") != `W/"v1"` {
		t.Fatalf("headers: %v", res.Header)
	}
	zr, err := gzip.NewReader(res.Body)
	if err != nil {
		t.Fatal(err)
	}
	body, _ := io.ReadAll(zr)
	if want := strings.Replace(page, "<head>", "<head>"+analytics.ScriptTag, 1); string(body) != want {
		t.Fatalf("body = %s", body)
	}

	// Opted out, fetched by script, or an API: the app's bytes as they were.
	for _, r := range []*http.Request{
		navigate("quiet.acme.example.com", "/"),
		func() *http.Request {
			r := navigate("shop.acme.example.com", "/")
			r.Header.Set("Sec-Fetch-Dest", "empty")
			return r
		}(),
		navigate("shop.acme.example.com", "/api/docs"),
	} {
		rec := httptest.NewRecorder()
		a.serveApp(rec, r)
		if got := (<-a.seen).Header.Get("Accept-Encoding"); got != "gzip, deflate, br" {
			t.Errorf("%s%s: the client's codings should reach the app, got %q", r.Host, r.URL.Path, got)
		}
		if strings.Contains(rec.Body.String(), analytics.ScriptPath) {
			t.Errorf("%s%s: no script expected", r.Host, r.URL.Path)
		}
	}
}

func TestGatewayServesTheScriptAndTakesItsEvents(t *testing.T) {
	a := newAnalyticsServer(t, "<html></html>")

	rec := httptest.NewRecorder()
	a.serveApp(rec, httptest.NewRequest(http.MethodGet, "https://shop.acme.example.com"+analytics.ScriptPath, nil))
	if rec.Code != http.StatusOK || !strings.HasPrefix(rec.Header().Get("Content-Type"), "text/javascript") ||
		!strings.Contains(rec.Body.String(), analytics.EventPath) {
		t.Fatalf("script: %d %v", rec.Code, rec.Header())
	}

	beacon := func(host string, payload map[string]any, edit ...func(*http.Request)) int {
		body, _ := json.Marshal(payload)
		r := httptest.NewRequest(http.MethodPost, "https://"+host+analytics.EventPath, strings.NewReader(string(body)))
		r.Header.Set("Content-Type", "text/plain;charset=UTF-8")
		r.Header.Set("Sec-Fetch-Site", "same-origin")
		r.Header.Set("User-Agent", chromeUA)
		r.RemoteAddr = "203.0.113.7:51234"
		for _, e := range edit {
			e(r)
		}
		rec := httptest.NewRecorder()
		a.serveApp(rec, r)
		return rec.Code
	}
	view := map[string]any{
		"k": "pageview", "u": "https://shop.acme.example.com/pricing?utm_source=newsletter&utm_medium=email&token=secret",
		"r": "https://www.google.com/search?q=private", "l": "de-CH", "z": "Europe/Zurich", "w": 1512,
	}
	if code := beacon("shop.acme.example.com", view); code != http.StatusNoContent {
		t.Fatalf("beacon answered %d", code)
	}
	_ = beacon("shop.acme.example.com", map[string]any{
		"k": "event", "u": "https://shop.acme.example.com/pricing", "n": "signup", "d": map[string]string{"plan": "pro"},
	})
	// Refused: another site, a bot, an API page, a page behind a login
	// without a session, malformed.
	_ = beacon("shop.acme.example.com", view, func(r *http.Request) { r.Header.Set("Sec-Fetch-Site", "cross-site") })
	_ = beacon("shop.acme.example.com", view, func(r *http.Request) { r.Header.Set("User-Agent", "curl/8.7.1") })
	_ = beacon("shop.acme.example.com", map[string]any{"k": "pageview", "u": "https://shop.acme.example.com/api/x"})
	_ = beacon("shop.acme.example.com", map[string]any{"k": "pageview", "u": "https://elsewhere.example.com/"})
	_ = beacon("private.acme.example.com", map[string]any{"k": "pageview", "u": "https://private.acme.example.com/"})
	_ = beacon("shop.acme.example.com", map[string]any{"k": "wat", "u": "https://shop.acme.example.com/"})

	// With analytics off, the path is the app's.
	body := strings.NewReader(`{}`)
	rec = httptest.NewRecorder()
	a.serveApp(rec, httptest.NewRequest(http.MethodPost, "https://quiet.acme.example.com"+analytics.EventPath, body))
	if got := <-a.seen; got.URL.Path != analytics.EventPath {
		t.Fatalf("an app without analytics should get its own %s, got %s", analytics.EventPath, got.URL.Path)
	}

	events := a.recorded(t)
	if len(events) != 2 {
		t.Fatalf("recorded %d events: %+v", len(events), events)
	}
	pageview, signup := events[0], events[1]
	want := telemetry.WebEvent{
		TimeUnixMilli: pageview.TimeUnixMilli, Space: "acme", App: "shop", Visitor: pageview.Visitor,
		Kind: telemetry.WebKindPageview, Path: "/pricing", Referrer: "google.com", Channel: analytics.ChannelEmail,
		UTMSource: "newsletter", UTMMedium: "email", Browser: "Chrome", OS: "macOS", Device: analytics.DeviceDesktop,
		Country: "CH", Language: "de-CH",
	}
	if pageview.Props != nil || !reflect.DeepEqual(pageview, want) {
		t.Fatalf("page view = %+v\nwant %+v", pageview, want)
	}
	if len(pageview.Visitor) != 64 || signup.Visitor != pageview.Visitor {
		t.Fatalf("one browser is one visitor: %q %q", pageview.Visitor, signup.Visitor)
	}
	if signup.Kind != telemetry.WebKindEvent || signup.Name != "signup" || signup.Props["plan"] != "pro" {
		t.Fatalf("event = %+v", signup)
	}
}
