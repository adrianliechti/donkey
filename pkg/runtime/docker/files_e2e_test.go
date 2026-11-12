//go:build e2e

package docker

import (
	"bytes"
	"context"
	"crypto/rand"
	"errors"
	"io"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/docker/docker/client"
)

type brokenUploadReader struct{}

func (brokenUploadReader) Read([]byte) (int, error) { return 0, errors.New("upload interrupted") }

// DONKEY_E2E_FILES=true go test -tags e2e ./pkg/runtime/docker -run TestFilesTransfersDocker -v
func TestFilesTransfersDocker(t *testing.T) {
	if os.Getenv("DONKEY_E2E_FILES") != "true" {
		t.Skip("set DONKEY_E2E_FILES=true to exercise a real Docker volume and helper")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	defer cancel()
	cli, err := client.NewClientWithOpts(client.FromEnv, client.WithAPIVersionNegotiation())
	if err != nil {
		t.Fatal(err)
	}
	defer cli.Close()
	r := &Runtime{cli: cli}
	space, app := "files-test-"+strings.ToLower(rand.Text()[:8]), "upload"
	if err := r.ensureOwnedVolume(ctx, volName(space, app), appLabels(space, app, roleStorage)); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		cleanup, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		if err := r.removeOwnedContainer(cleanup, filesHelperName(space, app), appLabels(space, app, roleFiles)); err != nil {
			t.Error(err)
		}
		if err := r.removeOwnedVolume(cleanup, volName(space, app), appLabels(space, app, roleStorage)); err != nil {
			t.Error(err)
		}
	})
	if err := r.initVolumeDirs(ctx, volName(space, app), []string{"app-data"}, appLabels(space, app, roleInit)); err != nil {
		t.Fatal(err)
	}
	const specialFolder = `folder ' $dollar\backslash`
	if err := r.MakeDir(ctx, space, app, "app-data/"+specialFolder); err != nil {
		t.Fatal(err)
	}
	data := bytes.Repeat([]byte{0, 255, 1, '\n', '\r'}, 700000)
	for _, name := range []string{"file.bin", "file with spaces.bin", "empty", specialFolder + "/file.bin"} {
		t.Run(name, func(t *testing.T) {
			want := data
			if name == "empty" {
				want = nil
			}
			path := "app-data/" + name
			if err := r.WriteFile(ctx, space, app, path, bytes.NewReader(want)); err != nil {
				t.Fatal(err)
			}
			var got bytes.Buffer
			if err := r.ReadFile(ctx, space, app, path, &got); err != nil {
				t.Fatal(err)
			}
			if !bytes.Equal(got.Bytes(), want) {
				t.Fatalf("round trip differs: got %d bytes, want %d", got.Len(), len(want))
			}
			if err := r.WriteFile(ctx, space, app, path, io.MultiReader(strings.NewReader("partial"), brokenUploadReader{})); err == nil {
				t.Fatal("interrupted upload reported success")
			}
			got.Reset()
			if err := r.ReadFile(ctx, space, app, path, &got); err != nil {
				t.Fatal(err)
			}
			if !bytes.Equal(got.Bytes(), want) {
				t.Fatal("interrupted upload replaced the previous file")
			}
		})
	}
	t.Run("directory_destination", func(t *testing.T) {
		uploadErr := r.WriteFile(ctx, space, app, "app-data", strings.NewReader("directory overwrite"))
		entries, err := r.ListFiles(ctx, space, app, "app-data")
		if err != nil {
			t.Fatal(err)
		}
		var names []string
		for _, entry := range entries {
			names = append(names, entry.Name)
			if strings.HasPrefix(entry.Name, ".donkey-upload-") {
				t.Errorf("temporary upload left behind: %s", entry.Name)
			}
		}
		t.Logf("directory destination upload error=%v; files=%q", uploadErr, names)
		if uploadErr == nil {
			t.Fatal("upload to an existing directory reported success")
		}
	})
}
