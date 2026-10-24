package server

// The console's database browser. donkey-server already sits in the cluster
// and already holds the binding, so it can speak to a space's database
// directly — no tunnel, no exposed port, the admin token is the only door.

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"math"
	"net/http"
	"net/url"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/adrianliechti/donkey/internal/addons"
	"github.com/adrianliechti/donkey/pkg/runtime"
)

// plainIdent is what the create endpoints accept as names — plain lowercase
// identifiers sidestep the quoting swamp entirely.
var plainIdent = regexp.MustCompile(`^[a-z_][a-z0-9_]*$`)

// dbConnect opens a connection to the space service the request names,
// using the binding's own DATABASE_URL — the browser sees exactly the
// database the apps see. db picks one of the service's databases; empty
// means the binding's own.
func (s *Server) dbConnect(ctx context.Context, r *http.Request, db string) (*databaseConn, error) {
	return s.dbConnection(ctx, r, db, false)
}

func (s *Server) dbBrowseConnect(ctx context.Context, r *http.Request, db string) (*databaseConn, error) {
	return s.dbConnection(ctx, r, db, true)
}

func (s *Server) dbConnection(ctx context.Context, r *http.Request, db string, browse bool) (*databaseConn, error) {
	protocols := []string{addons.ProtocolPostgres, addons.ProtocolMySQL}
	if browse {
		protocols = append(protocols, addons.ProtocolTrino)
	}
	service, err := s.serviceConnection(ctx, r, protocols...)
	if err != nil {
		return nil, err
	}
	if service.Protocol == addons.ProtocolTrino {
		token, err := s.userToken(ctx)
		if err != nil {
			return nil, err
		}
		client := newTrinoClient(service.Binding, db, token)
		return &databaseConn{protocol: service.Protocol, database: client.catalog, trino: client}, nil
	}
	conn, err := connectDatabase(ctx, service.Protocol, service.Binding["DATABASE_URL"], db)
	if err != nil {
		return nil, fmt.Errorf("%s is not answering: %w", r.PathValue("service"), err)
	}
	return conn, nil
}

// handleDbDatabases lists the service's databases — templates stay hidden,
// they are postgres' own scaffolding.
func (s *Server) handleDbDatabases(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := context.WithTimeout(r.Context(), databaseRequestTimeout)
	defer cancel()
	conn, err := s.dbBrowseConnect(ctx, r, "")
	if err != nil {
		writeErr(w, statusFor(err), err)
		return
	}
	defer conn.Close(ctx)
	if conn.trino != nil {
		conn.trino.serveDatabase(w, r, "databases")
		return
	}

	// Only databases the binding's user may connect to: the rest could not be
	// browsed, and sizing them would fail the whole list.
	query := `SELECT datname, pg_database_size(datname) FROM pg_database
		WHERE NOT datistemplate AND has_database_privilege(datname, 'CONNECT') ORDER BY datname`
	if conn.protocol == addons.ProtocolMySQL {
		query = `SELECT s.SCHEMA_NAME, COALESCE(SUM(t.DATA_LENGTH + t.INDEX_LENGTH), 0)
		FROM information_schema.SCHEMATA s LEFT JOIN information_schema.TABLES t ON t.TABLE_SCHEMA = s.SCHEMA_NAME
		WHERE s.SCHEMA_NAME NOT IN ('information_schema', 'mysql', 'performance_schema', 'sys')
		GROUP BY s.SCHEMA_NAME ORDER BY s.SCHEMA_NAME`
	}
	rows, err := conn.Query(ctx, query)
	if err != nil {
		writeErr(w, http.StatusBadGateway, err)
		return
	}
	defer rows.Close()
	type database struct {
		Name string `json:"name"`
		Size int64  `json:"size"`
	}
	out := []database{}
	for rows.Next() {
		var d database
		if err := rows.Scan(&d.Name, &d.Size); err != nil {
			writeErr(w, http.StatusBadGateway, err)
			return
		}
		out = append(out, d)
	}
	if err := rows.Err(); err != nil {
		writeErr(w, http.StatusBadGateway, err)
		return
	}
	rows.Close()
	// The binding's own database is the one apps see — the browser opens on it.
	var current string
	if err := conn.QueryRow(ctx, conn.currentDatabaseSQL()).Scan(&current); err != nil {
		writeErr(w, http.StatusBadGateway, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"databases": out, "default": current, "protocol": conn.protocol})
}

// handleDbSchemas exposes schema navigation for every database protocol.
func (s *Server) handleDbSchemas(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := context.WithTimeout(r.Context(), databaseRequestTimeout)
	defer cancel()
	conn, err := s.dbBrowseConnect(ctx, r, r.PathValue("db"))
	if err != nil {
		writeErr(w, statusFor(err), err)
		return
	}
	defer conn.Close(ctx)
	if conn.trino != nil {
		conn.trino.serveDatabase(w, r, "schemas")
		return
	}
	query := `SELECT schema_name FROM information_schema.schemata WHERE schema_name NOT IN ('pg_catalog', 'information_schema') AND schema_name NOT LIKE 'pg_toast%' ORDER BY schema_name`
	defaultSchema := "public"
	if conn.protocol == addons.ProtocolMySQL {
		query, defaultSchema = "SELECT DATABASE()", conn.database
	}
	rows, err := conn.Query(ctx, query)
	if err != nil {
		writeErr(w, http.StatusBadGateway, err)
		return
	}
	defer rows.Close()
	names := []string{}
	for rows.Next() {
		var name string
		if err := rows.Scan(&name); err != nil {
			writeErr(w, http.StatusBadGateway, err)
			return
		}
		names = append(names, name)
	}
	if err := rows.Err(); err != nil {
		writeErr(w, http.StatusBadGateway, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"schemas": names, "default": defaultSchema})
}

// handleDbCreateDatabase makes a database on the service. Plain identifiers
// only — the escape hatch for fancier names is the SQL view.
func (s *Server) handleDbCreateDatabase(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := context.WithTimeout(r.Context(), databaseRequestTimeout)
	defer cancel()
	var req struct {
		Name string `json:"name"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeErr(w, http.StatusBadRequest, err)
		return
	}
	if !plainIdent.MatchString(req.Name) {
		writeErr(w, http.StatusBadRequest, fmt.Errorf("%q won't do as a database name — lowercase letters, digits and underscores", req.Name))
		return
	}
	conn, err := s.dbConnect(ctx, r, "")
	if err != nil {
		writeErr(w, statusFor(err), err)
		return
	}
	defer conn.Close(ctx)
	if _, err := conn.Exec(ctx, "CREATE DATABASE "+conn.quote(req.Name)); err != nil {
		writeErr(w, http.StatusBadRequest, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "name": req.Name})
}

// handleDbDropDatabase removes a database — except the binding's own, which
// the connected apps depend on.
func (s *Server) handleDbDropDatabase(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := context.WithTimeout(r.Context(), databaseRequestTimeout)
	defer cancel()
	db := r.PathValue("db")
	conn, err := s.dbConnect(ctx, r, "")
	if err != nil {
		writeErr(w, statusFor(err), err)
		return
	}
	defer conn.Close(ctx)
	var current string
	if err := conn.QueryRow(ctx, conn.currentDatabaseSQL()).Scan(&current); err != nil {
		writeErr(w, http.StatusBadGateway, err)
		return
	}
	if db == current {
		writeErr(w, http.StatusBadRequest, fmt.Errorf("%s is the service's own database — the apps' bindings point at it, so it stays", db))
		return
	}
	// FORCE kicks lingering connections; without it, one idle session would
	// make the drop flake.
	query := "DROP DATABASE " + conn.quote(db)
	if conn.protocol == addons.ProtocolPostgres {
		query += " WITH (FORCE)"
	}
	if _, err := conn.Exec(ctx, query); err != nil {
		writeErr(w, http.StatusBadRequest, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true})
}

// handleDbDropTable removes one table, dependencies untouched — an error
// about them is better than a silent cascade.
func (s *Server) handleDbDropTable(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := context.WithTimeout(r.Context(), databaseRequestTimeout)
	defer cancel()
	conn, err := s.dbConnect(ctx, r, r.PathValue("db"))
	if err != nil {
		writeErr(w, statusFor(err), err)
		return
	}
	defer conn.Close(ctx)
	ident, err := conn.tableIdent(r.URL.Query().Get("schema"), r.PathValue("table"))
	if err != nil {
		writeErr(w, statusFor(err), err)
		return
	}
	if _, err := conn.Exec(ctx, "DROP TABLE "+ident); err != nil {
		writeErr(w, http.StatusBadRequest, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true})
}

// handleDbTables lists the user's tables and views — system schemas and
// partitions are the server's business, not the browser's.
func (s *Server) handleDbTables(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := context.WithTimeout(r.Context(), databaseRequestTimeout)
	defer cancel()
	conn, err := s.dbBrowseConnect(ctx, r, r.PathValue("db"))
	if err != nil {
		writeErr(w, statusFor(err), err)
		return
	}
	defer conn.Close(ctx)
	if conn.trino != nil {
		conn.trino.serveDatabase(w, r, "tables")
		return
	}

	// Rows is the planner's estimate — free, and honest enough for a
	// sidebar. Never analyzed (reltuples < 0) and views have none.
	query := `
		SELECT n.nspname, c.relname,
		  CASE WHEN c.relkind IN ('v', 'm') THEN 'view' ELSE 'table' END,
		  CASE WHEN c.reltuples < 0 OR c.relkind = 'v' THEN NULL ELSE c.reltuples::bigint END
		FROM pg_class c
		JOIN pg_namespace n ON n.oid = c.relnamespace
		WHERE c.relkind IN ('r', 'p', 'v', 'm', 'f') AND NOT c.relispartition
		  AND n.nspname NOT IN ('pg_catalog', 'information_schema') AND n.nspname NOT LIKE 'pg_toast%'
		ORDER BY 1, 2`
	var args []any
	if conn.protocol == addons.ProtocolMySQL {
		query = `SELECT TABLE_SCHEMA, TABLE_NAME, CASE WHEN TABLE_TYPE = 'VIEW' THEN 'view' ELSE 'table' END, TABLE_ROWS
		FROM information_schema.TABLES
		WHERE TABLE_SCHEMA = ? AND TABLE_TYPE IN ('BASE TABLE', 'VIEW', 'SYSTEM VERSIONED') ORDER BY TABLE_NAME`
		args = []any{conn.database}
	}
	if schema := r.URL.Query().Get("schema"); schema != "" {
		if conn.protocol == addons.ProtocolMySQL && schema != conn.database {
			writeErr(w, http.StatusBadRequest, fmt.Errorf("the schema must match the selected MySQL database"))
			return
		}
		if conn.protocol == addons.ProtocolPostgres {
			query = strings.Replace(query, "ORDER BY 1, 2", "AND n.nspname = $1 ORDER BY 1, 2", 1)
			args = []any{schema}
		}
	}
	rows, err := conn.Query(ctx, query, args...)
	if err != nil {
		writeErr(w, http.StatusBadGateway, err)
		return
	}
	defer rows.Close()
	type table struct {
		Schema string `json:"schema"`
		Name   string `json:"name"`
		Kind   string `json:"kind"`
		Rows   *int64 `json:"rows"`
	}
	out := []table{}
	for rows.Next() {
		var t table
		var estimate sql.NullInt64
		if err := rows.Scan(&t.Schema, &t.Name, &t.Kind, &estimate); err != nil {
			writeErr(w, http.StatusBadGateway, err)
			return
		}
		t.Rows = nullInt(estimate)
		out = append(out, t)
	}
	if err := rows.Err(); err != nil {
		writeErr(w, http.StatusBadGateway, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"tables": out})
}

func nullInt(v sql.NullInt64) *int64 {
	if !v.Valid {
		return nil
	}
	return &v.Int64
}

func nullString(v sql.NullString) *string {
	if !v.Valid {
		return nil
	}
	return &v.String
}

// tableColumn and tableIndex describe a table's structure the same way for
// every dialect; what a dialect does not know stays null.
type tableColumn struct {
	Name     string  `json:"name"`
	Type     string  `json:"type"`
	Nullable *bool   `json:"nullable"`
	Default  *string `json:"default"`
	Key      bool    `json:"key"`
	// Extra is how the column fills itself: identity, auto_increment,
	// generated.
	Extra   string `json:"extra,omitempty"`
	Comment string `json:"comment,omitempty"`
}

type tableIndex struct {
	Name       string   `json:"name"`
	Columns    []string `json:"columns"`
	Unique     bool     `json:"unique"`
	Primary    bool     `json:"primary"`
	Definition string   `json:"definition,omitempty"`
}

// handleDbTable describes one table: its columns, indexes, and what the
// catalog knows about its size. The grid reads the column types from here,
// so it is cheap — catalog lookups only, never a scan.
func (s *Server) handleDbTable(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := context.WithTimeout(r.Context(), databaseRequestTimeout)
	defer cancel()
	conn, err := s.dbBrowseConnect(ctx, r, r.PathValue("db"))
	if err != nil {
		writeErr(w, statusFor(err), err)
		return
	}
	defer conn.Close(ctx)
	if conn.trino != nil {
		conn.trino.serveDatabase(w, r, "table")
		return
	}
	schema, table := r.URL.Query().Get("schema"), r.PathValue("table")
	stats, err := conn.tableStats(ctx, schema, table)
	if err != nil {
		writeErr(w, statusFor(err), err)
		return
	}
	key, err := primaryKey(ctx, conn, schema, table)
	if err != nil {
		writeErr(w, http.StatusBadGateway, err)
		return
	}
	columns, err := conn.tableColumns(ctx, schema, table, key)
	if err != nil {
		writeErr(w, http.StatusBadGateway, err)
		return
	}
	indexes, err := conn.tableIndexes(ctx, schema, table)
	if err != nil {
		writeErr(w, http.StatusBadGateway, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"columns": columns, "indexes": indexes, "kind": stats.kind, "rows": stats.rows, "size": stats.size,
	})
}

type tableStats struct {
	kind       string
	rows, size *int64
}

// tableStats is the catalog's word on a table: whether it is one, its
// estimated rows and its size on disk. A missing table is the caller's
// mistake, not the database's.
func (c *databaseConn) tableStats(ctx context.Context, schema, table string) (tableStats, error) {
	ident, err := c.tableIdent(schema, table)
	if err != nil {
		return tableStats{}, err
	}
	query := `SELECT CASE WHEN c.relkind IN ('v', 'm') THEN 'view' ELSE 'table' END,
		CASE WHEN c.reltuples < 0 OR c.relkind = 'v' THEN NULL ELSE c.reltuples::bigint END,
		CASE WHEN c.relkind IN ('r', 'm') THEN pg_total_relation_size(c.oid) END
		FROM pg_class c WHERE c.oid = to_regclass($1)`
	args := []any{ident}
	if c.protocol == addons.ProtocolMySQL {
		if schema == "" {
			schema = c.database
		}
		query = `SELECT CASE WHEN TABLE_TYPE = 'VIEW' THEN 'view' ELSE 'table' END, TABLE_ROWS, DATA_LENGTH + INDEX_LENGTH
		FROM information_schema.TABLES WHERE TABLE_SCHEMA = ? AND TABLE_NAME = ?`
		args = []any{schema, table}
	}
	var out tableStats
	var rows, size sql.NullInt64
	if err := c.QueryRow(ctx, query, args...).Scan(&out.kind, &rows, &size); err != nil {
		if errors.Is(err, pgx.ErrNoRows) || errors.Is(err, sql.ErrNoRows) {
			return tableStats{}, runtime.UserError{Err: fmt.Errorf("there is no table %s", table)}
		}
		return tableStats{}, err
	}
	out.rows, out.size = nullInt(rows), nullInt(size)
	return out, nil
}

func (c *databaseConn) tableColumns(ctx context.Context, schema, table string, key []string) ([]tableColumn, error) {
	ident, err := c.tableIdent(schema, table)
	if err != nil {
		return nil, err
	}
	query := `SELECT a.attname, format_type(a.atttypid, a.atttypmod), NOT a.attnotnull, pg_get_expr(d.adbin, d.adrelid),
		CASE WHEN a.attidentity = 'a' THEN 'identity always' WHEN a.attidentity = 'd' THEN 'identity'
		     WHEN a.attgenerated <> '' THEN 'generated' ELSE '' END,
		COALESCE(col_description(a.attrelid, a.attnum), '')
		FROM pg_attribute a
		LEFT JOIN pg_attrdef d ON d.adrelid = a.attrelid AND d.adnum = a.attnum
		WHERE a.attrelid = $1::regclass AND a.attnum > 0 AND NOT a.attisdropped
		ORDER BY a.attnum`
	args := []any{ident}
	if c.protocol == addons.ProtocolMySQL {
		if schema == "" {
			schema = c.database
		}
		query = `SELECT COLUMN_NAME, COLUMN_TYPE, IS_NULLABLE = 'YES', COLUMN_DEFAULT, EXTRA, COLUMN_COMMENT
		FROM information_schema.COLUMNS WHERE TABLE_SCHEMA = ? AND TABLE_NAME = ? ORDER BY ORDINAL_POSITION`
		args = []any{schema, table}
	}
	rows, err := c.Query(ctx, query, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []tableColumn{}
	for rows.Next() {
		var col tableColumn
		var nullable bool
		var def sql.NullString
		if err := rows.Scan(&col.Name, &col.Type, &nullable, &def, &col.Extra, &col.Comment); err != nil {
			return nil, err
		}
		col.Nullable, col.Default, col.Key = &nullable, nullString(def), slices.Contains(key, col.Name)
		out = append(out, col)
	}
	return out, rows.Err()
}

func (c *databaseConn) tableIndexes(ctx context.Context, schema, table string) ([]tableIndex, error) {
	ident, err := c.tableIdent(schema, table)
	if err != nil {
		return nil, err
	}
	out := []tableIndex{}
	if c.protocol == addons.ProtocolPostgres {
		rows, err := c.Query(ctx, `SELECT i.relname, ix.indisunique, ix.indisprimary,
			ARRAY(SELECT pg_get_indexdef(ix.indexrelid, k, true) FROM generate_series(1, ix.indnkeyatts) AS k ORDER BY k),
			pg_get_indexdef(ix.indexrelid)
			FROM pg_index ix JOIN pg_class i ON i.oid = ix.indexrelid
			WHERE ix.indrelid = $1::regclass
			ORDER BY ix.indisprimary DESC, i.relname`, ident)
		if err != nil {
			return nil, err
		}
		defer rows.Close()
		for rows.Next() {
			var index tableIndex
			if err := rows.Scan(&index.Name, &index.Unique, &index.Primary, &index.Columns, &index.Definition); err != nil {
				return nil, err
			}
			out = append(out, index)
		}
		return out, rows.Err()
	}
	if schema == "" {
		schema = c.database
	}
	rows, err := c.Query(ctx, `SELECT INDEX_NAME, NON_UNIQUE, COLUMN_NAME FROM information_schema.STATISTICS
		WHERE TABLE_SCHEMA = ? AND TABLE_NAME = ? ORDER BY INDEX_NAME = 'PRIMARY' DESC, INDEX_NAME, SEQ_IN_INDEX`, schema, table)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	// One row per indexed column; consecutive rows of one name are one index.
	for rows.Next() {
		var name string
		var nonUnique int64
		var column sql.NullString
		if err := rows.Scan(&name, &nonUnique, &column); err != nil {
			return nil, err
		}
		if len(out) == 0 || out[len(out)-1].Name != name {
			out = append(out, tableIndex{Name: name, Unique: nonUnique == 0, Primary: name == "PRIMARY"})
		}
		part := "(expression)"
		if column.Valid {
			part = column.String
		}
		out[len(out)-1].Columns = append(out[len(out)-1].Columns, part)
	}
	return out, rows.Err()
}

// primaryKey names the columns that safely identify a row for grid edits.
func primaryKey(ctx context.Context, conn *databaseConn, schema, table string) ([]string, error) {
	ident, err := conn.tableIdent(schema, table)
	if err != nil {
		return nil, err
	}
	query := `SELECT a.attname FROM pg_index i
	JOIN pg_attribute a ON a.attrelid = i.indrelid AND a.attnum = ANY(i.indkey)
	WHERE i.indrelid = $1::regclass AND i.indisprimary ORDER BY array_position(i.indkey, a.attnum)`
	args := []any{ident}
	if conn.protocol == addons.ProtocolMySQL {
		if schema == "" {
			schema = conn.database
		}
		query = `SELECT COLUMN_NAME FROM information_schema.KEY_COLUMN_USAGE
		WHERE TABLE_SCHEMA = ? AND TABLE_NAME = ? AND CONSTRAINT_NAME = 'PRIMARY' ORDER BY ORDINAL_POSITION`
		args = []any{schema, table}
	}
	rows, err := conn.Query(ctx, query, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var col string
		if err := rows.Scan(&col); err != nil {
			return nil, err
		}
		out = append(out, col)
	}
	return out, rows.Err()
}

// cell turns a driver value into something JSON (and an <input>) can
// carry: strings and null, nothing else. Postgres rows arrive as text
// already (QueryText); this is for MySQL's driver values and Trino's.
func cell(v any) any {
	switch t := v.(type) {
	case nil:
		return nil
	case string:
		return t
	case []byte:
		return string(t)
	case time.Time:
		return t.Format(time.RFC3339Nano)
	case float64:
		return formatFloat(t, 64)
	case float32:
		return formatFloat(float64(t), 32)
	case map[string]any, []any:
		encoded, err := json.Marshal(t)
		if err != nil {
			return fmt.Sprint(t)
		}
		return string(encoded)
	default:
		return fmt.Sprint(t)
	}
}

// formatFloat writes a float the way a person reads one — 1000000, not
// 1e+06 — and switches to an exponent only where JavaScript would.
func formatFloat(f float64, bits int) string {
	if a := math.Abs(f); a != 0 && (a >= 1e21 || a < 1e-6) {
		return strconv.FormatFloat(f, 'g', -1, bits)
	}
	return strconv.FormatFloat(f, 'f', -1, bits)
}

func collectRows(rows *databaseRows) (columns []string, data [][]any, err error) {
	columns = append([]string{}, rows.columns...)
	data = [][]any{}
	for rows.Next() {
		row, err := rows.Cells()
		if err != nil {
			return nil, nil, err
		}
		data = append(data, row)
	}
	return columns, data, rows.Err()
}

// rowFilter narrows a page to the rows whose column compares to a value.
// The column is quoted and the value always bound as a parameter — neither
// ever becomes SQL text.
type rowFilter struct {
	Column, Op, Value string
}

var comparisons = map[string]string{"eq": "=", "ne": "<>", "lt": "<", "le": "<=", "gt": ">", "ge": ">="}

// likeEscape makes a value match itself inside a LIKE pattern. The escape
// character is '!', not the backslash: MySQL reads backslashes in string
// literals (unless NO_BACKSLASH_ESCAPES), so ESCAPE '\' means different
// things on different servers, and '!' means one.
func likeEscape(value string) string {
	return strings.NewReplacer("!", "!!", "%", "!%", "_", "!_").Replace(value)
}

func validIdent(name string) bool {
	return name != "" && !strings.ContainsRune(name, '\x00')
}

func (c *databaseConn) filterPredicate(f rowFilter, args *[]any) (string, error) {
	if !validIdent(f.Column) {
		return "", runtime.UserError{Err: fmt.Errorf("the filter needs a column")}
	}
	column := c.quote(f.Column)
	switch f.Op {
	case "null":
		return column + " IS NULL", nil
	case "notnull":
		return column + " IS NOT NULL", nil
	case "contains", "starts":
		pattern := likeEscape(f.Value) + "%"
		if f.Op == "contains" {
			pattern = "%" + pattern
		}
		*args = append(*args, pattern)
		// Any column can be searched as text; postgres' ILIKE ignores case
		// as MySQL's usual collations already do.
		if c.protocol == addons.ProtocolMySQL {
			return "CAST(" + column + " AS CHAR) LIKE " + c.placeholder(len(*args)) + " ESCAPE '!'", nil
		}
		return "CAST(" + column + " AS text) ILIKE " + c.placeholder(len(*args)) + " ESCAPE '!'", nil
	}
	op, ok := comparisons[f.Op]
	if !ok {
		return "", runtime.UserError{Err: fmt.Errorf("%q is not a filter operator", f.Op)}
	}
	// The value is text; the database casts it to the column's type, and
	// says so when it can't.
	*args = append(*args, f.Value)
	return column + " " + op + " " + c.placeholder(len(*args)), nil
}

// countCap bounds how far a page counts: exactly up to here, beyond it the
// planner's estimate or "more than". count(*) of a large table is a full
// scan, and a number in a footer is not worth one.
const countCap = 50_000

// rowsPage is one page of a table as the grid asks for it.
type rowsPage struct {
	ident         string
	key           []string
	filters       []rowFilter
	order         string
	desc          bool
	limit, offset int
}

// pageSQL builds the page's SELECT and its bounded count. Both share the
// filter's arguments. The SELECT asks for one row more than the page, which
// is how the grid learns there is a next page without counting to it.
func (c *databaseConn) pageSQL(p rowsPage) (selectSQL, countSQL string, args []any, err error) {
	var where []string
	for _, f := range p.filters {
		predicate, err := c.filterPredicate(f, &args)
		if err != nil {
			return "", "", nil, err
		}
		where = append(where, predicate)
	}
	clause := ""
	if len(where) > 0 {
		clause = " WHERE " + strings.Join(where, " AND ")
	}
	// The key columns always follow the sort column: stable paging needs a
	// stable order, and a sort column with duplicates alone would shuffle
	// pages.
	var ordering []string
	if p.order != "" {
		if !validIdent(p.order) {
			return "", "", nil, runtime.UserError{Err: fmt.Errorf("invalid sort column")}
		}
		dir := " ASC"
		if p.desc {
			dir = " DESC"
		}
		ordering = append(ordering, c.quote(p.order)+dir)
	}
	for _, k := range p.key {
		if k != p.order {
			ordering = append(ordering, c.quote(k))
		}
	}
	order := ""
	if len(ordering) > 0 {
		order = " ORDER BY " + strings.Join(ordering, ", ")
	}
	selectSQL = fmt.Sprintf("SELECT * FROM %s%s%s LIMIT %d OFFSET %d", p.ident, clause, order, p.limit+1, p.offset)
	countSQL = fmt.Sprintf("SELECT count(*) FROM (SELECT 1 FROM %s%s LIMIT %d) AS counted", p.ident, clause, countCap+1)
	return selectSQL, countSQL, args, nil
}

// handleDbRows serves one page of a table — sorted, filtered — with the key
// columns the grid needs to edit it, and as much of a count as is cheap.
func (s *Server) handleDbRows(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := context.WithTimeout(r.Context(), databaseRequestTimeout)
	defer cancel()
	conn, err := s.dbBrowseConnect(ctx, r, r.PathValue("db"))
	if err != nil {
		writeErr(w, statusFor(err), err)
		return
	}
	defer conn.Close(ctx)
	if conn.trino != nil {
		conn.trino.serveDatabase(w, r, "rows")
		return
	}

	q := r.URL.Query()
	schema, table := q.Get("schema"), r.PathValue("table")
	ident, err := conn.tableIdent(schema, table)
	if err != nil {
		writeErr(w, statusFor(err), err)
		return
	}
	limit, offset := pageBounds(q)
	key, err := primaryKey(ctx, conn, schema, table)
	if err != nil {
		writeErr(w, http.StatusBadRequest, err)
		return
	}
	page := rowsPage{ident: ident, key: key, order: q.Get("order"), desc: q.Get("dir") == "desc", limit: limit, offset: offset}
	if column := q.Get("where"); column != "" {
		page.filters = []rowFilter{{Column: column, Op: q.Get("op"), Value: q.Get("value")}}
	}
	selectSQL, countSQL, args, err := conn.pageSQL(page)
	if err != nil {
		writeErr(w, statusFor(err), err)
		return
	}
	rows, err := conn.QueryText(ctx, selectSQL, args...)
	if err != nil {
		writeErr(w, http.StatusBadRequest, err)
		return
	}
	columns, data, err := collectRows(rows)
	rows.Close()
	if err != nil {
		writeErr(w, http.StatusBadRequest, err)
		return
	}
	hasMore := len(data) > limit
	if hasMore {
		data = data[:limit]
	}

	var total int64
	if err := conn.QueryRow(ctx, countSQL, args...).Scan(&total); err != nil {
		writeErr(w, http.StatusBadRequest, err)
		return
	}
	count := "exact"
	if total > countCap {
		total, count = countCap, "atLeast"
		if len(page.filters) == 0 {
			if stats, err := conn.tableStats(ctx, schema, table); err == nil && stats.rows != nil && *stats.rows > countCap {
				total, count = *stats.rows, "estimated"
			}
		}
	}
	if key == nil {
		key = []string{}
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"columns": columns, "key": key, "rows": data, "total": total, "count": count,
		"limit": limit, "offset": offset, "hasMore": hasMore,
	})
}

// pageBounds reads ?limit= and ?offset=; the grid's pages are at most 200
// rows, so a request can never ask for the whole table.
func pageBounds(q url.Values) (limit, offset int) {
	limit, _ = strconv.Atoi(q.Get("limit"))
	if limit < 1 || limit > 200 {
		limit = 50
	}
	offset, _ = strconv.Atoi(q.Get("offset"))
	return limit, max(offset, 0)
}

type dbRowRequest struct {
	// Key identifies the row (primary key values); Values carries the
	// columns to write. Both are text — the database casts to the column
	// type. A null value writes NULL.
	Key    map[string]string  `json:"key"`
	Values map[string]*string `json:"values"`
}

// wherePK builds the key match; every key column must be present, so a
// half-identified row can never match more than intended.
func wherePK(conn *databaseConn, key map[string]string, pk []string, args *[]any) (string, error) {
	if len(pk) == 0 {
		return "", runtime.UserError{Err: fmt.Errorf("this table has no primary key — the grid can't safely change rows, use the SQL view")}
	}
	var parts []string
	for _, col := range pk {
		v, ok := key[col]
		if !ok {
			return "", runtime.UserError{Err: fmt.Errorf("the row's %s value is missing", col)}
		}
		*args = append(*args, v)
		parts = append(parts, conn.quote(col)+" = "+conn.placeholder(len(*args)))
	}
	return strings.Join(parts, " AND "), nil
}

func (s *Server) handleDbInsertRow(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := context.WithTimeout(r.Context(), databaseRequestTimeout)
	defer cancel()
	var req dbRowRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeErr(w, http.StatusBadRequest, err)
		return
	}
	if len(req.Values) == 0 {
		writeErr(w, http.StatusBadRequest, fmt.Errorf("the new row is empty"))
		return
	}
	conn, err := s.dbConnect(ctx, r, r.PathValue("db"))
	if err != nil {
		writeErr(w, statusFor(err), err)
		return
	}
	defer conn.Close(ctx)
	ident, err := conn.tableIdent(r.URL.Query().Get("schema"), r.PathValue("table"))
	if err != nil {
		writeErr(w, statusFor(err), err)
		return
	}
	var cols, holders []string
	var args []any
	for _, c := range slices.Sorted(maps.Keys(req.Values)) {
		args = append(args, req.Values[c])
		cols = append(cols, conn.quote(c))
		holders = append(holders, conn.placeholder(len(args)))
	}
	_, err = conn.Exec(ctx, fmt.Sprintf("INSERT INTO %s (%s) VALUES (%s)",
		ident, strings.Join(cols, ", "), strings.Join(holders, ", ")), args...)
	if err != nil {
		writeErr(w, http.StatusBadRequest, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true})
}

func (s *Server) handleDbUpdateRow(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := context.WithTimeout(r.Context(), databaseRequestTimeout)
	defer cancel()
	var req dbRowRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeErr(w, http.StatusBadRequest, err)
		return
	}
	if len(req.Values) == 0 {
		writeJSON(w, http.StatusOK, map[string]any{"ok": true, "affected": 0})
		return
	}
	conn, err := s.dbConnect(ctx, r, r.PathValue("db"))
	if err != nil {
		writeErr(w, statusFor(err), err)
		return
	}
	defer conn.Close(ctx)
	ident, err := conn.tableIdent(r.URL.Query().Get("schema"), r.PathValue("table"))
	if err != nil {
		writeErr(w, statusFor(err), err)
		return
	}
	pk, err := primaryKey(ctx, conn, r.URL.Query().Get("schema"), r.PathValue("table"))
	if err != nil {
		writeErr(w, http.StatusBadGateway, err)
		return
	}
	var args []any
	var sets []string
	for _, c := range slices.Sorted(maps.Keys(req.Values)) {
		args = append(args, req.Values[c])
		sets = append(sets, conn.quote(c)+" = "+conn.placeholder(len(args)))
	}
	where, err := wherePK(conn, req.Key, pk, &args)
	if err != nil {
		writeErr(w, statusFor(err), err)
		return
	}
	tag, err := conn.Exec(ctx, fmt.Sprintf("UPDATE %s SET %s WHERE %s", ident, strings.Join(sets, ", "), where), args...)
	if err != nil {
		writeErr(w, http.StatusBadRequest, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "affected": tag.RowsAffected()})
}

func (s *Server) handleDbDeleteRow(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := context.WithTimeout(r.Context(), databaseRequestTimeout)
	defer cancel()
	var req dbRowRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeErr(w, http.StatusBadRequest, err)
		return
	}
	conn, err := s.dbConnect(ctx, r, r.PathValue("db"))
	if err != nil {
		writeErr(w, statusFor(err), err)
		return
	}
	defer conn.Close(ctx)
	ident, err := conn.tableIdent(r.URL.Query().Get("schema"), r.PathValue("table"))
	if err != nil {
		writeErr(w, statusFor(err), err)
		return
	}
	pk, err := primaryKey(ctx, conn, r.URL.Query().Get("schema"), r.PathValue("table"))
	if err != nil {
		writeErr(w, http.StatusBadGateway, err)
		return
	}
	var args []any
	where, err := wherePK(conn, req.Key, pk, &args)
	if err != nil {
		writeErr(w, statusFor(err), err)
		return
	}
	tag, err := conn.Exec(ctx, fmt.Sprintf("DELETE FROM %s WHERE %s", ident, where), args...)
	if err != nil {
		writeErr(w, http.StatusBadRequest, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "affected": tag.RowsAffected()})
}

// handleDbQuery runs one statement as-is — the escape hatch for everything
// the grid doesn't do. The token holder owns this database; the server's
// job is transport, not judgement.
func (s *Server) handleDbQuery(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := context.WithTimeout(r.Context(), databaseRequestTimeout)
	defer cancel()
	var req struct {
		SQL    string  `json:"sql"`
		Schema *string `json:"schema,omitempty"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeErr(w, http.StatusBadRequest, err)
		return
	}
	if strings.TrimSpace(req.SQL) == "" {
		writeErr(w, http.StatusBadRequest, fmt.Errorf("nothing to run"))
		return
	}
	conn, err := s.dbBrowseConnect(ctx, r, r.PathValue("db"))
	if err != nil {
		writeErr(w, statusFor(err), err)
		return
	}
	defer conn.Close(ctx)
	if conn.trino != nil {
		conn.trino.serveQuery(w, r, req.SQL, req.Schema)
		return
	}
	if req.Schema != nil && *req.Schema != "" {
		if strings.ContainsRune(*req.Schema, '\x00') {
			writeErr(w, http.StatusBadRequest, fmt.Errorf("invalid schema"))
			return
		}
		if conn.protocol == addons.ProtocolMySQL && *req.Schema != conn.database {
			writeErr(w, http.StatusBadRequest, fmt.Errorf("the schema must match the selected MySQL database"))
			return
		}
		if conn.protocol == addons.ProtocolPostgres {
			if _, err := conn.Exec(ctx, "SET search_path TO "+conn.quote(*req.Schema)); err != nil {
				writeErr(w, http.StatusBadRequest, err)
				return
			}
		}
	}

	rows, err := conn.QueryText(ctx, req.SQL)
	if err != nil {
		writeErr(w, http.StatusBadRequest, err)
		return
	}
	defer rows.Close()
	const maxRows = 500
	columns := append([]string{}, rows.columns...)
	data := [][]any{}
	truncated := false
	for rows.Next() {
		if len(data) == maxRows {
			truncated = true
			break
		}
		row, err := rows.Cells()
		if err != nil {
			writeErr(w, http.StatusBadRequest, err)
			return
		}
		data = append(data, row)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		writeErr(w, http.StatusBadRequest, err)
		return
	}
	affected, err := conn.queryAffected(ctx, rows, len(data))
	if err != nil {
		writeErr(w, http.StatusBadRequest, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"columns": columns, "rows": data,
		"affected": affected, "truncated": truncated,
	})
}
