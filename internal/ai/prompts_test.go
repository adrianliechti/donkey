package ai

import (
	"strings"
	"testing"
)

func TestExtendedDockerfileInstructionsAppendOperatorGuidance(t *testing.T) {
	extra := "Use base images under registry.example.com/mirror/library only."
	got := extendedDockerfileInstructions("\n  " + extra + "\n")
	if !strings.HasPrefix(got, dockerfileInstructions) {
		t.Fatal("operator guidance replaced donkey's built-in builder contract")
	}
	if !strings.Contains(got, "Installation-specific builder instructions") || !strings.HasSuffix(got, extra) {
		t.Fatalf("operator guidance was not appended to builder instructions:\n%s", got)
	}
	if got := extendedDockerfileInstructions("  \n"); got != dockerfileInstructions {
		t.Fatal("empty guidance should leave builder instructions unchanged")
	}
}
