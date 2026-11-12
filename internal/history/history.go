// Package history keeps every app's source as a git repository: what each
// deploy uploaded, and what donkey's build changed on top of it.
//
// Each app has one bare repository, <dir>/<space>/<app>.git, written with
// go-git — no git binary, no work tree. Two branches tell the two authors
// apart:
//
//   - main is the uploads. Every deploy whose files differ from the last one
//     adds a commit by the person who deployed, so main diffs read as "what
//     they changed".
//   - platform is what was actually built, when donkey's build altered the
//     upload (a generated .donkey/Dockerfile, an automatic source repair).
//     Each such commit merges the upload it was built from into the branch:
//     the previous platform commit is its first parent, the upload its last.
//
// Every commit's change is read against its last parent — the previous
// upload for an upload, the upload it built for a platform commit — so the
// two authors never show up in each other's diffs. Tags name builds:
// build-<n> points at the commit build n used. The layout is plain git on
// purpose: a copy of the volume opens in any git client.
package history

import (
	"bytes"
	"cmp"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"time"

	"github.com/go-git/go-git/v5"
	"github.com/go-git/go-git/v5/plumbing"
	"github.com/go-git/go-git/v5/plumbing/object"

	"github.com/adrianliechti/donkey/internal/config"
)

const (
	MainBranch     = plumbing.ReferenceName("refs/heads/main")
	PlatformBranch = plumbing.ReferenceName("refs/heads/platform")
	buildTagPrefix = "refs/tags/build-"

	// MaxFileSize and MaxSnapshotSize bound what one commit keeps. A file
	// beyond either is left out and named in the commit message: history is
	// for source, and one checked-in video should not fill the volume.
	MaxFileSize     = 8 << 20
	MaxSnapshotSize = 128 << 20

	// maxDiffSize is the most content (both sides together) a line diff is
	// computed over; beyond it the change is reported without line counts.
	maxDiffSize = 2 << 20
)

// ErrNotFound covers an app without history, an unknown commit and a path
// that is not in the commit.
var ErrNotFound = errors.New("not found")

// ErrInvalid marks a request the history cannot mean: a malformed commit id,
// a path outside the tree, a name that is not a space or app.
var ErrInvalid = errors.New("invalid source history request")

type invalidError string

func (e invalidError) Error() string        { return string(e) }
func (e invalidError) Is(target error) bool { return target == ErrInvalid }

func invalid(format string, a ...any) error { return invalidError(fmt.Sprintf(format, a...)) }

var commitIDRE = regexp.MustCompile(`^[0-9a-f]{40}$`)

// Store is the set of repositories under one directory.
type Store struct {
	dir string
	// locks serializes writers per repository and keeps readers off a pack
	// that a repack is about to delete.
	locks sync.Map
	// changes caches what each commit changed. Commits never change, so
	// neither do their changes; the cache only has to stay bounded.
	changeCache changeCache
}

// changeCacheSize bounds the cache: a listing touches at most a few hundred
// commits, and starting over beyond that only costs recomputing some diffs.
const changeCacheSize = 4096

type changeCache struct {
	mu      sync.Mutex
	entries map[plumbing.Hash][]Change
}

func (c *changeCache) get(hash plumbing.Hash) ([]Change, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	changes, ok := c.entries[hash]
	return changes, ok
}

func (c *changeCache) put(hash plumbing.Hash, changes []Change) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.entries == nil || len(c.entries) >= changeCacheSize {
		c.entries = map[plumbing.Hash][]Change{}
	}
	c.entries[hash] = changes
}

// New opens (and creates) the store's directory.
func New(dir string) (*Store, error) {
	if dir == "" {
		return nil, errors.New("the source history directory is not set")
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return nil, fmt.Errorf("creating the source history directory: %w", err)
	}
	return &Store{dir: dir}, nil
}

// Author is who a commit is attributed to.
type Author struct {
	Name  string `json:"name"`
	Email string `json:"email,omitempty"`
}

// Platform is the author of every change donkey's build made itself.
var Platform = Author{Name: "Donkey Platform", Email: "platform@donkey"}

func (s *Store) lock(space, app string) *sync.RWMutex {
	value, _ := s.locks.LoadOrStore(space+"/"+app, &sync.RWMutex{})
	return value.(*sync.RWMutex)
}

func (s *Store) repoPath(space, app string) (string, error) {
	if !config.ValidName(space) || !config.ValidName(app) {
		return "", invalid("invalid source history scope %q/%q", space, app)
	}
	return filepath.Join(s.dir, space, app+".git"), nil
}

// open returns the app's repository; create makes it on first use.
func (s *Store) open(space, app string, create bool) (*git.Repository, error) {
	dir, err := s.repoPath(space, app)
	if err != nil {
		return nil, err
	}
	repo, err := git.PlainOpen(dir)
	if errors.Is(err, git.ErrRepositoryNotExists) {
		if !create {
			return nil, ErrNotFound
		}
		if err := os.MkdirAll(filepath.Dir(dir), 0o755); err != nil {
			return nil, err
		}
		// HEAD is main: a clone of the volume checks out the uploads.
		return git.PlainInitWithOptions(dir, &git.PlainInitOptions{
			Bare: true, InitOptions: git.InitOptions{DefaultBranch: MainBranch},
		})
	}
	return repo, err
}

// RemoveApp deletes an app's repository; its next deploy starts a new one.
func (s *Store) RemoveApp(space, app string) error {
	dir, err := s.repoPath(space, app)
	if err != nil {
		return err
	}
	mu := s.lock(space, app)
	mu.Lock()
	defer mu.Unlock()
	return os.RemoveAll(dir)
}

// RemoveSpace deletes every repository of a space. The folder is renamed
// aside first, in one step: a deploy recording at that moment can then only
// start a fresh repository, never write into a half-deleted one.
func (s *Store) RemoveSpace(space string) error {
	if !config.ValidName(space) {
		return invalid("invalid space %q", space)
	}
	doomed, err := os.MkdirTemp(s.dir, ".removed-"+space+"-")
	if err != nil {
		return err
	}
	err = os.Rename(filepath.Join(s.dir, space), filepath.Join(doomed, space))
	if err != nil && !errors.Is(err, fs.ErrNotExist) {
		os.Remove(doomed)
		return err
	}
	return os.RemoveAll(doomed)
}

// parseCommitID accepts the full hex form only: ids come from listings, and a
// prefix would make a URL mean different commits as history grows.
func parseCommitID(id string) (plumbing.Hash, error) {
	if !commitIDRE.MatchString(id) {
		return plumbing.ZeroHash, invalid("%q is not a commit id", id)
	}
	return plumbing.NewHash(id), nil
}

// cleanPath turns a request path into a tree path, refusing anything that
// would leave the tree.
func cleanPath(p string) (string, error) {
	p = strings.TrimPrefix(filepath.ToSlash(p), "/")
	clean := path.Clean(p)
	if p == "" || clean == "." || clean == ".." || strings.HasPrefix(clean, "../") || strings.ContainsRune(clean, 0) {
		return "", invalid("%q is not a file in this version", p)
	}
	return clean, nil
}

// signature is git's author line. A person without a display name is shown
// by their email, and the time is whole seconds — all git keeps. Names come
// from the IdP, so the characters that delimit the line are dropped, as git
// itself does: a "<" or a newline would otherwise break the commit.
func signature(a Author, when time.Time) object.Signature {
	email := signatureText(a.Email)
	return object.Signature{
		Name:  cmp.Or(signatureText(a.Name), email, "unknown"),
		Email: email,
		When:  when.UTC().Truncate(time.Second),
	}
}

var signatureCleaner = strings.NewReplacer("<", "", ">", "", "\n", " ", "\r", " ", "\x00", "")

func signatureText(s string) string {
	return strings.TrimSpace(signatureCleaner.Replace(s))
}

// isPlatform tells the platform's commits from people's: only the build signs
// with the platform's own identity.
func isPlatform(c *object.Commit) bool {
	return c.Author.Name == Platform.Name && c.Author.Email == Platform.Email
}

// IsBinary is git's own test: a NUL byte near the start.
func IsBinary(content []byte) bool {
	return bytes.IndexByte(content[:min(len(content), 8000)], 0) >= 0
}

// skipped reports a path git refuses to check out: a .git anywhere in it
// (a worktree's gitfile, a submodule's) would make the repository unclonable.
func skipped(p string) bool {
	for part := range strings.SplitSeq(p, "/") {
		if strings.EqualFold(part, ".git") {
			return true
		}
	}
	return false
}
