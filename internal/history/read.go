package history

import (
	"archive/zip"
	"errors"
	"io"
	"maps"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/aymanbagabas/go-udiff"
	"github.com/go-git/go-git/v5"
	"github.com/go-git/go-git/v5/plumbing"
	"github.com/go-git/go-git/v5/plumbing/filemode"
	"github.com/go-git/go-git/v5/plumbing/object"
)

const (
	KindUpload   = "upload"
	KindPlatform = "platform"
)

// Commit is one entry of an app's history.
type Commit struct {
	ID      string    `json:"id"`
	Parents []string  `json:"parents"`
	Kind    string    `json:"kind"`
	Author  Author    `json:"author"`
	Date    time.Time `json:"date"`
	Message string    `json:"message"`
	// Builds are the build numbers that used exactly this commit, newest
	// first: a redeploy of unchanged files is another build of the same one.
	Builds []int64 `json:"builds,omitempty"`
	Stats  Stats   `json:"stats"`
}

// Stats sums a commit's changes against its last parent.
type Stats struct {
	Files     int `json:"files"`
	Additions int `json:"additions"`
	Deletions int `json:"deletions"`
}

// Change is one file a commit touched.
type Change struct {
	Path   string `json:"path"`
	Status string `json:"status"` // added | modified | deleted
	// Additions and Deletions count lines; both are 0 for binary files and
	// files too large to diff.
	Additions int  `json:"additions"`
	Deletions int  `json:"deletions"`
	Binary    bool `json:"binary,omitempty"`
	TooLarge  bool `json:"tooLarge,omitempty"`
}

// CommitDetail is a commit with its changes.
type CommitDetail struct {
	Commit
	Changes []Change `json:"changes"`
}

// FileDiff is one file's change as a unified diff.
type FileDiff struct {
	Change
	Diff string `json:"diff,omitempty"`
}

// Entry is one file of a commit's tree.
type Entry struct {
	Path       string `json:"path"`
	Size       int64  `json:"size"`
	Executable bool   `json:"executable,omitempty"`
}

// Log lists the app's history newest first, uploads and platform commits
// interleaved by time. An app without history has an empty one.
func (s *Store) Log(space, app string, limit int) ([]Commit, error) {
	mu := s.lock(space, app)
	mu.RLock()
	defer mu.RUnlock()
	repo, err := s.open(space, app, false)
	if errors.Is(err, ErrNotFound) {
		return []Commit{}, nil
	}
	if err != nil {
		return nil, err
	}
	builds, err := buildTags(repo)
	if err != nil {
		return nil, err
	}
	// Each branch is its own chain: main through its parents, platform
	// through the previous platform commit of each merge. Each lane
	// contributes up to limit, so the merged list is right at the top.
	uploads, err := walkLane(repo, MainBranch, lastParent, limit)
	if err != nil {
		return nil, err
	}
	builtOn, err := walkLane(repo, PlatformBranch, previousPlatform, limit)
	if err != nil {
		return nil, err
	}
	out := make([]Commit, 0, min(limit, len(uploads)+len(builtOn)))
	for _, entry := range interleave(uploads, builtOn) {
		if len(out) == limit {
			break
		}
		detail, err := s.describe(repo, entry, builds)
		if err != nil {
			return nil, err
		}
		out = append(out, detail.Commit)
	}
	return out, nil
}

func walkLane(repo *git.Repository, branch plumbing.ReferenceName, next func(*object.Commit) plumbing.Hash, limit int) ([]*object.Commit, error) {
	var lane []*object.Commit
	commit, err := branchHead(repo, branch)
	for err == nil && commit != nil && len(lane) < limit {
		lane = append(lane, commit)
		previous := next(commit)
		if previous.IsZero() {
			break
		}
		commit, err = repo.CommitObject(previous)
	}
	return lane, err
}

// interleave merges the two newest-first lanes by time. Each lane is already
// in its own order, so only the heads are compared; at equal times a build's
// commit goes right above the upload it built, never past it.
func interleave(uploads, builtOn []*object.Commit) []*object.Commit {
	merged := make([]*object.Commit, 0, len(uploads)+len(builtOn))
	i, j := 0, 0
	for i < len(uploads) || j < len(builtOn) {
		takePlatform := i == len(uploads)
		if !takePlatform && j < len(builtOn) {
			p, u := builtOn[j], uploads[i]
			takePlatform = p.Committer.When.After(u.Committer.When) ||
				(p.Committer.When.Equal(u.Committer.When) && lastParent(p) == u.Hash)
		}
		if takePlatform {
			merged = append(merged, builtOn[j])
			j++
		} else {
			merged = append(merged, uploads[i])
			i++
		}
	}
	return merged
}

// Show is one commit with what it changed against its last parent.
func (s *Store) Show(space, app, id string) (*CommitDetail, error) {
	mu := s.lock(space, app)
	mu.RLock()
	defer mu.RUnlock()
	repo, commit, err := s.commit(space, app, id)
	if err != nil {
		return nil, err
	}
	builds, err := buildTags(repo)
	if err != nil {
		return nil, err
	}
	return s.describe(repo, commit, builds)
}

// Diff is one file's change in a commit, against its last parent.
func (s *Store) Diff(space, app, id, file string) (*FileDiff, error) {
	p, err := cleanPath(file)
	if err != nil {
		return nil, err
	}
	mu := s.lock(space, app)
	mu.RLock()
	defer mu.RUnlock()
	repo, commit, err := s.commit(space, app, id)
	if err != nil {
		return nil, err
	}
	before, after, err := commitTrees(repo, commit)
	if err != nil {
		return nil, err
	}
	old, oldOK := before[p]
	cur, curOK := after[p]
	if !oldOK && !curOK {
		return nil, ErrNotFound
	}
	change, oldText, newText, err := compare(repo, p, old, oldOK, cur, curOK)
	if err != nil {
		return nil, err
	}
	diff := &FileDiff{Change: change}
	if !change.Binary && !change.TooLarge && oldText != newText {
		diff.Diff = udiff.Unified("a/"+p, "b/"+p, oldText, newText)
	}
	return diff, nil
}

// Tree lists every file of a commit.
func (s *Store) Tree(space, app, id string) ([]Entry, error) {
	mu := s.lock(space, app)
	mu.RLock()
	defer mu.RUnlock()
	repo, commit, err := s.commit(space, app, id)
	if err != nil {
		return nil, err
	}
	tree, err := commit.Tree()
	if err != nil {
		return nil, err
	}
	files, err := treeEntries(tree)
	if err != nil {
		return nil, err
	}
	entries := make([]Entry, 0, len(files))
	for p, f := range files {
		// The size comes from the object header; no file content is read.
		size, err := repo.Storer.EncodedObjectSize(f.hash)
		if err != nil {
			return nil, err
		}
		entries = append(entries, Entry{Path: p, Size: size, Executable: f.mode == filemode.Executable})
	}
	slices.SortFunc(entries, func(a, b Entry) int { return strings.Compare(a.Path, b.Path) })
	return entries, nil
}

// ReadFile returns one file of a commit. Files are bounded by MaxFileSize,
// so holding one in memory is fine.
func (s *Store) ReadFile(space, app, id, file string) ([]byte, error) {
	p, err := cleanPath(file)
	if err != nil {
		return nil, err
	}
	mu := s.lock(space, app)
	mu.RLock()
	defer mu.RUnlock()
	_, commit, err := s.commit(space, app, id)
	if err != nil {
		return nil, err
	}
	tree, err := commit.Tree()
	if err != nil {
		return nil, err
	}
	f, err := tree.File(p)
	if err != nil {
		return nil, notFound(err)
	}
	r, err := f.Reader()
	if err != nil {
		return nil, err
	}
	defer r.Close()
	return io.ReadAll(r)
}

// Archive writes a commit's files as a zip, each under prefix.
func (s *Store) Archive(space, app, id, prefix string, w io.Writer) error {
	mu := s.lock(space, app)
	mu.RLock()
	defer mu.RUnlock()
	_, commit, err := s.commit(space, app, id)
	if err != nil {
		return err
	}
	tree, err := commit.Tree()
	if err != nil {
		return err
	}
	zw := zip.NewWriter(w)
	err = tree.Files().ForEach(func(f *object.File) error {
		header := &zip.FileHeader{Name: prefix + f.Name, Method: zip.Deflate, Modified: commit.Committer.When}
		header.SetMode(0o644)
		if f.Mode == filemode.Executable {
			header.SetMode(0o755)
		}
		out, err := zw.CreateHeader(header)
		if err != nil {
			return err
		}
		r, err := f.Reader()
		if err != nil {
			return err
		}
		defer r.Close()
		_, err = io.Copy(out, r)
		return err
	})
	if err != nil {
		return err
	}
	return zw.Close()
}

func (s *Store) commit(space, app, id string) (*git.Repository, *object.Commit, error) {
	hash, err := parseCommitID(id)
	if err != nil {
		return nil, nil, err
	}
	repo, err := s.open(space, app, false)
	if err != nil {
		return nil, nil, err
	}
	commit, err := repo.CommitObject(hash)
	if err != nil {
		return nil, nil, notFound(err)
	}
	return repo, commit, nil
}

func (s *Store) describe(repo *git.Repository, c *object.Commit, builds map[plumbing.Hash][]int64) (*CommitDetail, error) {
	changes, err := s.changes(repo, c)
	if err != nil {
		return nil, err
	}
	parents := make([]string, 0, len(c.ParentHashes))
	for _, p := range c.ParentHashes {
		parents = append(parents, p.String())
	}
	kind := KindUpload
	if isPlatform(c) {
		kind = KindPlatform
	}
	detail := &CommitDetail{
		Commit: Commit{
			ID: c.Hash.String(), Parents: parents, Kind: kind,
			Author:  Author{Name: c.Author.Name, Email: c.Author.Email},
			Date:    c.Committer.When.UTC(),
			Message: strings.TrimSpace(c.Message),
			Builds:  builds[c.Hash],
		},
		Changes: changes,
	}
	for _, change := range changes {
		detail.Stats.Files++
		detail.Stats.Additions += change.Additions
		detail.Stats.Deletions += change.Deletions
	}
	return detail, nil
}

// changes compares a commit with its last parent (the empty tree for the
// first upload). Commits are immutable, so the result is cached.
func (s *Store) changes(repo *git.Repository, c *object.Commit) ([]Change, error) {
	if cached, ok := s.changeCache.get(c.Hash); ok {
		return cached, nil
	}
	before, after, err := commitTrees(repo, c)
	if err != nil {
		return nil, err
	}
	paths := map[string]bool{}
	for p, f := range after {
		if old, ok := before[p]; !ok || old != f {
			paths[p] = true
		}
	}
	for p := range before {
		if _, ok := after[p]; !ok {
			paths[p] = true
		}
	}
	changes := make([]Change, 0, len(paths))
	for _, p := range slices.Sorted(maps.Keys(paths)) {
		old, oldOK := before[p]
		cur, curOK := after[p]
		change, _, _, err := compare(repo, p, old, oldOK, cur, curOK)
		if err != nil {
			return nil, err
		}
		changes = append(changes, change)
	}
	s.changeCache.put(c.Hash, changes)
	return changes, nil
}

func commitTrees(repo *git.Repository, c *object.Commit) (before, after map[string]treeFile, err error) {
	tree, err := c.Tree()
	if err != nil {
		return nil, nil, err
	}
	if after, err = treeEntries(tree); err != nil {
		return nil, nil, err
	}
	before = map[string]treeFile{}
	if len(c.ParentHashes) == 0 {
		return before, after, nil
	}
	parent, err := repo.CommitObject(lastParent(c))
	if err != nil {
		return nil, nil, err
	}
	parentTree, err := parent.Tree()
	if err != nil {
		return nil, nil, err
	}
	before, err = treeEntries(parentTree)
	return before, after, err
}

// compare describes one path's change and returns both sides' text when a
// line diff applies.
func compare(repo *git.Repository, p string, old treeFile, oldOK bool, cur treeFile, curOK bool) (Change, string, string, error) {
	change := Change{Path: p, Status: "modified"}
	switch {
	case !oldOK:
		change.Status = "added"
	case !curOK:
		change.Status = "deleted"
	}
	var oldText, newText []byte
	var err error
	if oldOK {
		if oldText, change.TooLarge, err = blobText(repo, old.hash); err != nil {
			return change, "", "", err
		}
	}
	if curOK && !change.TooLarge {
		if newText, change.TooLarge, err = blobText(repo, cur.hash); err != nil {
			return change, "", "", err
		}
	}
	if change.TooLarge || len(oldText)+len(newText) > maxDiffSize {
		change.TooLarge = true
		return change, "", "", nil
	}
	if IsBinary(oldText) || IsBinary(newText) {
		change.Binary = true
		return change, "", "", nil
	}
	change.Additions, change.Deletions = lineCounts(string(oldText), string(newText))
	return change, string(oldText), string(newText), nil
}

func blobText(repo *git.Repository, hash plumbing.Hash) ([]byte, bool, error) {
	blob, err := repo.BlobObject(hash)
	if err != nil {
		return nil, false, err
	}
	if blob.Size > maxDiffSize {
		return nil, true, nil
	}
	r, err := blob.Reader()
	if err != nil {
		return nil, false, err
	}
	defer r.Close()
	content, err := io.ReadAll(r)
	return content, false, err
}

func lineCounts(old, cur string) (additions, deletions int) {
	if old == "" {
		return countLines(cur), 0
	}
	if cur == "" {
		return 0, countLines(old)
	}
	diff, err := udiff.ToUnifiedDiff("", "", old, udiff.Lines(old, cur), 0)
	if err != nil {
		return 0, 0
	}
	for _, h := range diff.Hunks {
		for _, l := range h.Lines {
			switch l.Kind {
			case udiff.Insert:
				additions++
			case udiff.Delete:
				deletions++
			}
		}
	}
	return additions, deletions
}

func countLines(s string) int {
	if s == "" {
		return 0
	}
	n := strings.Count(s, "\n")
	if !strings.HasSuffix(s, "\n") {
		n++
	}
	return n
}

// buildTags maps commits to the builds that used them.
func buildTags(repo *git.Repository) (map[plumbing.Hash][]int64, error) {
	refs, err := repo.References()
	if err != nil {
		return nil, err
	}
	builds := map[plumbing.Hash][]int64{}
	err = refs.ForEach(func(ref *plumbing.Reference) error {
		number, ok := strings.CutPrefix(ref.Name().String(), buildTagPrefix)
		if !ok {
			return nil
		}
		if n, err := strconv.ParseInt(number, 10, 64); err == nil {
			builds[ref.Hash()] = append(builds[ref.Hash()], n)
		}
		return nil
	})
	for hash := range builds {
		slices.Sort(builds[hash])
		slices.Reverse(builds[hash])
	}
	return builds, err
}

// notFound folds go-git's many "not there" errors into ErrNotFound.
func notFound(err error) error {
	switch {
	case errors.Is(err, plumbing.ErrObjectNotFound), errors.Is(err, object.ErrFileNotFound),
		errors.Is(err, object.ErrDirectoryNotFound), errors.Is(err, object.ErrEntryNotFound):
		return ErrNotFound
	}
	return err
}
