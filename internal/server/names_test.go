package server

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"os"
	"path/filepath"
	"testing"

	"github.com/adrianliechti/donkey/internal/config"
)

// Non-developer folders carry spaces, capitals and parentheses. The app name
// has to become a legal one without anybody being asked to rename anything,
// and the files have to survive being packed and unpacked.
func TestAwkwardNamesSurvive(t *testing.T) {
	if got := config.SanitizeName("My Cool App"); got != "my-cool-app" {
		t.Fatalf(`SanitizeName("My Cool App") = %q, want "my-cool-app"`, got)
	}

	names := []string{
		"index.html",
		"styles/main style.css",
		"images/IMG_2024 (1).svg",
		"docs/notes (draft) v2.md",
	}
	var buf bytes.Buffer
	gz := gzip.NewWriter(&buf)
	tw := tar.NewWriter(gz)
	for _, n := range names {
		body := []byte("x")
		if err := tw.WriteHeader(&tar.Header{Name: n, Mode: 0o644, Size: int64(len(body))}); err != nil {
			t.Fatal(err)
		}
		if _, err := tw.Write(body); err != nil {
			t.Fatal(err)
		}
	}
	tw.Close()
	gz.Close()

	dir := t.TempDir()
	if _, err := extractContext(&buf, dir); err != nil {
		t.Fatal(err)
	}
	for _, n := range names {
		if _, err := os.Stat(filepath.Join(dir, filepath.FromSlash(n))); err != nil {
			t.Errorf("%s did not survive packing and unpacking: %v", n, err)
		}
	}
}
