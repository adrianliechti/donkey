//go:build e2e

package build

import (
	"context"
	"encoding/pem"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"net/http/httputil"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"

	bkclient "github.com/moby/buildkit/client"

	"github.com/adrianliechti/donkey/internal/ai"
	"github.com/adrianliechti/donkey/internal/config"
	"github.com/adrianliechti/donkey/internal/packages"
	"github.com/adrianliechti/donkey/pkg/runtime/kubernetes/manifests"
)

// Run without a Donkey installation or JFrog credentials:
//
// DONKEY_E2E_OS_PACKAGES=true go test -tags e2e -run '^TestOSPackageMirrors$' -v -timeout 20m ./internal/build
//
// The test owns a disposable BuildKit container. Real APT/APK clients install
// through donkey-packages and an authenticated, private-CA TLS mirror. The
// mirror forwards to the public archives, so Docker and internet are needed.
// DONKEY_E2E_BUILDER=true additionally exercises live model-generated recipes
// with OPENAI_API_KEY and DONKEY_MODEL, as in the existing builder suite.
func TestOSPackageMirrors(t *testing.T) {
	if os.Getenv("DONKEY_E2E_OS_PACKAGES") != "true" {
		t.Skip("set DONKEY_E2E_OS_PACKAGES=true to run real APT/APK builds in Docker")
	}
	ctx, cancel := context.WithTimeout(t.Context(), 18*time.Minute)
	defer cancel()
	addr, host := osPackageBuildkit(t, ctx)

	var mu sync.Mutex
	requests := map[string]int{}
	rejected := 0
	debian, _ := url.Parse("https://deb.debian.org/debian")
	alpine, _ := url.Parse("https://dl-cdn.alpinelinux.org/alpine")
	mirror := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		u, p, ok := r.BasicAuth()
		if !ok || u != "e2e-reader" || p != "e2e-token" {
			mu.Lock()
			rejected++
			mu.Unlock()
			w.Header().Set("WWW-Authenticate", `Basic realm="os-package-e2e"`)
			http.Error(w, "authentication required", http.StatusUnauthorized)
			return
		}
		mu.Lock()
		requests[r.URL.Path]++
		mu.Unlock()
		root, rest, _ := strings.Cut(strings.TrimPrefix(r.URL.Path, "/"), "/")
		var upstream *url.URL
		switch root {
		case "debian", "debian-unsigned":
			upstream = debian
			if root == "debian-unsigned" && (strings.HasSuffix(rest, "/InRelease") || strings.HasSuffix(rest, "/Release.gpg")) {
				http.NotFound(w, r)
				return
			}
		case "alpine":
			upstream = alpine
		default:
			http.NotFound(w, r)
			return
		}
		r.URL.Path, r.URL.RawPath = "/"+rest, ""
		r.Header.Del("Authorization")
		proxy := &httputil.ReverseProxy{Rewrite: func(pr *httputil.ProxyRequest) {
			pr.SetURL(upstream)
		}}
		proxy.ServeHTTP(w, r)
	}))
	t.Cleanup(mirror.Close)
	caFile := filepath.Join(t.TempDir(), "mirror-ca.crt")
	if err := os.WriteFile(caFile, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: mirror.Certificate().Raw}), 0o600); err != nil {
		t.Fatal(err)
	}
	mirrorURL, _ := url.Parse(mirror.URL)
	countArtifacts := func(root, suffix string) int {
		mu.Lock()
		defer mu.Unlock()
		count := 0
		for path, n := range requests {
			if strings.HasPrefix(path, root+"/") && strings.HasSuffix(path, suffix) {
				count += n
			}
		}
		return count
	}

	for _, tc := range []struct {
		name, root, suffix, recipe    string
		wantFailure, wrongCredentials bool
	}{
		{"debian-signed", "/debian", ".deb", debianPackageProbe(""), false, false},
		{"alpine-signed", "/alpine", ".apk", alpinePackageProbe, false, false},
		{"debian-unsigned-refused", "/debian-unsigned", ".deb", debianPackageProbe(""), true, false},
		{"debian-unsigned-update-only-refused", "/debian-unsigned", ".deb", debianPackageProbe("update-only"), true, false},
		{"debian-unsigned-install-only-refused", "/debian-unsigned", ".deb", debianPackageProbe("install-only"), true, false},
		{"debian-unsigned-allowed", "/debian-unsigned", ".deb", debianPackageProbe("unsigned"), false, false},
		{"alpine-wrong-credentials", "/alpine", ".apk", alpinePackageProbe, true, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cfg := osPackageProxyConfig(mirror.URL, mirrorURL.Host, caFile, tc.root)
			if tc.wrongCredentials {
				cfg.Password = "wrong-token"
			}
			endpoint := runOSPackageProxy(t, ctx, host, cfg)
			builder := &BuildKitBuilder{BuildkitAddr: addr, PackagesURL: endpoint,
				Settings: config.BuildSettings{DebianMirror: true, AlpineMirror: true, Provenance: "off"}}
			before := countArtifacts(tc.root, tc.suffix)
			mu.Lock()
			rejectedBefore := rejected
			mu.Unlock()
			err := buildOSPackageProbe(t, ctx, builder, tc.recipe, nil)
			if tc.wantFailure {
				if err == nil {
					t.Fatal("untrusted archive or wrong credential unexpectedly built successfully")
				}
				evidence := "not signed"
				if strings.Contains(tc.name, "update-only") {
					evidence = "unauthenticated packages"
				}
				if tc.wrongCredentials {
					mu.Lock()
					denied := rejected > rejectedBefore
					mu.Unlock()
					if !denied {
						t.Fatalf("build failed without reaching the mirror's authentication check: %v", err)
					}
					return
				}
				if !strings.Contains(err.Error(), evidence) {
					t.Fatalf("build failed for an unrelated reason; want %q: %v", evidence, err)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			if countArtifacts(tc.root, tc.suffix) <= before {
				t.Fatal("build succeeded without downloading a package through the authenticated mirror")
			}
		})
	}

	t.Run("live-model", func(t *testing.T) {
		if os.Getenv("DONKEY_E2E_BUILDER") != "true" {
			t.Skip("set DONKEY_E2E_BUILDER=true with OPENAI_API_KEY and DONKEY_MODEL to verify generated recipes")
		}
		assistant, err := ai.New(os.Getenv("DONKEY_MODEL"))
		if err != nil {
			t.Fatal(err)
		}
		for _, tc := range []struct{ name, base, root, suffix string }{
			{"debian", "python:3.12-slim-bookworm", "/debian-unsigned", ".deb"},
			{"alpine", "python:3.12-alpine3.21", "/alpine", ".apk"},
		} {
			t.Run(tc.name, func(t *testing.T) {
				dir := t.TempDir()
				source := "import ctypes, os\nfrom http.server import HTTPServer, BaseHTTPRequestHandler\nctypes.CDLL('libjansson.so.4')\nclass Handler(BaseHTTPRequestHandler):\n def do_GET(self):\n  self.send_response(200); self.end_headers(); self.wfile.write(b'jansson-ok')\nHTTPServer(('0.0.0.0', int(os.environ.get('PORT', '8080'))), Handler).serve_forever()\n"
				if err := os.WriteFile(filepath.Join(dir, "server.py"), []byte(source), 0o644); err != nil {
					t.Fatal(err)
				}
				summary, err := ai.Summarize(dir)
				if err != nil {
					t.Fatal(err)
				}
				instructions := "Use base image " + tc.base + ". Install the OS package providing libjansson.so.4. For Debian mirror packages allow unsigned repositories: apt-get update -o Acquire::AllowInsecureRepositories=true and apt-get install -y --allow-unauthenticated."
				result, err := assistant.WriteDockerfile(ctx, &ai.DockerfileInput{Summary: summary, Root: dir, Port: 8080, Instructions: instructions})
				if err != nil {
					t.Fatal(err)
				}
				t.Log(result.Dockerfile)
				endpoint := runOSPackageProxy(t, ctx, host, osPackageProxyConfig(mirror.URL, mirrorURL.Host, caFile, tc.root))
				builder := &BuildKitBuilder{BuildkitAddr: addr, PackagesURL: endpoint, Settings: config.BuildSettings{DebianMirror: true, AlpineMirror: true, Provenance: "off"}}
				before := countArtifacts(tc.root, tc.suffix)
				if err := os.WriteFile(filepath.Join(dir, "Dockerfile"), []byte(result.Dockerfile), 0o600); err != nil {
					t.Fatal(err)
				}
				_, err = builder.Build(ctx, Request{ContextDir: dir, DockerfilePath: "Dockerfile", Strategy: config.BuildDockerfile,
					ImageRef: "example.invalid/os-packages:live-" + tc.name, Platform: "linux/" + runtime.GOARCH,
					Verify: &VerifySpec{Port: 8080, Seconds: 3, HealthcheckPath: "/"}, Progress: ProgressSink{LogPath: filepath.Join(dir, "build.log")}})
				if err != nil {
					t.Fatal(err)
				}
				if countArtifacts(tc.root, tc.suffix) <= before {
					t.Fatal("generated recipe bypassed the mirror")
				}
			})
		}
	})
}

func osPackageProxyConfig(mirror, host, caFile, debianRoot string) packages.Config {
	if !strings.HasPrefix(debianRoot, "/debian") {
		debianRoot = "/debian"
	}
	return packages.Config{NPMUpstream: config.DefaultNPMUpstream, PyPIUpstream: config.DefaultPyPIUpstream,
		GoUpstream: config.DefaultGoUpstream, MavenUpstream: config.DefaultMavenUpstream, NuGetUpstream: config.DefaultNuGetUpstream,
		DebianUpstream: mirror + debianRoot, AlpineUpstream: mirror + "/alpine", CredentialHosts: []string{host},
		Username: "e2e-reader", Password: "e2e-token", CAFile: caFile}
}

func runOSPackageProxy(t *testing.T, ctx context.Context, host string, cfg packages.Config) string {
	t.Helper()
	listener, err := net.Listen("tcp", "0.0.0.0:0")
	if err != nil {
		t.Fatal(err)
	}
	port := listener.Addr().(*net.TCPAddr).Port
	listener.Close()
	cfg.Listen = fmt.Sprintf(":%d", port)
	proxyCtx, cancel := context.WithCancel(ctx)
	done := make(chan error, 1)
	go func() { done <- packages.Run(proxyCtx, &cfg) }()
	t.Cleanup(func() {
		cancel()
		if err := <-done; err != nil {
			t.Errorf("stopping proxy: %v", err)
		}
	})
	client := &http.Client{Timeout: time.Second}
	for deadline := time.Now().Add(10 * time.Second); time.Now().Before(deadline); {
		res, err := client.Get(fmt.Sprintf("http://127.0.0.1:%d/healthz", port))
		if err == nil {
			res.Body.Close()
			return fmt.Sprintf("http://%s:%d", host, port)
		}
		time.Sleep(100 * time.Millisecond)
	}
	t.Fatal("package proxy did not start")
	return ""
}

func osPackageBuildkit(t *testing.T, ctx context.Context) (string, string) {
	t.Helper()
	name := fmt.Sprintf("donkey-os-packages-e2e-%d", time.Now().UnixNano())
	docker := func(args ...string) string {
		t.Helper()
		out, err := exec.CommandContext(ctx, "docker", args...).CombinedOutput()
		if err != nil {
			t.Fatalf("docker %v: %v\n%s", args, err, out)
		}
		return strings.TrimSpace(string(out))
	}
	// donkey's own config file, so the engine garbage-collects the way an
	// installation's does — the scanner's database cache depends on it.
	configDir := t.TempDir()
	if err := os.WriteFile(filepath.Join(configDir, "buildkitd.toml"), []byte(manifests.BuildkitTOML(manifests.BuildkitConfig{})), 0o644); err != nil {
		t.Fatal(err)
	}
	docker("run", "--detach", "--rm", "--privileged", "--name", name, "--publish", "127.0.0.1::1234",
		"--volume", configDir+":/etc/buildkit:ro",
		config.DefaultBuildkitImage, "--addr", "tcp://0.0.0.0:1234", "--config", "/etc/buildkit/buildkitd.toml")
	t.Cleanup(func() {
		cleanupCtx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		if out, err := exec.CommandContext(cleanupCtx, "docker", "rm", "--force", name).CombinedOutput(); err != nil {
			t.Errorf("removing test BuildKit: %v: %s", err, out)
		}
	})
	addr := "tcp://" + docker("port", name, "1234/tcp")
	host := "host.docker.internal"
	if runtime.GOOS == "linux" {
		host = docker("inspect", "--format", "{{range .NetworkSettings.Networks}}{{.Gateway}}{{end}}", name)
	}
	client, err := bkclient.New(ctx, addr)
	if err != nil {
		t.Fatal(err)
	}
	defer client.Close()
	ready, cancel := context.WithTimeout(ctx, time.Minute)
	defer cancel()
	if err := client.Wait(ready); err != nil {
		t.Fatal(err)
	}
	return addr, host
}

func buildOSPackageProbe(t *testing.T, ctx context.Context, b *BuildKitBuilder, recipe string, verify *VerifySpec) error {
	t.Helper()
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "Dockerfile"), []byte(recipe), 0o600); err != nil {
		t.Fatal(err)
	}
	logPath := filepath.Join(dir, "build.log")
	_, err := b.Build(ctx, Request{ContextDir: dir, DockerfilePath: "Dockerfile", Strategy: config.BuildDockerfile,
		ImageRef: "example.invalid/os-packages:probe", Platform: "linux/" + runtime.GOARCH,
		Verify: verify, Progress: ProgressSink{LogPath: logPath}})
	contents, _ := os.ReadFile(logPath)
	t.Log(string(contents))
	if err != nil {
		return fmt.Errorf("%w\n%s", err, contents)
	}
	return nil
}

func debianPackageProbe(unsigned string) string {
	update, install := "", ""
	if unsigned == "unsigned" || unsigned == "update-only" {
		update = "-o Acquire::AllowInsecureRepositories=true"
	}
	if unsigned == "unsigned" || unsigned == "install-only" {
		install = "--allow-unauthenticated"
	}
	return fmt.Sprintf(`FROM debian:bookworm-slim
RUN test -n "$DONKEY_DEBIAN_MIRROR" \
 && . /etc/os-release \
 && rm -f /etc/apt/sources.list.d/debian.sources /etc/apt/sources.list.d/debian.list \
 && printf 'deb %%s %%s main\n' "$DONKEY_DEBIAN_MIRROR" "$VERSION_CODENAME" > /etc/apt/sources.list \
 && apt-get update %s \
 && apt-get install -y --no-install-recommends %s libjansson4 \
 && dpkg-query -W libjansson4
`, update, install)
}

const alpinePackageProbe = `FROM alpine:3.21
RUN test -n "$DONKEY_ALPINE_MIRROR" \
 && sed -i -E "s#https?://.*/(v[0-9]+\\.[0-9]+|edge)/#${DONKEY_ALPINE_MIRROR}/\\1/#" /etc/apk/repositories \
 && apk add --no-cache jansson \
 && apk info -e jansson
`
