package server

import (
	"archive/tar"
	"compress/gzip"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
)

const (
	maxContextBytes = 512 << 20 // matches the ingress proxy-body-size
	maxFileCount    = 50000
)

// untarContext safely extracts an uploaded tar.gz build context into dir,
// rejecting path traversal, absolute paths, and links.
func untarContext(r io.Reader, dir string) error {
	gz, err := gzip.NewReader(io.LimitReader(r, maxContextBytes))
	if err != nil {
		return fmt.Errorf("the upload is not a valid build context: %w", err)
	}
	defer gz.Close()
	tr := tar.NewReader(gz)
	files := 0
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
		name := filepath.Clean(hdr.Name)
		if filepath.IsAbs(name) || strings.HasPrefix(name, "..") {
			return fmt.Errorf("the build context contains an invalid path %q", hdr.Name)
		}
		target := filepath.Join(dir, name)
		switch hdr.Typeflag {
		case tar.TypeDir:
			if err := os.MkdirAll(target, 0o755); err != nil {
				return err
			}
		case tar.TypeReg:
			if err := os.MkdirAll(filepath.Dir(target), 0o755); err != nil {
				return err
			}
			f, err := os.OpenFile(target, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, os.FileMode(hdr.Mode)&0o777|0o400)
			if err != nil {
				return err
			}
			if _, err := io.Copy(f, tr); err != nil {
				f.Close()
				return err
			}
			f.Close()
		default:
			// Symlinks and specials are dropped: nothing donkey builds needs
			// them in the uploaded context, and they are traversal vectors.
		}
	}
}
