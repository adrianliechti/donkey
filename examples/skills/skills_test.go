package skills

import (
	"strings"
	"testing"
)

// The file is installed as an agent's SKILL.md as it is, so it has to be
// one: front matter naming the skill and saying when to use it.
func TestDonkeyIsASkill(t *testing.T) {
	front, body, ok := strings.Cut(strings.TrimPrefix(Donkey, "---\n"), "\n---\n")
	if !strings.HasPrefix(Donkey, "---\n") || !ok {
		t.Fatal("skill.md has no front matter")
	}
	if !strings.Contains(front, "name: donkey\n") || !strings.Contains(front, "description:") {
		t.Fatalf("front matter:\n%s", front)
	}
	// What an agent in a machine has to know and cannot find out by trying.
	for _, want := range []string{"## In a Donkey machine", "DONKEY_MACHINE_ID", "NPM_CONFIG_REGISTRY", "donkey packages run", "donkey deploy"} {
		if !strings.Contains(body, want) {
			t.Errorf("the skill does not mention %q", want)
		}
	}
}
