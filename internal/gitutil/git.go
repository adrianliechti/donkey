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
	gitDir := findGitDir(dir)
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
	if b, err := os.ReadFile(filepath.Join(gitDir, filepath.FromSlash(ref))); err == nil {
		return short(strings.TrimSpace(string(b)))
	}
	// Ref not loose — look it up in packed-refs.
	f, err := os.Open(filepath.Join(gitDir, "packed-refs"))
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

// findGitDir walks up from dir looking for .git; handles .git files
// (worktrees/submodules) pointing elsewhere via "gitdir: <path>".
func findGitDir(dir string) string {
	dir, err := filepath.Abs(dir)
	if err != nil {
		return ""
	}
	for {
		g := filepath.Join(dir, ".git")
		fi, err := os.Stat(g)
		if err == nil {
			if fi.IsDir() {
				return g
			}
			b, err := os.ReadFile(g)
			if err != nil {
				return ""
			}
			target := strings.TrimSpace(strings.TrimPrefix(strings.TrimSpace(string(b)), "gitdir:"))
			if !filepath.IsAbs(target) {
				target = filepath.Join(dir, target)
			}
			return target
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			return ""
		}
		dir = parent
	}
}

func short(sha string) string {
	if len(sha) < 12 {
		return ""
	}
	return sha[:12]
}
