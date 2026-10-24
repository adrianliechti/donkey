package server

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/adrianliechti/donkey/internal/ai"
	"github.com/adrianliechti/donkey/pkg/runtime"
)

type consoleRuntime struct {
	planRuntime
	connection runtime.ServiceConnection
}

func (r consoleRuntime) ServiceConnection(context.Context, string, string) (runtime.ServiceConnection, error) {
	return r.connection, nil
}
func (consoleRuntime) ListServices(context.Context, string, string) ([]runtime.ServiceBindingInfo, error) {
	panic("opening a console must not list other services or credentials")
}

func TestServiceConsolesRespectExplicitProtocol(t *testing.T) {
	connection := runtime.ServiceConnection{Type: "vendor", Protocol: "custom", Binding: map[string]string{
		"DATABASE_URL": "postgres://u:secret@db/app", "REDIS_URL": "redis://cache", "AWS_ENDPOINT_URL_S3": "https://store", "OPENAI_BASE_URL": "https://model/v1",
	}}
	s := planServer(consoleRuntime{connection: connection}, &ai.Analysis{})
	for _, path := range []string{"databases", "keys", "buckets", "ai/models"} {
		req := httptest.NewRequest("GET", "/v1/spaces/default/services/vendor/"+path, nil)
		asAdmin(t, req)
		rec := httptest.NewRecorder()
		s.routes().ServeHTTP(rec, req)
		if rec.Code != 400 || strings.Contains(rec.Body.String(), "secret") {
			t.Fatalf("%s bypassed protocol or exposed credentials: %d %s", path, rec.Code, rec.Body.String())
		}
	}
}

func TestOpenAIConsoleUsesProviderBindingAndKeepsSessionPrivate(t *testing.T) {
	for _, key := range []string{"provider-secret", ""} {
		t.Run(fmt.Sprintf("key=%t", key != ""), func(t *testing.T) {
			requests := make(chan *http.Request, 2)
			upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				requests <- r.Clone(r.Context())
				w.Header().Set("Content-Type", "text/event-stream")
				w.Header().Set("Set-Cookie", "upstream=private")
				fmt.Fprint(w, "data: [DONE]\n\n")
			}))
			defer upstream.Close()
			s := planServer(consoleRuntime{connection: runtime.ServiceConnection{Type: "model-vendor", Protocol: "openai", Binding: map[string]string{
				"OPENAI_BASE_URL": upstream.URL + "/provider/v1/", "OPENAI_API_KEY": key,
			}}}, &ai.Analysis{})
			for _, call := range []struct{ method, path string }{{"GET", "models"}, {"POST", "chat/completions"}} {
				path := "/v1/spaces/default/services/vendor/ai/" + call.path
				req := httptest.NewRequest(call.method, path, strings.NewReader(`{"model":"test"}`))
				rec := httptest.NewRecorder()
				s.routes().ServeHTTP(rec, req)
				if rec.Code != 401 {
					t.Fatal("console accepted an unauthenticated request")
				}
				select {
				case <-requests:
					t.Fatal("unauthenticated request reached provider")
				default:
				}
				req = httptest.NewRequest(call.method, path, strings.NewReader(`{"model":"test"}`))
				asAdmin(t, req)
				req.Header.Set("Cookie", "session=private")
				req.Header.Set("X-Custom-Credential", "private")
				rec = httptest.NewRecorder()
				s.routes().ServeHTTP(rec, req)
				if rec.Code != 200 || !strings.Contains(rec.Body.String(), "[DONE]") || rec.Header().Get("Set-Cookie") != "" {
					t.Fatalf("proxy response: %d %s", rec.Code, rec.Body.String())
				}
				got := <-requests
				if got.URL.Path != "/provider/v1/"+call.path {
					t.Fatalf("wrong provider path %s", got.URL.Path)
				}
				wantAuth := ""
				if key != "" {
					wantAuth = "Bearer " + key
				}
				if got.Header.Get("Authorization") != wantAuth {
					t.Fatal("proxy sent the wrong credential")
				}
				for _, header := range []string{"Cookie", "X-Access-Token", "X-Custom-Credential"} {
					if got.Header.Get(header) != "" {
						t.Fatalf("leaked %s to provider", header)
					}
				}
			}
		})
	}
}

func TestS3ConsoleSupportsTemporaryProviderCredentials(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("X-Amz-Security-Token") != "session-token" {
			t.Error("temporary credential token missing")
		}
		fmt.Fprint(w, `<?xml version="1.0"?><ListAllMyBucketsResult xmlns="http://s3.amazonaws.com/doc/2006-03-01/"><Buckets><Bucket><Name>provider-bucket</Name><CreationDate>2026-01-01T00:00:00Z</CreationDate></Bucket></Buckets></ListAllMyBucketsResult>`)
	}))
	defer upstream.Close()
	s := planServer(consoleRuntime{connection: runtime.ServiceConnection{Type: "s3-vendor", Protocol: "s3", Binding: map[string]string{
		"AWS_ENDPOINT_URL_S3": upstream.URL, "AWS_ACCESS_KEY_ID": "access", "AWS_SECRET_ACCESS_KEY": "secret", "AWS_SESSION_TOKEN": "session-token", "AWS_REGION": "us-east-1",
	}}}, &ai.Analysis{})
	req := httptest.NewRequest("GET", "/v1/spaces/default/services/vendor/buckets", nil)
	asAdmin(t, req)
	rec := httptest.NewRecorder()
	s.routes().ServeHTTP(rec, req)
	if rec.Code != 200 || !strings.Contains(rec.Body.String(), "provider-bucket") {
		t.Fatalf("S3 provider: %d %s", rec.Code, rec.Body.String())
	}
}

func TestExaConsoleUsesProviderBindingAndKeepsSessionPrivate(t *testing.T) {
	for _, key := range []string{"provider-secret", ""} {
		t.Run(fmt.Sprintf("key=%t", key != ""), func(t *testing.T) {
			requests := make(chan *http.Request, 2)
			upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				requests <- r.Clone(r.Context())
				w.Header().Set("Content-Type", "application/json")
				w.Header().Set("Set-Cookie", "upstream=private")
				fmt.Fprint(w, `{"results":[]}`)
			}))
			defer upstream.Close()
			s := planServer(consoleRuntime{connection: runtime.ServiceConnection{Type: "model-vendor", Protocol: "exa", Binding: map[string]string{
				"EXA_BASE_URL": upstream.URL + "/provider/exa/", "EXA_API_KEY": key,
			}}}, &ai.Analysis{})
			for _, call := range []struct{ method, path string }{{"POST", "search"}, {"POST", "contents"}} {
				path := "/v1/spaces/default/services/vendor/exa/" + call.path
				req := httptest.NewRequest(call.method, path, strings.NewReader(`{"model":"test"}`))
				rec := httptest.NewRecorder()
				s.routes().ServeHTTP(rec, req)
				if rec.Code != 401 {
					t.Fatal("console accepted an unauthenticated request")
				}
				select {
				case <-requests:
					t.Fatal("unauthenticated request reached provider")
				default:
				}
				req = httptest.NewRequest(call.method, path, strings.NewReader(`{"model":"test"}`))
				asAdmin(t, req)
				req.Header.Set("Cookie", "session=private")
				req.Header.Set("X-Custom-Credential", "private")
				rec = httptest.NewRecorder()
				s.routes().ServeHTTP(rec, req)
				if rec.Code != 200 || !strings.Contains(rec.Body.String(), "results") || rec.Header().Get("Set-Cookie") != "" {
					t.Fatalf("proxy response: %d %s", rec.Code, rec.Body.String())
				}
				got := <-requests
				if got.URL.Path != "/provider/exa/"+call.path {
					t.Fatalf("wrong provider path %s", got.URL.Path)
				}
				wantAuth := ""
				if key != "" {
					wantAuth = key
				}
				if got.Header.Get("x-api-key") != wantAuth {
					t.Fatal("proxy sent the wrong credential")
				}
				for _, header := range []string{"Authorization", "Cookie", "X-Access-Token", "X-Custom-Credential"} {
					if got.Header.Get(header) != "" {
						t.Fatalf("leaked %s to provider", header)
					}
				}
			}
		})
	}
}

// A binding URL without a path (an Exa endpoint attached at its root) still
// yields an absolute request path upstream.
func TestServiceConsoleToURLWithoutPath(t *testing.T) {
	var requested string
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requested = r.RequestURI
		fmt.Fprint(w, `{"results":[]}`)
	}))
	defer upstream.Close()
	s := planServer(consoleRuntime{connection: runtime.ServiceConnection{Type: "exa", Protocol: "exa", Binding: map[string]string{
		"EXA_BASE_URL": upstream.URL, "EXA_API_KEY": "key",
	}}}, &ai.Analysis{})
	req := httptest.NewRequest("POST", "/v1/spaces/default/services/exa/exa/search", strings.NewReader(`{}`))
	asAdmin(t, req)
	rec := httptest.NewRecorder()
	s.routes().ServeHTTP(rec, req)
	if rec.Code != 200 || requested != "/search" {
		t.Fatalf("console: %d %s, upstream saw %q", rec.Code, rec.Body, requested)
	}
}

// A provider refusing the service's key is the service's problem, not the
// admin's session: the console must not take it for a 401 and sign them out.
func TestServiceConsoleReportsRefusedCredentialAsBadGateway(t *testing.T) {
	for _, test := range []struct{ protocol, key, path, body, want string }{
		{"openai", "OPENAI_BASE_URL", "ai/models", `{"error":{"message":"Incorrect API key provided","type":"invalid_request_error"}}`, "Incorrect API key provided"},
		{"exa", "EXA_BASE_URL", "exa/search", `{"error":"connect the exa service and use its binding key"}`, "connect the exa service"},
		{"exa", "EXA_BASE_URL", "exa/contents", `not json`, "refused the service's credential"},
	} {
		t.Run(test.path, func(t *testing.T) {
			upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("WWW-Authenticate", "Bearer")
				w.WriteHeader(http.StatusUnauthorized)
				fmt.Fprint(w, test.body)
			}))
			defer upstream.Close()
			s := planServer(consoleRuntime{connection: runtime.ServiceConnection{Type: "vendor", Protocol: test.protocol, Binding: map[string]string{
				test.key: upstream.URL + "/v1",
			}}}, &ai.Analysis{})
			method := "POST"
			if test.path == "ai/models" {
				method = "GET"
			}
			req := httptest.NewRequest(method, "/v1/spaces/default/services/vendor/"+test.path, strings.NewReader(`{}`))
			asAdmin(t, req)
			rec := httptest.NewRecorder()
			s.routes().ServeHTTP(rec, req)
			if rec.Code != http.StatusBadGateway || !strings.Contains(rec.Body.String(), test.want) || rec.Header().Get("WWW-Authenticate") != "" {
				t.Fatalf("console: %d %s %v", rec.Code, rec.Body, rec.Header())
			}
		})
	}
}

// The chat streams: each event reaches the console as the provider sends it,
// not when the answer is complete.
func TestOpenAIConsoleStreamsEventsAsTheyArrive(t *testing.T) {
	release := make(chan struct{})
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		if !strings.Contains(string(body), `"stream_options"`) {
			t.Errorf("request body changed on the way: %s", body)
		}
		w.Header().Set("Content-Type", "text/event-stream")
		fmt.Fprint(w, "data: {\"choices\":[{\"delta\":{\"content\":\"Hel\"}}]}\n\n")
		w.(http.Flusher).Flush()
		<-release
		fmt.Fprint(w, "data: [DONE]\n\n")
	}))
	defer upstream.Close()
	defer close(release)
	s := planServer(consoleRuntime{connection: runtime.ServiceConnection{Type: "model-vendor", Protocol: "openai", Binding: map[string]string{
		"OPENAI_BASE_URL": upstream.URL + "/v1",
	}}}, &ai.Analysis{})
	console := httptest.NewServer(s.routes())
	defer console.Close()
	req, _ := http.NewRequest("POST", console.URL+"/v1/spaces/default/services/vendor/ai/chat/completions",
		strings.NewReader(`{"model":"m","stream":true,"stream_options":{"include_usage":true},"messages":[]}`))
	asAdmin(t, req)
	res, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer res.Body.Close()
	if res.Header.Get("Content-Type") != "text/event-stream" {
		t.Fatalf("content type %q", res.Header.Get("Content-Type"))
	}
	first := make([]byte, 64)
	n, err := res.Body.Read(first)
	if err != nil || !strings.Contains(string(first[:n]), "Hel") {
		t.Fatalf("first event did not arrive on its own: %q %v", first[:n], err)
	}
}
