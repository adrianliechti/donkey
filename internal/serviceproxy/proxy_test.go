package serviceproxy

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/adrianliechti/donkey/internal/config"
)

func allowCaller(_ context.Context, ip, id string) (bool, error) {
	return id == "erp" && (ip == "192.0.2.1" || ip == "127.0.0.1"), nil
}

func proxyHandler(t *testing.T, service config.ProxyService) http.Handler {
	t.Helper()
	h, err := Handler(config.ProxyServices{service}, allowCaller)
	if err != nil {
		t.Fatal(err)
	}
	return h
}

func TestProxyPreservesRequestsAndRewritesURLs(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		if r.Method != "POST" || string(body) != "payload" || r.RequestURI != "/base/a%2Fb/%25?fixed=yes&x=one%2Ftwo" {
			t.Errorf("upstream request = %s %s %q", r.Method, r.RequestURI, body)
		}
		if r.Host == "app.local" {
			t.Errorf("upstream Host was not rewritten: %q", r.Host)
		}
		if r.Header.Get("Authorization") != "Bearer upstream-token" || r.Referer() != "" || r.Header.Get("Proxy-Authorization") != "" {
			t.Errorf("upstream headers = %v", r.Header)
		}
		w.Header().Set("Location", "/base/next%2Fpage?x=1#anchor")
		http.SetCookie(w, &http.Cookie{Name: "session", Value: "abc", Domain: "enterprise.local", Path: "/base", HttpOnly: true})
		w.WriteHeader(http.StatusTemporaryRedirect)
		w.Write([]byte("result"))
	}))
	defer upstream.Close()
	h := proxyHandler(t, config.ProxyService{Name: "erp", TargetURL: upstream.URL + "/base?fixed=yes"})
	r := httptest.NewRequest("POST", "http://app.local/services/erp/a%2Fb/%25?x=one%2Ftwo", strings.NewReader("payload"))
	r.Header.Set("Authorization", "Bearer upstream-token")
	r.Header.Set("Proxy-Authorization", "Basic do-not-forward")
	r.Header.Set("Referer", "http://app.local/services/erp/")
	w := httptest.NewRecorder()
	h.ServeHTTP(w, r)
	if w.Code != http.StatusTemporaryRedirect || w.Body.String() != "result" {
		t.Fatalf("response = %d %q", w.Code, w.Body.String())
	}
	if got, want := w.Header().Get("Location"), "/services/erp/next%2Fpage?x=1#anchor"; got != want {
		t.Errorf("Location = %q, want %q", got, want)
	}
	cookies := w.Result().Cookies()
	if len(cookies) != 1 || cookies[0].Domain != "" || cookies[0].Path != "/services/erp" || !cookies[0].HttpOnly {
		t.Errorf("cookies = %+v", cookies)
	}
}

func TestProxyBasePathAndRedirects(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/base/" {
			w.Header().Set("Location", "/base")
			w.WriteHeader(http.StatusMovedPermanently)
			return
		}
		if r.URL.Path != "/base" {
			t.Errorf("upstream path = %q", r.URL.Path)
		}
		w.Write([]byte("root"))
	}))
	defer upstream.Close()
	proxy := httptest.NewServer(proxyHandler(t, config.ProxyService{Name: "erp", TargetURL: upstream.URL + "/base"}))
	defer proxy.Close()
	response, err := proxy.Client().Get(proxy.URL + "/services/erp/")
	if err != nil {
		t.Fatal(err)
	}
	defer response.Body.Close()
	body, _ := io.ReadAll(response.Body)
	if response.StatusCode != http.StatusOK || string(body) != "root" {
		t.Errorf("root response = %d %q", response.StatusCode, body)
	}
}

func TestProxyRejectsUnauthorizedAndEscapingRequests(t *testing.T) {
	h := proxyHandler(t, config.ProxyService{Name: "erp", TargetURL: "https://example.com/base"})
	for _, tc := range []struct {
		path string
		code int
	}{
		{"/services/other/", http.StatusNotFound},
		{"/services/erp/../outside", http.StatusBadRequest},
		{"/services/erp/%2e%2e/outside", http.StatusBadRequest},
		{"/services/erp/%252e%252e%252foutside", http.StatusBadRequest},
		{"/services/erp/a%5cb", http.StatusBadRequest},
		{"/services/erp/a%00b", http.StatusBadRequest},
	} {
		t.Run(tc.path, func(t *testing.T) {
			w := httptest.NewRecorder()
			h.ServeHTTP(w, httptest.NewRequest("GET", tc.path, nil))
			if w.Code != tc.code {
				t.Errorf("status = %d, want %d", w.Code, tc.code)
			}
		})
	}
	w := httptest.NewRecorder()
	h.ServeHTTP(w, httptest.NewRequest(http.MethodConnect, "http://example.com/services/erp/", nil))
	if w.Code != http.StatusMethodNotAllowed {
		t.Errorf("CONNECT status = %d", w.Code)
	}
}

func TestRedirectOutsideTargetBaseKeepsItsDestination(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Location", "../login?next=api")
		w.WriteHeader(http.StatusFound)
	}))
	defer upstream.Close()
	h := proxyHandler(t, config.ProxyService{Name: "erp", TargetURL: upstream.URL + "/base"})
	w := httptest.NewRecorder()
	h.ServeHTTP(w, httptest.NewRequest("GET", "/services/erp/items", nil))
	if got, want := w.Header().Get("Location"), upstream.URL+"/login?next=api"; w.Code != http.StatusFound || got != want {
		t.Fatalf("redirect = %d %q, want %q", w.Code, got, want)
	}
}

func TestProxyUsesOutboundHTTPProxy(t *testing.T) {
	proxy := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.String() != "http://enterprise.local/base/items?q=1" || r.Host != "enterprise.local" {
			t.Errorf("forward proxy request = %s Host %s", r.URL, r.Host)
		}
		if r.Header.Get("Proxy-Authorization") != "Basic dXNlcjpwYXNz" {
			t.Errorf("proxy credentials = %q", r.Header.Get("Proxy-Authorization"))
		}
		w.Write([]byte("through proxy"))
	}))
	defer proxy.Close()
	proxyURL, _ := url.Parse(proxy.URL)
	proxyURL.User = url.UserPassword("user", "pass")
	h := proxyHandler(t, config.ProxyService{Name: "erp", TargetURL: "http://enterprise.local/base", ProxyURL: proxyURL.String()})
	w := httptest.NewRecorder()
	h.ServeHTTP(w, httptest.NewRequest("GET", "/services/erp/items?q=1", nil))
	if w.Code != http.StatusOK || w.Body.String() != "through proxy" {
		t.Errorf("response = %d %q", w.Code, w.Body.String())
	}
}

// The client reaches an HTTPS target via an HTTPS CONNECT proxy. Only the
// shared service gateway needs the platform trust store; apps use HTTP.
func TestProxyUsesPlatformTrustForTargetAndHTTPSProxy(t *testing.T) {
	upstream := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		username, password, ok := r.BasicAuth()
		if !ok || username != "api" || password != "password" || r.URL.Path != "/base/items" || r.Header.Get("Proxy-Authorization") != "" {
			t.Errorf("target request = %s %v", r.URL, r.Header)
		}
		w.Write([]byte("trusted"))
	}))
	defer upstream.Close()
	proxy := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodConnect || r.Header.Get("Proxy-Authorization") != "Basic dXNlcjpwYXNz" {
			t.Errorf("CONNECT request = %s %v", r.Method, r.Header)
			http.Error(w, "CONNECT required", http.StatusBadRequest)
			return
		}
		target, err := net.DialTimeout("tcp", r.Host, 3*time.Second)
		if err != nil {
			t.Error(err)
			http.Error(w, "unavailable", http.StatusBadGateway)
			return
		}
		defer target.Close()
		conn, buffered, err := w.(http.Hijacker).Hijack()
		if err != nil {
			t.Error(err)
			return
		}
		defer conn.Close()
		fmt.Fprint(buffered, "HTTP/1.1 200 Connection Established\r\n\r\n")
		buffered.Flush()
		go func() { io.Copy(target, buffered); target.Close() }()
		io.Copy(conn, target)
	}))
	defer proxy.Close()
	proxyURL, _ := url.Parse(proxy.URL)
	proxyURL.User = url.UserPassword("user", "pass")
	targetURL, _ := url.Parse(upstream.URL + "/base")
	targetURL.User = url.UserPassword("api", "password")
	service := config.ProxyService{Name: "erp", TargetURL: targetURL.String(), ProxyURL: proxyURL.String()}
	w := httptest.NewRecorder()
	proxyHandler(t, service).ServeHTTP(w, httptest.NewRequest("GET", "/services/erp/items", nil))
	if w.Code != http.StatusBadGateway {
		t.Fatalf("untrusted certificate status = %d, want 502", w.Code)
	}
	// Emulate the platform's configured roots. Production uses the system
	// bundle through DefaultTransport, without an offering-specific CA API.
	original := http.DefaultTransport
	transport := original.(*http.Transport).Clone()
	roots := x509.NewCertPool()
	roots.AddCert(upstream.Certificate())
	transport.TLSClientConfig = &tls.Config{RootCAs: roots}
	http.DefaultTransport = transport
	t.Cleanup(func() { http.DefaultTransport = original; transport.CloseIdleConnections() })
	w = httptest.NewRecorder()
	proxyHandler(t, service).ServeHTTP(w, httptest.NewRequest("GET", "/services/erp/items", nil))
	if w.Code != http.StatusOK || w.Body.String() != "trusted" {
		t.Errorf("trusted response = %d %q", w.Code, w.Body.String())
	}
}

func TestProxyRejectsSpoofedCallerAndAccessFailure(t *testing.T) {
	h := proxyHandler(t, config.ProxyService{Name: "erp", TargetURL: "https://example.com/base"})
	r := httptest.NewRequest("GET", "/services/erp/", nil)
	r.RemoteAddr = "192.0.2.2:1234"
	r.Header.Set("X-Forwarded-For", "192.0.2.1")
	r.Header.Set("Forwarded", "for=192.0.2.1")
	w := httptest.NewRecorder()
	h.ServeHTTP(w, r)
	if w.Code != http.StatusForbidden {
		t.Fatalf("spoofed caller status = %d", w.Code)
	}
	h, err := Handler(config.ProxyServices{{Name: "erp", TargetURL: "https://example.com"}}, func(context.Context, string, string) (bool, error) { return false, fmt.Errorf("runtime unavailable") })
	if err != nil {
		t.Fatal(err)
	}
	w = httptest.NewRecorder()
	h.ServeHTTP(w, httptest.NewRequest("GET", "/services/erp/", nil))
	if w.Code != http.StatusServiceUnavailable {
		t.Fatalf("identity failure status = %d", w.Code)
	}
}

func TestReloadChangesTargetsAndRefusesInvalidConfig(t *testing.T) {
	target := func(value string) *httptest.Server {
		return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { w.Write([]byte(value)) }))
	}
	one, two := target("one"), target("two")
	defer one.Close()
	defer two.Close()
	data := config.ProxyServices{{Name: "erp", TargetURL: one.URL}}.JSON()
	h := &reloadHandler{read: func() ([]byte, error) { return []byte(data), nil }, access: allowCaller}
	assertResponse := func(code int, body string) {
		t.Helper()
		w := httptest.NewRecorder()
		h.ServeHTTP(w, httptest.NewRequest("GET", "/services/erp/", nil))
		if w.Code != code || (body != "" && w.Body.String() != body) {
			t.Errorf("dynamic response = %d %q, want %d %q", w.Code, w.Body.String(), code, body)
		}
	}
	assertResponse(http.StatusOK, "one")
	data = config.ProxyServices{{Name: "erp", TargetURL: two.URL}}.JSON()
	assertResponse(http.StatusOK, "two")
	valid := data
	data = `[{"name":"erp","targetUrl":"file:///private"}]`
	assertResponse(http.StatusServiceUnavailable, "")
	data = valid
	assertResponse(http.StatusOK, "two")
	// The Docker runtime writes the file as YAML.
	data = "- name: erp\n  targetUrl: " + one.URL + "\n"
	assertResponse(http.StatusOK, "one")
	data = `[]`
	assertResponse(http.StatusNotFound, "")
}
