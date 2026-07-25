package build

import (
	"regexp"
	"testing"
	"time"
)

func TestImageTag(t *testing.T) {
	at := time.Date(2026, 7, 25, 14, 30, 22, 0, time.UTC)

	got := ImageTag("registry.example.com/donkey", "shop", "4f2a9c1b03de", at, false)
	if want := "registry.example.com/donkey/shop:4f2a9c1b03de-20260725143022"; got != want {
		t.Errorf("with sha: got %q, want %q", got, want)
	}
	// No git repository: the timestamp alone still rolls the Deployment.
	if got, want := ImageTag("registry.example.com/donkey/", "shop", "", at, false),
		"registry.example.com/donkey/shop:20260725143022"; got != want {
		t.Errorf("without sha: got %q, want %q", got, want)
	}
}

// A registry that refuses to overwrite a tag would reject the second of two
// deploys of one commit in the same second, so those tags must differ.
func TestImageTagImmutable(t *testing.T) {
	at := time.Date(2026, 7, 25, 14, 30, 22, 0, time.UTC)
	shape := regexp.MustCompile(`^registry\.example\.com/donkey/shop:4f2a9c1b03de-20260725143022-[0-9a-z]+$`)

	first := ImageTag("registry.example.com/donkey", "shop", "4f2a9c1b03de", at, true)
	if !shape.MatchString(first) {
		t.Fatalf("unexpected tag shape: %q", first)
	}
	for range 8 {
		if second := ImageTag("registry.example.com/donkey", "shop", "4f2a9c1b03de", at, true); second == first {
			t.Fatal("two immutable-registry tags for the same commit and second collided")
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
