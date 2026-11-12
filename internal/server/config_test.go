package server

import (
	"reflect"
	"strings"
	"testing"

	"github.com/adrianliechti/donkey/internal/config"
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
	t.Setenv("DONKEY_BUILDER_DEBIAN_MIRROR", "true")
	t.Setenv("DONKEY_BUILDER_ALPINE_MIRROR", "true")
	t.Setenv("DONKEY_SESSION_KEY", strings.Repeat("00", 32))
	t.Setenv("DONKEY_TELEMETRY_KEY", strings.Repeat("11", 32))

	cfg, err := FromEnv()
	if err != nil {
		t.Fatal(err)
	}
	if !cfg.Build.DebianMirror || !cfg.Build.AlpineMirror {
		t.Fatal("OS mirror feature flags did not reach builder settings")
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

func TestConfigCommonOfferings(t *testing.T) {
	t.Setenv("DONKEY_REGISTRY_URL", "push.example.com/donkey")
	t.Setenv("DONKEY_DOMAIN", "apps.example.com")
	t.Setenv("DONKEY_BUILDKIT_ADDR", "tcp://donkey-buildkitd:1234")
	t.Setenv("DONKEY_SESSION_KEY", strings.Repeat("00", 32))
	t.Setenv("DONKEY_TELEMETRY_KEY", strings.Repeat("11", 32))
	for _, raw := range []string{"", "[]", `["postgres","redis"]`, `["bad id"]`, `["postgres","postgres"]`, `"postgres"`} {
		t.Run(raw, func(t *testing.T) {
			t.Setenv("DONKEY_COMMON_OFFERINGS", raw)
			cfg, err := FromEnv()
			invalid := raw == `["bad id"]` || raw == `["postgres","postgres"]` || raw == `"postgres"`
			if invalid {
				if err == nil {
					t.Fatal("invalid common offerings accepted")
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			if !(config.SpaceConfig{}).OfferingAllowed(cfg.CommonOfferings, "postgres") {
				t.Fatal("valid common offering denied")
			}
			if (raw == "" || raw == "[]") && !(config.SpaceConfig{}).OfferingAllowed(cfg.CommonOfferings, "custom") {
				t.Fatal("empty common offerings must allow all")
			}
		})
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

func TestS3ServiceUsesRustFSImageEnvironment(t *testing.T) {
	t.Setenv("DONKEY_REGISTRY_URL", "registry.example.com/donkey")
	t.Setenv("DONKEY_DOMAIN", "apps.example.com")
	t.Setenv("DONKEY_BUILDKIT_ADDR", "tcp://donkey-buildkitd:1234")
	t.Setenv("DONKEY_SESSION_KEY", strings.Repeat("00", 32))
	t.Setenv("DONKEY_TELEMETRY_KEY", strings.Repeat("11", 32))
	const repository = "mirror.example.com/rustfs/rustfs"
	t.Setenv("DONKEY_RUSTFS_IMAGE", repository)
	cfg, err := FromEnv()
	if err != nil {
		t.Fatal(err)
	}
	for _, typ := range []string{"s3", "rustfs"} {
		if cfg.AddonImages.Repository(typ) != repository {
			t.Fatalf("%s lost its RustFS image override", typ)
		}
	}
}

func TestConfigReadsScanSettings(t *testing.T) {
	t.Setenv("DONKEY_REGISTRY_URL", "push.example.com/donkey")
	t.Setenv("DONKEY_DOMAIN", "apps.example.com")
	t.Setenv("DONKEY_BUILDKIT_ADDR", "tcp://donkey-buildkitd:1234")
	t.Setenv("DONKEY_SESSION_KEY", strings.Repeat("00", 32))
	t.Setenv("DONKEY_TELEMETRY_KEY", strings.Repeat("11", 32))
	t.Setenv("DONKEY_BUILDER_SCAN_DB_REPOSITORY", "jfrog.corp/ghcr/aquasecurity/trivy-db")
	t.Setenv("DONKEY_BUILDER_SCAN_BLOCK", "high")
	t.Setenv("DONKEY_BUILDER_SCAN_TRIAGE", "off")

	cfg, err := FromEnv()
	if err != nil {
		t.Fatal(err)
	}
	scan := cfg.Build.Scan
	if !scan.IsEnabled() || scan.DBRepository != "jfrog.corp/ghcr/aquasecurity/trivy-db" || scan.BlockSeverity() != "high" || scan.AssistantTriage() {
		t.Errorf("scan settings = %+v", scan)
	}
	t.Setenv("DONKEY_BUILDER_SCAN_BLOCK", "everything")
	if _, err := FromEnv(); err == nil || !strings.Contains(err.Error(), "build.scan.block") {
		t.Errorf("an unknown threshold must be refused at startup: %v", err)
	}
}
