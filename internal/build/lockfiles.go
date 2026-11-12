package build

import (
	"bytes"
	"io/fs"
	"os"
	"path/filepath"

	"github.com/tonistiigi/fsutil"

	"github.com/adrianliechti/donkey/internal/packages"
)

// mirroredContextFS is the app's folder as the build receives it: every file
// as uploaded, except that the lockfiles name the package mirror wherever
// they named a registry. A lockfile records where each package was
// downloaded from — the public registry, an enterprise one, `donkey
// packages` on a developer's machine — and a frozen install follows those
// addresses rather than the registry the build configures. Inside a build
// only the mirror is certain to answer, and only it holds the upstream's
// credential, so without this a perfectly good lockfile fails the install
// that was meant to honor it. Versions and checksums are untouched: the
// build installs exactly what the lockfile pins.
//
// Registries the app's own configuration assigns to particular packages are
// read first and left alone. See packages.RewriteLockfile.
func mirroredContextFS(dir, mirror string) (fsutil.FS, error) {
	base, err := fsutil.NewFS(dir)
	if err != nil {
		return nil, err
	}
	var lockfiles, declared []string
	err = filepath.WalkDir(dir, func(path string, entry fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if entry.IsDir() {
			// Packages ship lockfiles of their own; nothing installs from them.
			if entry.Name() == "node_modules" || entry.Name() == ".git" {
				return filepath.SkipDir
			}
			return nil
		}
		switch {
		case !entry.Type().IsRegular():
		case packages.IsLockfile(entry.Name()):
			lockfiles = append(lockfiles, path)
		case packages.IsRegistryConfig(entry.Name()):
			content, err := os.ReadFile(path)
			if err != nil {
				return err
			}
			declared = append(declared, packages.DeclaredRegistries(entry.Name(), content)...)
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	files := map[string][]byte{}
	for _, path := range lockfiles {
		content, err := os.ReadFile(path)
		if err != nil {
			return nil, err
		}
		if rewritten := packages.RewriteLockfile(filepath.Base(path), content, mirror, declared); !bytes.Equal(rewritten, content) {
			relative, err := filepath.Rel(dir, path)
			if err != nil {
				return nil, err
			}
			files[relative] = rewritten
		}
	}
	if len(files) == 0 {
		return base, nil
	}
	return &overlayFS{FS: base, files: files}, nil
}
