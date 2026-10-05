package server

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http/httptest"
	"net/url"
	"os"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/adrianliechti/donkey/internal/addons"
	"github.com/adrianliechti/donkey/internal/ai"
	"github.com/adrianliechti/donkey/pkg/runtime"
)

type databaseTestRuntime struct {
	planRuntime
	dsn, protocol string
}

func (r databaseTestRuntime) ServiceBinding(context.Context, string, string, string) (map[string]string, error) {
	return map[string]string{"DATABASE_URL": r.dsn}, nil
}
func (r databaseTestRuntime) ServiceConnection(context.Context, string, string) (runtime.ServiceConnection, error) {
	return runtime.ServiceConnection{Binding: map[string]string{"DATABASE_URL": r.dsn}, Type: "external-vendor", Protocol: r.protocol}, nil
}
func (r databaseTestRuntime) ListServices(context.Context, string, string) ([]runtime.ServiceBindingInfo, error) {
	return []runtime.ServiceBindingInfo{{Name: "db", Type: "external-vendor", Protocol: r.protocol, Keys: []string{"DATABASE_URL"}}}, nil
}
func (r databaseTestRuntime) ServiceAttachments(context.Context, string, string) ([]string, error) {
	return nil, nil
}

func databaseCall(t *testing.T, s *Server, method, suffix string, body any, status int) map[string]any {
	t.Helper()
	data, err := json.Marshal(body)
	if err != nil {
		t.Fatal(err)
	}
	req := httptest.NewRequest(method, "/v1/spaces/default/services/db"+suffix, bytes.NewReader(data))
	asAdmin(t, req)
	rec := httptest.NewRecorder()
	s.routes().ServeHTTP(rec, req)
	if rec.Code != status {
		t.Fatalf("%s %s: status %d, want %d: %s", method, suffix, rec.Code, status, rec.Body.String())
	}
	var out map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatal(err)
	}
	return out
}

func TestDatabaseDialectQuotesAndBindsKeys(t *testing.T) {
	for _, protocol := range []string{"postgres", "mysql"} {
		t.Run(protocol, func(t *testing.T) {
			c := &databaseConn{protocol: protocol, database: "app"}
			var args []any
			where, err := wherePK(c, map[string]string{"id": "1 OR 1=1", "tenant": "a'b"}, []string{"id", "tenant"}, &args)
			if err != nil || strings.Contains(where, "OR 1=1") || !reflect.DeepEqual(args, []any{"1 OR 1=1", "a'b"}) {
				t.Fatalf("unsafe key predicate: %s %v", where, err)
			}
			if _, err := wherePK(c, map[string]string{"id": "1"}, []string{"id", "tenant"}, &args); err == nil {
				t.Fatal("accepted an incomplete composite key")
			}
			if _, err := wherePK(c, nil, nil, &args); err == nil {
				t.Fatal("accepted a table without a primary key")
			}
			if protocol == "mysql" {
				if c.quote("a`b", "order") != "`a``b`.`order`" || where != "`id` = ? AND `tenant` = ?" {
					t.Fatal("incorrect MySQL quoting or parameters")
				}
			} else if c.quote(`a"b`, "order") != `"a""b"."order"` || where != `"id" = $1 AND "tenant" = $2` {
				t.Fatal("incorrect PostgreSQL quoting or parameters")
			}
		})
	}
}

func TestMySQLURLPreservesCredentialsDatabaseAndTLS(t *testing.T) {
	u := &url.URL{Scheme: "mysql", Host: "[::1]:3307", Path: "/app", User: url.UserPassword("a@b", "p@ss:/?#%"), RawQuery: "tls=true&charset=utf8mb4"}
	cfg, err := mysqlURLConfig(u, "other/name")
	if err != nil {
		t.Fatal(err)
	}
	if cfg.User != "a@b" || cfg.Passwd != "p@ss:/?#%" || cfg.DBName != "other/name" || cfg.Addr != "[::1]:3307" || cfg.TLS == nil || cfg.TLS.InsecureSkipVerify {
		t.Fatal("URL options or credentials changed")
	}
	for _, query := range []string{"allowAllFiles=true", "multiStatements=true", "tls=invalid", "tls=true&tls=false", "charset=utf8mb4;DROP"} {
		u.RawQuery = query
		if _, err := mysqlURLConfig(u, "app"); err == nil {
			t.Fatalf("accepted %q", query)
		}
	}
}

func TestExternalDatabaseMetadataAndUnsupportedProtocol(t *testing.T) {
	s := planServer(databaseTestRuntime{dsn: "postgres://u:secret@db/app", protocol: "postgres"}, &ai.Analysis{})
	req := httptest.NewRequest("GET", "/v1/spaces/default/services", nil)
	asAdmin(t, req)
	rec := httptest.NewRecorder()
	s.routes().ServeHTTP(rec, req)
	if rec.Code != 200 || !strings.Contains(rec.Body.String(), `"protocol":"postgres"`) || strings.Contains(rec.Body.String(), "secret") || strings.Contains(rec.Body.String(), "PGHOST") {
		t.Fatalf("external service lost protocol or leaked credentials: %s", rec.Body.String())
	}
	s = planServer(databaseTestRuntime{dsn: "mysql://u:secret@db/app", protocol: "postgres"}, &ai.Analysis{})
	result := databaseCall(t, s, "GET", "/databases", nil, 400)
	if strings.Contains(fmt.Sprint(result), "secret") {
		t.Fatal("validation exposed credentials")
	}
}

// Run against disposable databases or a local addon tunnel. No fixture
// credentials are committed; each run removes only its own unique table.
func TestDatabaseEditorIntegration(t *testing.T) {
	for _, protocol := range []string{addons.ProtocolPostgres, addons.ProtocolMySQL} {
		t.Run(protocol, func(t *testing.T) {
			dsn := os.Getenv("DONKEY_TEST_" + strings.ToUpper(protocol) + "_URL")
			if dsn == "" {
				t.Skip("set DONKEY_TEST_" + strings.ToUpper(protocol) + "_URL to run the database integration test")
			}
			s := planServer(databaseTestRuntime{dsn: dsn, protocol: protocol}, &ai.Analysis{})
			info := databaseCall(t, s, "GET", "/databases", nil, 200)
			db := info["default"].(string)
			if info["protocol"] != protocol {
				t.Fatal("lost protocol")
			}
			prefix := "/databases/" + url.PathEscape(db)
			name := fmt.Sprintf("donkey_editor_%d", time.Now().UnixNano())
			c := &databaseConn{protocol: protocol, database: db}
			table := c.quote(name)
			query := func(statement string) map[string]any {
				return databaseCall(t, s, "POST", prefix+"/query", map[string]string{"sql": statement}, 200)
			}
			query("CREATE TABLE " + table + " (tenant integer NOT NULL, id bigint NOT NULL, label text, PRIMARY KEY (tenant, id))")
			t.Cleanup(func() { databaseCall(t, s, "DELETE", prefix+"/tables/"+name, nil, 200) })
			queryResult := query("INSERT INTO " + table + " (tenant, id, label) VALUES (1, 9007199254740993, 'before')")
			if queryResult["affected"] != float64(1) {
				t.Fatalf("SQL command count: %v", queryResult)
			}
			tables := databaseCall(t, s, "GET", prefix+"/tables", nil, 200)
			if !strings.Contains(fmt.Sprint(tables), name) {
				t.Fatal("table missing from browser")
			}
			path := prefix + "/tables/" + name + "/rows"
			page := databaseCall(t, s, "GET", path+"?order=id&dir=desc", nil, 200)
			if page["total"] != float64(1) || !reflect.DeepEqual(page["key"], []any{"tenant", "id"}) || !strings.Contains(fmt.Sprint(page["rows"]), "9007199254740993") {
				t.Fatalf("bad grid: %v", page)
			}
			key := map[string]string{"tenant": "1", "id": "9007199254740993"}
			edited := databaseCall(t, s, "PUT", path, dbRowRequest{Key: key, Values: map[string]string{"label": "after 'quote' 🫏"}}, 200)
			if edited["affected"] != float64(1) {
				t.Fatal("row not updated")
			}
			result := query("SELECT label FROM " + table)
			if !strings.Contains(fmt.Sprint(result["rows"]), "after 'quote' 🫏") {
				t.Fatal("edit was not saved")
			}
			databaseCall(t, s, "DELETE", path, dbRowRequest{Key: map[string]string{"id": "9007199254740993"}}, 400)
			databaseCall(t, s, "POST", path, dbRowRequest{Values: map[string]string{"tenant": "2", "id": "2", "label": "inserted"}}, 200)
			databaseCall(t, s, "DELETE", path, dbRowRequest{Key: key}, 200)
			page = databaseCall(t, s, "GET", path, nil, 200)
			if page["total"] != float64(1) || !strings.Contains(fmt.Sprint(page["rows"]), "inserted") {
				t.Fatal("row CRUD failed")
			}
			result = query("WITH RECURSIVE seq(n) AS (SELECT 1 UNION ALL SELECT n+1 FROM seq WHERE n<505) SELECT n FROM seq")
			if result["truncated"] != true || len(result["rows"].([]any)) != 500 {
				t.Fatal("query row limit failed")
			}
			databaseCall(t, s, "DELETE", prefix, nil, 400)
		})
	}
}
