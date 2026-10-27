//go:build e2e

// Package testenv provides disposable engines for integration tests.
package testenv

import (
	"context"
	"fmt"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	bkclient "github.com/moby/buildkit/client"

	"github.com/adrianliechti/donkey/internal/config"
	"github.com/adrianliechti/donkey/internal/packages"
	"github.com/adrianliechti/donkey/pkg/runtime/kubernetes/manifests"
)

// Buildkit starts an isolated engine and removes it when the test completes.
func Buildkit(t *testing.T, ctx context.Context) (string, string) {
	t.Helper()
	name := fmt.Sprintf("donkey-build-e2e-%d", time.Now().UnixNano())
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
	configuration := manifests.BuildkitTOML(manifests.BuildkitConfig{})
	args := []string{"run", "--detach", "--rm", "--privileged", "--name", name, "--publish", "127.0.0.1::1234", "--volume", configDir + ":/etc/buildkit:ro"}
	// An optional test-only resolver keeps local DNS failures out of live
	// registry tests, without changing Docker or the host's configuration.
	if dns := os.Getenv("DONKEY_E2E_DNS"); dns != "" {
		if net.ParseIP(dns) == nil {
			t.Fatal("DONKEY_E2E_DNS must be a DNS server IP address")
		}
		args = append(args, "--dns", dns)
		configuration += fmt.Sprintf("\n[dns]\n  nameservers = [%q]\n", dns)
		original := net.DefaultResolver
		net.DefaultResolver = &net.Resolver{PreferGo: true, Dial: func(ctx context.Context, network, _ string) (net.Conn, error) {
			return (&net.Dialer{}).DialContext(ctx, network, net.JoinHostPort(dns, "53"))
		}}
		t.Cleanup(func() { net.DefaultResolver = original })
	}
	if err := os.WriteFile(filepath.Join(configDir, "buildkitd.toml"), []byte(configuration), 0o644); err != nil {
		t.Fatal(err)
	}
	args = append(args, config.DefaultBuildkitImage, "--addr", "tcp://0.0.0.0:1234", "--config", "/etc/buildkit/buildkitd.toml")
	docker(args...)
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
	} else if os.Getenv("DONKEY_E2E_DNS") != "" {
		// Public resolvers cannot resolve Docker Desktop's private host name.
		// Resolve it once using Docker's default DNS, then use the address.
		resolved := strings.Fields(docker("run", "--rm", "--entrypoint", "getent", config.DefaultBuildkitImage, "hosts", host))
		if len(resolved) == 0 || net.ParseIP(resolved[0]) == nil {
			t.Fatal("Docker Desktop did not resolve the host address")
		}
		host = resolved[0]
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

// Trivy runs the real scanner in Docker, sharing its database with the host.
func Trivy(t *testing.T) (string, func(context.Context, string, []string, []string) ([]byte, error)) {
	t.Helper()
	dir := filepath.Join(t.TempDir(), "scanner")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	return dir, func(ctx context.Context, work string, args []string, env []string) ([]byte, error) {
		mapped := make([]string, 0, len(args))
		for i := 0; i < len(args); i++ {
			if args[i] == "--cache-dir" && i+1 < len(args) {
				mapped = append(mapped, "--cache-dir", "/cache")
				i++
				continue
			}
			mapped = append(mapped, args[i])
		}
		name := fmt.Sprintf("donkey-trivy-e2e-%d", time.Now().UnixNano())
		docker := []string{"run", "--rm", "--name", name, "--volume", filepath.Join(dir, "cache") + ":/cache"}
		if dns := os.Getenv("DONKEY_E2E_DNS"); dns != "" {
			docker = append(docker, "--dns", dns)
		}
		if work != "" {
			docker = append(docker, "--volume", work+":/work", "--workdir", "/work")
		}
		for _, kv := range env {
			if strings.HasPrefix(kv, "TRIVY_") && !strings.HasPrefix(kv, "TRIVY_CACHE_DIR=") {
				docker = append(docker, "--env", kv)
			}
		}
		docker = append(docker, "--env", "TRIVY_CACHE_DIR=/cache", config.DefaultScannerImage)
		docker = append(docker, mapped...)
		out, err := exec.CommandContext(ctx, "docker", docker...).CombinedOutput()
		if ctx.Err() != nil {
			// Killing the Docker CLI does not stop its container.
			cleanupCtx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
			defer cancel()
			_ = exec.CommandContext(cleanupCtx, "docker", "rm", "--force", name).Run()
		}
		return out, err
	}
}

// PackageProxy runs the real package service and stops it with the test.
func PackageProxy(t *testing.T, ctx context.Context, host string, cfg packages.Config) string {
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
