package server

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/adrianliechti/donkey/internal/addons"
	"github.com/adrianliechti/donkey/pkg/runtime"
)

const trinoQueryTimeout = 2 * time.Minute

// Use Trino's direct JSON protocol. In particular, never follow a redirect
// carrying the user's credential, or opt into object-storage downloads.
var trinoHTTPClient = &http.Client{
	Timeout:       30 * time.Second,
	CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
}

type trinoClient struct {
	base                   *url.URL
	token, catalog, schema string
}

type trinoResult struct {
	Columns   []string `json:"columns"`
	Rows      [][]any  `json:"rows"`
	Affected  int64    `json:"affected"`
	Truncated bool     `json:"truncated"`
}

type trinoPage struct {
	NextURI string `json:"nextUri"`
	Columns []struct {
		Name string `json:"name"`
	} `json:"columns"`
	Data        [][]any `json:"data"`
	UpdateCount *int64  `json:"updateCount"`
	Error       *struct {
		Message string `json:"message"`
	} `json:"error"`
}

// Continuations must stay under the configured coordinator's statement API.
// This also applies to cancellation; a response cannot redirect a credential
// to another service or downgrade an HTTPS connection.
func (c *trinoClient) nextURL(raw string) (*url.URL, error) {
	u, err := url.Parse(raw)
	if err != nil {
		return nil, fmt.Errorf("Trino returned an invalid continuation URL")
	}
	u = c.base.ResolveReference(u)
	prefix := strings.TrimRight(c.base.EscapedPath(), "/") + "/v1/statement/"
	if u.Scheme != c.base.Scheme || !strings.EqualFold(u.Host, c.base.Host) || u.User != nil || u.Fragment != "" || !strings.HasPrefix(u.EscapedPath(), prefix) {
		return nil, fmt.Errorf("Trino continuation URL must stay on the configured statement endpoint")
	}
	// Reject path traversal even when repeatedly escaped.
	path := u.EscapedPath()
	for {
		for _, part := range strings.Split(path, "/") {
			if part == "." || part == ".." || strings.ContainsAny(part, "\\\x00\r\n") {
				return nil, fmt.Errorf("Trino returned an invalid continuation path")
			}
		}
		decoded, err := url.PathUnescape(path)
		if err != nil || decoded == path {
			break
		}
		path = decoded
	}
	return u, nil
}

func (c *trinoClient) request(ctx context.Context, method string, target *url.URL, sql string) (*http.Response, error) {
	for attempt := 0; ; attempt++ {
		req, err := http.NewRequestWithContext(ctx, method, target.String(), strings.NewReader(sql))
		if err != nil {
			return nil, fmt.Errorf("invalid Trino request")
		}
		// Present the user's IdP token to the add-on proxy. The add-on owns
		// delegation to Trino. Send it on the configured HTTP or HTTPS URL,
		// including result fetches, retries and cancellation requests.
		req.Header.Set("Authorization", "Bearer "+c.token)
		req.Header.Set("X-Trino-Source", "donkey-console")
		req.Header.Set("Accept", "application/json")
		if method == http.MethodPost {
			req.Header.Set("Content-Type", "text/plain; charset=utf-8")
		}
		if c.catalog != "" {
			req.Header.Set("X-Trino-Catalog", c.catalog)
		}
		if c.schema != "" {
			req.Header.Set("X-Trino-Schema", c.schema)
		}
		res, err := trinoHTTPClient.Do(req)
		if err != nil {
			return nil, fmt.Errorf("the Trino endpoint did not answer")
		}
		if res.StatusCode >= 200 && res.StatusCode < 300 {
			return res, nil
		}
		res.Body.Close()
		switch res.StatusCode {
		case http.StatusTooManyRequests, http.StatusBadGateway, http.StatusServiceUnavailable, http.StatusGatewayTimeout:
			if attempt < 5 && method != http.MethodDelete {
				delay := 100 * time.Millisecond
				if seconds, err := strconv.Atoi(res.Header.Get("Retry-After")); err == nil && seconds > 0 {
					delay = time.Duration(min(seconds, 120)) * time.Second
				} else if until, err := http.ParseTime(res.Header.Get("Retry-After")); err == nil && time.Until(until) > 0 {
					delay = min(time.Until(until), trinoQueryTimeout)
				}
				timer := time.NewTimer(delay)
				select {
				case <-timer.C:
					continue
				case <-ctx.Done():
					timer.Stop()
					return nil, ctx.Err()
				}
			}
		}
		if res.StatusCode == http.StatusUnauthorized || res.StatusCode == http.StatusForbidden {
			return nil, fmt.Errorf("Trino rejected your Donkey credentials (HTTP %d)", res.StatusCode)
		}
		return nil, fmt.Errorf("Trino returned HTTP %d", res.StatusCode)
	}
}

func (c *trinoClient) query(ctx context.Context, sql string, limit int) (*trinoResult, error) {
	out := &trinoResult{Columns: []string{}, Rows: [][]any{}}
	target := c.base.JoinPath("v1/statement")
	method := http.MethodPost
	var pending *url.URL
	defer func() {
		if pending == nil {
			return
		}
		// A browser abort/timeout must still release the running query.
		cleanup, cancel := context.WithTimeout(context.WithoutCancel(ctx), 2*time.Second)
		defer cancel()
		if res, err := c.request(cleanup, http.MethodDelete, pending, ""); err == nil {
			res.Body.Close()
		}
	}()
	for {
		res, err := c.request(ctx, method, target, sql)
		if err != nil {
			return nil, err
		}
		var page trinoPage
		decoder := json.NewDecoder(io.LimitReader(res.Body, 16<<20))
		decoder.UseNumber() // bigint and decimal must not pass through float64.
		err = decoder.Decode(&page)
		res.Body.Close()
		if err != nil {
			return nil, fmt.Errorf("Trino returned an invalid or oversized result")
		}
		pending = nil
		if page.NextURI != "" {
			pending, err = c.nextURL(page.NextURI)
			if err != nil {
				return nil, err
			}
		}
		if page.Error != nil {
			return nil, fmt.Errorf("Trino: %s", page.Error.Message)
		}
		if len(page.Columns) > 0 {
			out.Columns = out.Columns[:0]
			for _, col := range page.Columns {
				out.Columns = append(out.Columns, col.Name)
			}
		}
		for _, row := range page.Data {
			if len(out.Rows) == limit {
				out.Truncated = true
				return out, nil
			}
			values := make([]any, len(row))
			for i, value := range row {
				switch value.(type) {
				case []any, map[string]any:
					encoded, _ := json.Marshal(value)
					values[i] = string(encoded)
				default:
					values[i] = cell(value)
				}
			}
			out.Rows = append(out.Rows, values)
		}
		if page.UpdateCount != nil {
			out.Affected = *page.UpdateCount
		}
		if pending == nil {
			return out, nil
		}
		target, method, sql = pending, http.MethodGet, ""
	}
}

func trinoIdent(value string) (string, error) {
	if value == "" || strings.ContainsAny(value, "\x00\r\n") {
		return "", fmt.Errorf("a catalog, schema or table name is missing or invalid")
	}
	return `"` + strings.ReplaceAll(value, `"`, `""`) + `"`, nil
}

// trinoToken is the caller's IdP access token for donkey's API — the endpoint
// behind TRINO_URL exchanges it on-behalf-of for Trino's. The gateway mints it
// into the sealed token it forwards; the server never mints one. Never
// donkey's own sealed token: nothing outside donkey can read it, and anyone
// holding it could replay it against this API.
func (s *Server) trinoToken(ctx context.Context) (string, error) {
	if s.cfg.OIDCScope == "" {
		return "", runtime.UserError{Err: fmt.Errorf("Trino needs the user's identity provider token, and this installation names no auth.scope")}
	}
	if id := requestIdentity(ctx); id != nil && id.AccessToken != "" {
		return id.AccessToken, nil
	}
	return "", runtime.UserError{Err: fmt.Errorf("this request carries no identity provider token for Trino — open the console through donkey's address, or sign out and in again")}
}

// Each database request brings the current caller's credential. The client
// exists for that request only; tokens never enter a shared pool or binding.
func newTrinoClient(binding map[string]string, database, token string) *trinoClient {
	base, _ := url.Parse(binding["TRINO_URL"])
	catalog := binding["TRINO_CATALOG"]
	if database != "" {
		catalog = database
	}
	return &trinoClient{base: base, token: token, catalog: catalog, schema: binding["TRINO_SCHEMA"]}
}

// Catalogs are the database API's top-level containers. Transport and SQL
// dialect differences remain inside this adapter, behind the shared routes.
func (c *trinoClient) serveDatabase(w http.ResponseWriter, r *http.Request, operation string) {
	ctx, cancel := context.WithTimeout(r.Context(), trinoQueryTimeout)
	defer cancel()
	query, limit, offset := "SHOW CATALOGS", 10000, 0
	defaultName := c.catalog
	switch operation {
	case "databases":
		c.catalog, c.schema = "", ""
	case "schemas", "tables", "rows":
		catalog, err := trinoIdent(c.catalog)
		if err != nil {
			writeErr(w, http.StatusBadRequest, err)
			return
		}
		defaultName, c.schema = c.schema, ""
		query = "SHOW SCHEMAS FROM " + catalog
		if operation != "schemas" {
			c.schema = r.URL.Query().Get("schema")
			if c.schema == "" {
				c.schema = defaultName
			}
			schema, err := trinoIdent(c.schema)
			if err != nil {
				writeErr(w, http.StatusBadRequest, err)
				return
			}
			query = "SHOW TABLES FROM " + catalog + "." + schema
			if operation == "rows" {
				table, err := trinoIdent(r.PathValue("table"))
				if err != nil {
					writeErr(w, http.StatusBadRequest, err)
					return
				}
				limit, _ = strconv.Atoi(r.URL.Query().Get("limit"))
				if limit < 1 || limit > 200 {
					limit = 50
				}
				offset, _ = strconv.Atoi(r.URL.Query().Get("offset"))
				if offset < 0 {
					offset = 0
				}
				order := ""
				if column := r.URL.Query().Get("order"); column != "" {
					column, err = trinoIdent(column)
					if err != nil {
						writeErr(w, http.StatusBadRequest, err)
						return
					}
					direction := " ASC"
					if r.URL.Query().Get("dir") == "desc" {
						direction = " DESC"
					}
					order = " ORDER BY " + column + direction
				}
				query = fmt.Sprintf("SELECT * FROM %s.%s.%s%s OFFSET %d LIMIT %d", catalog, schema, table, order, offset, limit+1)
			}
		}
	}
	result, err := c.query(ctx, query, limit)
	if err != nil {
		writeErr(w, http.StatusBadGateway, err)
		return
	}
	if operation == "rows" {
		// Full-table counts are often expensive or unavailable on Trino.
		writeJSON(w, http.StatusOK, map[string]any{
			"columns": result.Columns, "rows": result.Rows, "key": []string{},
			"total": nil, "limit": limit, "offset": offset, "hasMore": result.Truncated,
		})
		return
	}
	if result.Truncated {
		writeErr(w, http.StatusBadGateway, fmt.Errorf("Trino returned more than %d %s; narrow the selection using SQL", limit, operation))
		return
	}
	names := []string{}
	for _, row := range result.Rows {
		if len(row) > 0 {
			if name, ok := row[0].(string); ok {
				names = append(names, name)
			}
		}
	}
	switch operation {
	case "databases":
		databases := []map[string]any{}
		for _, name := range names {
			databases = append(databases, map[string]any{"name": name, "size": nil})
		}
		writeJSON(w, http.StatusOK, map[string]any{"databases": databases, "default": defaultName, "protocol": addons.ProtocolTrino})
	case "schemas":
		writeJSON(w, http.StatusOK, map[string]any{"schemas": names, "default": defaultName})
	case "tables":
		tables := []map[string]any{}
		for _, name := range names {
			tables = append(tables, map[string]any{"name": name, "schema": c.schema, "rows": nil})
		}
		writeJSON(w, http.StatusOK, map[string]any{"tables": tables})
	}
}

func (c *trinoClient) serveQuery(w http.ResponseWriter, r *http.Request, statement string, schema *string) {
	if schema != nil {
		c.schema = *schema
	}
	for _, value := range []string{c.catalog, c.schema} {
		if strings.ContainsAny(value, "\x00\r\n") {
			writeErr(w, http.StatusBadRequest, fmt.Errorf("invalid catalog or schema"))
			return
		}
	}
	ctx, cancel := context.WithTimeout(r.Context(), trinoQueryTimeout)
	defer cancel()
	result, err := c.query(ctx, statement, 500)
	if err != nil {
		writeErr(w, http.StatusBadGateway, err)
		return
	}
	writeJSON(w, http.StatusOK, result)
}
