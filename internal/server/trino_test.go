package server

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"reflect"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/adrianliechti/donkey/internal/ai"
	"github.com/adrianliechti/donkey/internal/identity"
	"github.com/adrianliechti/donkey/pkg/runtime"
)

// trinoTestServer has auth.scope configured; the gateway's sealed tokens carry
// the IdP access token "entra-<subject>".
func trinoTestServer(endpoint string) *Server {
	s := planServer(consoleRuntime{connection: runtime.ServiceConnection{
		Type: "trino", Protocol: "trino", Binding: map[string]string{"TRINO_URL": endpoint, "TRINO_CATALOG": "hive", "TRINO_SCHEMA": "analytics"},
	}}, &ai.Analysis{})
	s.cfg.OIDCScope = "api://donkey/.default"
	return s
}

func trinoIdentityToken(t *testing.T, subject, accessToken string) string {
	t.Helper()
	tok, err := identity.SealToken(testSessionKey, identity.Identity{
		Subject: subject, Email: subject + "@example.com", Expires: time.Now().Add(time.Hour).Unix(),
		AccessToken: accessToken,
	})
	if err != nil {
		t.Fatal(err)
	}
	return tok
}

func trinoCall(t *testing.T, s *Server, method, suffix, body string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(method, "/v1/spaces/default/services/trino"+suffix, strings.NewReader(body))
	req.Header.Set("X-Access-Token", trinoIdentityToken(t, "u1", "entra-u1"))
	rec := httptest.NewRecorder()
	s.routes().ServeHTTP(rec, req)
	return rec
}

func TestTrinoConsoleForwardsCurrentUserTokenOverHTTPAndHTTPS(t *testing.T) {
	for _, tls := range []bool{false, true} {
		t.Run(fmt.Sprintf("tls=%t", tls), func(t *testing.T) {
			var expectedToken, sealed string
			var calls atomic.Int32
			upstream := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				calls.Add(1)
				if r.Header.Get("Authorization") != "Bearer "+expectedToken {
					t.Error("did not present this user's IdP access token as Bearer")
				}
				if strings.Contains(fmt.Sprint(r.Header), sealed) {
					t.Error("leaked donkey's sealed token to Trino")
				}
				for _, header := range []string{"Cookie", "X-Access-Token", "X-Auth-Request-User", "X-Custom-Credential", "X-Auth-Request-Access-Token"} {
					if r.Header.Get(header) != "" {
						t.Errorf("leaked %s", header)
					}
				}
				if r.URL.Path != "/engine/v1/statement" || r.Method != "POST" {
					t.Errorf("wrong statement endpoint: %s %s", r.Method, r.URL.Path)
				}
				statement, _ := io.ReadAll(r.Body)
				if string(statement) != "SHOW CATALOGS" {
					t.Errorf("unexpected SQL: %s", statement)
				}
				fmt.Fprint(w, `{"columns":[{"name":"Catalog"}],"data":[["hive"]]}`)
			}))
			if tls {
				upstream.StartTLS()
			} else {
				upstream.Start()
			}
			defer upstream.Close()
			previous := trinoHTTPClient
			trinoHTTPClient = upstream.Client()
			trinoHTTPClient.CheckRedirect = previous.CheckRedirect
			defer func() { trinoHTTPClient = previous }()
			s := trinoTestServer(upstream.URL + "/engine")
			for i, subject := range []string{"u1", "ada"} {
				expectedToken = "entra-" + subject
				sealed = trinoIdentityToken(t, subject, "entra-"+subject)
				req := httptest.NewRequest("GET", "/v1/spaces/default/services/trino/databases", nil)
				// Either header carries the sealed identity; the IdP access
				// token inside is the one the gateway minted.
				if i == 0 {
					req.Header.Set("X-Access-Token", sealed)
				} else {
					req.Header.Set("Authorization", "Bearer "+sealed)
				}
				req.Header.Set("Cookie", "session=private")
				req.Header.Set("X-Custom-Credential", "private")
				req.Header.Set("X-Auth-Request-User", "attacker")
				req.Header.Set("X-Auth-Request-Access-Token", "forged")
				rec := httptest.NewRecorder()
				s.routes().ServeHTTP(rec, req)
				if rec.Code != 200 || !strings.Contains(rec.Body.String(), `"databases":[{"name":"hive","size":null}]`) || !strings.Contains(rec.Body.String(), `"default":"hive"`) {
					t.Fatalf("catalogs: %d %s", rec.Code, rec.Body)
				}
			}
			if calls.Load() != 2 {
				t.Fatalf("unexpected calls: %d", calls.Load())
			}
		})
	}
}

// Without an IdP token there is nothing Trino could verify — the request must
// fail with what to do about it, not fall back to donkey's own credential.
func TestTrinoRefusesWithoutIdentityProviderToken(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		t.Error("reached the Trino add-on without an IdP access token")
	}))
	defer upstream.Close()

	unscoped := trinoTestServer(upstream.URL)
	unscoped.cfg.OIDCScope = ""
	if rec := trinoCall(t, unscoped, "GET", "/databases", ""); rec.Code != 400 || !strings.Contains(rec.Body.String(), "auth.scope") {
		t.Fatalf("no auth.scope: %d %s", rec.Code, rec.Body)
	}

	s := trinoTestServer(upstream.URL)
	req := httptest.NewRequest("GET", "/v1/spaces/default/services/trino/databases", nil)
	req.Header.Set("X-Access-Token", trinoIdentityToken(t, "u1", ""))
	rec := httptest.NewRecorder()
	s.routes().ServeHTTP(rec, req)
	if rec.Code != 400 || !strings.Contains(rec.Body.String(), "sign out and in again") {
		t.Fatalf("request without an access token: %d %s", rec.Code, rec.Body)
	}
}

func TestTrinoConsoleQuotesNamesAndSetsQueryContext(t *testing.T) {
	var expectedSQL, expectedCatalog, expectedSchema string
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		statement, _ := io.ReadAll(r.Body)
		if string(statement) != expectedSQL {
			t.Errorf("SQL = %q, want %q", statement, expectedSQL)
		}
		if r.Header.Get("X-Trino-Catalog") != expectedCatalog || r.Header.Get("X-Trino-Schema") != expectedSchema {
			t.Error("lost query context")
		}
		fmt.Fprint(w, `{"columns":[{"name":"name"}],"data":[["events"]]}`)
	}))
	defer upstream.Close()
	s := trinoTestServer(upstream.URL)
	for _, tc := range []struct{ method, path, body, sql, catalog, schema string }{
		{"GET", `/databases/hive%22prod/schemas`, "", `SHOW SCHEMAS FROM "hive""prod"`, `hive"prod`, ""},
		{"GET", `/databases/hive/tables?schema=odd%22schema`, "", `SHOW TABLES FROM "hive"."odd""schema"`, "hive", `odd"schema`},
		{"GET", `/databases/hive/tables/odd%22table/rows?schema=analytics&limit=200`, "", `SELECT * FROM "hive"."analytics"."odd""table" OFFSET 0 LIMIT 201`, "hive", "analytics"},
		{"GET", `/databases/hive/tables/odd%22table?schema=odd%22schema`, "", `SHOW COLUMNS FROM "hive"."odd""schema"."odd""table"`, "hive", `odd"schema`},
		{"GET", `/databases/hive/tables/events/rows?schema=analytics&limit=2&offset=4&order=odd%22column&dir=desc`, "", `SELECT * FROM "hive"."analytics"."events" ORDER BY "odd""column" DESC OFFSET 4 LIMIT 3`, "hive", "analytics"},
		{"POST", "/databases/iceberg/query", `{"sql":"SELECT * FROM events","schema":"reports"}`, `SELECT * FROM events`, "iceberg", "reports"},
		{"POST", "/query", `{"sql":"SHOW CATALOGS","schema":""}`, `SHOW CATALOGS`, "hive", ""},
	} {
		expectedSQL, expectedCatalog, expectedSchema = tc.sql, tc.catalog, tc.schema
		rec := trinoCall(t, s, tc.method, tc.path, tc.body)
		if rec.Code != 200 {
			t.Fatalf("%s: %d %s", tc.path, rec.Code, rec.Body)
		}
	}
	for _, body := range []string{`{}`, `{"sql":" "}`, `{"sql":"SELECT 1","schema":"bad\r\nheader"}`} {
		if rec := trinoCall(t, s, "POST", "/query", body); rec.Code != 400 {
			t.Fatalf("bad input: %d %s", rec.Code, rec.Body)
		}
	}
	if rec := trinoCall(t, s, "GET", "/databases/hive/tables?schema=%00", ""); rec.Code != 400 {
		t.Fatal("accepted an invalid schema")
	}
}

func TestTrinoUsesTheSharedDatabasePageContract(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, `{"columns":[{"name":"id"}],"data":[[1],[2],[3]]}`)
	}))
	defer upstream.Close()
	rec := trinoCall(t, trinoTestServer(upstream.URL), "GET", "/databases/hive/tables/events/rows?schema=analytics&limit=2&offset=4", "")
	var result struct {
		Columns       []string `json:"columns"`
		Rows          [][]any  `json:"rows"`
		Key           []string `json:"key"`
		Total         *int64   `json:"total"`
		Limit, Offset int
		HasMore       bool `json:"hasMore"`
	}
	if rec.Code != 200 {
		t.Fatalf("rows: %d %s", rec.Code, rec.Body)
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &result); err != nil {
		t.Fatal(err)
	}
	if result.Total != nil || len(result.Rows) != 2 || result.Key == nil || len(result.Key) != 0 || result.Limit != 2 || result.Offset != 4 || !result.HasMore {
		t.Fatalf("shared page contract: %+v", result)
	}
}

func TestTrinoDescribesATableInTheSharedStructureContract(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, `{"columns":[{"name":"Column"},{"name":"Type"},{"name":"Extra"},{"name":"Comment"}],
			"data":[["id","bigint","",""],["day","date","partition key","when it happened"]]}`)
	}))
	defer upstream.Close()
	rec := trinoCall(t, trinoTestServer(upstream.URL), "GET", "/databases/hive/tables/events?schema=analytics", "")
	if rec.Code != 200 {
		t.Fatalf("structure: %d %s", rec.Code, rec.Body)
	}
	var result struct {
		Columns []tableColumn `json:"columns"`
		Indexes []tableIndex  `json:"indexes"`
		Kind    string        `json:"kind"`
		Rows    *int64        `json:"rows"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &result); err != nil {
		t.Fatal(err)
	}
	want := []tableColumn{{Name: "id", Type: "bigint"}, {Name: "day", Type: "date", Extra: "partition key", Comment: "when it happened"}}
	if !reflect.DeepEqual(result.Columns, want) || result.Indexes == nil || len(result.Indexes) != 0 || result.Kind != "table" || result.Rows != nil {
		t.Fatalf("structure contract: %+v", result)
	}
}

func TestTrinoDirectProtocolPaginationPreservesValues(t *testing.T) {
	var endpoint string
	var calls atomic.Int32
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		if r.Header.Get("Authorization") != "Bearer test-token" {
			t.Error("missing token on result fetch")
		}
		switch r.URL.Path {
		case "/v1/statement":
			fmt.Fprintf(w, `{"nextUri":%q}`, endpoint+"/v1/statement/queued/1")
		case "/v1/statement/queued/1":
			fmt.Fprintf(w, `{"columns":[{"name":"id"},{"name":"decimal"},{"name":"nullable"},{"name":"nested"}],"data":[[9007199254740993,123.456789123456789,null,{"a":[9007199254740993]}]],"nextUri":%q}`, endpoint+"/v1/statement/executing/2")
		case "/v1/statement/executing/2":
			fmt.Fprint(w, `{"data":[[2,"text",true,[1,null]]],"updateCount":7}`)
		default:
			t.Errorf("unexpected continuation: %s", r.URL.Path)
		}
	}))
	defer upstream.Close()
	endpoint = upstream.URL
	base, _ := url.Parse(endpoint)
	result, err := (&trinoClient{base: base, token: "test-token"}).query(context.Background(), "SELECT values", 500)
	want := [][]any{{"9007199254740993", "123.456789123456789", nil, `{"a":[9007199254740993]}`}, {"2", "text", "true", `[1,null]`}}
	if err != nil || !reflect.DeepEqual(result.Rows, want) || result.Affected != 7 || result.Truncated || calls.Load() != 3 {
		t.Fatalf("result: %+v %v", result, err)
	}
}

func TestTrinoQueryLimitsCancelOutstandingResults(t *testing.T) {
	for _, limit := range []int{200, 500} {
		t.Run(fmt.Sprint(limit), func(t *testing.T) {
			var endpoint string
			var canceled atomic.Bool
			upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.Method == "DELETE" {
					if r.Header.Get("Authorization") != "Bearer token" {
						t.Error("missing cancellation token")
					}
					canceled.Store(true)
					w.WriteHeader(204)
					return
				}
				rows := make([][]int, limit+1)
				for i := range rows {
					rows[i] = []int{i}
				}
				_ = json.NewEncoder(w).Encode(map[string]any{"columns": []map[string]string{{"name": "id"}}, "data": rows, "nextUri": endpoint + "/v1/statement/executing/next"})
			}))
			defer upstream.Close()
			endpoint = upstream.URL
			base, _ := url.Parse(endpoint)
			result, err := (&trinoClient{base: base, token: "token"}).query(context.Background(), "SELECT many", limit)
			if err != nil || len(result.Rows) != limit || !result.Truncated || !canceled.Load() {
				t.Fatalf("limit or cancellation failed: %+v %v", result, err)
			}
		})
	}
}

func TestTrinoBrowserAbortCancelsTheRunningQuery(t *testing.T) {
	var endpoint string
	var canceled atomic.Bool
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer token" {
			t.Error("missing bearer token on query, result fetch or abort cancellation")
		}
		if r.Method == "DELETE" {
			canceled.Store(true)
			w.WriteHeader(204)
			return
		}
		if r.Method == "POST" {
			fmt.Fprintf(w, `{"nextUri":%q}`, endpoint+"/v1/statement/executing/next")
			return
		}
		cancel()
		<-r.Context().Done()
	}))
	defer upstream.Close()
	endpoint = upstream.URL
	base, _ := url.Parse(endpoint)
	_, err := (&trinoClient{base: base, token: "token"}).query(ctx, "SELECT slow", 500)
	if err == nil || !canceled.Load() {
		t.Fatalf("abort left a running query: %v", err)
	}
}

func TestTrinoRefusesForeignContinuationsAndRedirects(t *testing.T) {
	var foreignCalls atomic.Int32
	foreign := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { foreignCalls.Add(1) }))
	defer foreign.Close()
	for _, redirect := range []bool{false, true} {
		upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if redirect {
				http.Redirect(w, r, foreign.URL, 307)
			} else {
				fmt.Fprintf(w, `{"nextUri":%q}`, foreign.URL+"/v1/statement/next")
			}
		}))
		base, _ := url.Parse(upstream.URL)
		_, err := (&trinoClient{base: base, token: "private-token"}).query(context.Background(), "SELECT 1", 500)
		upstream.Close()
		if err == nil || strings.Contains(err.Error(), "private-token") {
			t.Fatalf("accepted redirect or disclosed token: %v", err)
		}
	}
	if foreignCalls.Load() != 0 {
		t.Fatal("credential reached another endpoint")
	}
	base, _ := url.Parse("https://coordinator/engine")
	c := &trinoClient{base: base}
	for _, raw := range []string{"http://coordinator/engine/v1/statement/next", "https://coordinator/elsewhere", "https://coordinator/engine/v1/statement/%252e%252e/secret", "https://user@coordinator/engine/v1/statement/next", "https://coordinator/engine/v1/statement/next#fragment"} {
		if _, err := c.nextURL(raw); err == nil {
			t.Fatalf("accepted continuation %q", raw)
		}
	}
}

func TestTrinoErrorsAndTransientRetries(t *testing.T) {
	for _, tc := range []struct {
		status int
		body   string
		want   string
	}{
		{200, `{"error":{"message":"line 1: unknown column"}}`, "unknown column"},
		{200, "not JSON", "invalid or oversized"},
		{401, "credential details", "rejected your Donkey credentials"},
		{403, "credential details", "rejected your Donkey credentials"},
	} {
		upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(tc.status); fmt.Fprint(w, tc.body) }))
		base, _ := url.Parse(upstream.URL)
		_, err := (&trinoClient{base: base, token: "private-token"}).query(context.Background(), "SELECT bad", 500)
		upstream.Close()
		if err == nil || !strings.Contains(err.Error(), tc.want) || strings.Contains(err.Error(), "credential details") {
			t.Fatalf("unexpected error: %v", err)
		}
	}
	var attempts atomic.Int32
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer token" {
			t.Error("missing bearer token on retry")
		}
		switch attempts.Add(1) {
		case 1:
			w.WriteHeader(503)
		case 2:
			w.Header().Set("Retry-After", "0")
			w.WriteHeader(429)
		default:
			fmt.Fprint(w, `{"updateCount":3}`)
		}
	}))
	defer upstream.Close()
	base, _ := url.Parse(upstream.URL)
	result, err := (&trinoClient{base: base, token: "token"}).query(context.Background(), "INSERT", 500)
	if err != nil || result.Affected != 3 || attempts.Load() != 3 {
		t.Fatalf("retry: %+v %v", result, err)
	}
}

func TestTrinoConsoleRequiresAuthenticationAndProtocol(t *testing.T) {
	var calls atomic.Int32
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { calls.Add(1) }))
	defer upstream.Close()
	s := trinoTestServer(upstream.URL)
	req := httptest.NewRequest("GET", "/v1/spaces/default/services/trino/databases", nil)
	rec := httptest.NewRecorder()
	s.routes().ServeHTTP(rec, req)
	if rec.Code != 401 {
		t.Fatal("accepted an anonymous query")
	}
	for _, request := range []struct{ method, path, body string }{
		{"POST", "/databases", `{"name":"new_database"}`},
		{"DELETE", "/databases/hive", ""},
		{"DELETE", "/databases/hive/tables/events", ""},
		{"POST", "/databases/hive/tables/events/rows", `{"values":{"id":"1"}}`},
		{"PUT", "/databases/hive/tables/events/rows", `{"key":{"id":"1"},"values":{"id":"2"}}`},
		{"DELETE", "/databases/hive/tables/events/rows", `{"key":{"id":"1"}}`},
	} {
		if rec := trinoCall(t, s, request.method, request.path, request.body); rec.Code != 400 {
			t.Fatalf("unsupported mutation: %d %s", rec.Code, rec.Body)
		}
	}
	s.rt = consoleRuntime{connection: runtime.ServiceConnection{Type: "vendor", Protocol: "custom", Binding: map[string]string{"TRINO_URL": upstream.URL}}}
	if rec := trinoCall(t, s, "GET", "/databases", ""); rec.Code != 400 {
		t.Fatal("ignored explicit service protocol")
	}
	if calls.Load() != 0 {
		t.Fatal("unauthorized request reached upstream")
	}
}

// Trino's HTTP tunnel passes its statement API through as the caller: their
// IdP token and the client's session headers, never a typed X-Trino-User or
// donkey's own credentials, and nothing outside /v1/statement.
func TestTrinoTunnel(t *testing.T) {
	var calls atomic.Int32
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		if r.Header.Get("Authorization") != "Bearer entra-u1" {
			t.Error("did not present the caller's IdP access token")
		}
		for _, h := range []string{"X-Trino-User", "X-Trino-Original-User", "X-Trino-Query-Data-Encoding", "X-Access-Token", "Cookie", "X-Auth-Request-User"} {
			if r.Header.Get(h) != "" {
				t.Errorf("leaked %s", h)
			}
		}
		if r.Header.Get("X-Trino-Catalog") != "hive" {
			t.Error("dropped the client's session header")
		}
		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("X-Trino-Set-Schema", "reports")
		w.Header().Set("Set-Cookie", "upstream=1")
		switch r.Method + " " + r.URL.RequestURI() {
		case "POST /engine/v1/statement":
			if statement, _ := io.ReadAll(r.Body); string(statement) != "SELECT 1" {
				t.Errorf("statement = %q", statement)
			}
			fmt.Fprintf(w, `{"nextUri":"http://%s/engine/v1/statement/queued/q1/x/1"}`, r.Host)
		case "GET /engine/v1/statement/queued/q1/x/1?maxWait=1s":
			fmt.Fprint(w, `{"data":[[1]]}`)
		case "DELETE /engine/v1/statement/executing/q1/x/2":
			w.WriteHeader(http.StatusNoContent)
		default:
			t.Errorf("unexpected %s %s", r.Method, r.URL.RequestURI())
		}
	}))
	defer upstream.Close()
	s := trinoTestServer(upstream.URL + "/engine")
	send := func(method, path, body string) *httptest.ResponseRecorder {
		req := httptest.NewRequest(method, "/v1/spaces/default/services/trino/tunnel/"+path, strings.NewReader(body))
		req.Header.Set("X-Access-Token", trinoIdentityToken(t, "u1", "entra-u1"))
		req.Header.Set("X-Trino-User", "someone-else")
		req.Header.Set("X-Trino-Original-User", "someone-else")
		req.Header.Set("X-Trino-Catalog", "hive")
		req.Header.Set("X-Trino-Query-Data-Encoding", "json+zstd,json")
		req.Header.Set("Cookie", "session=private")
		req.Header.Set("X-Auth-Request-User", "forged")
		rec := httptest.NewRecorder()
		s.routes().ServeHTTP(rec, req)
		return rec
	}

	rec := send("POST", "v1/statement", "SELECT 1")
	if rec.Code != 200 || !strings.Contains(rec.Body.String(), "/engine/v1/statement/queued/q1/x/1") ||
		rec.Header().Get("X-Trino-Set-Schema") != "reports" || rec.Header().Get("Set-Cookie") != "" {
		t.Fatalf("start: %d %v %s", rec.Code, rec.Header(), rec.Body)
	}
	if rec := send("GET", "v1/statement/queued/q1/x/1?maxWait=1s", ""); rec.Code != 200 || rec.Body.String() != `{"data":[[1]]}` {
		t.Fatalf("follow: %d %s", rec.Code, rec.Body)
	}
	if rec := send("DELETE", "v1/statement/executing/q1/x/2", ""); rec.Code != 204 {
		t.Fatalf("cancel: %d %s", rec.Code, rec.Body)
	}
	before := calls.Load()
	for _, refused := range []struct{ method, path string }{
		{"GET", "v1/info"}, {"GET", "v1/query"}, {"GET", "ui/"}, {"GET", "v1/statements"},
		{"GET", "v1/statement/../info"}, {"GET", "v1/statement/%2e%2e/info"},
	} {
		if rec := send(refused.method, refused.path, ""); rec.Code < 300 {
			t.Errorf("%s %s passed: %d", refused.method, refused.path, rec.Code)
		}
	}
	if calls.Load() != before {
		t.Fatal("a refused path reached Trino")
	}
}

// A TRINO_URL without a path still reaches the statement API by its absolute
// path: a relative request line is no request the add-on can answer.
func TestTrinoTunnelToURLWithoutPath(t *testing.T) {
	var requested atomic.Value
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requested.Store(r.RequestURI)
		fmt.Fprint(w, `{}`)
	}))
	defer upstream.Close()
	s := trinoTestServer(upstream.URL)
	req := httptest.NewRequest("POST", "/v1/spaces/default/services/trino/tunnel/v1/statement", strings.NewReader("SELECT 1"))
	req.Header.Set("X-Access-Token", trinoIdentityToken(t, "u1", "entra-u1"))
	rec := httptest.NewRecorder()
	s.routes().ServeHTTP(rec, req)
	if rec.Code != 200 || requested.Load() != "/v1/statement" {
		t.Fatalf("tunnel: %d %s, upstream saw %v", rec.Code, rec.Body, requested.Load())
	}
}
