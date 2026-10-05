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
	t.Setenv("DONKEY_REDIS_IMAGE", "jfrog.corp/dockerhub/library/redis")
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
	if cfg.AddonImages.Redis != "jfrog.corp/dockerhub/library/redis" {
		t.Fatalf("Redis image repository = %q", cfg.AddonImages.Redis)
	}
	t.Setenv("DONKEY_REDIS_IMAGE", "jfrog.corp/dockerhub/library/redis:8")
	if _, err := FromEnv(); err == nil || !strings.Contains(err.Error(), "addonImages.redis") {
		t.Fatalf("tagged Redis repository must be rejected: %v", err)
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

func TestConfigBusyboxImage(t *testing.T) {
	t.Setenv("DONKEY_REGISTRY_URL", "push.example.com/donkey")
	t.Setenv("DONKEY_DOMAIN", "apps.example.com")
	t.Setenv("DONKEY_BUILDKIT_ADDR", "tcp://donkey-buildkitd:1234")
	t.Setenv("DONKEY_SESSION_KEY", strings.Repeat("00", 32))
	t.Setenv("DONKEY_TELEMETRY_KEY", strings.Repeat("11", 32))
	for _, tc := range []struct {
		image   string
		want    string
		invalid bool
	}{
		{want: "busybox:1.37"},
		{image: "jfrog.corp/dockerhub/library/busybox:1.37", want: "jfrog.corp/dockerhub/library/busybox:1.37"},
		{image: "https://jfrog.corp/dockerhub/library/busybox:1.37", invalid: true},
	} {
		t.Run(tc.image, func(t *testing.T) {
			t.Setenv("DONKEY_BUSYBOX_IMAGE", tc.image)
			cfg, err := FromEnv()
			if tc.invalid {
				if err == nil || !strings.Contains(err.Error(), "busyboxImage") {
					t.Fatalf("expected a busyboxImage configuration error, got %v", err)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			if cfg.BusyboxImage != tc.want {
				t.Fatalf("BusyboxImage = %q, want %q", cfg.BusyboxImage, tc.want)
			}
		})
	}
}
