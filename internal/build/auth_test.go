package build

import (
	"context"
	"encoding/pem"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/adrianliechti/donkey/internal/config"
	sessionauth "github.com/moby/buildkit/session/auth"
)

func TestRegistryAuthUsesPrefixHostForPushesAndSameHostMirrors(t *testing.T) {
	t.Setenv("DONKEY_REGISTRY_PASSWORD", "s3cret")
	reg := config.Registry{
		URL:      "registry.example.com/platform/donkey",
		Username: "builder",
	}
	got, err := registryAuthConfig(reg, "registry.example.com")
	if err != nil {
		t.Fatal(err)
	}
	if got.Username != "builder" || got.Password != "s3cret" {
		t.Fatalf("the prefixed registry lost its push credentials: %+v", got)
	}
	got, err = registryAuthConfig(reg, "docker.io")
	if err != nil {
		t.Fatal(err)
	}
	if got.Username != "" || got.Password != "" {
		t.Fatalf("enterprise credentials leaked to a public registry: %+v", got)
	}
}

func TestRegistryAuthTLSConfigsCoverPushAndMirrorTokenRequests(t *testing.T) {
	reg := config.Registry{URL: "push.example.com/platform/donkey", CAFile: "/etc/donkey/registry-ca/ca.crt"}
	got := registryAuthTLSConfigs(reg, map[string]string{
		"docker.io": "https://mirror.example.com:5443/dockerhub",
		"ghcr.io":   "mirror.example.com:5443/ghcr",
	})
	want := map[string][]string{
		"push.example.com":        {reg.CAFile},
		"mirror.example.com:5443": {reg.CAFile},
	}
	if len(got) != len(want) {
		t.Fatalf("TLS hosts = %v, want %v", got, want)
	}
	for host, roots := range want {
		if got[host] == nil || !reflect.DeepEqual(got[host].RootCAs, roots) {
			t.Errorf("TLS config for %s = %+v, want roots %v", host, got[host], roots)
		}
	}
	if configs := registryAuthTLSConfigs(config.Registry{URL: reg.URL}, map[string]string{"docker.io": "mirror.example.com"}); configs != nil {
		t.Fatalf("publicly trusted registries should not create TLS overrides: %v", configs)
	}
}

func TestRegistryAuthFetchesMirrorTokenThroughPrivateCA(t *testing.T) {
	mirror := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"token":"mirror-token","expires_in":300}`))
	}))
	defer mirror.Close()

	caFile := filepath.Join(t.TempDir(), "ca.crt")
	certificate := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: mirror.Certificate().Raw})
	if err := os.WriteFile(caFile, certificate, 0o600); err != nil {
		t.Fatal(err)
	}
	host := strings.TrimPrefix(mirror.URL, "https://")
	attachable, err := registryAuth(config.Registry{
		URL: "push.example.com/donkey", CAFile: caFile,
	}, map[string]string{"docker.io": mirror.URL + "/dockerhub"})
	if err != nil {
		t.Fatal(err)
	}
	fetcher, ok := attachable.(interface {
		FetchToken(context.Context, *sessionauth.FetchTokenRequest) (*sessionauth.FetchTokenResponse, error)
	})
	if !ok {
		t.Fatalf("BuildKit auth provider does not expose token exchange: %T", attachable)
	}
	response, err := fetcher.FetchToken(t.Context(), &sessionauth.FetchTokenRequest{
		Host: host, Realm: mirror.URL, Service: "mirror",
	})
	if err != nil {
		t.Fatalf("private-CA mirror token exchange failed: %v", err)
	}
	if response.Token != "mirror-token" {
		t.Fatalf("token = %q", response.Token)
	}
}

func TestRegistryAuthUsesDockerConfigForSeparateMirrorHost(t *testing.T) {
	t.Setenv("DONKEY_REGISTRY_PASSWORD", "push-secret")
	dir := t.TempDir()
	path := filepath.Join(dir, "mirror.json")
	contents := `{"auths":{"mirror.example.com":{"username":"mirror-user","password":"mirror-secret"}}}`
	if err := os.WriteFile(path, []byte(contents), 0o600); err != nil {
		t.Fatal(err)
	}
	reg := config.Registry{
		URL:             "push.example.com/platform/donkey",
		Username:        "push-user",
		DockerConfigDir: dir,
	}
	got, err := registryAuthConfig(reg, "mirror.example.com")
	if err != nil {
		t.Fatal(err)
	}
	if got.Username != "mirror-user" || got.Password != "mirror-secret" {
		t.Fatalf("mirror credentials were not selected by host: %+v", got)
	}

	rotated := `{"auths":{"mirror.example.com":{"username":"mirror-user","password":"rotated-secret"}}}`
	if err := os.WriteFile(path, []byte(rotated), 0o600); err != nil {
		t.Fatal(err)
	}
	got, err = registryAuthConfig(reg, "mirror.example.com")
	if err != nil {
		t.Fatal(err)
	}
	if got.Password != "rotated-secret" {
		t.Fatalf("rotated mirror credentials were not reloaded: %+v", got)
	}
}
