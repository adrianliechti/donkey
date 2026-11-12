package build

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"io/fs"
	"path/filepath"
	"regexp"
	"strings"

	"github.com/moby/buildkit/frontend/dockerfile/parser"
	"github.com/tonistiigi/fsutil"
	"github.com/tonistiigi/fsutil/types"
)

var packageBuildArgs = []string{
	"NPM_CONFIG_REGISTRY",
	"PIP_INDEX_URL",
	"PIP_TRUSTED_HOST",
	"GOPROXY",
	"GOSUMDB",
	"GONOSUMDB",
	"DONKEY_MAVEN_MIRROR",
	"MAVEN_REPO_CENTRAL",
	"MVNW_REPOURL",
	"DONKEY_NUGET_SOURCE",
	"UV_INDEX_URL",
	"DONKEY_DEBIAN_MIRROR",
	"DONKEY_ALPINE_MIRROR",
}

// overlayFS is a directory as a build sees it: every file as it is on disk,
// except the ones named here, which read as other contents. The app's own
// files stay byte-for-byte untouched.
type overlayFS struct {
	fsutil.FS
	// files holds the replaced contents by path relative to the directory.
	files map[string][]byte
}

// Walk must advertise the overlaid size as well as Open's overlaid bytes.
// BuildKit uses the stat from Walk when transferring a file and can
// otherwise truncate longer contents to the original on-disk size.
func (overlay *overlayFS) Walk(ctx context.Context, target string, fn fs.WalkDirFunc) error {
	return overlay.FS.Walk(ctx, target, func(path string, entry fs.DirEntry, err error) error {
		contents, replaced := overlay.files[filepath.Clean(path)]
		if err != nil || !replaced {
			return fn(path, entry, err)
		}
		info, err := entry.Info()
		if err != nil {
			return fn(path, entry, err)
		}
		stat, ok := info.Sys().(*types.Stat)
		if !ok {
			return fn(path, entry, nil)
		}
		updated := stat.Clone()
		updated.Size = int64(len(contents))
		return fn(path, &fsutil.DirEntryInfo{Stat: updated}, nil)
	})
}

func (overlay *overlayFS) Open(path string) (io.ReadCloser, error) {
	if contents, replaced := overlay.files[filepath.Clean(path)]; replaced {
		return io.NopCloser(bytes.NewReader(contents)), nil
	}
	return overlay.FS.Open(path)
}

func packageDockerfileFS(path string, args ...string) (fsutil.FS, error) {
	base, err := fsutil.NewFS(filepath.Dir(path))
	if err != nil {
		return nil, err
	}
	source, err := base.Open(filepath.Base(path))
	if err != nil {
		return nil, err
	}
	defer source.Close()
	contents, err := io.ReadAll(source)
	if err != nil {
		return nil, err
	}
	contents, err = injectPackageBuildArgs(contents, args...)
	if err != nil {
		return nil, fmt.Errorf("reading Dockerfile for package configuration: %w", err)
	}
	return &overlayFS{FS: base, files: map[string][]byte{filepath.Base(path): contents}}, nil
}

// injectPackageBuildArgs declares donkey's internal package endpoints in
// every stage. Dockerfile build args do not otherwise exist inside RUN, even
// when the frontend received --build-arg; doing this in an overlay leaves the
// app's Dockerfile byte-for-byte untouched on disk.
func injectPackageBuildArgs(source []byte, args ...string) ([]byte, error) {
	for _, key := range args {
		if !buildArgRE.MatchString(key) {
			return nil, fmt.Errorf("invalid build variable %q", key)
		}
	}
	result, err := parser.Parse(bytes.NewReader(source))
	if err != nil {
		return nil, err
	}
	after := map[int]bool{}
	for _, instruction := range result.AST.Children {
		if strings.EqualFold(instruction.Value, "from") {
			after[instruction.EndLine] = true
		}
	}
	if len(after) == 0 {
		return source, nil
	}

	var out bytes.Buffer
	for index, line := range bytes.SplitAfter(source, []byte("\n")) {
		out.Write(line)
		if !after[index+1] {
			continue
		}
		if len(line) > 0 && !bytes.HasSuffix(line, []byte("\n")) {
			out.WriteByte('\n')
		}
		seen := map[string]bool{}
		for _, name := range append(append([]string{}, packageBuildArgs...), args...) {
			if seen[name] {
				continue
			}
			seen[name] = true
			fmt.Fprintf(&out, "ARG %s\n", name)
		}
	}
	return out.Bytes(), nil
}

var buildArgRE = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]*$`)
