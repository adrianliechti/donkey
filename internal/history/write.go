package history

import (
	"errors"
	"fmt"
	"io"
	"io/fs"
	"log"
	"os"
	"path"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/go-git/go-git/v5"
	"github.com/go-git/go-git/v5/plumbing"
	"github.com/go-git/go-git/v5/plumbing/filemode"
	"github.com/go-git/go-git/v5/plumbing/format/packfile"
	"github.com/go-git/go-git/v5/plumbing/object"
	"github.com/go-git/go-git/v5/plumbing/storer"
	"github.com/go-git/go-git/v5/storage/memory"
)

// repackAfter is how many packs a repository collects before they are
// merged into one. Every commit lands as its own small pack — one file on
// the volume instead of one per object, which matters on NFS — and opening
// a repository reads every pack's index, so they must not pile up.
const repackAfter = 16

// packGrace is how old a pack must be before a repack may delete it.
const packGrace = time.Hour

// UploadInput is one deploy's files, as uploaded.
type UploadInput struct {
	// Dir is the uploaded build context on disk.
	Dir     string
	Author  Author
	When    time.Time
	Message string
}

// FileChange is one file donkey's build changed in its copy of the upload.
type FileChange struct {
	Path       string
	Content    []byte
	Executable bool
	Deleted    bool
}

// PlatformInput is what a build changed on top of an upload commit.
type PlatformInput struct {
	Base    string
	Changes []FileChange
	When    time.Time
	Message string
}

// treeFile is one file of a tree being assembled.
type treeFile struct {
	hash plumbing.Hash
	mode filemode.FileMode
}

// RecordUpload commits a deploy's upload on main, unless it is identical to
// the last one — a redeploy of unchanged files is not a change. It returns
// the commit that stands for the upload, new or not.
func (s *Store) RecordUpload(space, app string, in UploadInput) (string, error) {
	mu := s.lock(space, app)
	mu.Lock()
	defer mu.Unlock()
	repo, err := s.open(space, app, true)
	if err != nil {
		return "", err
	}
	w := newObjectWriter(repo)
	files, omitted, err := w.readDir(in.Dir)
	if err != nil {
		return "", err
	}
	tree, err := w.tree(files)
	if err != nil {
		return "", err
	}
	head, err := branchHead(repo, MainBranch)
	if err != nil {
		return "", err
	}
	if head != nil && head.TreeHash == tree {
		return head.Hash.String(), nil
	}
	var parents []plumbing.Hash
	if head != nil {
		parents = []plumbing.Hash{head.Hash}
	}
	sig := signature(in.Author, in.When)
	commit, err := w.commit(&object.Commit{
		Author: sig, Committer: sig, Message: withOmitted(in.Message, omitted),
		TreeHash: tree, ParentHashes: parents,
	})
	if err != nil {
		return "", err
	}
	if err := w.flush(); err != nil {
		return "", err
	}
	if err := advance(repo, MainBranch, commit, head); err != nil {
		return "", err
	}
	s.maybeRepack(space, app, repo)
	return commit.String(), nil
}

// RecordPlatform commits the base upload with the build's changes applied on
// the platform branch. Changes that cancel out (or none) record nothing and
// name the base itself; the same result as the last build of that base
// names that commit again.
func (s *Store) RecordPlatform(space, app string, in PlatformInput) (string, error) {
	baseHash, err := parseCommitID(in.Base)
	if err != nil {
		return "", err
	}
	mu := s.lock(space, app)
	mu.Lock()
	defer mu.Unlock()
	repo, err := s.open(space, app, false)
	if err != nil {
		return "", err
	}
	base, err := repo.CommitObject(baseHash)
	if err != nil {
		return "", notFound(err)
	}
	baseTree, err := base.Tree()
	if err != nil {
		return "", err
	}
	files, err := treeEntries(baseTree)
	if err != nil {
		return "", err
	}
	w := newObjectWriter(repo)
	var omitted []string
	for _, change := range in.Changes {
		p, err := cleanPath(change.Path)
		if err != nil {
			return "", err
		}
		if skipped(p) {
			continue
		}
		if change.Deleted {
			delete(files, p)
			continue
		}
		if len(change.Content) > MaxFileSize {
			omitted = append(omitted, fmt.Sprintf("%s (%s)", p, humanSize(int64(len(change.Content)))))
			continue
		}
		hash, err := w.blob(change.Content)
		if err != nil {
			return "", err
		}
		files[p] = treeFile{hash: hash, mode: fileMode(change.Executable)}
	}
	tree, err := w.tree(files)
	if err != nil {
		return "", err
	}
	if tree == base.TreeHash {
		return base.Hash.String(), nil
	}
	head, err := branchHead(repo, PlatformBranch)
	if err != nil {
		return "", err
	}
	if head != nil && head.TreeHash == tree && lastParent(head) == base.Hash {
		return head.Hash.String(), nil
	}
	parents := []plumbing.Hash{base.Hash}
	if head != nil {
		parents = []plumbing.Hash{head.Hash, base.Hash}
	}
	sig := signature(Platform, in.When)
	commit, err := w.commit(&object.Commit{
		Author: sig, Committer: sig, Message: withOmitted(in.Message, omitted),
		TreeHash: tree, ParentHashes: parents,
	})
	if err != nil {
		return "", err
	}
	if err := w.flush(); err != nil {
		return "", err
	}
	if err := advance(repo, PlatformBranch, commit, head); err != nil {
		return "", err
	}
	s.maybeRepack(space, app, repo)
	return commit.String(), nil
}

// TagBuild names the commit a build used: build-<n>, like a release tag. A
// tag never moves: if the build numbers ever start over (a reset telemetry
// volume), old commits keep their builds and the newcomer stays untagged.
func (s *Store) TagBuild(space, app string, build int64, commit string) error {
	if build <= 0 {
		return nil
	}
	hash, err := parseCommitID(commit)
	if err != nil {
		return err
	}
	mu := s.lock(space, app)
	mu.Lock()
	defer mu.Unlock()
	repo, err := s.open(space, app, false)
	if err != nil {
		return err
	}
	name := plumbing.ReferenceName(buildTagPrefix + strconv.FormatInt(build, 10))
	if existing, err := repo.Reference(name, false); err == nil {
		if existing.Hash() != hash {
			return fmt.Errorf("build %d is already tagged on %s", build, existing.Hash())
		}
		return nil
	}
	return repo.Storer.SetReference(plumbing.NewHashReference(name, hash))
}

// lastParent is what a commit's change is read against (see the package doc).
func lastParent(c *object.Commit) plumbing.Hash {
	if len(c.ParentHashes) == 0 {
		return plumbing.ZeroHash
	}
	return c.ParentHashes[len(c.ParentHashes)-1]
}

// previousPlatform is the platform commit before this one, if any: only a
// merge has one, the first platform commit's sole parent is its upload.
func previousPlatform(c *object.Commit) plumbing.Hash {
	if len(c.ParentHashes) < 2 {
		return plumbing.ZeroHash
	}
	return c.ParentHashes[0]
}

func branchHead(repo *git.Repository, branch plumbing.ReferenceName) (*object.Commit, error) {
	ref, err := repo.Reference(branch, false)
	if errors.Is(err, plumbing.ErrReferenceNotFound) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return repo.CommitObject(ref.Hash())
}

// advance moves a branch from the head this write started from. The in-process
// lock already serializes writers; the check is the guard for a second
// process sharing the volume.
func advance(repo *git.Repository, branch plumbing.ReferenceName, commit plumbing.Hash, from *object.Commit) error {
	next := plumbing.NewHashReference(branch, commit)
	if from == nil {
		return repo.Storer.SetReference(next)
	}
	return repo.Storer.CheckAndSetReference(next, plumbing.NewHashReference(branch, from.Hash))
}

// maybeRepack merges a repository's packs once there are enough of them. It
// runs after the commit is safely in place, so a failure here costs only
// tidiness: it is logged, never reported as a failed write, and the next
// commit tries again.
func (s *Store) maybeRepack(space, app string, repo *git.Repository) {
	packed, ok := repo.Storer.(storer.PackedObjectStorer)
	if !ok {
		return
	}
	packs, err := packed.ObjectPacks()
	if err != nil {
		log.Printf("source history %s/%s: listing packs: %v", space, app, err)
		return
	}
	// Packs from the last hour are neither counted nor deleted: during a
	// rolling update another donkey-server may have just written one for a
	// commit it has not published yet. They join the next repack once old.
	settled := time.Now().Add(-packGrace)
	dir, _ := s.repoPath(space, app)
	old := 0
	for _, pack := range packs {
		info, err := os.Stat(filepath.Join(dir, "objects", "pack", "pack-"+pack.String()+".pack"))
		if err == nil && info.ModTime().Before(settled) {
			old++
		}
	}
	if old >= repackAfter {
		err = repo.RepackObjects(&git.RepackConfig{OnlyDeletePacksOlderThan: settled})
	}
	if err != nil {
		log.Printf("source history %s/%s: repacking: %v", space, app, err)
	}
}

// objectWriter collects the objects a commit adds in memory, then writes them
// as one pack. Objects the repository already has are not written again.
type objectWriter struct {
	repo   *git.Repository
	mem    *memory.Storage
	hashes []plumbing.Hash
	size   int64
}

func newObjectWriter(repo *git.Repository) *objectWriter {
	return &objectWriter{repo: repo, mem: memory.NewStorage()}
}

func (w *objectWriter) put(obj plumbing.EncodedObject) (plumbing.Hash, error) {
	hash := obj.Hash()
	if w.repo.Storer.HasEncodedObject(hash) == nil {
		return hash, nil
	}
	if _, err := w.mem.EncodedObject(plumbing.AnyObject, hash); err == nil {
		return hash, nil
	}
	if _, err := w.mem.SetEncodedObject(obj); err != nil {
		return plumbing.ZeroHash, err
	}
	w.hashes = append(w.hashes, hash)
	return hash, nil
}

func (w *objectWriter) blob(content []byte) (plumbing.Hash, error) {
	obj := w.mem.NewEncodedObject()
	obj.SetType(plumbing.BlobObject)
	obj.SetSize(int64(len(content)))
	writer, err := obj.Writer()
	if err != nil {
		return plumbing.ZeroHash, err
	}
	if _, err := writer.Write(content); err != nil {
		return plumbing.ZeroHash, err
	}
	if err := writer.Close(); err != nil {
		return plumbing.ZeroHash, err
	}
	return w.put(obj)
}

// readDir turns an upload into tree files. Only regular files count — the
// upload never carries links — and the size limits leave out what history
// should not keep, reported back so the commit can say so.
func (w *objectWriter) readDir(dir string) (map[string]treeFile, []string, error) {
	files := map[string]treeFile{}
	var omitted []string
	err := filepath.WalkDir(dir, func(full string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel, err := filepath.Rel(dir, full)
		if err != nil {
			return err
		}
		rel = filepath.ToSlash(rel)
		if skipped(rel) {
			if d.IsDir() {
				return filepath.SkipDir
			}
			return nil
		}
		if !d.Type().IsRegular() {
			return nil
		}
		info, err := d.Info()
		if err != nil {
			return err
		}
		if info.Size() > MaxFileSize || w.size+info.Size() > MaxSnapshotSize {
			omitted = append(omitted, fmt.Sprintf("%s (%s)", rel, humanSize(info.Size())))
			return nil
		}
		content, err := os.ReadFile(full)
		if err != nil {
			return err
		}
		w.size += int64(len(content))
		hash, err := w.blob(content)
		if err != nil {
			return err
		}
		files[rel] = treeFile{hash: hash, mode: fileMode(info.Mode()&0o111 != 0)}
		return nil
	})
	return files, omitted, err
}

// tree writes the nested tree objects for a flat set of files and returns
// the root's hash.
func (w *objectWriter) tree(files map[string]treeFile) (plumbing.Hash, error) {
	type dir struct {
		files map[string]treeFile
		dirs  map[string]*dir
	}
	newDir := func() *dir { return &dir{files: map[string]treeFile{}, dirs: map[string]*dir{}} }
	root := newDir()
	for p, f := range files {
		parts := strings.Split(p, "/")
		at := root
		for _, part := range parts[:len(parts)-1] {
			next, ok := at.dirs[part]
			if !ok {
				next = newDir()
				at.dirs[part] = next
			}
			at = next
		}
		at.files[parts[len(parts)-1]] = f
	}
	var write func(d *dir) (plumbing.Hash, error)
	write = func(d *dir) (plumbing.Hash, error) {
		entries := make([]object.TreeEntry, 0, len(d.files)+len(d.dirs))
		for name, f := range d.files {
			entries = append(entries, object.TreeEntry{Name: name, Mode: f.mode, Hash: f.hash})
		}
		for name, sub := range d.dirs {
			hash, err := write(sub)
			if err != nil {
				return plumbing.ZeroHash, err
			}
			entries = append(entries, object.TreeEntry{Name: name, Mode: filemode.Dir, Hash: hash})
		}
		sort.Sort(object.TreeEntrySorter(entries))
		obj := w.mem.NewEncodedObject()
		if err := (&object.Tree{Entries: entries}).Encode(obj); err != nil {
			return plumbing.ZeroHash, err
		}
		return w.put(obj)
	}
	return write(root)
}

func (w *objectWriter) commit(c *object.Commit) (plumbing.Hash, error) {
	obj := w.mem.NewEncodedObject()
	if err := c.Encode(obj); err != nil {
		return plumbing.ZeroHash, err
	}
	return w.put(obj)
}

// flush writes the collected objects into the repository as one pack.
func (w *objectWriter) flush() (err error) {
	if len(w.hashes) == 0 {
		return nil
	}
	writer, ok := w.repo.Storer.(storer.PackfileWriter)
	if !ok {
		for _, hash := range w.hashes {
			obj, err := w.mem.EncodedObject(plumbing.AnyObject, hash)
			if err != nil {
				return err
			}
			if _, err := w.repo.Storer.SetEncodedObject(obj); err != nil {
				return err
			}
		}
		return nil
	}
	pack, err := writer.PackfileWriter()
	if err != nil {
		return err
	}
	defer func() {
		if cerr := pack.Close(); err == nil {
			err = cerr
		}
	}()
	_, err = packfile.NewEncoder(pack, w.mem, false).Encode(w.hashes, 10)
	return err
}

// treeEntries flattens a tree into its files.
func treeEntries(tree *object.Tree) (map[string]treeFile, error) {
	files := map[string]treeFile{}
	walker := object.NewTreeWalker(tree, true, nil)
	defer walker.Close()
	for {
		name, entry, err := walker.Next()
		if err != nil {
			if errors.Is(err, io.EOF) {
				return files, nil
			}
			return nil, err
		}
		if entry.Mode == filemode.Dir || entry.Mode == filemode.Submodule {
			continue
		}
		files[path.Clean(name)] = treeFile{hash: entry.Hash, mode: entry.Mode}
	}
}

func fileMode(executable bool) filemode.FileMode {
	if executable {
		return filemode.Executable
	}
	return filemode.Regular
}

// withOmitted appends the files a commit left out to its message, so the
// history says what it is missing rather than silently differing.
func withOmitted(message string, omitted []string) string {
	message = strings.TrimSpace(message)
	if len(omitted) == 0 {
		return message + "\n"
	}
	shown := omitted
	if len(shown) > 20 {
		shown = shown[:20]
	}
	note := "Not kept, too large for the source history: " + strings.Join(shown, ", ")
	if len(omitted) > len(shown) {
		note += fmt.Sprintf(" and %d more", len(omitted)-len(shown))
	}
	return message + "\n\n" + note + "\n"
}

func humanSize(n int64) string {
	const unit = 1024
	if n < unit {
		return fmt.Sprintf("%d B", n)
	}
	div, exp := int64(unit), 0
	for m := n / unit; m >= unit; m /= unit {
		div *= unit
		exp++
	}
	return fmt.Sprintf("%.1f %ciB", float64(n)/float64(div), "KMGTPE"[exp])
}
