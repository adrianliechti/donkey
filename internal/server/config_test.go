package server

import (
	"reflect"
	"strings"
	"testing"
)

func TestSplitCommaSeparated(t *testing.T) {
	got := splitCommaSeparated("oci-mirror-registry-example-com, shared-registry, ,oci-mirror-registry-example-com")
	want := []string{"oci-mirror-registry-example-com", "shared-registry"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("splitCommaSeparated() = %v, want %v", got, want)
	}
}

func TestConfigReadsRegistryMirrorsForBuildSessionAuth(t *testing.T) {
	t.Setenv("DONKEY_REGISTRY_URL", "push.example.com/donkey")
	t.Setenv("DONKEY_REGISTRY_MIRRORS", `{"docker.io":"https://mirror.example.com/dockerhub"}`)
	t.Setenv("DONKEY_DOMAIN", "apps.example.com")
	t.Setenv("DONKEY_BUILDKIT_ADDR", "tcp://donkey-buildkitd:1234")
	t.Setenv("DONKEY_SESSION_KEY", strings.Repeat("00", 32))
	t.Setenv("DONKEY_TELEMETRY_KEY", strings.Repeat("11", 32))

	cfg, err := FromEnv()
	if err != nil {
		t.Fatal(err)
	}
	if got := cfg.RegistryMirrors["docker.io"]; got != "https://mirror.example.com/dockerhub" {
		t.Fatalf("docker.io mirror = %q", got)
	}
}

func TestRegistryMirrorSummaryIsStableAndExplicit(t *testing.T) {
	got := registryMirrorSummary(map[string]string{
		"ghcr.io":   "mirror.example.com/ghcr",
		"docker.io": "mirror.example.com/dockerhub",
	})
	want := "docker.io → mirror.example.com/dockerhub, ghcr.io → mirror.example.com/ghcr"
	if got != want {
		t.Fatalf("mirror summary = %q, want %q", got, want)
	}
}
