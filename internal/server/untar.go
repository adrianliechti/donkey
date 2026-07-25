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

const (
	maxContextBytes = 512 << 20 // the largest build context donkey accepts
	// maxExtractBytes caps the *unpacked* size: gzip expands up to ~1000x, so
	// limiting the compressed stream alone would let one upload fill the
	// control plane's disk.
	maxExtractBytes = int64(2) << 30
	maxFileCount    = 50000
)

// extractContext unpacks an uploaded build context into dir. The CLI sends
// tar.gz; the browser sends zip — the format is decided by the bytes, not by
// a header a client could get wrong.
func extractContext(r io.Reader, dir string) error {
	tmp, err := os.CreateTemp("", "donkey-upload-*")
	if err != nil {
		return err
	}
	defer os.Remove(tmp.Name())
	defer tmp.Close()
	size, err := io.Copy(tmp, io.LimitReader(r, maxContextBytes+1))
	if err != nil {
		return fmt.Errorf("reading the upload: %w", err)
	}
	if size > maxContextBytes {
		return fmt.Errorf("the upload is too large (over %d MB)", maxContextBytes>>20)
	}
	var magic [4]byte
	if _, err := tmp.ReadAt(magic[:], 0); err != nil {
		return fmt.Errorf("the upload is not a valid build context")
	}
	switch {
	case magic[0] == 'P' && magic[1] == 'K':
		if err := unzipContext(tmp, size, dir); err != nil {
			return err
		}
		// Zips usually wrap everything in one folder (GitHub downloads,
		// macOS "Compress") — the app lives one level down then.
		return stripSingleRoot(dir)
	case magic[0] == 0x1f && magic[1] == 0x8b:
		if _, err := tmp.Seek(0, io.SeekStart); err != nil {
			return err
		}
		return untarContext(tmp, dir)
	default:
		return fmt.Errorf("the upload is not a valid build context — expected tar.gz (CLI) or zip (console)")
	}
}

// untarContext safely extracts a tar.gz build context into dir, rejecting
// path traversal, absolute paths, and links.
func untarContext(r io.Reader, dir string) error {
	gz, err := gzip.NewReader(r)
	if err != nil {
		return fmt.Errorf("the upload is not a valid build context: %w", err)
	}
	defer gz.Close()
	tr := tar.NewReader(gz)
	files := 0
	remaining := maxExtractBytes
	for {
		hdr, err := tr.Next()
		if err == io.EOF {
			return nil
		}
		if err != nil {
			return fmt.Errorf("reading the build context: %w", err)
		}
		files++
		if files > maxFileCount {
			return fmt.Errorf("the build context has too many files")
		}
		name, ok := safeRelPath(hdr.Name)
		if !ok {
			return fmt.Errorf("the build context contains an invalid path %q", hdr.Name)
		}
		// The same exclusions the CLI applies while packing, enforced here so
		// they hold for any client, not only donkey's own.
		if skippedUploadPath(name) {
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
			n, err := writeExtracted(target, tr, os.FileMode(hdr.Mode), remaining)
			if err != nil {
				return err
			}
			remaining -= n
		default:
			// Symlinks and specials are dropped: nothing donkey builds needs
			// them in the uploaded context, and they are traversal vectors.
		}
	}
}

// unzipContext safely extracts a zip build context into dir with the same
// guards as the tar path, plus the CLI's pack-time skip rules — a browser
// upload arrives as-is, so node_modules and key material are dropped here.
func unzipContext(f *os.File, size int64, dir string) error {
	zr, err := zip.NewReader(f, size)
	if err != nil {
		return fmt.Errorf("the upload is not a valid zip: %w", err)
	}
	files := 0
	remaining := maxExtractBytes
	for _, entry := range zr.File {
		files++
		if files > maxFileCount {
			return fmt.Errorf("the build context has too many files")
		}
		name, ok := safeRelPath(entry.Name)
		if !ok {
			return fmt.Errorf("the build context contains an invalid path %q", entry.Name)
		}
		if skippedUploadPath(name) {
			continue
		}
		target := filepath.Join(dir, name)
		isDir := strings.HasSuffix(entry.Name, "/") || entry.FileInfo().IsDir()
		if isDir {
			// A skip-listed directory must not even be created: an empty
			// .git/ can flip detection heuristics, and any extra top-level
			// entry defeats the single-root unwrap below.
			if config.SkippedUploadDir(filepath.Base(name)) {
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
		n, err := writeExtracted(target, rc, entry.FileInfo().Mode(), remaining)
		rc.Close()
		if err != nil {
			return err
		}
		remaining -= n
	}
	return nil
}

// skippedUploadPath applies the CLI's pack-time exclusions to an uploaded
// path: VCS/artifact directories anywhere in the path, and secret-shaped
// basenames. Enforced server-side so the guarantee holds for every client,
// not only donkey's own.
func skippedUploadPath(name string) bool {
	parts := strings.Split(filepath.ToSlash(name), "/")
	for _, p := range parts[:len(parts)-1] {
		if config.SkippedUploadDir(p) {
			return true
		}
	}
	return config.SensitiveUploadFile(parts[len(parts)-1])
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

// maxRootSearchDepth bounds how deep donkey looks for an app's donkey.yaml
// inside an upload before giving up.
const maxRootSearchDepth = 3

// contextRoot picks the folder inside an upload that actually holds the app.
// An upload is not always rooted at the app: a repo whose service lives in
// backend/, a folder dragged from one level up, a monorepo zip. An explicit
// path always wins; otherwise a single donkey.yaml decides, and several make
// it the user's call rather than a guess. Returns the folder to build and the
// path it took relative to the upload ("" when it is the upload itself).
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
	found := findAppConfigs(dir, maxRootSearchDepth)
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

// findAppConfigs lists the folders holding a donkey.yaml, breadth-first, up
// to depth levels below dir (dir itself excluded).
func findAppConfigs(dir string, depth int) []string {
	var found []string
	level := []string{dir}
	for d := 0; d < depth && len(found) == 0; d++ {
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

// writeExtracted copies one entry, reading one byte past the remaining
// budget so an oversized context is reported instead of silently truncated —
// a half-written source file would fail the build with a nonsense error.
func writeExtracted(target string, r io.Reader, mode os.FileMode, remaining int64) (int64, error) {
	f, err := os.OpenFile(target, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, mode&0o777|0o400)
	if err != nil {
		return 0, err
	}
	n, err := io.Copy(f, io.LimitReader(r, remaining+1))
	f.Close()
	if err != nil {
		return n, err
	}
	if n > remaining {
		return n, errTooLarge()
	}
	return n, nil
}

func errTooLarge() error {
	return fmt.Errorf("the build context is too large once unpacked (over %d GB)", maxExtractBytes>>30)
}
