// Package gitutil resolves the current commit without the git binary or a
// full git library — donkey only needs a short sha for image tags.
package gitutil

import (
	"bufio"
	"os"
	"path/filepath"
	"strings"
)

// HeadSHA returns the short (12 char) commit sha of HEAD for the repository
// containing dir, or "" when dir is not inside a git repository or HEAD
// cannot be resolved. It never fails: image tags fall back to timestamps.
func HeadSHA(dir string) string {
	_, gitDir := findRepo(dir)
	if gitDir == "" {
		return ""
	}
	head, err := os.ReadFile(filepath.Join(gitDir, "HEAD"))
	if err != nil {
		return ""
	}
	h := strings.TrimSpace(string(head))
	if !strings.HasPrefix(h, "ref: ") {
		return short(h) // detached HEAD: the file holds the sha itself
	}
	ref := strings.TrimPrefix(h, "ref: ")
	// Refs live in the common dir: for a linked worktree that is the main
	// repository's .git (named by the "commondir" file), not the worktree's
	// own gitdir.
	refDir := commonDir(gitDir)
	if b, err := os.ReadFile(filepath.Join(refDir, filepath.FromSlash(ref))); err == nil {
		return short(strings.TrimSpace(string(b)))
	}
	// Ref not loose — look it up in packed-refs.
	f, err := os.Open(filepath.Join(refDir, "packed-refs"))
	if err != nil {
		return ""
	}
	defer f.Close()
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		line := sc.Text()
		if strings.HasPrefix(line, "#") || strings.HasPrefix(line, "^") {
			continue
		}
		sha, name, ok := strings.Cut(line, " ")
		if ok && name == ref {
			return short(sha)
		}
	}
	return ""
}

// RepoRoot returns the folder containing the repository dir belongs to, or ""
// when dir is not inside one. donkey uses it to name spaces: every app in a
// repository belongs together.
func RepoRoot(dir string) string {
	root, _ := findRepo(dir)
	return root
}

// findRepo walks up from dir looking for .git and returns the repository root
// and its resolved git directory. A .git file (linked worktrees, submodules)
// points elsewhere via "gitdir: <path>".
func findRepo(dir string) (root, gitDir string) {
	dir, err := filepath.Abs(dir)
	if err != nil {
		return "", ""
	}
	for {
		g := filepath.Join(dir, ".git")
		fi, err := os.Stat(g)
		if err == nil {
			if fi.IsDir() {
				return dir, g
			}
			b, err := os.ReadFile(g)
			if err != nil {
				return dir, ""
			}
			target := strings.TrimSpace(strings.TrimPrefix(strings.TrimSpace(string(b)), "gitdir:"))
			if !filepath.IsAbs(target) {
				target = filepath.Join(dir, target)
			}
			return dir, target
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			return "", ""
		}
		dir = parent
	}
}

// commonDir resolves where refs live: gitDir itself, unless a "commondir"
// file (present in linked worktrees) points at the main repository's .git.
func commonDir(gitDir string) string {
	b, err := os.ReadFile(filepath.Join(gitDir, "commondir"))
	if err != nil {
		return gitDir
	}
	target := strings.TrimSpace(string(b))
	if !filepath.IsAbs(target) {
		target = filepath.Join(gitDir, target)
	}
	return target
}

func short(sha string) string {
	if len(sha) < 12 {
		return ""
	}
	return sha[:12]
}
