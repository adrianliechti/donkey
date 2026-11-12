package apiclient

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httputil"
	"net/url"
	"strings"

	"github.com/adrianliechti/donkey/internal/server"
)

// Packages checks that the installation's package mirror can be used from
// this machine: the server offers it, and the sign-in is good. Asked once
// before a package manager starts, so an expired login reads as one instead
// of as a registry error in the middle of an install.
func (c *Client) Packages(ctx context.Context) error {
	req, err := c.req(ctx, http.MethodGet, "/v1/packages/", nil, nil)
	if err != nil {
		return err
	}
	resp, err := c.http.Do(req)
	if err != nil {
		return fmt.Errorf("couldn't reach the donkey server at %s: %w", c.BaseURL, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 400 {
		return apiError(resp)
	}
	// A server from before the mirror was offered answers this path with the
	// console's page.
	var out struct {
		OK bool `json:"ok"`
	}
	if json.NewDecoder(resp.Body).Decode(&out) != nil || !out.OK {
		return fmt.Errorf("the donkey server at %s doesn't offer its package mirror to developer machines yet — it needs an update to match this CLI", c.BaseURL)
	}
	return nil
}

// PackagesProxy carries a package manager's requests to the installation's
// package mirror: what arrives on a local address leaves as a signed-in call
// to donkey-server, which names that address so the links in the answers
// lead back to it.
func (c *Client) PackagesProxy() (*httputil.ReverseProxy, error) {
	base, err := url.Parse(c.BaseURL + "/v1/packages")
	if err != nil {
		return nil, err
	}
	// An install asks for hundreds of packages at once. Where the server
	// speaks only HTTP/1.1, the default of two kept connections would redo
	// the TLS handshake for most of them.
	transport, _ := c.http.Transport.(*http.Transport)
	if transport == nil {
		transport = http.DefaultTransport.(*http.Transport)
	}
	transport = transport.Clone()
	transport.MaxIdleConnsPerHost = 32
	return &httputil.ReverseProxy{
		Rewrite: func(pr *httputil.ProxyRequest) {
			pr.SetURL(base)
			pr.Out.Header.Set(server.PackagesAddressHeader, pr.In.Host)
			// Whatever the package manager is configured to send a registry
			// (an .npmrc token, say) is not donkey's to receive. The
			// transport negotiates compression itself.
			pr.Out.Header.Set("Authorization", "Bearer "+c.Token)
			pr.Out.Header.Del("Cookie")
			pr.Out.Header.Del("Accept-Encoding")
		},
		Transport: transport,
		// The server tidies an untidy path ("//") by redirecting to the tidy
		// one — under its own route, which the local address does not have.
		ModifyResponse: func(res *http.Response) error {
			if location, ok := strings.CutPrefix(res.Header.Get("Location"), "/v1/packages/"); ok {
				res.Header.Set("Location", "/"+location)
			}
			return nil
		},
		ErrorHandler: func(w http.ResponseWriter, _ *http.Request, err error) {
			http.Error(w, fmt.Sprintf("donkey couldn't reach its server at %s: %v", c.BaseURL, err), http.StatusBadGateway)
		},
	}, nil
}
