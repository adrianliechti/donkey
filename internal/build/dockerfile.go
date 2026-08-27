package build

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"io/fs"
	"path/filepath"
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
}

type dockerfileOverlay struct {
	fsutil.FS
	name     string
	contents []byte
}

// Walk must advertise the overlaid size as well as Open's overlaid bytes.
// BuildKit uses the stat from Walk when transferring the Dockerfile and can
// otherwise truncate injected arguments to the original on-disk size.
func (overlay *dockerfileOverlay) Walk(ctx context.Context, target string, fn fs.WalkDirFunc) error {
	return overlay.FS.Walk(ctx, target, func(path string, entry fs.DirEntry, err error) error {
		if err != nil || filepath.Clean(path) != filepath.Clean(overlay.name) {
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
		updated.Size = int64(len(overlay.contents))
		return fn(path, &fsutil.DirEntryInfo{Stat: updated}, nil)
	})
}

func (fs *dockerfileOverlay) Open(path string) (io.ReadCloser, error) {
	if filepath.Clean(path) == filepath.Clean(fs.name) {
		return io.NopCloser(bytes.NewReader(fs.contents)), nil
	}
	return fs.FS.Open(path)
}

func packageDockerfileFS(path string) (fsutil.FS, error) {
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
	contents, err = injectPackageBuildArgs(contents)
	if err != nil {
		return nil, fmt.Errorf("reading Dockerfile for package configuration: %w", err)
	}
	return &dockerfileOverlay{FS: base, name: filepath.Base(path), contents: contents}, nil
}

// injectPackageBuildArgs declares donkey's internal package endpoints in
// every stage. Dockerfile build args do not otherwise exist inside RUN, even
// when the frontend received --build-arg; doing this in an overlay leaves the
// app's Dockerfile byte-for-byte untouched on disk.
func injectPackageBuildArgs(source []byte) ([]byte, error) {
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
		for _, name := range packageBuildArgs {
			fmt.Fprintf(&out, "ARG %s\n", name)
		}
	}
	return out.Bytes(), nil
}
