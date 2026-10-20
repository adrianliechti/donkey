package server

import (
	"context"
	"database/sql"
	"fmt"
	"net"
	"net/url"
	"strings"
	"time"

	"github.com/go-sql-driver/mysql"
	"github.com/jackc/pgx/v5"

	"github.com/adrianliechti/donkey/internal/addons"
	"github.com/adrianliechti/donkey/pkg/runtime"
)

// databaseConn keeps one physical connection for the entire request. In
// particular, MariaDB's ROW_COUNT() must run on the query's own connection.
type databaseConn struct {
	protocol string
	database string
	pg       *pgx.Conn
	mysql    *sql.Conn
	pool     *sql.DB
	trino    *trinoClient
}

func connectDatabase(ctx context.Context, protocol, dsn, database string) (*databaseConn, error) {
	u, err := url.Parse(dsn)
	protocol = addons.NormalizeProtocol(protocol)
	if err != nil || u.Hostname() == "" || addons.NormalizeProtocol(u.Scheme) != protocol {
		return nil, runtime.UserError{Err: fmt.Errorf("the database connection URL must match the service's protocol")}
	}
	c := &databaseConn{protocol: protocol, database: strings.TrimPrefix(u.Path, "/")}
	if database != "" {
		c.database = database
	}
	switch protocol {
	case addons.ProtocolPostgres:
		if database != "" {
			u.Path, u.RawPath = "/"+database, ""
		}
		cfg, err := pgx.ParseConfig(u.String())
		if err != nil {
			return nil, runtime.UserError{Err: fmt.Errorf("invalid PostgreSQL connection options")}
		}
		c.pg, err = pgx.ConnectConfig(ctx, cfg)
		if err != nil {
			return nil, err
		}
	case addons.ProtocolMySQL:
		cfg, err := mysqlURLConfig(u, c.database)
		if err != nil {
			return nil, runtime.UserError{Err: err}
		}
		connector, err := mysql.NewConnector(cfg)
		if err != nil {
			return nil, runtime.UserError{Err: fmt.Errorf("invalid MySQL connection options")}
		}
		c.pool = sql.OpenDB(connector)
		c.pool.SetMaxOpenConns(1)
		c.mysql, err = c.pool.Conn(ctx)
		if err != nil {
			c.pool.Close()
			return nil, err
		}
	default:
		return nil, runtime.UserError{Err: fmt.Errorf("the database editor supports the postgres and mysql protocols")}
	}
	return c, nil
}

func mysqlURLConfig(u *url.URL, database string) (*mysql.Config, error) {
	port := u.Port()
	if port == "" {
		port = "3306"
	}
	options, err := url.ParseQuery(u.RawQuery)
	if err != nil {
		return nil, fmt.Errorf("invalid MySQL connection options")
	}
	for key, values := range options {
		if len(values) != 1 {
			return nil, fmt.Errorf("duplicate MySQL connection option")
		}
		switch key {
		case "tls", "timeout", "readTimeout", "writeTimeout", "parseTime", "loc", "charset", "collation":
		default:
			return nil, fmt.Errorf("unsupported MySQL connection option %q", key)
		}
	}
	for key, value := range map[string]string{"timeout": "10s", "readTimeout": "30s", "writeTimeout": "30s", "charset": "utf8mb4"} {
		if !options.Has(key) {
			options.Set(key, value)
		}
	}
	for _, key := range []string{"charset", "collation"} {
		if value := options.Get(key); value != "" && !plainIdent.MatchString(value) {
			return nil, fmt.Errorf("invalid MySQL %s", key)
		}
	}
	// Parse options using the driver's rules, but assign credentials as
	// structured fields: URL-escaped passwords never become DSN syntax.
	cfg, err := mysql.ParseDSN("tcp(" + net.JoinHostPort(u.Hostname(), port) + ")/" + url.PathEscape(database) + "?" + options.Encode())
	if err != nil {
		return nil, fmt.Errorf("invalid MySQL connection options")
	}
	if u.User != nil {
		cfg.User = u.User.Username()
		cfg.Passwd, _ = u.User.Password()
	}
	return cfg, nil
}

func (c *databaseConn) Close(ctx context.Context) {
	if c.pg != nil {
		_ = c.pg.Close(ctx)
	}
	if c.mysql != nil {
		_ = c.mysql.Close()
		_ = c.pool.Close()
	}
}

type databaseRows struct {
	pg      pgx.Rows
	mysql   *sql.Rows
	columns []string
}

func (c *databaseConn) Query(ctx context.Context, query string, args ...any) (*databaseRows, error) {
	r := &databaseRows{}
	var err error
	if c.pg != nil {
		r.pg, err = c.pg.Query(ctx, query, args...)
		if err == nil {
			for _, f := range r.pg.FieldDescriptions() {
				r.columns = append(r.columns, f.Name)
			}
		}
	} else {
		r.mysql, err = c.mysql.QueryContext(ctx, query, args...)
		if err == nil {
			r.columns, err = r.mysql.Columns()
			if err != nil {
				r.mysql.Close()
			}
		}
	}
	return r, err
}

func (r *databaseRows) Next() bool {
	if r.pg != nil {
		return r.pg.Next()
	}
	return r.mysql.Next()
}
func (r *databaseRows) Err() error {
	if r.pg != nil {
		return r.pg.Err()
	}
	return r.mysql.Err()
}
func (r *databaseRows) Close() {
	if r.pg != nil {
		r.pg.Close()
	} else {
		_ = r.mysql.Close()
	}
}
func (r *databaseRows) Scan(args ...any) error {
	if r.pg != nil {
		return r.pg.Scan(args...)
	}
	return r.mysql.Scan(args...)
}
func (r *databaseRows) Values() ([]any, error) {
	if r.pg != nil {
		return r.pg.Values()
	}
	values := make([]any, len(r.columns))
	dest := make([]any, len(values))
	for i := range values {
		dest[i] = &values[i]
	}
	return values, r.mysql.Scan(dest...)
}

type databaseRow interface{ Scan(...any) error }

func (c *databaseConn) QueryRow(ctx context.Context, query string, args ...any) databaseRow {
	if c.pg != nil {
		return c.pg.QueryRow(ctx, query, args...)
	}
	return c.mysql.QueryRowContext(ctx, query, args...)
}

type affectedRows int64

func (n affectedRows) RowsAffected() int64 { return int64(n) }
func (c *databaseConn) Exec(ctx context.Context, query string, args ...any) (affectedRows, error) {
	if c.pg != nil {
		tag, err := c.pg.Exec(ctx, query, args...)
		return affectedRows(tag.RowsAffected()), err
	}
	result, err := c.mysql.ExecContext(ctx, query, args...)
	if err != nil {
		return 0, err
	}
	n, err := result.RowsAffected()
	return affectedRows(n), err
}

func (c *databaseConn) quote(parts ...string) string {
	if c.protocol == addons.ProtocolPostgres {
		return pgx.Identifier(parts).Sanitize()
	}
	out := make([]string, len(parts))
	for i, part := range parts {
		out[i] = "`" + strings.ReplaceAll(part, "`", "``") + "`"
	}
	return strings.Join(out, ".")
}

func (c *databaseConn) placeholder(n int) string {
	if c.protocol == addons.ProtocolMySQL {
		return "?"
	}
	return fmt.Sprintf("$%d", n)
}

func (c *databaseConn) tableIdent(schema, table string) (string, error) {
	if table == "" || strings.ContainsRune(table, '\x00') || strings.ContainsRune(schema, '\x00') {
		return "", runtime.UserError{Err: fmt.Errorf("a valid table name is required")}
	}
	if schema == "" {
		if c.protocol == addons.ProtocolMySQL {
			schema = c.database
		} else {
			schema = "public"
		}
	}
	return c.quote(schema, table), nil
}

func (c *databaseConn) currentDatabaseSQL() string {
	if c.protocol == addons.ProtocolMySQL {
		return "SELECT DATABASE()"
	}
	return "SELECT current_database()"
}

// Query results keep their driver's command count. MySQL exposes the count
// for a statement without columns through ROW_COUNT() on the same session.
func (c *databaseConn) queryAffected(ctx context.Context, rows *databaseRows, returned int) (int64, error) {
	if rows.pg != nil {
		return rows.pg.CommandTag().RowsAffected(), nil
	}
	if len(rows.columns) > 0 {
		return int64(returned), nil
	}
	var n int64
	err := c.QueryRow(ctx, "SELECT ROW_COUNT()").Scan(&n)
	return n, err
}

const databaseRequestTimeout = 30 * time.Second
