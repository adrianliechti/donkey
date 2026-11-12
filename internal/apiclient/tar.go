package apiclient

import (
	"archive/tar"
	"compress/gzip"
	"io"
	"os"
	"path/filepath"

	"github.com/adrianliechti/donkey/internal/config"
)

// TarContext streams dir as tar.gz into w. It returns the paths it left out
// (secret-shaped files, symlinks) so the caller can say so — a file the build
// needed but never received is a debugging mystery otherwise.
func TarContext(dir string, w io.Writer) ([]string, error) {
	gz := gzip.NewWriter(w)
	tw := tar.NewWriter(gz)
	var skipped []string
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
			if config.SkippedUploadDir(d.Name()) {
				return filepath.SkipDir
			}
			return tw.WriteHeader(&tar.Header{Name: rel + "/", Typeflag: tar.TypeDir, Mode: 0o755})
		}
		if !d.Type().IsRegular() {
			skipped = append(skipped, rel)
			return nil // symlinks etc. are not uploaded
		}
		if config.SensitiveUploadFile(d.Name()) {
			skipped = append(skipped, rel)
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
		return nil, err
	}
	if err := tw.Close(); err != nil {
		return nil, err
	}
	return skipped, gz.Close()
}

// TarContextToTemp writes the context to a temp file and returns it opened
// for reading (so the HTTP request has a known length and is retryable),
// along with the paths that were left out.
func TarContextToTemp(dir string) (*os.File, []string, error) {
	f, err := os.CreateTemp("", "donkey-context-*.tar.gz")
	if err != nil {
		return nil, nil, err
	}
	skipped, err := TarContext(dir, f)
	if err != nil {
		f.Close()
		os.Remove(f.Name())
		return nil, nil, err
	}
	if _, err := f.Seek(0, io.SeekStart); err != nil {
		f.Close()
		os.Remove(f.Name())
		return nil, nil, err
	}
	return f, skipped, nil
}
