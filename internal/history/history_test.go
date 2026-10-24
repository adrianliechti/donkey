package history

import (
	"archive/zip"
	"bytes"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/go-git/go-git/v5"
)

var (
	adrian = Author{Name: "Adrian", Email: "adrian@example.com"}
	t0     = time.Date(2026, 10, 24, 9, 0, 0, 0, time.UTC)
)

func writeTree(t *testing.T, files map[string]string) string {
	t.Helper()
	dir := t.TempDir()
	for name, content := range files {
		full := filepath.Join(dir, filepath.FromSlash(name))
		if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
			t.Fatal(err)
		}
		mode := os.FileMode(0o644)
		if strings.HasSuffix(name, ".sh") {
			mode = 0o755
		}
		if err := os.WriteFile(full, []byte(content), mode); err != nil {
			t.Fatal(err)
		}
	}
	return dir
}

func newStore(t *testing.T) *Store {
	t.Helper()
	store, err := New(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	return store
}

func upload(t *testing.T, store *Store, files map[string]string, when time.Time) string {
	t.Helper()
	rec, err := store.RecordUpload("acme", "web", UploadInput{
		Dir: writeTree(t, files), Author: adrian, When: when, Message: "Deploy web",
	})
	if err != nil {
		t.Fatal(err)
	}
	return rec
}

func TestUploadsAndPlatformCommits(t *testing.T) {
	store := newStore(t)

	first := upload(t, store, map[string]string{
		"main.py":          "print('hi')\n",
		"lib/util.py":      "a = 1\nb = 2\n",
		"scripts/start.sh": "#!/bin/sh\n",
	}, t0)

	// The same files again are not a change.
	again := upload(t, store, map[string]string{
		"main.py":          "print('hi')\n",
		"lib/util.py":      "a = 1\nb = 2\n",
		"scripts/start.sh": "#!/bin/sh\n",
	}, t0.Add(time.Minute))
	if again != first {
		t.Fatalf("an unchanged upload should name the head again: %+v", again)
	}

	second := upload(t, store, map[string]string{
		"main.py":          "print('hello')\nprint('again')\n",
		"lib/util.py":      "a = 1\nb = 2\n",
		"scripts/start.sh": "#!/bin/sh\n",
		"README.md":        "# web\n",
	}, t0.Add(2*time.Minute))
	if second == first {
		t.Fatal("a changed upload should create a commit")
	}

	// The build wrote its recipe and repaired one file.
	built, err := store.RecordPlatform("acme", "web", PlatformInput{
		Base: second, When: t0.Add(3 * time.Minute), Message: "Build #2",
		Changes: []FileChange{
			{Path: ".donkey/Dockerfile", Content: []byte("FROM python:3.12\nCMD python main.py\n")},
			{Path: "lib/util.py", Content: []byte("a = 1\nb = 3\n")},
			{Path: "README.md", Deleted: true},
		},
	})
	if err != nil || built == second {
		t.Fatalf("platform commit: %+v, %v", built, err)
	}
	same, err := store.RecordPlatform("acme", "web", PlatformInput{
		Base: second, When: t0.Add(4 * time.Minute), Message: "Build #3",
		Changes: []FileChange{
			{Path: ".donkey/Dockerfile", Content: []byte("FROM python:3.12\nCMD python main.py\n")},
			{Path: "lib/util.py", Content: []byte("a = 1\nb = 3\n")},
			{Path: "README.md", Deleted: true},
		},
	})
	if err != nil || same != built {
		t.Fatalf("an identical rebuild should name the platform commit again: %+v, %v", same, err)
	}
	noop, err := store.RecordPlatform("acme", "web", PlatformInput{
		Base: second, When: t0.Add(5 * time.Minute), Message: "Build #4",
		Changes: []FileChange{{Path: "main.py", Content: []byte("print('hello')\nprint('again')\n")}},
	})
	if err != nil || noop != second {
		t.Fatalf("changes that change nothing should name the upload: %+v, %v", noop, err)
	}
	if err := store.TagBuild("acme", "web", 2, built); err != nil {
		t.Fatal(err)
	}
	if err := store.TagBuild("acme", "web", 3, built); err != nil {
		t.Fatal(err)
	}

	log, err := store.Log("acme", "web", 50)
	if err != nil {
		t.Fatal(err)
	}
	if len(log) != 3 {
		t.Fatalf("log has %d commits, want 3: %+v", len(log), log)
	}
	if log[0].ID != built || log[0].Kind != KindPlatform || log[0].Author != Platform {
		t.Fatalf("newest should be the platform commit: %+v", log[0])
	}
	if got := log[0].Builds; len(got) != 2 || got[0] != 3 || got[1] != 2 {
		t.Fatalf("platform commit builds = %v, want [3 2]", got)
	}
	if log[0].Stats != (Stats{Files: 3, Additions: 3, Deletions: 2}) {
		t.Fatalf("platform stats = %+v", log[0].Stats)
	}
	if log[0].Parents[len(log[0].Parents)-1] != second {
		t.Fatalf("a platform commit's last parent is the upload it built: %v", log[0].Parents)
	}
	if log[1].ID != second || log[1].Kind != KindUpload || log[1].Author != adrian {
		t.Fatalf("then the second upload: %+v", log[1])
	}
	if log[1].Stats != (Stats{Files: 2, Additions: 3, Deletions: 1}) {
		t.Fatalf("upload stats = %+v", log[1].Stats)
	}
	if log[2].ID != first || len(log[2].Parents) != 0 || log[2].Stats.Additions != 4 {
		t.Fatalf("then the first upload, counted as all added: %+v", log[2])
	}

	detail, err := store.Show("acme", "web", built)
	if err != nil {
		t.Fatal(err)
	}
	want := []Change{
		{Path: ".donkey/Dockerfile", Status: "added", Additions: 2},
		{Path: "README.md", Status: "deleted", Deletions: 1},
		{Path: "lib/util.py", Status: "modified", Additions: 1, Deletions: 1},
	}
	if len(detail.Changes) != len(want) {
		t.Fatalf("changes = %+v", detail.Changes)
	}
	for i := range want {
		if detail.Changes[i] != want[i] {
			t.Fatalf("change %d = %+v, want %+v", i, detail.Changes[i], want[i])
		}
	}
	if detail.Kind != KindPlatform {
		t.Fatalf("show should know a platform commit: %q", detail.Kind)
	}

	diff, err := store.Diff("acme", "web", built, "lib/util.py")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(diff.Diff, "-b = 2") || !strings.Contains(diff.Diff, "+b = 3") {
		t.Fatalf("diff = %q", diff.Diff)
	}
	if _, err := store.Diff("acme", "web", built, "nope.txt"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("diff of an untouched path should be ErrNotFound, got %v", err)
	}

	tree, err := store.Tree("acme", "web", built)
	if err != nil {
		t.Fatal(err)
	}
	var paths []string
	for _, e := range tree {
		paths = append(paths, e.Path)
	}
	if strings.Join(paths, ",") != ".donkey/Dockerfile,lib/util.py,main.py,scripts/start.sh" {
		t.Fatalf("tree = %v", paths)
	}
	if !tree[3].Executable || tree[2].Size != int64(len("print('hello')\nprint('again')\n")) {
		t.Fatalf("tree entries lost their mode or size: %+v", tree)
	}

	content, err := store.ReadFile("acme", "web", first, "lib/util.py")
	if err != nil || string(content) != "a = 1\nb = 2\n" {
		t.Fatalf("read old version = %q, %v", content, err)
	}
	if _, err := store.ReadFile("acme", "web", first, "../../etc/passwd"); err == nil {
		t.Fatal("a path outside the tree must be refused")
	}

	var archive bytes.Buffer
	if err := store.Archive("acme", "web", built, "web/", &archive); err != nil {
		t.Fatal(err)
	}
	zr, err := zip.NewReader(bytes.NewReader(archive.Bytes()), int64(archive.Len()))
	if err != nil {
		t.Fatal(err)
	}
	got := map[string]string{}
	for _, f := range zr.File {
		r, err := f.Open()
		if err != nil {
			t.Fatal(err)
		}
		b, _ := io.ReadAll(r)
		r.Close()
		got[f.Name] = string(b)
		if f.Name == "web/scripts/start.sh" && f.Mode()&0o111 == 0 {
			t.Fatal("the zip should keep the executable bit")
		}
	}
	if len(got) != 4 || got["web/.donkey/Dockerfile"] != "FROM python:3.12\nCMD python main.py\n" {
		t.Fatalf("archive = %v", got)
	}

	// The repository is plain git: it opens, HEAD is main, tags resolve.
	repo, err := git.PlainOpen(filepath.Join(store.dir, "acme", "web.git"))
	if err != nil {
		t.Fatal(err)
	}
	head, err := repo.Head()
	if err != nil || head.Hash().String() != second {
		t.Fatalf("HEAD = %v, %v", head, err)
	}
	tag, err := repo.Tag("build-2")
	if err != nil || tag.Hash().String() != built {
		t.Fatalf("build-2 = %v, %v", tag, err)
	}
}

func TestPlatformBranchMergesEachUpload(t *testing.T) {
	store := newStore(t)
	var uploads, builds []string
	for i := range 3 {
		up := upload(t, store, map[string]string{"app.py": strings.Repeat("x\n", i+1)}, t0.Add(time.Duration(2*i)*time.Minute))
		built, err := store.RecordPlatform("acme", "web", PlatformInput{
			Base: up, When: t0.Add(time.Duration(2*i+1) * time.Minute), Message: "Build",
			Changes: []FileChange{{Path: ".donkey/Dockerfile", Content: []byte("FROM python\n")}},
		})
		if err != nil || built == up {
			t.Fatalf("platform %d: %+v %v", i, built, err)
		}
		uploads, builds = append(uploads, up), append(builds, built)
	}
	log, err := store.Log("acme", "web", 10)
	if err != nil || len(log) != 6 {
		t.Fatalf("log: %d entries, %v", len(log), err)
	}
	for i, c := range log {
		round := 2 - i/2
		wantID, wantKind := builds[round], KindPlatform
		if i%2 == 1 {
			wantID, wantKind = uploads[round], KindUpload
		}
		if c.ID != wantID || c.Kind != wantKind {
			t.Fatalf("entry %d = %s %s, want %s %s", i, c.Kind, c.ID, wantKind, wantID)
		}
		// Each side's change is its own: the recipe for the platform, one
		// more line for every upload after the first.
		if c.Kind == KindPlatform && (c.Stats != Stats{Files: 1, Additions: 1}) {
			t.Fatalf("platform stats = %+v", c.Stats)
		}
		if c.Kind == KindUpload && round > 0 && (c.Stats != Stats{Files: 1, Additions: 1}) {
			t.Fatalf("upload stats = %+v", c.Stats)
		}
	}
	if got := log[0].Parents; len(got) != 2 || got[0] != builds[1] || got[1] != uploads[2] {
		t.Fatalf("the newest platform commit merges upload 3 into build 2: %v", got)
	}
	if detail, err := store.Show("acme", "web", builds[0]); err != nil || detail.Kind != KindPlatform {
		t.Fatalf("the oldest platform commit is still known as one: %+v %v", detail, err)
	}
}

func TestEqualTimesKeepEachBuildAboveItsUpload(t *testing.T) {
	store := newStore(t)
	var want []string
	for i := range 3 {
		up := upload(t, store, map[string]string{"app.py": strings.Repeat("x\n", i+1)}, t0)
		built, err := store.RecordPlatform("acme", "web", PlatformInput{
			Base: up, When: t0, Message: "Build",
			Changes: []FileChange{{Path: ".donkey/Dockerfile", Content: []byte("FROM python\n")}},
		})
		if err != nil {
			t.Fatal(err)
		}
		want = append([]string{built, up}, want...)
	}
	log, err := store.Log("acme", "web", 10)
	if err != nil {
		t.Fatal(err)
	}
	for i, c := range log {
		if c.ID != want[i] {
			t.Fatalf("entry %d = %s %s; one second can hold several deploys, and each build still sits on its upload", i, c.Kind, c.ID)
		}
	}
}

func TestHostileInputStaysValidGit(t *testing.T) {
	store := newStore(t)
	// A worktree's gitfile and a submodule's folder would make the
	// repository unclonable; a name with git's own delimiters would break
	// the commit itself.
	dir := writeTree(t, map[string]string{
		"app.py": "ok\n", ".git": "gitdir: /home/me/repo/.git/worktrees/x\n", "libs/x/.GIT/config": "[core]\n",
	})
	first, err := store.RecordUpload("acme", "web", UploadInput{
		Dir: dir, Author: Author{Name: "Eve <root>\nInjected: yes", Email: "eve@example.com>"}, When: t0, Message: "Deploy web",
	})
	if err != nil {
		t.Fatal(err)
	}
	built, err := store.RecordPlatform("acme", "web", PlatformInput{
		Base: first, When: t0, Message: "Build",
		Changes: []FileChange{{Path: "sub/.git", Content: []byte("x")}, {Path: "Dockerfile", Content: []byte("FROM x\n")}},
	})
	if err != nil {
		t.Fatal(err)
	}
	tree, err := store.Tree("acme", "web", built)
	if err != nil {
		t.Fatal(err)
	}
	if len(tree) != 2 || tree[0].Path != "Dockerfile" || tree[1].Path != "app.py" {
		t.Fatalf("tree = %+v", tree)
	}
	log, err := store.Log("acme", "web", 10)
	if err != nil {
		t.Fatal(err)
	}
	if got := log[1].Author; got.Name != "Eve root Injected: yes" || got.Email != "eve@example.com" {
		t.Fatalf("author = %+v", got)
	}

	// A build tag never moves.
	if err := store.TagBuild("acme", "web", 1, first); err != nil {
		t.Fatal(err)
	}
	if err := store.TagBuild("acme", "web", 1, first); err != nil {
		t.Fatalf("tagging the same commit again is fine: %v", err)
	}
	if err := store.TagBuild("acme", "web", 1, built); err == nil {
		t.Fatal("re-pointing a build tag must be refused")
	}
	if detail, err := store.Show("acme", "web", first); err != nil || len(detail.Builds) != 1 || detail.Builds[0] != 1 {
		t.Fatalf("build 1 still names the first upload: %+v %v", detail, err)
	}
}

func TestLargeFilesAreLeftOutAndNamed(t *testing.T) {
	store := newStore(t)
	dir := writeTree(t, map[string]string{"app.js": "ok\n"})
	if err := os.WriteFile(filepath.Join(dir, "video.mp4"), bytes.Repeat([]byte{1}, MaxFileSize+1), 0o644); err != nil {
		t.Fatal(err)
	}
	rec, err := store.RecordUpload("acme", "web", UploadInput{Dir: dir, Author: adrian, When: t0, Message: "Deploy web"})
	if err != nil {
		t.Fatal(err)
	}
	detail, err := store.Show("acme", "web", rec)
	if err != nil {
		t.Fatal(err)
	}
	if len(detail.Changes) != 1 || !strings.Contains(detail.Message, "video.mp4 (8.0 MiB)") {
		t.Fatalf("the commit should hold app.js and name what it left out: %+v", detail)
	}
}

func TestBinaryChangesHaveNoLineCounts(t *testing.T) {
	store := newStore(t)
	first := upload(t, store, map[string]string{"logo.png": "\x89PNG\x00\x01"}, t0)
	second := upload(t, store, map[string]string{"logo.png": "\x89PNG\x00\x02"}, t0.Add(time.Minute))
	detail, err := store.Show("acme", "web", second)
	if err != nil {
		t.Fatal(err)
	}
	if len(detail.Changes) != 1 || !detail.Changes[0].Binary || detail.Changes[0].Additions != 0 {
		t.Fatalf("changes = %+v", detail.Changes)
	}
	diff, err := store.Diff("acme", "web", second, "logo.png")
	if err != nil || diff.Diff != "" || !diff.Binary {
		t.Fatalf("binary diff = %+v, %v", diff, err)
	}
	if first == second {
		t.Fatal("different bytes are a different commit")
	}
}

func TestCommitsLandAsPacksAndAreRepacked(t *testing.T) {
	store := newStore(t)
	packDir := filepath.Join(store.dir, "acme", "web.git", "objects", "pack")
	countPacks := func() int {
		t.Helper()
		packs, err := filepath.Glob(filepath.Join(packDir, "*.pack"))
		if err != nil {
			t.Fatal(err)
		}
		return len(packs)
	}
	for i := range repackAfter + 1 {
		upload(t, store, map[string]string{"n.txt": strings.Repeat("x\n", i+1)}, t0.Add(time.Duration(i)*time.Minute))
	}
	// Fresh packs may belong to another server's unpublished commit: none
	// is merged away while it is young.
	if n := countPacks(); n != repackAfter+1 {
		t.Fatalf("%d packs, want all %d kept while fresh", n, repackAfter+1)
	}
	old := time.Now().Add(-2 * packGrace)
	packs, _ := filepath.Glob(filepath.Join(packDir, "*"))
	for _, p := range packs {
		if err := os.Chtimes(p, old, old); err != nil {
			t.Fatal(err)
		}
	}
	upload(t, store, map[string]string{"n.txt": "settled\n"}, t0.Add(time.Hour))
	if n := countPacks(); n != 2 {
		t.Fatalf("%d packs left; the old ones should be merged into one, beside the newest", n)
	}
	loose, err := filepath.Glob(filepath.Join(store.dir, "acme", "web.git", "objects", "??", "*"))
	if err != nil || len(loose) != 0 {
		t.Fatalf("no loose objects should be written: %v %v", loose, err)
	}
	log, err := store.Log("acme", "web", 100)
	if err != nil || len(log) != repackAfter+2 {
		t.Fatalf("history after repack: %d commits, %v", len(log), err)
	}
	if log[0].Stats != (Stats{Files: 1, Additions: 1, Deletions: repackAfter + 1}) {
		t.Fatalf("stats after repack = %+v", log[0].Stats)
	}
}

func TestMissingHistoryAndBadInput(t *testing.T) {
	store := newStore(t)
	log, err := store.Log("acme", "web", 10)
	if err != nil || len(log) != 0 {
		t.Fatalf("an app without history has an empty log: %v %v", log, err)
	}
	if _, err := store.Show("acme", "web", strings.Repeat("a", 40)); !errors.Is(err, ErrNotFound) {
		t.Fatalf("show without history = %v", err)
	}
	if _, err := store.Show("acme", "web", "HEAD"); err == nil {
		t.Fatal("only full commit ids are accepted")
	}
	if _, err := store.RecordUpload("../x", "web", UploadInput{Dir: t.TempDir()}); err == nil {
		t.Fatal("an invalid space must be refused")
	}
	rec := upload(t, store, map[string]string{"a": "1\n"}, t0)
	if _, err := store.Show("acme", "web", strings.Repeat("b", 40)); !errors.Is(err, ErrNotFound) {
		t.Fatalf("an unknown commit = %v", err)
	}
	if _, err := store.RecordPlatform("acme", "web", PlatformInput{
		Base: rec, Changes: []FileChange{{Path: "../escape", Content: []byte("x")}},
	}); err == nil {
		t.Fatal("a change outside the tree must be refused")
	}
	if err := store.RemoveApp("acme", "web"); err != nil {
		t.Fatal(err)
	}
	if log, err := store.Log("acme", "web", 10); err != nil || len(log) != 0 {
		t.Fatalf("a removed app has no history: %v %v", log, err)
	}
	if again := upload(t, store, map[string]string{"a": "1\n"}, t0); again == "" {
		t.Fatal("a new app by the same name starts a new history")
	}
	if err := store.RemoveSpace("acme"); err != nil {
		t.Fatal(err)
	}
	if log, err := store.Log("acme", "web", 10); err != nil || len(log) != 0 {
		t.Fatalf("a removed space has no history: %v %v", log, err)
	}
}
