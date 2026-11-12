package docker

import (
	"bytes"
	"context"
	"crypto/rand"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"path"
	"sort"
	"strconv"
	"strings"
	"time"

	cerrdefs "github.com/containerd/errdefs"
	"github.com/docker/docker/api/types/container"
	"github.com/docker/docker/api/types/mount"
	"github.com/docker/docker/pkg/stdcopy"

	"github.com/adrianliechti/donkey/internal/config"
	"github.com/adrianliechti/donkey/internal/filetransfer"
	rt "github.com/adrianliechti/donkey/pkg/runtime"
)

// The app's disk as a file browser — the same model as on Kubernetes: a
// small busybox helper holds the whole volume at /data (app images are
// commonly distroless, and the moment the files matter most is exactly when
// the app container is crashing), and every operation is a busybox command
// executed in it. The path rules and stat parsing are shared vocabulary
// with the Kubernetes provider, kept behavior-identical.

const (
	filesMount     = "/data"
	statFormat     = "%f|%s|%Y|%n"
	filesReadyWait = 60 * time.Second
)

// File type bits of a Unix mode, spelled out so this file stays portable.
const (
	sIFMT   = 0xF000
	sIFDIR  = 0x4000
	sIFLNK  = 0xA000
	sIFIFO  = 0x1000
	sIFSOCK = 0xC000
	sIFBLK  = 0x6000
	sIFCHR  = 0x2000
)

func filesHelperName(space, app string) string { return ctrName(space, app) + ".files" }

func (r *Runtime) filesImage() string {
	if r.opts.BusyboxImage != "" {
		return r.opts.BusyboxImage
	}
	return config.DefaultBusyboxImage
}

// filesHelper returns a running helper container for the app's disk,
// starting one if needed.
func (r *Runtime) filesHelper(ctx context.Context, space, app string) (string, error) {
	volume, err := r.cli.VolumeInspect(ctx, volName(space, app))
	if err != nil {
		// Tell the two "nothing to browse" cases apart.
		appInfo, containerErr := r.cli.ContainerInspect(ctx, ctrName(space, app))
		if cerrdefs.IsNotFound(containerErr) {
			return "", rt.ErrNotDeployed
		}
		if !cerrdefs.IsNotFound(err) {
			return "", err
		}
		if containerErr != nil {
			return "", containerErr
		}
		if appInfo.Config == nil {
			return "", fmt.Errorf("the Docker container %s has no inspectable configuration", ctrName(space, app))
		}
		if err := requireIdentity("container", ctrName(space, app), appInfo.Config.Labels,
			appLabels(space, app, roleWorkload)); err != nil {
			return "", err
		}
		return "", rt.ErrNoDisk
	}
	if err := requireIdentity("volume", volName(space, app), volume.Labels,
		appLabels(space, app, roleStorage)); err != nil {
		return "", err
	}
	name := filesHelperName(space, app)
	image := r.filesImage()
	info, err := r.cli.ContainerInspect(ctx, name)
	if err == nil && info.State != nil && info.State.Running {
		if info.Config == nil {
			return "", fmt.Errorf("the Docker container %s has no inspectable configuration", name)
		}
		if err := requireIdentity("container", name, info.Config.Labels,
			appLabels(space, app, roleFiles)); err != nil {
			return "", err
		}
		if info.Config.Image == image {
			return name, nil
		}
	}
	if err == nil {
		if err := r.removeOwnedContainer(ctx, name, appLabels(space, app, roleFiles)); err != nil {
			return "", err
		}
	} else if !cerrdefs.IsNotFound(err) {
		return "", err
	}
	if err := r.pullImage(ctx, image); err != nil {
		return "", err
	}
	labels := appLabels(space, app, roleFiles)
	_, err = r.cli.ContainerCreate(ctx, &container.Config{
		Image: image,
		// The helper stops by itself: browsing files once must not leave a
		// container behind forever.
		Entrypoint: []string{"sleep", "2700"},
		Labels:     labels,
	}, &container.HostConfig{
		AutoRemove: true,
		Mounts: []mount.Mount{{
			Type: mount.TypeVolume, Source: volName(space, app), Target: filesMount,
		}},
	}, nil, nil, name)
	if err != nil {
		if !strings.Contains(err.Error(), "already in use") {
			return "", fmt.Errorf("opening the app's disk: %w", err)
		}
		info, inspectErr := r.cli.ContainerInspect(ctx, name)
		if inspectErr != nil {
			return "", inspectErr
		}
		if info.Config == nil {
			return "", fmt.Errorf("the Docker container %s has no inspectable configuration", name)
		}
		if err := requireIdentity("container", name, info.Config.Labels, labels); err != nil {
			return "", err
		}
	}
	if err := r.cli.ContainerStart(ctx, name, container.StartOptions{}); err != nil {
		return "", err
	}
	deadline := time.Now().Add(filesReadyWait)
	for {
		info, err := r.cli.ContainerInspect(ctx, name)
		if err == nil && info.State != nil && info.State.Running {
			return name, nil
		}
		if time.Now().After(deadline) {
			return "", fmt.Errorf("the app's disk didn't open in time")
		}
		select {
		case <-ctx.Done():
			return "", ctx.Err()
		case <-time.After(500 * time.Millisecond):
		}
	}
}

// execFiles runs one command in the helper. Only the exit code decides
// failure — busybox tools write to stderr on success too (dd's transfer
// stats) — and stderr's first line then makes the error honest.
func (r *Runtime) execFiles(ctx context.Context, helper string, argv []string, stdin io.Reader, stdout io.Writer) error {
	exec, err := r.cli.ContainerExecCreate(ctx, helper, container.ExecOptions{
		Cmd:          argv,
		AttachStdin:  stdin != nil,
		AttachStdout: true,
		AttachStderr: true,
	})
	if err != nil {
		return err
	}
	attach, err := r.cli.ContainerExecAttach(ctx, exec.ID, container.ExecStartOptions{})
	if err != nil {
		return err
	}
	defer attach.Close()

	if stdout == nil {
		stdout = io.Discard
	}
	var stderr bytes.Buffer
	done := make(chan error, 1)
	go func() {
		_, err := stdcopy.StdCopy(stdout, &stderr, attach.Reader)
		done <- err
	}()
	if stdin != nil {
		if _, err := io.Copy(attach.Conn, stdin); err != nil {
			return err
		}
	}
	_ = attach.CloseWrite()
	if err := <-done; err != nil {
		return err
	}
	inspect, err := r.cli.ContainerExecInspect(ctx, exec.ID)
	if err != nil {
		return err
	}
	if inspect.ExitCode != 0 {
		if line := firstLine(strings.TrimSpace(stderr.String())); line != "" {
			return execError(fmt.Errorf("%s", line))
		}
		return fmt.Errorf("the command failed (exit code %d)", inspect.ExitCode)
	}
	return nil
}

// execError maps busybox's stderr onto the honest file errors.
func execError(err error) error {
	msg := err.Error()
	switch {
	case strings.Contains(msg, "No such file or directory"):
		return rt.ErrNotFound
	case strings.Contains(msg, "Is a directory"):
		return rt.ErrNotAFile
	case strings.Contains(msg, "Not a directory"):
		return rt.ErrNotAFolder
	}
	return err
}

func firstLine(s string) string {
	if i := strings.IndexByte(s, '\n'); i >= 0 {
		return s[:i]
	}
	return s
}

// resolveAll resolves paths in the helper and refuses anything that left the
// disk on the way — a shortcut on the volume must never become a way to
// read the machine's own files.
func (r *Runtime) resolveAll(ctx context.Context, helper string, paths ...string) ([]string, error) {
	var out bytes.Buffer
	if err := r.execFiles(ctx, helper, append([]string{"realpath"}, paths...), nil, &out); err != nil {
		return nil, err
	}
	lines := strings.Split(strings.TrimRight(out.String(), "\n"), "\n")
	if len(lines) != len(paths) {
		return nil, fmt.Errorf("donkey couldn't work out where %q is on the disk",
			strings.TrimPrefix(paths[0], filesMount+"/"))
	}
	for _, l := range lines {
		if !insideVolume(l) {
			return nil, fmt.Errorf("%w — it is a shortcut that leads off the app's disk", rt.ErrBadPath)
		}
	}
	return lines, nil
}

func (r *Runtime) ListFiles(ctx context.Context, space, app, subPath string) ([]rt.FileEntry, error) {
	rel, err := cleanRelPath(subPath)
	if err != nil {
		return nil, err
	}
	helper, err := r.filesHelper(ctx, space, app)
	if err != nil {
		return nil, err
	}
	resolved, err := r.resolveAll(ctx, helper, volumePath(rel))
	if err != nil {
		return nil, err
	}
	dir := resolved[0]
	// Bounded, as on Kubernetes: a folder with a million files stops being
	// listed once there is plainly enough, rather than buffering all of it.
	out := &capWriter{limit: rt.MaxListEntries * 1024}
	argv := []string{"find", dir, "-maxdepth", "1", "-exec", "stat", "-c", statFormat, "{}", ";"}
	if err := r.execFiles(ctx, helper, argv, nil, out); err != nil && !out.full() {
		return nil, err
	}
	entries := []rt.FileEntry{}
	self := false
	for _, line := range strings.Split(out.buf.String(), "\n") {
		e, full, ok := parseStatLine(line)
		if !ok {
			continue
		}
		if full == dir {
			if !e.IsDir {
				return nil, rt.ErrNotAFolder
			}
			self = true
			continue
		}
		if len(entries) >= rt.MaxListEntries {
			break
		}
		entries = append(entries, e)
	}
	if !self && len(entries) == 0 {
		return nil, rt.ErrNotFound
	}
	sort.Slice(entries, func(i, j int) bool {
		if entries[i].IsDir != entries[j].IsDir {
			return entries[i].IsDir
		}
		a, b := strings.ToLower(entries[i].Name), strings.ToLower(entries[j].Name)
		if a != b {
			return a < b
		}
		return entries[i].Name < entries[j].Name
	})
	return entries, nil
}

func (r *Runtime) StorageFolders(ctx context.Context, space, app string) ([]rt.StorageFolder, error) {
	spec, err := r.runRecord(space, app)
	if err != nil || spec == nil {
		return nil, err
	}
	var out []rt.StorageFolder
	for _, p := range spec.Paths {
		out = append(out, rt.StorageFolder{Name: config.StorageSubPath(p), Path: p})
	}
	return out, nil
}

func (r *Runtime) ReadFile(ctx context.Context, space, app, subPath string, w io.Writer) error {
	rel, err := cleanRelPath(subPath)
	if err != nil {
		return err
	}
	if rel == "" {
		return fmt.Errorf("%w — name the file you want to download", rt.ErrBadPath)
	}
	helper, err := r.filesHelper(ctx, space, app)
	if err != nil {
		return err
	}
	resolved, err := r.resolveAll(ctx, helper, volumePath(rel))
	if err != nil {
		return err
	}
	return r.execFiles(ctx, helper, []string{"cat", resolved[0]}, nil, w)
}

func (r *Runtime) WriteFile(ctx context.Context, space, app, subPath string, in io.Reader) error {
	rel, err := cleanRelPath(subPath)
	if err != nil {
		return err
	}
	if rel == "" {
		return fmt.Errorf("%w — name the file you want to write", rt.ErrBadPath)
	}
	helper, err := r.filesHelper(ctx, space, app)
	if err != nil {
		return err
	}
	// Resolve the whole destination, not just its folder: a name that is
	// already a symlink pointing off the disk would otherwise make `mv`
	// write through it. A name that doesn't exist yet resolves to itself.
	paths, err := r.resolveAll(ctx, helper, volumePath(parentRel(rel)), volumePath(rel))
	if err != nil {
		return err
	}
	dir, target := paths[0], paths[1]
	tmp := dir + "/.donkey-upload-" + strings.ToLower(rand.Text()[:12])

	if err := filetransfer.Write(in, tmp, func(argv []string, stdin io.Reader, stdout io.Writer) error {
		return r.execFiles(ctx, helper, argv, stdin, stdout)
	}); err != nil {
		r.removeQuietly(ctx, helper, tmp)
		return err
	}
	// The app runs as some other user and only shares the disk's group, so a
	// file donkey wrote must be group-writable or the app can never replace it.
	if err := r.execFiles(ctx, helper, []string{"chmod", "664", tmp}, nil, nil); err != nil {
		r.removeQuietly(ctx, helper, tmp)
		return err
	}
	if err := r.execFiles(ctx, helper, []string{"mv", "-fT", tmp, target}, nil, nil); err != nil {
		r.removeQuietly(ctx, helper, tmp)
		return err
	}
	return nil
}

func (r *Runtime) RemoveFile(ctx context.Context, space, app, subPath string) error {
	rel, err := cleanRelPath(subPath)
	if err != nil {
		return err
	}
	if rel == "" {
		return fmt.Errorf("%w — the disk itself cannot be deleted here (use `donkey destroy`)", rt.ErrBadPath)
	}
	helper, err := r.filesHelper(ctx, space, app)
	if err != nil {
		return err
	}
	// Only the parent is resolved, as on Kubernetes: rm removes a shortcut
	// itself and never follows it, so deleting a link cannot delete what it
	// points at — and a dangling link stays deletable.
	dir, err := r.resolveAll(ctx, helper, volumePath(parentRel(rel)))
	if err != nil {
		return err
	}
	if dir[0] == filesMount && path.Base(rel) == "" {
		return fmt.Errorf("%w — the disk itself cannot be deleted here (use `donkey destroy`)", rt.ErrBadPath)
	}
	return r.execFiles(ctx, helper, []string{"rm", "-rf", dir[0] + "/" + path.Base(rel)}, nil, nil)
}

func (r *Runtime) MoveFile(ctx context.Context, space, app, from, to string) error {
	fromRel, err := cleanRelPath(from)
	if err != nil {
		return err
	}
	toRel, err := cleanRelPath(to)
	if err != nil {
		return err
	}
	if fromRel == "" || toRel == "" {
		return fmt.Errorf("%w — name what to move and where to", rt.ErrBadPath)
	}
	helper, err := r.filesHelper(ctx, space, app)
	if err != nil {
		return err
	}
	// The source's parent, not the source: mv must move a shortcut itself
	// rather than what it points at.
	paths, err := r.resolveAll(ctx, helper, volumePath(parentRel(fromRel)), volumePath(parentRel(toRel)), volumePath(toRel))
	if err != nil {
		return err
	}
	return r.execFiles(ctx, helper, []string{"mv", paths[0] + "/" + path.Base(fromRel), paths[2]}, nil, nil)
}

func (r *Runtime) MakeDir(ctx context.Context, space, app, subPath string) error {
	rel, err := cleanRelPath(subPath)
	if err != nil {
		return err
	}
	if rel == "" {
		return fmt.Errorf("%w — name the folder you want to create", rt.ErrBadPath)
	}
	helper, err := r.filesHelper(ctx, space, app)
	if err != nil {
		return err
	}
	dir, err := r.resolveAll(ctx, helper, volumePath(parentRel(rel)))
	if err != nil {
		return err
	}
	// -m 775 and -p, as on Kubernetes: a bare mkdir leaves the folder
	// root-owned 0755 inside a group-writable disk, so a non-root app cannot
	// write into what the browser just made — and without -p, creating a
	// folder that exists is an error rather than a no-op.
	return r.execFiles(ctx, helper,
		[]string{"mkdir", "-m", "775", "-p", dir[0] + "/" + path.Base(rel)}, nil, nil)
}

func (r *Runtime) removeQuietly(ctx context.Context, helper, target string) {
	ctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 30*time.Second)
	defer cancel()
	_ = r.execFiles(ctx, helper, []string{"rm", "-f", target}, nil, nil)
}

func cleanRelPath(p string) (string, error) {
	if strings.TrimSpace(p) == "" {
		return "", nil
	}
	if strings.ContainsRune(p, 0) {
		return "", fmt.Errorf("%w — it contains a character that isn't allowed in a name", rt.ErrBadPath)
	}
	if strings.HasPrefix(p, "/") {
		return "", fmt.Errorf("%w — paths are relative to the disk, so leave the leading slash off (app-data/uploads, not /app-data/uploads)", rt.ErrBadPath)
	}
	clean := path.Clean(p)
	if clean == "." {
		return "", nil
	}
	if clean == ".." || strings.HasPrefix(clean, "../") {
		return "", fmt.Errorf("%w — it points outside the app's disk", rt.ErrBadPath)
	}
	return clean, nil
}

func volumePath(rel string) string {
	if rel == "" {
		return filesMount
	}
	return filesMount + "/" + rel
}

func parentRel(rel string) string {
	dir := path.Dir(rel)
	if dir == "." || dir == "/" {
		return ""
	}
	return dir
}

func insideVolume(p string) bool {
	return p == filesMount || strings.HasPrefix(p, filesMount+"/")
}

func parseStatLine(line string) (rt.FileEntry, string, bool) {
	line = strings.TrimRight(line, "\r")
	parts := strings.SplitN(line, "|", 4)
	if len(parts) != 4 {
		return rt.FileEntry{}, "", false
	}
	raw, err := strconv.ParseUint(strings.TrimSpace(parts[0]), 16, 32)
	if err != nil {
		return rt.FileEntry{}, "", false
	}
	size, err := strconv.ParseInt(strings.TrimSpace(parts[1]), 10, 64)
	if err != nil {
		return rt.FileEntry{}, "", false
	}
	secs, err := strconv.ParseInt(strings.TrimSpace(parts[2]), 10, 64)
	if err != nil {
		return rt.FileEntry{}, "", false
	}
	full := parts[3]
	if full == "" {
		return rt.FileEntry{}, "", false
	}
	return rt.FileEntry{
		Name:    path.Base(full),
		Size:    size,
		Mode:    modeText(uint32(raw)),
		ModTime: time.Unix(secs, 0).UTC(),
		IsDir:   raw&sIFMT == sIFDIR,
		IsLink:  raw&sIFMT == sIFLNK,
	}, full, true
}

func modeText(raw uint32) string {
	m := fs.FileMode(raw & 0o777)
	switch raw & sIFMT {
	case sIFDIR:
		m |= fs.ModeDir
	case sIFLNK:
		m |= fs.ModeSymlink
	case sIFIFO:
		m |= fs.ModeNamedPipe
	case sIFSOCK:
		m |= fs.ModeSocket
	case sIFBLK:
		m |= fs.ModeDevice
	case sIFCHR:
		m |= fs.ModeDevice | fs.ModeCharDevice
	}
	return m.String()
}

// capWriter keeps a bounded amount of output and then asks to be stopped.
type capWriter struct {
	buf   bytes.Buffer
	limit int
}

func (w *capWriter) Write(p []byte) (int, error) {
	room := w.limit - w.buf.Len()
	if room <= 0 {
		return 0, errCapReached
	}
	if len(p) > room {
		w.buf.Write(p[:room])
		return len(p), errCapReached
	}
	w.buf.Write(p)
	return len(p), nil
}

// full reports that the writer stopped the command rather than the command
// stopping itself — the output is a beginning, not the whole answer.
func (w *capWriter) full() bool { return w.buf.Len() >= w.limit }

var errCapReached = errors.New("output limit reached")
