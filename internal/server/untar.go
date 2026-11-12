package server

import (
	"archive/tar"
	"archive/zip"
	"compress/gzip"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/adrianliechti/donkey/internal/config"
)

// extractContext unpacks an uploaded build context into dir. The CLI sends
// tar.gz; the browser sends zip — the format is decided by the bytes, not by
// a header a client could get wrong. It reports what it left out: a file the
// build needed but never got is a mystery worth preventing.
func extractContext(r io.Reader, dir string) ([]string, error) {
	tmp, err := os.CreateTemp("", "donkey-upload-*")
	if err != nil {
		return nil, err
	}
	defer os.Remove(tmp.Name())
	defer tmp.Close()
	size, err := io.Copy(tmp, r)
	if err != nil {
		return nil, fmt.Errorf("reading the upload: %w", err)
	}
	var magic [4]byte
	if _, err := tmp.ReadAt(magic[:], 0); err != nil {
		return nil, fmt.Errorf("the upload is not a valid build context")
	}
	var skipped skipList
	switch {
	case magic[0] == 'P' && magic[1] == 'K':
		if err := unzipContext(tmp, size, dir, &skipped); err != nil {
			return nil, err
		}
		// Zips usually wrap everything in one folder (GitHub downloads,
		// macOS "Compress") — the app lives one level down then.
		return skipped.list(), stripSingleRoot(dir)
	case magic[0] == 0x1f && magic[1] == 0x8b:
		if _, err := tmp.Seek(0, io.SeekStart); err != nil {
			return nil, err
		}
		return skipped.list(), untarContext(tmp, dir, &skipped)
	default:
		return nil, fmt.Errorf("the upload is not a valid build context — expected tar.gz (CLI) or zip (console)")
	}
}

// maxSkipReported bounds what the skip list hands back: it is there to be
// read, and nobody reads the fortieth line of it.
const maxSkipReported = 12

// skipList collects what the upload lost on the way in, without letting a
// node_modules of ten thousand files become ten thousand lines — a directory
// rule reports the directory, once.
type skipList struct {
	seen map[string]bool
	out  []string
}

func (s *skipList) add(path string) {
	if s.seen == nil {
		s.seen = map[string]bool{}
	}
	if s.seen[path] {
		return
	}
	s.seen[path] = true
	s.out = append(s.out, path)
}

func (s *skipList) list() []string {
	if len(s.out) <= maxSkipReported {
		return s.out
	}
	return append(append([]string{}, s.out[:maxSkipReported]...),
		fmt.Sprintf("and %d more", len(s.out)-maxSkipReported))
}

// untarContext safely extracts a tar.gz build context into dir, rejecting
// path traversal, absolute paths, and links.
func untarContext(r io.Reader, dir string, skipped *skipList) error {
	gz, err := gzip.NewReader(r)
	if err != nil {
		return fmt.Errorf("the upload is not a valid build context: %w", err)
	}
	defer gz.Close()
	tr := tar.NewReader(gz)
	for {
		hdr, err := tr.Next()
		if err == io.EOF {
			return nil
		}
		if err != nil {
			return fmt.Errorf("reading the build context: %w", err)
		}
		name, ok := safeRelPath(hdr.Name)
		if !ok {
			return fmt.Errorf("the build context contains an invalid path %q", hdr.Name)
		}
		// The same exclusions the CLI applies while packing, enforced here so
		// they hold for any client, not only donkey's own.
		if left, ok := skippedUploadPath(name); ok {
			skipped.add(left)
			continue
		}
		target := filepath.Join(dir, name)
		switch hdr.Typeflag {
		case tar.TypeDir:
			if err := os.MkdirAll(target, 0o755); err != nil {
				return err
			}
		case tar.TypeReg:
			// Archives don't have to list a directory before the files in it
			// (`tar czf ctx.tgz $(git ls-files)` doesn't).
			if err := os.MkdirAll(filepath.Dir(target), 0o755); err != nil {
				return err
			}
			err := writeExtracted(target, tr, os.FileMode(hdr.Mode))
			if err != nil {
				return err
			}
		default:
			// Symlinks and specials are dropped: nothing donkey builds needs
			// them in the uploaded context, and they are traversal vectors.
		}
	}
}

// unzipContext safely extracts a zip build context into dir with the same
// guards as the tar path, plus the CLI's pack-time skip rules — a browser
// upload arrives as-is, so node_modules and key material are dropped here.
func unzipContext(f *os.File, size int64, dir string, skipped *skipList) error {
	zr, err := zip.NewReader(f, size)
	if err != nil {
		return fmt.Errorf("the upload is not a valid zip: %w", err)
	}
	for _, entry := range zr.File {
		name, ok := safeRelPath(entry.Name)
		if !ok {
			return fmt.Errorf("the build context contains an invalid path %q", entry.Name)
		}
		if left, ok := skippedUploadPath(name); ok {
			skipped.add(left)
			continue
		}
		target := filepath.Join(dir, name)
		isDir := strings.HasSuffix(entry.Name, "/") || entry.FileInfo().IsDir()
		if isDir {
			// A skip-listed directory must not even be created: an empty
			// .git/ can flip detection heuristics, and any extra top-level
			// entry defeats the single-root unwrap below.
			if config.SkippedUploadDir(filepath.Base(name)) {
				skipped.add(filepath.ToSlash(name) + "/")
				continue
			}
			if err := os.MkdirAll(target, 0o755); err != nil {
				return err
			}
			continue
		}
		if !entry.FileInfo().Mode().IsRegular() {
			continue // symlinks and specials are dropped, as in the tar path
		}
		if err := os.MkdirAll(filepath.Dir(target), 0o755); err != nil {
			return err
		}
		rc, err := entry.Open()
		if err != nil {
			return fmt.Errorf("reading %s from the zip: %w", entry.Name, err)
		}
		err = writeExtracted(target, rc, entry.FileInfo().Mode())
		rc.Close()
		if err != nil {
			return err
		}
	}
	return nil
}

// skippedUploadPath applies the CLI's pack-time exclusions server-side, so
// the guarantee holds for every client. Returns what to report as left
// behind: the directory that did it, or the file itself.
func skippedUploadPath(name string) (string, bool) {
	parts := strings.Split(filepath.ToSlash(name), "/")
	for i, p := range parts[:len(parts)-1] {
		if config.SkippedUploadDir(p) {
			return strings.Join(parts[:i+1], "/") + "/", true
		}
	}
	if config.SensitiveUploadFile(parts[len(parts)-1]) {
		return filepath.ToSlash(name), true
	}
	return "", false
}

// stripSingleRoot moves a lone top-level folder's contents up one level, so
// a zip that wraps the app in "myapp-main/" deploys like the folder itself.
func stripSingleRoot(dir string) error {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return err
	}
	if len(entries) != 1 || !entries[0].IsDir() {
		return nil
	}
	root := filepath.Join(dir, entries[0].Name())
	inner, err := os.ReadDir(root)
	if err != nil {
		return err
	}
	// A wrapper containing something with its own name (myapp/myapp) would
	// collide on the move — leave such a zip untouched rather than failing
	// the whole deploy.
	for _, e := range inner {
		if e.Name() == entries[0].Name() {
			return nil
		}
	}
	for _, e := range inner {
		if err := os.Rename(filepath.Join(root, e.Name()), filepath.Join(dir, e.Name())); err != nil {
			return err
		}
	}
	return os.Remove(root)
}

// contextRoot finds the folder that actually holds the app — an upload is not
// always rooted at it. An explicit path wins; otherwise a single donkey.yaml
// decides, and several make it the user's call. Returns the folder and its
// path relative to the upload ("" when it is the upload itself).
func contextRoot(dir, explicit string) (string, string, error) {
	if explicit != "" {
		sub, ok := safeRelPath(explicit)
		if !ok {
			return "", "", fmt.Errorf("%q is not a folder inside the upload", explicit)
		}
		full := filepath.Join(dir, sub)
		if fi, err := os.Stat(full); err != nil || !fi.IsDir() {
			return "", "", fmt.Errorf("the upload has no folder %q", explicit)
		}
		return full, filepath.ToSlash(sub), nil
	}
	if _, err := os.Stat(filepath.Join(dir, config.AppFileName)); err == nil {
		return dir, "", nil
	}
	found := findAppConfigs(dir)
	switch len(found) {
	case 0:
		// Nothing to go on — build the upload as given (a name and port from
		// the console can still carry it).
		return dir, "", nil
	case 1:
		rel, err := filepath.Rel(dir, found[0])
		if err != nil {
			return dir, "", nil
		}
		return found[0], filepath.ToSlash(rel), nil
	default:
		var rels []string
		for _, f := range found {
			if rel, err := filepath.Rel(dir, f); err == nil {
				rels = append(rels, filepath.ToSlash(rel))
			}
		}
		sort.Strings(rels)
		return "", "", fmt.Errorf(
			"this upload holds several apps (%s) — deploy one at a time from its own folder, or name the folder with `path=`",
			strings.Join(rels, ", "))
	}
}

// findAppConfigs finds the nearest folders holding a donkey.yaml, regardless
// of their depth within the upload (dir itself excluded).
func findAppConfigs(dir string) []string {
	var found []string
	level := []string{dir}
	for len(level) > 0 && len(found) == 0 {
		var next []string
		for _, parent := range level {
			entries, err := os.ReadDir(parent)
			if err != nil {
				continue
			}
			for _, e := range entries {
				if !e.IsDir() || config.SkippedUploadDir(e.Name()) {
					continue
				}
				child := filepath.Join(parent, e.Name())
				next = append(next, child)
				if _, err := os.Stat(filepath.Join(child, config.AppFileName)); err == nil {
					found = append(found, child)
				}
			}
		}
		level = next
	}
	return found
}

func safeRelPath(name string) (string, bool) {
	// Legacy Windows zip tools write backslash separators; on Linux they
	// would extract as one literal filename (and dodge the skip rules), so
	// they are normalized first.
	clean := filepath.Clean(filepath.FromSlash(strings.ReplaceAll(name, `\`, "/")))
	if filepath.IsAbs(clean) || clean == ".." || strings.HasPrefix(clean, ".."+string(filepath.Separator)) {
		return "", false
	}
	// Windows drive-relative paths ("C:foo") are not absolute on Linux.
	if len(clean) > 1 && clean[1] == ':' {
		return "", false
	}
	return clean, true
}

// writeExtracted streams an entry to disk without a size ceiling. Archive
// readers report truncation and checksum failures instead of accepting a
// partial source file. Path confinement is enforced before opening the file.
func writeExtracted(target string, r io.Reader, mode os.FileMode) error {
	f, err := os.OpenFile(target, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, mode&0o777|0o400)
	if err != nil {
		return err
	}
	_, err = io.Copy(f, r)
	closeErr := f.Close()
	if err != nil {
		return err
	}
	return closeErr
}
