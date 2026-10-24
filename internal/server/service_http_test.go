package server

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/adrianliechti/donkey/internal/ai"
	"github.com/adrianliechti/donkey/internal/config"
	"github.com/adrianliechti/donkey/internal/identity"
	"github.com/adrianliechti/donkey/internal/serviceproxy"
	"github.com/adrianliechti/donkey/pkg/runtime"
)

func TestHTTPConsoleUsesSharedProxyAndKeepsLoginPrivate(t *testing.T) {
	key := bytes.Repeat([]byte{8}, 32)
	requests := 0
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests++
		body, _ := io.ReadAll(r.Body)
		if r.Method != "POST" || r.URL.RequestURI() != "/services/erp/items%2Fone?x=yes" || string(body) != "payload" {
			t.Errorf("request = %s %s %q", r.Method, r.URL.RequestURI(), body)
		}
		if r.Header.Get("Authorization") != "Bearer app-key" || r.Header.Get("Accept") != "application/json" || r.Header.Get("Cookie") != "" || r.Header.Get("X-Access-Token") != "" {
			t.Errorf("unexpected headers: %v", r.Header)
		}
		var grant struct{ Space, Offering, Method, URI string }
		if err := identity.Unseal(key, "donkey/proxy-test/v1", r.Header.Get(serviceproxy.TestRequestHeader), &grant); err != nil ||
			grant.Space != "default" || grant.Offering != "erp" || grant.Method != r.Method || grant.URI != r.URL.RequestURI() {
			t.Error("request was not scoped to its space, offering, method, and URL")
		}
		w.Header().Set("Location", "http://external.invalid/must-not-follow")
		w.Header().Set("Set-Cookie", "provider=private")
		w.WriteHeader(http.StatusTemporaryRedirect)
		io.WriteString(w, `{"message":"ok"}`)
	}))
	defer upstream.Close()
	server := httpConsoleServer(upstream.URL, key)
	payload := `{"method":"POST","url":"/items%2Fone?x=yes","headers":{"Authorization":"Bearer app-key","Accept":"application/json"},"body":"payload"}`
	r := httptest.NewRequest("POST", "/v1/spaces/default/services/erp/http/request", strings.NewReader(payload))
	w := httptest.NewRecorder()
	server.routes().ServeHTTP(w, r)
	if w.Code != 401 || requests != 0 {
		t.Fatal("unauthenticated console reached the proxy")
	}
	r = httptest.NewRequest("POST", r.URL.String(), strings.NewReader(payload))
	asAdmin(t, r)
	r.Header.Set("Cookie", "admin-session=private")
	w = httptest.NewRecorder()
	server.routes().ServeHTTP(w, r)
	var response struct {
		Status  int
		Body    string
		Headers http.Header
	}
	if err := json.Unmarshal(w.Body.Bytes(), &response); err != nil {
		t.Fatal(err)
	}
	if w.Code != 200 || response.Status != 307 || response.Body != `{"message":"ok"}` || requests != 1 || w.Header().Get("Set-Cookie") != "" {
		t.Fatalf("response = %d %s", w.Code, w.Body.String())
	}
}

func httpConsoleServer(proxyURL string, key []byte) *Server {
	s := planServer(consoleRuntime{connection: runtime.ServiceConnection{Type: "erp", Protocol: "http",
		Binding: map[string]string{"ERP_URL": proxyURL + "/services/erp/"}}}, &ai.Analysis{})
	s.cfg.TelemetryKey = key
	s.cfg.ProxyServices = config.ProxyServices{{Name: "erp", TargetURL: "https://target.invalid"}}
	return s
}

func TestHTTPConsoleRejectsEscapingTargetsAndReservedHeaders(t *testing.T) {
	for _, input := range []string{
		`{"method":"GET","url":"http://other.internal/private"}`,
		`{"method":"GET","url":"//other.internal/private"}`,
		`{"method":"GET","url":"http://proxy.internal/services/other/private"}`,
		`{"method":"GET","url":"/path#fragment"}`,
		`{"method":"CONNECT","url":"/"}`,
		`{"method":"GET","url":"/","headers":{"X-Donkey-Proxy-Test":"forged"}}`,
		`{"method":"GET","url":"/","headers":{"Accept":"ok\r\nInjected: bad"}}`,
		`{"method":"GET","url":"/","headers":{"Host":"private.internal"}}`,
	} {
		s := httpConsoleServer("http://proxy.internal", bytes.Repeat([]byte{8}, 32))
		r := httptest.NewRequest("POST", "/v1/spaces/default/services/erp/http/request", strings.NewReader(input))
		asAdmin(t, r)
		w := httptest.NewRecorder()
		s.routes().ServeHTTP(w, r)
		if w.Code != http.StatusBadRequest {
			t.Errorf("%s: %d %s", input, w.Code, w.Body.String())
		}
	}
}

func TestHTTPConsoleBoundsResponseBody(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		io.WriteString(w, strings.Repeat("x", httpTestBodyLimit+100))
	}))
	defer upstream.Close()
	s := httpConsoleServer(upstream.URL, bytes.Repeat([]byte{8}, 32))
	r := httptest.NewRequest("POST", "/v1/spaces/default/services/erp/http/request", strings.NewReader(`{"method":"GET","url":"/"}`))
	asAdmin(t, r)
	w := httptest.NewRecorder()
	s.routes().ServeHTTP(w, r)
	var response struct {
		Body      string
		Truncated bool
	}
	if err := json.Unmarshal(w.Body.Bytes(), &response); err != nil {
		t.Fatal(err)
	}
	if w.Code != 200 || len(response.Body) != httpTestBodyLimit || !response.Truncated {
		t.Fatal("response was not bounded")
	}
}

// Binary bodies cross the JSON envelope as base64, byte for byte; text stays
// text, even when the size limit cuts it inside a character.
func TestHTTPConsoleCarriesBinaryBodiesAsBase64(t *testing.T) {
	cut := strings.Repeat("a", httpTestBodyLimit-1) + "é"
	for _, test := range []struct {
		name, body, encoding string
		truncated            bool
	}{
		{"text", `{"ok":true}`, "", false},
		{"binary", "\x89PNG\r\n\x1a\n\x00\xff", "base64", false},
		{"text cut inside a character", cut, "", true},
	} {
		t.Run(test.name, func(t *testing.T) {
			upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				io.WriteString(w, test.body)
			}))
			defer upstream.Close()
			s := httpConsoleServer(upstream.URL, bytes.Repeat([]byte{8}, 32))
			r := httptest.NewRequest("POST", "/v1/spaces/default/services/erp/http/request", strings.NewReader(`{"method":"GET","url":"/items?page=2"}`))
			asAdmin(t, r)
			w := httptest.NewRecorder()
			s.routes().ServeHTTP(w, r)
			var response struct {
				Body, Encoding, URL string
				Size                int
				Truncated           bool
			}
			if err := json.Unmarshal(w.Body.Bytes(), &response); err != nil {
				t.Fatal(err)
			}
			if w.Code != 200 || response.Encoding != test.encoding || response.Truncated != test.truncated {
				t.Fatalf("response = %d %+v", w.Code, response)
			}
			if response.URL != upstream.URL+"/services/erp/items?page=2" {
				t.Fatalf("url = %q", response.URL)
			}
			want := min(len(test.body), httpTestBodyLimit)
			if response.Size != want {
				t.Fatalf("size = %d, want %d", response.Size, want)
			}
			if test.encoding == "base64" {
				decoded, err := base64.StdEncoding.DecodeString(response.Body)
				if err != nil || string(decoded) != test.body {
					t.Fatalf("binary body changed: %q", decoded)
				}
			}
		})
	}
}
