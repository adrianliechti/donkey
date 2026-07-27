package ai

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"testing"
)

// A builder is only as good as the apps it has been pointed at, and the
// hardest part of judging one is getting a corpus of real projects with
// known-correct answers. Railpack has already done that work: 122 example
// projects under examples/, each one built and HTTP-checked in CI, each with
// a committed snapshot of the plan railpack produces for it.
//
// So donkey scores itself against that. For every example, the snapshot says
// what railpack concluded — the start command it chose, whether it decided the
// app was a static site, and where it thought the build output went — and this
// test asks donkey's own scanner the same questions and counts the
// disagreements.
//
// It is deliberately a *scorecard*, not a pass/fail gate on every row. Some
// disagreements are donkey being wrong, some are railpack solving a different
// problem (it can install any mise package; donkey writes a Dockerfile), and
// some are genuinely ambiguous. What matters is the number, and that it moves
// the right way when a detection rule changes.
//
//	task builder:detect    # fetches the corpus and scores against it
//	DONKEY_CORPUS=/path/to/railpack go test ./internal/ai -run TestCorpus -v
const corpusEnv = "DONKEY_CORPUS"

// corpusRoot is a railpack checkout: it needs examples/ and
// core/__snapshots__/ side by side.
func corpusRoot(t *testing.T) string {
	t.Helper()
	root := os.Getenv(corpusEnv)
	if root == "" {
		t.Skipf("set %s to a railpack checkout (see `task builder:detect`)", corpusEnv)
	}
	if _, err := os.Stat(filepath.Join(root, "examples")); err != nil {
		t.Fatalf("%s=%s has no examples/ directory", corpusEnv, root)
	}
	return root
}

// railpackPlan is the part of a committed snapshot that says what railpack
// decided.
type railpackPlan struct {
	Deploy struct {
		StartCmd string `json:"startCommand"`
		Inputs   []struct {
			Step    string   `json:"step"`
			Include []string `json:"include"`
		} `json:"inputs"`
	} `json:"deploy"`
	Steps []struct {
		Name     string            `json:"name"`
		Commands []json.RawMessage `json:"commands"`
	} `json:"steps"`
}

// isStatic reports whether railpack decided to serve this app as files. Its
// static path always ends in Caddy, so the start command is the tell.
func (p *railpackPlan) isStatic() bool {
	return strings.Contains(p.Deploy.StartCmd, "caddy run")
}

// outputDir is the folder railpack copies into the Caddy image — the build
// step's include, which is exactly what donkey's scanner tries to predict.
func (p *railpackPlan) outputDir() string {
	for _, in := range p.Deploy.Inputs {
		if in.Step != "build" {
			continue
		}
		for _, inc := range in.Include {
			if inc == "." || strings.HasPrefix(inc, "/") {
				continue
			}
			return strings.Trim(inc, "/")
		}
	}
	return ""
}

// installCommand digs the dependency install out of the plan's install step,
// which is how the snapshot records the package manager railpack picked.
var installRe = regexp.MustCompile(`(pnpm|yarn|bun|npm|uv|poetry|pipenv|pdm|pip|bundle|composer|go mod|mix|cargo)[a-z ]*(install|ci|sync|download|deps|fetch)`)

func (p *railpackPlan) packageManager() string {
	for _, step := range p.Steps {
		if step.Name != "install" {
			continue
		}
		for _, raw := range step.Commands {
			if m := installRe.FindStringSubmatch(strings.ToLower(string(raw))); m != nil {
				return m[1]
			}
		}
	}
	return ""
}

// loadSnapshots maps example name → the plan railpack committed for it.
func loadSnapshots(t *testing.T, root string) map[string]*railpackPlan {
	t.Helper()
	dir := filepath.Join(root, "core", "__snapshots__")
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Skipf("no snapshots at %s: %v", dir, err)
	}
	// TestGenerateBuildPlanForExamples_<name>_1.snap.json
	nameRe := regexp.MustCompile(`^TestGenerateBuildPlanForExamples_(.+)_\d+\.snap\.json$`)
	out := map[string]*railpackPlan{}
	for _, e := range entries {
		m := nameRe.FindStringSubmatch(e.Name())
		if m == nil {
			continue
		}
		b, err := os.ReadFile(filepath.Join(dir, e.Name()))
		if err != nil {
			continue
		}
		var p railpackPlan
		if json.Unmarshal(b, &p) != nil {
			continue
		}
		out[m[1]] = &p
	}
	return out
}

// scoreboard tallies agreement on one question.
type scoreboard struct {
	name       string
	agree      int
	disagree   int
	notScored  int
	mismatches []string
}

func (s *scoreboard) hit(ok bool, detail string) {
	if ok {
		s.agree++
		return
	}
	s.disagree++
	s.mismatches = append(s.mismatches, detail)
}

func (s *scoreboard) report(t *testing.T) {
	t.Helper()
	total := s.agree + s.disagree
	if total == 0 {
		t.Logf("%-22s no comparable examples", s.name)
		return
	}
	pct := float64(s.agree) / float64(total) * 100
	t.Logf("%-22s %3d/%-3d  %5.1f%%  (%d not scored)", s.name, s.agree, total, pct, s.notScored)
	sort.Strings(s.mismatches)
	for _, m := range s.mismatches {
		t.Logf("      ✗ %s", m)
	}
}

// TestCorpusDetectionParity is the answer to "are we on par with railpack".
// It runs donkey's scanner over railpack's own example projects and compares
// each verdict with the plan railpack committed for that project.
func TestCorpusDetectionParity(t *testing.T) {
	root := corpusRoot(t)
	snapshots := loadSnapshots(t, root)
	examples, err := os.ReadDir(filepath.Join(root, "examples"))
	if err != nil {
		t.Fatal(err)
	}

	static := &scoreboard{name: "static site"}
	outDir := &scoreboard{name: "static output dir"}
	pkgMgr := &scoreboard{name: "package manager"}
	runtime := &scoreboard{name: "runtime found"}
	starts := &scoreboard{name: "start command found"}

	var scanned int
	for _, e := range examples {
		if !e.IsDir() {
			continue
		}
		name := e.Name()
		plan, ok := snapshots[name]
		if !ok {
			continue
		}
		dir := filepath.Join(root, "examples", name)
		sig := Scan(dir)
		scanned++

		// Did we agree that this is a static site?
		wantStatic := plan.isStatic()
		gotStatic := sig.Static != nil && sig.Static.CustomStart == ""
		static.hit(wantStatic == gotStatic, fmt.Sprintf(
			"%s: railpack static=%v, donkey static=%v", name, wantStatic, gotStatic))

		// Where does the build output go? Only meaningful where both agree it
		// is a static site.
		if wantStatic && gotStatic {
			want, got := plan.outputDir(), sig.Static.OutputDir
			if want == "" {
				outDir.notScored++
			} else {
				outDir.hit(strings.Trim(got, "/") == want, fmt.Sprintf(
					"%s: railpack %q, donkey %q", name, want, got))
			}
		}

		// Which package manager installs the dependencies?
		if want := plan.packageManager(); want != "" {
			got := strings.ToLower(values(sig.PackageManager))
			// yarn 1 and yarn berry are both "yarn" to railpack's plan.
			pkgMgr.hit(strings.Contains(got, want), fmt.Sprintf(
				"%s: railpack %q, donkey found %q", name, want,
				strings.ReplaceAll(strings.TrimSpace(got), "\n", "; ")))
		} else {
			pkgMgr.notScored++
		}

		// Every example railpack can build is one donkey must at least
		// recognize the language of.
		runtime.hit(len(sig.Runtimes) > 0, name+": donkey found no runtime at all")

		// And one it must have some idea how to start. railpack's own
		// StartCommandHelp exists because this is the question that most often
		// has no answer in the files.
		if plan.Deploy.StartCmd != "" {
			hasStart := len(sig.StartCommands) > 0 || sig.Static != nil
			starts.hit(hasStart, name+": donkey found no start command candidate")
		} else {
			starts.notScored++
		}
	}

	if scanned == 0 {
		t.Fatal("no examples had a matching snapshot — is DONKEY_CORPUS a railpack checkout?")
	}
	t.Logf("scored %d railpack examples", scanned)
	for _, s := range []*scoreboard{runtime, pkgMgr, static, outDir, starts} {
		s.report(t)
	}

	// Every one of these is at 100% against railpack's own corpus, so the gate
	// is set just below: high enough that a real regression fails the build,
	// with room for railpack to add an example donkey has not seen yet.
	// Raising a number here is the point of the exercise; lowering one should
	// take an argument.
	for _, s := range []*scoreboard{runtime, pkgMgr, static, outDir, starts} {
		if total := s.agree + s.disagree; total > 0 {
			if pct := float64(s.agree) / float64(total) * 100; pct < 95 {
				t.Errorf("%s parity fell to %.1f%% (want ≥95%%)", s.name, pct)
			}
		}
	}
}
