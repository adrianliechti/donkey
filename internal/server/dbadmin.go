package server

// The console's database browser. donkey-server already sits in the cluster
// and already holds the binding, so it can speak to a space's database
// directly — no tunnel, no exposed port, the admin token is the only door.

import (
	"context"
	"encoding/json"
	"fmt"
	"maps"
	"net/http"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/adrianliechti/donkey/internal/addons"
	"github.com/adrianliechti/donkey/internal/config"
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
	space, err := s.space(r)
	if err != nil {
		return nil, runtime.UserError{Err: err}
	}
	name := r.PathValue("service")
	if !config.ValidName(name) {
		return nil, runtime.UserError{Err: fmt.Errorf("%q is not a valid service name", name)}
	}
	data, err := s.rt.ServiceBinding(ctx, space, addons.SharedOwner, name)
	if err != nil {
		return nil, err
	}
	if len(data) == 0 {
		return nil, runtime.UserError{Err: fmt.Errorf("the space has no service named %q", name)}
	}
	dsn, ok := data["DATABASE_URL"]
	if !ok {
		return nil, runtime.UserError{Err: fmt.Errorf("%s has no database connection URL", name)}
	}
	metadata, err := s.serviceMetadata(ctx, space, name, data)
	if err != nil {
		return nil, err
	}
	conn, err := connectDatabase(ctx, metadata.Protocol, dsn, db)
	if err != nil {
		return nil, fmt.Errorf("%s is not answering: %w", name, err)
	}
	return conn, nil
}

// handleDbDatabases lists the service's databases — templates stay hidden,
// they are postgres' own scaffolding.
func (s *Server) handleDbDatabases(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := context.WithTimeout(r.Context(), databaseRequestTimeout)
	defer cancel()
	conn, err := s.dbConnect(ctx, r, "")
	if err != nil {
		writeErr(w, statusFor(err), err)
		return
	}
	defer conn.Close(ctx)

	query := `SELECT datname, pg_database_size(datname) FROM pg_database WHERE NOT datistemplate ORDER BY datname`
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

// handleDbTables lists the user's tables — system schemas are the server's
// business, not the browser's.
func (s *Server) handleDbTables(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := context.WithTimeout(r.Context(), databaseRequestTimeout)
	defer cancel()
	conn, err := s.dbConnect(ctx, r, r.PathValue("db"))
	if err != nil {
		writeErr(w, statusFor(err), err)
		return
	}
	defer conn.Close(ctx)

	query := `
		SELECT n.nspname, c.relname, GREATEST(c.reltuples, 0)::bigint
		FROM pg_class c
		JOIN pg_namespace n ON n.oid = c.relnamespace
		WHERE c.relkind IN ('r', 'p')
		  AND n.nspname NOT IN ('pg_catalog', 'information_schema')
		ORDER BY 1, 2`
	var args []any
	if conn.protocol == addons.ProtocolMySQL {
		query = `SELECT TABLE_SCHEMA, TABLE_NAME, COALESCE(TABLE_ROWS, 0) FROM information_schema.TABLES
		WHERE TABLE_SCHEMA = ? AND TABLE_TYPE = 'BASE TABLE' ORDER BY TABLE_NAME`
		args = []any{conn.database}
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
		// Rows is the planner's estimate — free, and honest enough for a
		// sidebar.
		Rows int64 `json:"rows"`
	}
	out := []table{}
	for rows.Next() {
		var t table
		if err := rows.Scan(&t.Schema, &t.Name, &t.Rows); err != nil {
			writeErr(w, http.StatusBadGateway, err)
			return
		}
		out = append(out, t)
	}
	if err := rows.Err(); err != nil {
		writeErr(w, http.StatusBadGateway, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"tables": out})
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

// cell turns a database value into something JSON (and an <input>) can
// carry: strings and null, nothing else.
func cell(v any) any {
	switch t := v.(type) {
	case nil:
		return nil
	case []byte:
		return string(t)
	case time.Time:
		return t.Format(time.RFC3339)
	default:
		return fmt.Sprint(t)
	}
}

func collectRows(rows *databaseRows) (columns []string, data [][]any, err error) {
	columns = append(columns, rows.columns...)
	for rows.Next() {
		values, err := rows.Values()
		if err != nil {
			return nil, nil, err
		}
		row := make([]any, len(values))
		for i, v := range values {
			row[i] = cell(v)
		}
		data = append(data, row)
	}
	return columns, data, rows.Err()
}

// handleDbRows serves one page of a table, with the key columns the grid
// needs to edit it.
func (s *Server) handleDbRows(w http.ResponseWriter, r *http.Request) {
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
	limit, _ := strconv.Atoi(r.URL.Query().Get("limit"))
	if limit < 1 || limit > 200 {
		limit = 50
	}
	offset, _ := strconv.Atoi(r.URL.Query().Get("offset"))
	if offset < 0 {
		offset = 0
	}

	key, err := primaryKey(ctx, conn, r.URL.Query().Get("schema"), r.PathValue("table"))
	if err != nil {
		writeErr(w, http.StatusBadGateway, err)
		return
	}
	// The caller may sort by one column (?order=&dir=); the key columns are
	// always appended — stable paging needs a stable order, and a sort
	// column with duplicates alone would shuffle pages.
	var ordering []string
	if col := r.URL.Query().Get("order"); col != "" {
		dir := " ASC"
		if r.URL.Query().Get("dir") == "desc" {
			dir = " DESC"
		}
		ordering = append(ordering, conn.quote(col)+dir)
	}
	for _, k := range key {
		ordering = append(ordering, conn.quote(k))
	}
	order := ""
	if len(ordering) > 0 {
		order = " ORDER BY " + strings.Join(ordering, ", ")
	}

	var total int64
	if err := conn.QueryRow(ctx, "SELECT count(*) FROM "+ident).Scan(&total); err != nil {
		writeErr(w, http.StatusBadGateway, err)
		return
	}
	rows, err := conn.Query(ctx, fmt.Sprintf("SELECT * FROM %s%s LIMIT %d OFFSET %d", ident, order, limit, offset))
	if err != nil {
		writeErr(w, http.StatusBadGateway, err)
		return
	}
	defer rows.Close()
	columns, data, err := collectRows(rows)
	if err != nil {
		writeErr(w, http.StatusBadGateway, err)
		return
	}
	if key == nil {
		key = []string{}
	}
	if data == nil {
		data = [][]any{}
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"columns": columns, "key": key, "rows": data, "total": total, "limit": limit, "offset": offset,
	})
}

type dbRowRequest struct {
	// Key identifies the row (primary key values); Values carries the
	// columns to write. Both are text — postgres casts to the column type.
	Key    map[string]string `json:"key"`
	Values map[string]string `json:"values"`
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
		SQL string `json:"sql"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeErr(w, http.StatusBadRequest, err)
		return
	}
	if strings.TrimSpace(req.SQL) == "" {
		writeErr(w, http.StatusBadRequest, fmt.Errorf("nothing to run"))
		return
	}
	conn, err := s.dbConnect(ctx, r, r.PathValue("db"))
	if err != nil {
		writeErr(w, statusFor(err), err)
		return
	}
	defer conn.Close(ctx)

	rows, err := conn.Query(ctx, req.SQL)
	if err != nil {
		writeErr(w, http.StatusBadRequest, err)
		return
	}
	defer rows.Close()
	const maxRows = 500
	var columns []string
	columns = append(columns, rows.columns...)
	data := [][]any{}
	truncated := false
	for rows.Next() {
		if len(data) == maxRows {
			truncated = true
			break
		}
		values, err := rows.Values()
		if err != nil {
			writeErr(w, http.StatusBadRequest, err)
			return
		}
		row := make([]any, len(values))
		for i, v := range values {
			row[i] = cell(v)
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
	if columns == nil {
		columns = []string{}
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"columns": columns, "rows": data,
		"affected": affected, "truncated": truncated,
	})
}
