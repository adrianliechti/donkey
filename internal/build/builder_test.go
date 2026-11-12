package build

import (
	"regexp"
	"testing"
	"time"
)

func TestImageTag(t *testing.T) {
	at := time.Date(2026, 7, 25, 14, 30, 22, 0, time.UTC)

	got := ImageTag("registry.example.com/donkey", "acme", "shop", "4f2a9c1b03de", at, false)
	if want := "registry.example.com/donkey/acme/shop:4f2a9c1b03de-20260725143022"; got != want {
		t.Errorf("with sha: got %q, want %q", got, want)
	}
	// No git repository: the timestamp alone still rolls the Deployment.
	if got, want := ImageTag("registry.example.com/donkey/", "acme", "shop", "", at, false),
		"registry.example.com/donkey/acme/shop:20260725143022"; got != want {
		t.Errorf("without sha: got %q, want %q", got, want)
	}
}

// A registry that refuses to overwrite a tag would reject the second of two
// deploys of one commit in the same second, so those tags must differ.
func TestImageTagImmutable(t *testing.T) {
	at := time.Date(2026, 7, 25, 14, 30, 22, 0, time.UTC)
	shape := regexp.MustCompile(`^registry\.example\.com/donkey/acme/shop:4f2a9c1b03de-20260725143022-[0-9a-z]+$`)

	first := ImageTag("registry.example.com/donkey", "acme", "shop", "4f2a9c1b03de", at, true)
	if !shape.MatchString(first) {
		t.Fatalf("unexpected tag shape: %q", first)
	}
	for range 8 {
		if second := ImageTag("registry.example.com/donkey", "acme", "shop", "4f2a9c1b03de", at, true); second == first {
			t.Fatal("two immutable-registry tags for the same commit and second collided")
		}
	}
}

func TestImageTime(t *testing.T) {
	at := time.Date(2026, 7, 25, 14, 30, 22, 0, time.UTC)
	for _, ref := range []string{
		ImageTag("registry.example.com:5000/donkey", "acme", "shop", "4f2a9c1b03de", at, false),
		ImageTag("registry.example.com/donkey", "acme", "shop", "", at, false),
		ImageTag("registry.example.com/donkey", "acme", "shop", "4f2a9c1b03de", at, true),
		ImageTag("registry.example.com/donkey", "acme", "shop", "4f2a9c1b03de", at, false) + "@sha256:0123",
	} {
		if got, ok := ImageTime(ref); !ok || !got.Equal(at) {
			t.Errorf("ImageTime(%q) = %v, %v; want %v", ref, got, ok, at)
		}
	}
	for _, ref := range []string{
		"registry.example.com:5000/donkey/acme/shop",
		"registry.example.com/donkey/acme/shop:latest",
		"registry.example.com/donkey/acme/shop@sha256:20260725143022",
		"registry.example.com/donkey/acme/shop:4f2a9c1b03de-2026072514302",
		"",
	} {
		if got, ok := ImageTime(ref); ok {
			t.Errorf("ImageTime(%q) = %v; want no time", ref, got)
		}
	}
}

func TestSameRegistry(t *testing.T) {
	const reg = "registry.example.com/donkey"
	for ref, want := range map[string]bool{
		"registry.example.com/donkey/shop:abc": true,
		// Anything donkey didn't build must not be deployable under an app's
		// name — it would inherit that app's env and addon secrets.
		"docker.io/attacker/dump:latest":            false,
		"registry.example.com/other/shop:abc":       false,
		"evil.example.com/registry.example.com/x:1": false,
	} {
		if got := SameRegistry(reg, ref); got != want {
			t.Errorf("SameRegistry(%q) = %v, want %v", ref, got, want)
		}
	}
}
