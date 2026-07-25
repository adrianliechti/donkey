package apiclient

import (
	"archive/tar"
	"compress/gzip"
	"io"
	"os"
	"path/filepath"
	"strings"
)

// skipDirs are never uploaded: build inputs come from sources, not local
// artifacts or VCS internals.
var skipDirs = map[string]bool{
	".git": true, "node_modules": true, ".venv": true, "venv": true,
	"__pycache__": true, ".idea": true, ".vscode": true,
}

// TarContext streams dir as tar.gz into w.
func TarContext(dir string, w io.Writer) error {
	gz := gzip.NewWriter(w)
	tw := tar.NewWriter(gz)
	err := filepath.WalkDir(dir, func(path string, d os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel, err := filepath.Rel(dir, path)
		if err != nil || rel == "." {
			return nil
		}
		rel = filepath.ToSlash(rel)
		if d.IsDir() {
			if skipDirs[d.Name()] {
				return filepath.SkipDir
			}
			return tw.WriteHeader(&tar.Header{Name: rel + "/", Typeflag: tar.TypeDir, Mode: 0o755})
		}
		if !d.Type().IsRegular() {
			return nil // symlinks etc. are not uploaded
		}
		if sensitiveFile(d.Name()) {
			return nil // never ship key material to the build
		}
		info, err := d.Info()
		if err != nil {
			return err
		}
		if err := tw.WriteHeader(&tar.Header{
			Name: rel, Mode: int64(info.Mode().Perm()), Size: info.Size(), ModTime: info.ModTime(),
		}); err != nil {
			return err
		}
		f, err := os.Open(path)
		if err != nil {
			return err
		}
		_, err = io.Copy(tw, f)
		f.Close()
		return err
	})
	if err != nil {
		return err
	}
	if err := tw.Close(); err != nil {
		return err
	}
	return gz.Close()
}

// TarContextToTemp writes the context to a temp file and returns it opened
// for reading (so the HTTP request has a known length and is retryable).
func TarContextToTemp(dir string) (*os.File, error) {
	f, err := os.CreateTemp("", "donkey-context-*.tar.gz")
	if err != nil {
		return nil, err
	}
	if err := TarContext(dir, f); err != nil {
		f.Close()
		os.Remove(f.Name())
		return nil, err
	}
	if _, err := f.Seek(0, io.SeekStart); err != nil {
		f.Close()
		os.Remove(f.Name())
		return nil, err
	}
	return f, nil
}

// Basename-based guard against uploading obvious secret files even when the
// user has them lying around; mirrors the AI summarizer denylist spirit.
func sensitiveFile(name string) bool {
	lower := strings.ToLower(name)
	return strings.HasPrefix(lower, ".env") || strings.HasSuffix(lower, ".pem") || strings.HasPrefix(lower, "id_")
}
