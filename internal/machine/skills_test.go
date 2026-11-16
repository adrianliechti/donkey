package machine

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func writeSkill(t *testing.T, dir, name string) {
	t.Helper()
	if err := os.MkdirAll(dir, 0o750); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "SKILL.md"), []byte("---\nname: "+name+"\n---\n"), 0o640); err != nil {
		t.Fatal(err)
	}
}

// The installation's skills are linked where the agent looks for a person's
// own — one named donkey in place of the built-in one — and withdrawn with
// the installation's. Nothing of the owner's is replaced.
func TestInstallSkills(t *testing.T) {
	home, from := t.TempDir(), t.TempDir()
	skills := filepath.Join(home, ".agents", "skills")

	// The way a ConfigMap volume lays it out: each skill a link into a
	// generation folder, and bookkeeping no skill is named after.
	generation := filepath.Join(from, "..2026_11_16_10_00_00.1")
	for _, name := range []string{"donkey", "company", "clash"} {
		writeSkill(t, filepath.Join(generation, name), name)
	}
	if err := os.Symlink(filepath.Base(generation), filepath.Join(from, "..data")); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"donkey", "company", "clash"} {
		if err := os.Symlink(filepath.Join("..data", name), filepath.Join(from, name)); err != nil {
			t.Fatal(err)
		}
	}
	// What is not a skill folder is no skill.
	if err := os.WriteFile(filepath.Join(from, "README.md"), []byte("hi"), 0o640); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(from, "empty"), 0o750); err != nil {
		t.Fatal(err)
	}

	// An earlier start's built-in donkey skill, and the owner's own skills —
	// one of them named like one of the installation's.
	writeSkill(t, filepath.Join(skills, "donkey"), "donkey")
	writeSkill(t, filepath.Join(skills, "mine"), "mine")
	writeSkill(t, filepath.Join(skills, "clash"), "clash")

	err := installSkills(home, from)
	if err == nil || !strings.Contains(err.Error(), "clash") {
		t.Fatalf("the owner's clash went unmentioned: %v", err)
	}
	for _, name := range []string{"donkey", "company"} {
		if target, err := os.Readlink(filepath.Join(skills, name)); err != nil || target != filepath.Join(from, name) {
			t.Errorf("%s: link to %q (%v)", name, target, err)
		}
		if _, err := os.Stat(filepath.Join(skills, name, "SKILL.md")); err != nil {
			t.Errorf("%s does not resolve: %v", name, err)
		}
	}
	for _, name := range []string{"mine", "clash"} {
		if info, err := os.Lstat(filepath.Join(skills, name)); err != nil || !info.IsDir() {
			t.Errorf("the owner's %s was replaced: %v", name, err)
		}
	}
	for _, name := range []string{"README.md", "README", "empty", "..data"} {
		if _, err := os.Lstat(filepath.Join(skills, name)); err == nil {
			t.Errorf("%s was taken for a skill", name)
		}
	}
	// A second start finds everything in place.
	if err := installSkills(home, from); err == nil || !strings.Contains(err.Error(), "clash") {
		t.Fatalf("second start: %v", err)
	}

	// The installation withdraws its skills: their links go, and the built-in
	// donkey skill comes back.
	for _, name := range []string{"donkey", "company", "clash"} {
		if err := os.Remove(filepath.Join(from, name)); err != nil {
			t.Fatal(err)
		}
	}
	if err := installSkills(home, from); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Lstat(filepath.Join(skills, "company")); err == nil {
		t.Error("a withdrawn skill stayed")
	}
	info, err := os.Lstat(filepath.Join(skills, "donkey"))
	if err != nil || !info.IsDir() {
		t.Fatalf("the built-in donkey skill is not back: %v", err)
	}
	if b, _ := os.ReadFile(filepath.Join(skills, "donkey", "SKILL.md")); !strings.Contains(string(b), "donkey deploy") {
		t.Fatalf("donkey skill = %q", b)
	}
	if _, err := os.Lstat(filepath.Join(skills, "mine")); err != nil {
		t.Error("the owner's own skill went")
	}
}

// A machine without skills of the installation's — nothing mounted — gets
// the built-in donkey skill and nothing else.
func TestInstallSkillsWithoutTheInstallations(t *testing.T) {
	home := t.TempDir()
	if err := installSkills(home, filepath.Join(home, "not-mounted")); err != nil {
		t.Fatal(err)
	}
	entries, err := os.ReadDir(filepath.Join(home, ".agents", "skills"))
	if err != nil || len(entries) != 1 || entries[0].Name() != "donkey" || !entries[0].IsDir() {
		t.Fatalf("skills = %v (%v)", entries, err)
	}
}
