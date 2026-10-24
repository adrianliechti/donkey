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

func dbValue(s string) *string { return &s }

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

func TestDatabaseFilterBindsValuesAndQuotesColumns(t *testing.T) {
	hostile := `x'); DROP TABLE users; --`
	for _, tc := range []struct {
		protocol, column, op, value, want string
		args                              []any
	}{
		{"postgres", "email", "eq", hostile, `"email" = $1`, []any{hostile}},
		{"postgres", `we"ird`, "ne", "1", `"we""ird" <> $1`, []any{"1"}},
		{"postgres", "n", "ge", "5", `"n" >= $1`, []any{"5"}},
		{"postgres", "name", "contains", "50%_off!", `CAST("name" AS text) ILIKE $1 ESCAPE '!'`, []any{"%50!%!_off!!%"}},
		{"postgres", "name", "starts", "a", `CAST("name" AS text) ILIKE $1 ESCAPE '!'`, []any{"a%"}},
		{"postgres", "deleted_at", "null", "ignored", `"deleted_at" IS NULL`, nil},
		{"postgres", "deleted_at", "notnull", "", `"deleted_at" IS NOT NULL`, nil},
		{"mysql", "email", "eq", hostile, "`email` = ?", []any{hostile}},
		{"mysql", "we`ird", "lt", "1", "`we``ird` < ?", []any{"1"}},
		{"mysql", "name", "contains", `back\slash`, "CAST(`name` AS CHAR) LIKE ? ESCAPE '!'", []any{`%back\slash%`}},
	} {
		c := &databaseConn{protocol: tc.protocol, database: "app"}
		var args []any
		got, err := c.filterPredicate(rowFilter{Column: tc.column, Op: tc.op, Value: tc.value}, &args)
		if err != nil || got != tc.want || !reflect.DeepEqual(args, tc.args) {
			t.Errorf("%s %s %s: got %q %v (%v), want %q %v", tc.protocol, tc.column, tc.op, got, args, err, tc.want, tc.args)
		}
		if strings.Contains(got, "DROP") {
			t.Fatalf("a value became SQL: %s", got)
		}
	}
	c := &databaseConn{protocol: "postgres"}
	for _, f := range []rowFilter{{Column: "", Op: "eq"}, {Column: "a\x00b", Op: "eq"}, {Column: "a", Op: "; DROP"}, {Column: "a", Op: "like"}} {
		var args []any
		if got, err := c.filterPredicate(f, &args); err == nil {
			t.Errorf("accepted %+v as %q", f, got)
		}
	}
}

func TestDatabasePageSQLOrdersStablyAndBoundsTheCount(t *testing.T) {
	pg := &databaseConn{protocol: "postgres"}
	page := rowsPage{
		ident: pg.quote("public", "orders"), key: []string{"tenant", "id"},
		filters: []rowFilter{{Column: "status", Op: "eq", Value: "open"}, {Column: "note", Op: "contains", Value: "x"}},
		order:   "id", desc: true, limit: 100, offset: 200,
	}
	selectSQL, countSQL, args, err := pg.pageSQL(page)
	if err != nil {
		t.Fatal(err)
	}
	if want := `SELECT * FROM "public"."orders" WHERE "status" = $1 AND CAST("note" AS text) ILIKE $2 ESCAPE '!' ORDER BY "id" DESC, "tenant" LIMIT 101 OFFSET 200`; selectSQL != want {
		t.Errorf("select:\n got %s\nwant %s", selectSQL, want)
	}
	if want := `SELECT count(*) FROM (SELECT 1 FROM "public"."orders" WHERE "status" = $1 AND CAST("note" AS text) ILIKE $2 ESCAPE '!' LIMIT 50001) AS counted`; countSQL != want {
		t.Errorf("count:\n got %s\nwant %s", countSQL, want)
	}
	if !reflect.DeepEqual(args, []any{"open", "%x%"}) {
		t.Errorf("args: %v", args)
	}

	my := &databaseConn{protocol: "mysql", database: "app"}
	selectSQL, _, args, err = my.pageSQL(rowsPage{ident: my.quote("app", "t"), order: "a`b", limit: 50})
	if err != nil || selectSQL != "SELECT * FROM `app`.`t` ORDER BY `a``b` ASC LIMIT 51 OFFSET 0" || len(args) != 0 {
		t.Errorf("mysql select: %s %v %v", selectSQL, args, err)
	}
	// Without a sort or a key there is no order to keep — and no ORDER BY.
	selectSQL, _, _, _ = my.pageSQL(rowsPage{ident: "`app`.`t`", limit: 10})
	if strings.Contains(selectSQL, "ORDER BY") {
		t.Errorf("invented an order: %s", selectSQL)
	}
	if _, _, _, err := pg.pageSQL(rowsPage{ident: `"t"`, order: "a\x00"}); err == nil {
		t.Error("accepted a sort column with a NUL")
	}
	if _, _, _, err := pg.pageSQL(rowsPage{ident: `"t"`, filters: []rowFilter{{Column: "a", Op: "nope"}}}); err == nil {
		t.Error("accepted an unknown operator")
	}
}

func TestCellRendersDriverValuesAsText(t *testing.T) {
	when := time.Date(2026, 1, 2, 3, 4, 5, 600, time.UTC)
	for _, tc := range []struct {
		in   any
		want any
	}{
		{nil, nil},
		{"text", "text"},
		{[]byte("bytes"), "bytes"},
		{int64(9007199254740993), "9007199254740993"},
		{float64(1e6), "1000000"},
		{float64(0.25), "0.25"},
		{float64(1e21), "1e+21"},
		{float32(1.5), "1.5"},
		{true, "true"},
		{when, "2026-01-02T03:04:05.0000006Z"},
		{map[string]any{"a": []any{1, "b"}}, `{"a":[1,"b"]}`},
	} {
		if got := cell(tc.in); got != tc.want {
			t.Errorf("cell(%#v) = %#v, want %#v", tc.in, got, tc.want)
		}
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
			schemas := databaseCall(t, s, "GET", prefix+"/schemas", nil, 200)
			if len(schemas["schemas"].([]any)) == 0 {
				t.Fatal("database has no browsable schemas")
			}
			name := fmt.Sprintf("donkey_editor_%d", time.Now().UnixNano())
			c := &databaseConn{protocol: protocol, database: db}
			table := c.quote(name)
			query := func(statement string) map[string]any {
				return databaseCall(t, s, "POST", prefix+"/query", map[string]string{"sql": statement}, 200)
			}
			script := func(statements []string, status int) map[string]any {
				return databaseCall(t, s, "POST", prefix+"/query", map[string]any{"statements": statements}, status)
			}
			query("CREATE TABLE " + table + " (tenant integer NOT NULL, id bigint NOT NULL, label text, PRIMARY KEY (tenant, id))")
			t.Cleanup(func() { databaseCall(t, s, "DELETE", prefix+"/tables/"+name, nil, 200) })
			queryResult := query("INSERT INTO " + table + " (tenant, id, label) VALUES (1, 9007199254740993, 'before')")
			if queryResult["affected"] != float64(1) {
				t.Fatalf("SQL command count: %v", queryResult)
			}
			// Every statement in a run must share its session. A failure closes
			// that session, rolling back an unfinished explicit transaction.
			for _, ending := range []string{"ROLLBACK", "COMMIT", "error"} {
				query("UPDATE " + table + " SET label = 'before'")
				batch := []string{"BEGIN", "UPDATE " + table + " SET label = 'changed'"}
				status := 200
				if ending == "error" {
					batch = append(batch, "SELECT * FROM "+c.quote(name+"_missing"), "COMMIT")
					status = 400
				} else {
					batch = append(batch, ending, "SELECT label FROM "+table)
				}
				result := script(batch, status)
				want := "before"
				if ending == "COMMIT" {
					want = "changed"
				}
				if status == 200 && !strings.Contains(fmt.Sprint(result["rows"]), want) {
					t.Fatalf("%s returned wrong last result: %v", ending, result)
				}
				if status == 400 && !strings.Contains(fmt.Sprint(result), "statement 3 of 4") {
					t.Fatalf("missing statement error: %v", result)
				}
				if got := query("SELECT label FROM " + table); !strings.Contains(fmt.Sprint(got["rows"]), want) {
					t.Fatalf("%s did not preserve transaction boundaries: %v", ending, got)
				}
			}
			query("UPDATE " + table + " SET label = 'before'")
			// Reject an invalid batch before executing any part of it.
			script([]string{"UPDATE " + table + " SET label = 'invalid'", " "}, 400)
			if got := query("SELECT label FROM " + table); !strings.Contains(fmt.Sprint(got["rows"]), "before") {
				t.Fatalf("partially executed an invalid batch: %v", got)
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
			if page["count"] != "exact" || page["hasMore"] != false {
				t.Fatalf("count: %v", page)
			}
			filtered := databaseCall(t, s, "GET", path+"?where=label&op=contains&value=FOR", nil, 200)
			if filtered["total"] != float64(1) {
				t.Fatalf("contains filter: %v", filtered)
			}
			filtered = databaseCall(t, s, "GET", path+"?"+url.Values{"where": {"label"}, "op": {"eq"}, "value": {"x' OR '1'='1"}}.Encode(), nil, 200)
			if filtered["total"] != float64(0) {
				t.Fatalf("a filter value became SQL: %v", filtered)
			}
			if protocol == addons.ProtocolPostgres {
				// MySQL quietly reads it as 0; postgres says what is wrong.
				databaseCall(t, s, "GET", path+"?where=id&op=gt&value=not-a-number", nil, 400)
			}
			databaseCall(t, s, "GET", path+"?where=id&op=between&value=1", nil, 400)
			structure := databaseCall(t, s, "GET", prefix+"/tables/"+name, nil, 200)
			columns := structure["columns"].([]any)
			if len(columns) != 3 || columns[0].(map[string]any)["key"] != true || columns[2].(map[string]any)["nullable"] != true ||
				!strings.Contains(fmt.Sprint(structure["indexes"]), "tenant") || structure["kind"] != "table" {
				t.Fatalf("structure: %v", structure)
			}
			databaseCall(t, s, "GET", prefix+"/tables/"+name+"_missing", nil, 400)
			key := map[string]string{"tenant": "1", "id": "9007199254740993"}
			edited := databaseCall(t, s, "PUT", path, dbRowRequest{Key: key, Values: map[string]*string{"label": dbValue("after 'quote' 🫏")}}, 200)
			if edited["affected"] != float64(1) {
				t.Fatal("row not updated")
			}
			result := query("SELECT label FROM " + table)
			if !strings.Contains(fmt.Sprint(result["rows"]), "after 'quote' 🫏") {
				t.Fatal("edit was not saved")
			}
			databaseCall(t, s, "PUT", path, dbRowRequest{Key: key, Values: map[string]*string{"label": nil}}, 200)
			if nulls := databaseCall(t, s, "GET", path+"?where=label&op=null", nil, 200); nulls["total"] != float64(1) {
				t.Fatalf("null was not written: %v", nulls)
			}
			databaseCall(t, s, "DELETE", path, dbRowRequest{Key: map[string]string{"id": "9007199254740993"}}, 400)
			databaseCall(t, s, "POST", path, dbRowRequest{Values: map[string]*string{"tenant": dbValue("2"), "id": dbValue("2"), "label": dbValue("inserted")}}, 200)
			databaseCall(t, s, "DELETE", path, dbRowRequest{Key: key}, 200)
			page = databaseCall(t, s, "GET", path, nil, 200)
			if page["total"] != float64(1) || !strings.Contains(fmt.Sprint(page["rows"]), "inserted") {
				t.Fatal("row CRUD failed")
			}
			if protocol == addons.ProtocolPostgres {
				// Past the cap a count says "at least" — or, unfiltered, what the
				// planner estimates once it has looked at the table.
				query("INSERT INTO " + table + " (tenant, id, label) SELECT 3, g, 'bulk' FROM generate_series(1, 50001) AS g")
				big := databaseCall(t, s, "GET", path+"?where=tenant&op=eq&value=3", nil, 200)
				if big["count"] != "atLeast" || big["total"] != float64(50000) || big["hasMore"] != true {
					t.Fatalf("bounded count: %v", big["count"])
				}
				query("ANALYZE " + table)
				if big = databaseCall(t, s, "GET", path, nil, 200); big["count"] != "estimated" || big["total"].(float64) < 50000 {
					t.Fatalf("estimated count: %v %v", big["count"], big["total"])
				}
			}
			result = query("WITH RECURSIVE seq(n) AS (SELECT 1 UNION ALL SELECT n+1 FROM seq WHERE n<505) SELECT n FROM seq")
			if result["truncated"] != true || len(result["rows"].([]any)) != 500 {
				t.Fatal("query row limit failed")
			}
			databaseCall(t, s, "DELETE", prefix, nil, 400)
		})
	}
}
