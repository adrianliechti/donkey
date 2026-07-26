// Package apiclient is the CLI's transport to donkey-server.
package apiclient

import (
	"bufio"
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"strings"
	"time"

	"github.com/gorilla/websocket"

	"github.com/adrianliechti/donkey/internal/ai"
	"github.com/adrianliechti/donkey/internal/server"
)

type Client struct {
	BaseURL string
	Token   string
	http    *http.Client
}

// New builds the transport. caFile is a PEM bundle to trust on top of the
// system roots — installations whose certificates come from a private CA need
// it, and a wrong path is an error worth hearing immediately.
func New(baseURL, token, caFile string) (*Client, error) {
	// No overall timeout: deploys and log follows are long-lived streams.
	httpClient := &http.Client{}
	if caFile != "" {
		pool, err := caPool(caFile)
		if err != nil {
			return nil, err
		}
		// Clone the default transport: it carries proxy support (corporate
		// networks — exactly where private CAs live), sane dial timeouts and
		// HTTP/2; a bare &http.Transport{} would silently lose all of that.
		tr := http.DefaultTransport.(*http.Transport).Clone()
		tr.TLSClientConfig = &tls.Config{RootCAs: pool}
		httpClient.Transport = tr
	}
	return &Client{
		BaseURL: strings.TrimSuffix(baseURL, "/"),
		Token:   token,
		http:    httpClient,
	}, nil
}

func caPool(caFile string) (*x509.CertPool, error) {
	pem, err := os.ReadFile(caFile)
	if err != nil {
		return nil, fmt.Errorf("couldn't read the certificate authority file: %w", err)
	}
	pool, err := x509.SystemCertPool()
	if err != nil {
		pool = x509.NewCertPool()
	}
	if !pool.AppendCertsFromPEM(pem) {
		return nil, fmt.Errorf("%s doesn't contain a certificate donkey can use (expected PEM)", caFile)
	}
	return pool, nil
}

func (c *Client) req(ctx context.Context, method, path string, query url.Values, body io.Reader) (*http.Request, error) {
	u := c.BaseURL + path
	if len(query) > 0 {
		u += "?" + query.Encode()
	}
	req, err := http.NewRequestWithContext(ctx, method, u, body)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Authorization", "Bearer "+c.Token)
	return req, nil
}

func (c *Client) doJSON(ctx context.Context, method, path string, query url.Values, body any, out any) error {
	var rd io.Reader
	if body != nil {
		b, err := json.Marshal(body)
		if err != nil {
			return err
		}
		rd = strings.NewReader(string(b))
	}
	req, err := c.req(ctx, method, path, query, rd)
	if err != nil {
		return err
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := c.http.Do(req)
	if err != nil {
		return fmt.Errorf("couldn't reach the donkey server at %s: %w", c.BaseURL, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 400 {
		return apiError(resp)
	}
	if out != nil {
		if err := json.NewDecoder(resp.Body).Decode(out); err != nil {
			return fmt.Errorf("the server at %s didn't answer like a donkey server — is the address right (and the server up to date)?", c.BaseURL)
		}
	}
	return nil
}

func apiError(resp *http.Response) error {
	var e struct {
		Error string `json:"error"`
	}
	b, _ := io.ReadAll(io.LimitReader(resp.Body, 8192))
	if json.Unmarshal(b, &e) == nil && e.Error != "" {
		return fmt.Errorf("%s", e.Error)
	}
	return fmt.Errorf("the server answered HTTP %d", resp.StatusCode)
}

// Ping verifies connectivity and the token, riding out brief hiccups
// (a control plane that just restarted answers 502 for a few seconds). It
// hits the cheap ping endpoint — the expensive dependency probes belong to
// `donkey doctor` alone.
func (c *Client) Ping(ctx context.Context) error {
	var lastErr error
	for attempt := 0; attempt < 6; attempt++ {
		if attempt > 0 {
			select {
			case <-ctx.Done():
				return lastErr
			case <-time.After(2 * time.Second):
			}
		}
		// The response body is checked, not just the status: an older server
		// (or any web server) answers unknown GET paths with 200 + HTML via
		// the SPA fallback, which must not count as "token accepted".
		var out struct {
			OK bool `json:"ok"`
		}
		pctx, cancel := context.WithTimeout(ctx, 10*time.Second)
		lastErr = c.doJSON(pctx, http.MethodGet, "/v1/ping", nil, nil, &out)
		cancel()
		if lastErr == nil && !out.OK {
			lastErr = fmt.Errorf("the server at %s didn't answer like a donkey server — is the address right (and the server up to date)?", c.BaseURL)
			return lastErr
		}
		if lastErr == nil {
			return nil
		}
		if !transientPingError(lastErr) {
			return lastErr
		}
	}
	return lastErr
}

func transientPingError(err error) bool {
	// A name that doesn't resolve is a typo, not a hiccup — retrying it six
	// times only delays the obvious answer.
	var dnsErr *net.DNSError
	if errors.As(err, &dnsErr) {
		return false
	}
	msg := err.Error()
	for _, s := range []string{"HTTP 502", "HTTP 503", "HTTP 504", "connection refused", "couldn't reach"} {
		if strings.Contains(msg, s) {
			return true
		}
	}
	return false
}

type DeployOpts struct {
	// Space the app belongs to; empty lets the server fall back to
	// donkey.yaml.
	Space     string
	Platform  string
	Tag       string
	DryRun    bool
	NoWait    bool
	BuildOnly bool
	NoBuild   bool
	// SHA is the app's git commit (if any) — becomes part of the image tag.
	SHA string
}

// appPath builds the space-scoped API path of one app.
func appPath(space, app, suffix string) string {
	return "/v1/spaces/" + space + "/apps/" + app + suffix
}

// Deploy uploads the tar.gz build context and follows the progress stream.
// onLine receives step messages (always user-visible), onProgress the raw
// build output (verbose). Returns the final result.
func (c *Client) Deploy(ctx context.Context, contextTar io.Reader, opts DeployOpts, onLine, onProgress func(string)) (*server.DeployResult, error) {
	q := url.Values{}
	if opts.Space != "" {
		q.Set("space", opts.Space)
	}
	if opts.Platform != "" {
		q.Set("platform", opts.Platform)
	}
	if opts.Tag != "" {
		q.Set("tag", opts.Tag)
	}
	if opts.SHA != "" {
		q.Set("sha", opts.SHA)
	}
	setBool := func(k string, v bool) {
		if v {
			q.Set(k, "true")
		}
	}
	setBool("dryRun", opts.DryRun)
	setBool("noWait", opts.NoWait)
	setBool("buildOnly", opts.BuildOnly)
	setBool("noBuild", opts.NoBuild)

	req, err := c.req(ctx, http.MethodPost, "/v1/deploys", q, contextTar)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/gzip")
	resp, err := c.http.Do(req)
	if err != nil {
		return nil, fmt.Errorf("couldn't reach the donkey server at %s: %w", c.BaseURL, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 400 {
		return nil, apiError(resp)
	}

	sc := bufio.NewScanner(resp.Body)
	sc.Buffer(make([]byte, 0, 64*1024), 4*1024*1024)
	var result *server.DeployResult
	for sc.Scan() {
		var msg struct {
			Line     string               `json:"line"`
			Progress string               `json:"progress"`
			Result   *server.DeployResult `json:"result"`
		}
		if err := json.Unmarshal(sc.Bytes(), &msg); err != nil {
			continue
		}
		switch {
		case msg.Result != nil:
			result = msg.Result
		case msg.Line != "":
			onLine(msg.Line)
		case msg.Progress != "":
			onProgress(msg.Progress)
		}
	}
	if err := sc.Err(); err != nil {
		return nil, fmt.Errorf("the connection to the server broke mid-deploy: %w", err)
	}
	if result == nil {
		return nil, fmt.Errorf("the server didn't finish the deploy (no result received)")
	}
	return result, nil
}

func (c *Client) Status(ctx context.Context, space, app string) (*server.StatusResponse, error) {
	var out server.StatusResponse
	err := c.doJSON(ctx, http.MethodGet, appPath(space, app, "/status"), nil, nil, &out)
	return &out, err
}

func (c *Client) Logs(ctx context.Context, space, app, component string, follow bool, tail int, since time.Duration, out io.Writer) error {
	q := url.Values{}
	q.Set("component", component)
	q.Set("tail", fmt.Sprint(tail))
	if follow {
		q.Set("follow", "true")
	}
	if since > 0 {
		q.Set("since", since.String())
	}
	req, err := c.req(ctx, http.MethodGet, appPath(space, app, "/logs"), q, nil)
	if err != nil {
		return err
	}
	resp, err := c.http.Do(req)
	if err != nil {
		return fmt.Errorf("couldn't reach the donkey server: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 400 {
		return apiError(resp)
	}
	_, err = io.Copy(out, resp.Body)
	return err
}

func (c *Client) GetEnv(ctx context.Context, space, app string) (map[string]string, error) {
	var out struct {
		Env map[string]string `json:"env"`
	}
	err := c.doJSON(ctx, http.MethodGet, appPath(space, app, "/env"), nil, nil, &out)
	return out.Env, err
}

func (c *Client) PutEnv(ctx context.Context, space, app string, env map[string]string, restart bool) (bool, error) {
	var out struct {
		Restarted bool `json:"restarted"`
	}
	err := c.doJSON(ctx, http.MethodPut, appPath(space, app, "/env"), nil, server.PutEnvRequest{Env: env, Restart: restart}, &out)
	return out.Restarted, err
}

// AddAddonResult says what connecting a service changed: whether the space
// service was created on this occasion, and whether the running app was
// restarted with the binding.
type AddAddonResult struct {
	Created   bool `json:"created"`
	Restarted bool `json:"restarted"`
}

func (c *Client) AddAddon(ctx context.Context, space, app string, req server.AddonRequest) (AddAddonResult, error) {
	var out AddAddonResult
	err := c.doJSON(ctx, http.MethodPost, appPath(space, app, "/services"), nil, req, &out)
	return out, err
}

// RemoveAddon detaches the app from a space service; the service keeps
// running for the space.
func (c *Client) RemoveAddon(ctx context.Context, space, app, addon string) error {
	return c.doJSON(ctx, http.MethodDelete, appPath(space, app, "/services/"+addon), nil, nil, nil)
}

// SpaceAddon is one service of a space and the apps using it.
type SpaceAddon struct {
	Name       string   `json:"name"`
	Keys       []string `json:"keys"`
	AttachedTo []string `json:"attachedTo"`
}

func (c *Client) SpaceAddons(ctx context.Context, space string) ([]SpaceAddon, error) {
	var out struct {
		Addons []SpaceAddon `json:"addons"`
	}
	err := c.doJSON(ctx, http.MethodGet, "/v1/spaces/"+url.PathEscape(space)+"/services", nil, nil, &out)
	return out.Addons, err
}

// AddonBindingInfo is a service's contract with values, plus its in-cluster
// address (empty for one running elsewhere).
type AddonBindingInfo struct {
	Binding map[string]string `json:"binding"`
	Host    string            `json:"host"`
	Port    int               `json:"port"`
}

func (c *Client) AddonBinding(ctx context.Context, space, addon string) (AddonBindingInfo, error) {
	var out AddonBindingInfo
	err := c.doJSON(ctx, http.MethodGet,
		"/v1/spaces/"+url.PathEscape(space)+"/services/"+url.PathEscape(addon)+"/binding", nil, nil, &out)
	return out, err
}

// DialAddonTunnel opens one carried TCP connection to a space service — the
// server bridges the WebSocket onto the service's port.
func (c *Client) DialAddonTunnel(ctx context.Context, space, addon string) (*websocket.Conn, error) {
	wsURL := strings.Replace(c.BaseURL, "http", "ws", 1) +
		"/v1/spaces/" + url.PathEscape(space) + "/services/" + url.PathEscape(addon) + "/tunnel"
	dialer := websocket.Dialer{
		Proxy:            http.ProxyFromEnvironment,
		HandshakeTimeout: 15 * time.Second,
	}
	// The tunnel must trust the same private CA the rest of the client does —
	// but with the HTTP/2 protocol advertisement stripped: net/http adds "h2"
	// to the shared config, and a WebSocket cannot ride on it.
	if tr, ok := c.http.Transport.(*http.Transport); ok && tr != nil && tr.TLSClientConfig != nil {
		cfg := tr.TLSClientConfig.Clone()
		cfg.NextProtos = nil
		dialer.TLSClientConfig = cfg
	}
	conn, resp, err := dialer.DialContext(ctx, wsURL, http.Header{"Authorization": {"Bearer " + c.Token}})
	if err != nil {
		if resp != nil {
			return nil, apiError(resp)
		}
		return nil, fmt.Errorf("couldn't open the tunnel: %w", err)
	}
	return conn, nil
}

// RemoveSpaceAddon removes a service from the space, detaching every app
// that uses it.
func (c *Client) RemoveSpaceAddon(ctx context.Context, space, addon string, keepData bool) error {
	q := url.Values{}
	if keepData {
		q.Set("keepData", "true")
	}
	return c.doJSON(ctx, http.MethodDelete, "/v1/spaces/"+url.PathEscape(space)+"/services/"+addon, q, nil, nil)
}

func (c *Client) Destroy(ctx context.Context, space, app string, keepData bool) error {
	q := url.Values{}
	if keepData {
		q.Set("keepData", "true")
	}
	return c.doJSON(ctx, http.MethodDelete, appPath(space, app, ""), q, nil, nil)
}

// Apps lists everything deployed, across all spaces.
func (c *Client) Apps(ctx context.Context) ([]server.AppListEntry, error) {
	var out struct {
		Apps []server.AppListEntry `json:"apps"`
	}
	err := c.doJSON(ctx, http.MethodGet, "/v1/apps", nil, nil, &out)
	return out.Apps, err
}

func (c *Client) Doctor(ctx context.Context) ([]server.DoctorCheck, string, error) {
	var out struct {
		Checks []server.DoctorCheck `json:"checks"`
		Domain string               `json:"domain"`
	}
	err := c.doJSON(ctx, http.MethodGet, "/v1/doctor", nil, nil, &out)
	return out.Checks, out.Domain, err
}

// Analyze asks the server's assistant to read a repository summary. The CLI
// never talks to OpenAI itself — only the server holds the key.
func (c *Client) Analyze(ctx context.Context, in *ai.AnalyzeInput) (*ai.Analysis, error) {
	var out ai.Analysis
	err := c.doJSON(ctx, http.MethodPost, "/v1/ai/analyze", nil, in, &out)
	return &out, err
}
