//go:build e2e

// Package e2e tests a RUNNING donkey installation end to end, through the
// same doors real users take: the server API (the CLI's client) and the
// gateway's https edge (a browser's view). It is runtime-agnostic — the same
// suite verifies a kind and a docker installation, because it never talks to
// Kubernetes or Docker directly.
//
// It needs (hack/verify.sh sets all of this up):
//
//	DONKEY_E2E_SERVER       the API base URL (direct or port-forwarded)
//	DONKEY_E2E_SESSION_KEY  the installation's session key (hex) — the suite
//	                        mints its own token, like `donkey login` would
//	DONKEY_E2E_DOMAIN       the installation's app domain
//	DONKEY_E2E_GATEWAY      host:port of the https edge (optional — without
//	                        it the through-the-gateway tests are skipped)
//	DONKEY_E2E_CA           PEM file the gateway's certificates chain to
//
// Run: go test -tags e2e -count=1 -timeout 30m ./e2e
package e2e

import (
	"bytes"
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/adrianliechti/donkey/internal/apiclient"
	"github.com/adrianliechti/donkey/internal/identity"
	"github.com/adrianliechti/donkey/internal/server"
)

const (
	space = "e2e"
	app   = "e2e-web"
)

type suite struct {
	api    *apiclient.Client
	server string
	token  string
	domain string
	// edge dials every hostname at the gateway's one address — curl's
	// --resolve, in Go. Nil when DONKEY_E2E_GATEWAY isn't set.
	edge *http.Client
}

func newSuite(t *testing.T) *suite {
	t.Helper()
	server := os.Getenv("DONKEY_E2E_SERVER")
	if server == "" {
		t.Skip("DONKEY_E2E_SERVER not set — the e2e suite needs a running installation (task verify)")
	}
	key, err := hex.DecodeString(os.Getenv("DONKEY_E2E_SESSION_KEY"))
	if err != nil || len(key) != 32 {
		t.Fatal("DONKEY_E2E_SESSION_KEY must be 32 bytes of hex (the installation's session key)")
	}
	token, err := identity.SealToken(key, identity.Identity{
		Subject: "e2e",
		Email:   "e2e@localhost",
		Expires: time.Now().Add(2 * time.Hour).Unix(),
	})
	if err != nil {
		t.Fatal(err)
	}
	domain := os.Getenv("DONKEY_E2E_DOMAIN")
	if domain == "" {
		t.Fatal("DONKEY_E2E_DOMAIN must name the installation's app domain")
	}
	api, err := apiclient.New(server, token, os.Getenv("DONKEY_E2E_CA"))
	if err != nil {
		t.Fatal(err)
	}
	s := &suite{api: api, server: server, token: token, domain: domain}
	if gw := os.Getenv("DONKEY_E2E_GATEWAY"); gw != "" {
		tlsCfg := &tls.Config{}
		if caFile := os.Getenv("DONKEY_E2E_CA"); caFile != "" {
			pem, err := os.ReadFile(caFile)
			if err != nil {
				t.Fatal(err)
			}
			pool := x509.NewCertPool()
			pool.AppendCertsFromPEM(pem)
			tlsCfg.RootCAs = pool
		}
		dialer := &net.Dialer{Timeout: 10 * time.Second}
		s.edge = &http.Client{
			Timeout: 30 * time.Second,
			// Redirects are assertions here (to the IdP, to https), never
			// something to follow.
			CheckRedirect: func(*http.Request, []*http.Request) error {
				return http.ErrUseLastResponse
			},
			Transport: &http.Transport{
				TLSClientConfig: tlsCfg,
				DialContext: func(ctx context.Context, network, _ string) (net.Conn, error) {
					return dialer.DialContext(ctx, network, gw)
				},
			},
		}
	}
	return s
}

// get fetches one app URL through the gateway; withToken rides the sealed
// CLI token as Bearer, the way `donkey open`'s curl examples do.
func (s *suite) get(t *testing.T, url string, withToken bool) (*http.Response, string) {
	t.Helper()
	req, err := http.NewRequest(http.MethodGet, url, nil)
	if err != nil {
		t.Fatal(err)
	}
	if withToken {
		req.Header.Set("Authorization", "Bearer "+s.token)
	}
	resp, err := s.edge.Do(req)
	if err != nil {
		t.Fatalf("GET %s: %v", url, err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 64<<10))
	return resp, string(body)
}

// waitBody polls a URL through the gateway until the body contains want.
func (s *suite) waitBody(t *testing.T, url, want string, timeout time.Duration) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	var last string
	for time.Now().Before(deadline) {
		resp, body := s.get(t, url, true)
		last = fmt.Sprintf("%d: %s", resp.StatusCode, body)
		if resp.StatusCode == http.StatusOK && strings.Contains(body, want) {
			return
		}
		time.Sleep(2 * time.Second)
	}
	t.Fatalf("%s never served %q; last response: %s", url, want, last)
}

// raw calls a server endpoint the CLI has no verb for (network, spaces).
func (s *suite) raw(t *testing.T, method, path string, body any, out any) *http.Response {
	t.Helper()
	var rd io.Reader
	if body != nil {
		b, err := json.Marshal(body)
		if err != nil {
			t.Fatal(err)
		}
		rd = bytes.NewReader(b)
	}
	req, err := http.NewRequest(method, s.server+path, rd)
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Authorization", "Bearer "+s.token)
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	httpClient := &http.Client{Timeout: 5 * time.Minute}
	if ca := os.Getenv("DONKEY_E2E_CA"); ca != "" {
		pem, err := os.ReadFile(ca)
		if err != nil {
			t.Fatal(err)
		}
		pool := x509.NewCertPool()
		pool.AppendCertsFromPEM(pem)
		tr := http.DefaultTransport.(*http.Transport).Clone()
		tr.TLSClientConfig = &tls.Config{RootCAs: pool}
		httpClient.Transport = tr
	}
	resp, err := httpClient.Do(req)
	if err != nil {
		t.Fatalf("%s %s: %v", method, path, err)
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("%s %s: %d — %s", method, path, resp.StatusCode, raw)
	}
	if out != nil {
		if err := json.Unmarshal(raw, out); err != nil {
			t.Fatalf("%s %s: %v in %s", method, path, err, raw)
		}
	}
	return resp
}

func (s *suite) appURL(path string) string {
	return "https://" + app + "." + space + "." + s.domain + path
}

// TestEndToEnd is one ordered story: deploy → reach → change → destroy.
// Subtests share the suite and depend on each other — the price of testing a
// real installation instead of mocks.
func TestEndToEnd(t *testing.T) {
	s := newSuite(t)
	ctx := context.Background()

	t.Run("ping", func(t *testing.T) {
		if err := s.api.Ping(ctx); err != nil {
			t.Fatalf("the server didn't accept the connection: %v", err)
		}
	})

	t.Run("doctor", func(t *testing.T) {
		checks, _, err := s.api.Doctor(ctx)
		if err != nil {
			t.Fatal(err)
		}
		for _, c := range checks {
			if !c.OK {
				t.Errorf("doctor: %s — %s", c.Name, c.Detail)
			}
		}
	})

	t.Run("deploy", func(t *testing.T) {
		dir := t.TempDir()
		copyDir(t, filepath.Join("..", "testdata", "fastapi-sample"), dir)
		writeFile(t, filepath.Join(dir, "donkey.yaml"), fmt.Sprintf(
			"name: %s\nspace: %s\nport: 8000\nhealthcheck:\n  path: /healthz\nstorage:\n  size: 1Gi\n  paths:\n    - /app/data\n    - /app/uploads\n", app, space))

		var tarBuf bytes.Buffer
		if _, err := apiclient.TarContext(dir, &tarBuf); err != nil {
			t.Fatal(err)
		}
		deadline, cancel := context.WithTimeout(ctx, 15*time.Minute)
		defer cancel()
		res, err := s.api.Deploy(deadline, &tarBuf, apiclient.DeployOpts{Space: space},
			func(line string) { t.Log(line) }, func(string) {})
		if err != nil {
			t.Fatalf("deploy failed: %v", err)
		}
		if !res.OK {
			t.Fatalf("deploy failed: %s\n%s", res.Error, res.Diagnosis)
		}
		if res.URL == "" {
			t.Fatal("a routed app should get a URL")
		}
	})

	t.Run("status", func(t *testing.T) {
		st, err := s.api.Status(ctx, space, app)
		if err != nil {
			t.Fatal(err)
		}
		if !st.Found || !st.Healthy {
			t.Fatalf("the app should be healthy: %+v", st)
		}
		if !st.HasStorage {
			t.Error("the app should own a disk")
		}
		if st.Exposure != "protected" {
			t.Errorf("apps are login-protected by default, got %q", st.Exposure)
		}
	})

	t.Run("apps", func(t *testing.T) {
		apps, err := s.api.Apps(ctx)
		if err != nil {
			t.Fatal(err)
		}
		for _, a := range apps {
			if a.Name == app && a.Space == space {
				return
			}
		}
		t.Fatalf("the overview should list %s/%s", space, app)
	})

	t.Run("logs", func(t *testing.T) {
		var buf bytes.Buffer
		if err := s.api.Logs(ctx, space, app, "web", false, 50, 0, &buf); err != nil {
			t.Fatal(err)
		}
		if buf.Len() == 0 {
			t.Error("a running app should have logs")
		}
	})

	if s.edge == nil {
		t.Log("DONKEY_E2E_GATEWAY not set — skipping the through-the-gateway tests")
	} else {
		t.Run("gateway/login-redirect", func(t *testing.T) {
			// A browser without a session is sent into the login.
			req, err := http.NewRequest(http.MethodGet, s.appURL("/healthz"), nil)
			if err != nil {
				t.Fatal(err)
			}
			req.Header.Set("Accept", "text/html")
			resp, err := s.edge.Do(req)
			if err != nil {
				t.Fatal(err)
			}
			resp.Body.Close()
			if resp.StatusCode < 300 || resp.StatusCode >= 400 {
				t.Fatalf("a protected app should redirect browsers to the login, got %d", resp.StatusCode)
			}
			if resp.Header.Get("Location") == "" {
				t.Fatal("the redirect should say where the login is")
			}
		})

		t.Run("gateway/api-unauthorized", func(t *testing.T) {
			// Non-browser clients get a plain 401, never a redirect chain.
			resp, _ := s.get(t, s.appURL("/healthz"), false)
			if resp.StatusCode != http.StatusUnauthorized {
				t.Fatalf("an API call without identity should get 401, got %d", resp.StatusCode)
			}
		})

		t.Run("gateway/token-access", func(t *testing.T) {
			s.waitBody(t, s.appURL("/healthz"), `"ok":true`, time.Minute)
		})

		t.Run("env", func(t *testing.T) {
			if _, err := s.api.PutEnv(ctx, space, app, map[string]string{"GREETING": "hi-from-e2e"}, true); err != nil {
				t.Fatal(err)
			}
			s.waitBody(t, s.appURL("/"), "hi-from-e2e", 3*time.Minute)
		})

		t.Run("addon", func(t *testing.T) {
			deadline, cancel := context.WithTimeout(ctx, 5*time.Minute)
			defer cancel()
			if _, err := s.api.AddAddon(deadline, space, app, server.AddonRequest{Type: "postgres", Name: "postgres"}); err != nil {
				t.Fatal(err)
			}
			s.waitBody(t, s.appURL("/db-check"), `"database":"ok"`, 3*time.Minute)

			addons, err := s.api.SpaceAddons(ctx, space)
			if err != nil {
				t.Fatal(err)
			}
			found := false
			for _, a := range addons {
				if a.Name == "postgres" {
					found = true
					for _, attached := range a.AttachedTo {
						if attached == app {
							return
						}
					}
				}
			}
			if !found {
				t.Fatal("the space should list its postgres")
			}
			t.Fatal("postgres should be attached to the app")
		})

		t.Run("network/public", func(t *testing.T) {
			s.raw(t, http.MethodPut, "/v1/spaces/"+space+"/apps/"+app+"/network",
				map[string]string{"exposure": "public"}, nil)
			deadline := time.Now().Add(time.Minute)
			for {
				resp, _ := s.get(t, s.appURL("/healthz"), false)
				if resp.StatusCode == http.StatusOK {
					break
				}
				if time.Now().After(deadline) {
					t.Fatalf("a public app should serve without login, got %d", resp.StatusCode)
				}
				time.Sleep(2 * time.Second)
			}
		})

		t.Run("network/internal", func(t *testing.T) {
			s.raw(t, http.MethodPut, "/v1/spaces/"+space+"/apps/"+app+"/network",
				map[string]string{"exposure": "internal"}, nil)
			deadline := time.Now().Add(time.Minute)
			for {
				resp, _ := s.get(t, s.appURL("/healthz"), true)
				if resp.StatusCode == http.StatusNotFound {
					break
				}
				if time.Now().After(deadline) {
					t.Fatalf("an internal app should vanish from the edge, got %d", resp.StatusCode)
				}
				time.Sleep(2 * time.Second)
			}
			s.raw(t, http.MethodPut, "/v1/spaces/"+space+"/apps/"+app+"/network",
				map[string]string{"exposure": "protected"}, nil)
		})
	}

	t.Run("files", func(t *testing.T) {
		if err := s.api.UploadFile(ctx, space, app, "app-data/hello.txt", strings.NewReader("hello from e2e")); err != nil {
			t.Fatal(err)
		}
		listing, err := s.api.ListFiles(ctx, space, app, "app-data")
		if err != nil {
			t.Fatal(err)
		}
		found := false
		for _, e := range listing.Entries {
			if e.Name == "hello.txt" {
				found = true
			}
		}
		if !found {
			t.Fatalf("the upload should appear in the listing: %+v", listing.Entries)
		}
		var buf bytes.Buffer
		if err := s.api.DownloadFile(ctx, space, app, "app-data/hello.txt", &buf); err != nil {
			t.Fatal(err)
		}
		if buf.String() != "hello from e2e" {
			t.Fatalf("the download should match the upload, got %q", buf.String())
		}
	})

	t.Run("destroy", func(t *testing.T) {
		if err := s.api.Destroy(ctx, space, app, false); err != nil {
			t.Fatal(err)
		}
		deadline := time.Now().Add(2 * time.Minute)
		for {
			st, err := s.api.Status(ctx, space, app)
			if err == nil && !st.Found {
				break
			}
			if time.Now().After(deadline) {
				t.Fatalf("the app should be gone after destroy: %+v (%v)", st, err)
			}
			time.Sleep(2 * time.Second)
		}
	})

	t.Run("remove-space", func(t *testing.T) {
		// The space still holds its postgres (and its data) — removing the
		// space takes them along.
		if err := s.api.RemoveSpaceAddon(ctx, space, "postgres", false); err != nil {
			t.Log(err) // the addon subtest may have been skipped
		}
		deadline := time.Now().Add(2 * time.Minute)
		for {
			var out struct {
				Spaces []string `json:"spaces"`
			}
			s.raw(t, http.MethodDelete, "/v1/spaces/"+space, nil, nil)
			s.raw(t, http.MethodGet, "/v1/spaces", nil, &out)
			gone := true
			for _, sp := range out.Spaces {
				if sp == space {
					gone = false
				}
			}
			if gone {
				return
			}
			if time.Now().After(deadline) {
				t.Fatalf("the space should be gone, still listed: %v", out.Spaces)
			}
			time.Sleep(2 * time.Second)
		}
	})
}

func copyDir(t *testing.T, from, to string) {
	t.Helper()
	entries, err := os.ReadDir(from)
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range entries {
		if e.IsDir() {
			sub := filepath.Join(to, e.Name())
			if err := os.MkdirAll(sub, 0o755); err != nil {
				t.Fatal(err)
			}
			copyDir(t, filepath.Join(from, e.Name()), sub)
			continue
		}
		b, err := os.ReadFile(filepath.Join(from, e.Name()))
		if err != nil {
			t.Fatal(err)
		}
		writeFile(t, filepath.Join(to, e.Name()), string(b))
	}
}

func writeFile(t *testing.T, path, content string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}
