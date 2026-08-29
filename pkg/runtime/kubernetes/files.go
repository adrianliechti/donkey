package kubernetes

import (
	"bytes"
	"context"
	"crypto/rand"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"net/url"
	"path"
	"sort"
	"strconv"
	"strings"
	"time"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/kubernetes/scheme"
	"k8s.io/client-go/tools/remotecommand"
	utilexec "k8s.io/client-go/util/exec"
	"k8s.io/streaming/pkg/httpstream"

	rt "github.com/adrianliechti/donkey/pkg/runtime"
	"github.com/adrianliechti/donkey/pkg/runtime/kubernetes/manifests"
)

// The files of an app's disk are reached through a small helper pod, never
// through the app's own container: app images are commonly distroless (no
// shell to exec into), and the moment the files matter most — a full disk, an
// app that keeps crashing — is exactly the moment no app pod is running.
// The helper mounts the whole volume at /data, so every folder the app stores
// (one per storage path, see config.StorageSubPath) is visible at once.

const (
	filesMount     = "/data"
	filesContainer = "files"

	// The helper stops by itself: an admin who browses files once must not
	// leave a pod behind that holds the app's disk attached to a node.
	//
	// Lifetime must outlast the longest operation, and a helper is only
	// reused while at least that long remains — otherwise a large upload
	// started near the end would be killed halfway, leaving its temp file on
	// the disk with no pod left to clean it up. Keep this above
	// server.filesTransferTimeout.
	filesHelperLifetime = 45 * time.Minute
	filesHelperGrace    = 25 * time.Minute

	filesReadyTimeout = 90 * time.Second

	// statFormat asks busybox stat for: raw mode (hex), size, mtime, name.
	// Everything but the name is numeric, so splitting on "|" from the left
	// survives names that contain the separator.
	statFormat = "%f|%s|%Y|%n"

	annotationFilesExpires = manifests.AnnotationFilesExpires
)

// File type bits of a Unix mode, spelled out so this file stays portable
// (the donkey CLI also builds on Windows, where syscall has no S_IF*).
const (
	sIFMT   = 0xF000
	sIFREG  = 0x8000
	sIFDIR  = 0x4000
	sIFLNK  = 0xA000
	sIFIFO  = 0x1000
	sIFSOCK = 0xC000
	sIFBLK  = 0x6000
	sIFCHR  = 0x2000
)

var (
	// FilesHelperImage is the image the helper pod runs. It only needs a
	// shell-free toolbox (find, stat, cat, dd, mv, rm, mkdir); a mirror can be
	// set here for clusters that can't reach Docker Hub.
	FilesHelperImage = "busybox:1.37"
)

// filesHelperRef binds every command to the exact helper that was checked.
// Names are deterministic and can be reused after deletion; the UID prevents
// a replacement Pod with that name from inheriting an already-authorized file
// operation, while space/app keep the ownership check explicit at each hop.
type filesHelperRef struct {
	name  string
	uid   types.UID
	space string
	app   string
}

// ListFiles returns the entries of one folder on the app's disk. An empty
// subPath lists the root of the volume, where each app storage path has its
// own folder.
func (c *Client) ListFiles(ctx context.Context, namespace, objectName, space, app, subPath string) ([]rt.FileEntry, error) {
	rel, err := cleanRelPath(subPath)
	if err != nil {
		return nil, err
	}
	helper, err := c.filesHelper(ctx, namespace, objectName, space, app)
	if err != nil {
		return nil, err
	}
	dir, err := c.resolveInside(ctx, namespace, helper, volumePath(rel))
	if err != nil {
		return nil, err
	}
	// -maxdepth 1 without -mindepth also reports the folder itself, which is
	// how "you asked for a file" is told apart from "the folder is empty" —
	// without a second round trip.
	out := &capWriter{limit: rt.MaxListEntries * 1024}
	argv := []string{"find", dir, "-maxdepth", "1", "-exec", "stat", "-c", statFormat, "{}", ";"}
	// A folder with a million files stops being listed once there is plainly
	// enough: the command is cut short rather than streamed to its end.
	if err := c.execInPod(ctx, namespace, helper, argv, nil, out); err != nil && !out.full() {
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
		// find said nothing at all about the folder it was pointed at.
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

// ReadFile streams one file off the disk into w. Nothing is buffered, so a
// large file costs the server no memory.
func (c *Client) ReadFile(ctx context.Context, namespace, objectName, space, app, subPath string, w io.Writer) error {
	rel, err := cleanRelPath(subPath)
	if err != nil {
		return err
	}
	if rel == "" {
		return fmt.Errorf("%w — name the file you want to download", rt.ErrBadPath)
	}
	helper, err := c.filesHelper(ctx, namespace, objectName, space, app)
	if err != nil {
		return err
	}
	// Resolving first settles the one question that must never be answered
	// wrong — is this still on the app's disk. A missing file or a folder is
	// caught by the read itself, which writes nothing before it fails, so the
	// caller still gets a proper answer instead of half a download.
	file, err := c.resolveInside(ctx, namespace, helper, volumePath(rel))
	if err != nil {
		return err
	}
	return c.execInPod(ctx, namespace, helper, []string{"cat", file}, nil, w)
}

// WriteFile stores r at subPath, replacing whatever was there. The upload
// lands in a temporary file next to the target and is moved into place only
// once it arrived complete — a broken connection never destroys the old file.
func (c *Client) WriteFile(ctx context.Context, namespace, objectName, space, app, subPath string, r io.Reader) error {
	rel, err := cleanRelPath(subPath)
	if err != nil {
		return err
	}
	if rel == "" {
		return fmt.Errorf("%w — name the file you want to write", rt.ErrBadPath)
	}
	helper, err := c.filesHelper(ctx, namespace, objectName, space, app)
	if err != nil {
		return err
	}
	// Resolve the whole destination, not just its folder: a name that is
	// already a symlink pointing off the disk would otherwise make `mv` write
	// through it. A name that doesn't exist yet resolves to itself.
	paths, err := c.resolveAll(ctx, namespace, helper,
		volumePath(parentRel(rel)), volumePath(rel))
	if err != nil {
		return err
	}
	dir, target := paths[0], paths[1]
	tmp := dir + "/.donkey-upload-" + strings.ToLower(rand.Text()[:12])

	limited := newLimitReader(r, rt.MaxUploadBytes)
	// dd reads until stdin ends, so the file is written straight from the
	// request body; 64K blocks keep the round trips down.
	err = c.execInPod(ctx, namespace, helper, []string{"dd", "of=" + tmp, "bs=65536"}, limited, nil)
	if err != nil || limited.exceeded() {
		c.removeQuietly(ctx, namespace, helper, tmp)
		if limited.exceeded() {
			return fmt.Errorf("%w — the limit is %d MB", rt.ErrTooLarge, rt.MaxUploadBytes>>20)
		}
		return err
	}
	// The app runs as some other user and only shares the disk's group, so a
	// file donkey wrote must be group-writable or the app can never replace it.
	if err := c.execInPod(ctx, namespace, helper, []string{"chmod", "664", tmp}, nil, nil); err != nil {
		c.removeQuietly(ctx, namespace, helper, tmp)
		return err
	}
	if err := c.execInPod(ctx, namespace, helper, []string{"mv", tmp, target}, nil, nil); err != nil {
		c.removeQuietly(ctx, namespace, helper, tmp)
		return err
	}
	return nil
}

// RemoveFile deletes a file or a whole folder. Deleting something that isn't
// there is not an error — the point is that it is gone.
func (c *Client) RemoveFile(ctx context.Context, namespace, objectName, space, app, subPath string) error {
	rel, err := cleanRelPath(subPath)
	if err != nil {
		return err
	}
	if rel == "" {
		return fmt.Errorf("%w — the disk itself cannot be deleted here (use `donkey destroy`)", rt.ErrBadPath)
	}
	helper, err := c.filesHelper(ctx, namespace, objectName, space, app)
	if err != nil {
		return err
	}
	// Only the parent is resolved: rm removes a shortcut itself and never
	// follows it, so a link pointing out of the volume cannot delete anything
	// outside it.
	dir, err := c.resolveInside(ctx, namespace, helper, volumePath(parentRel(rel)))
	if err != nil {
		return err
	}
	return c.execInPod(ctx, namespace, helper, []string{"rm", "-rf", dir + "/" + path.Base(rel)}, nil, nil)
}

// MoveFile renames or moves something on the disk. A destination that is an
// existing folder means "move it in there", as everywhere else.
func (c *Client) MoveFile(ctx context.Context, namespace, objectName, space, app, from, to string) error {
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
	helper, err := c.filesHelper(ctx, namespace, objectName, space, app)
	if err != nil {
		return err
	}
	// Both ends are resolved in full: moving *onto* a symlink that leads off
	// the disk would otherwise put the file outside the volume, where nothing
	// can reach it again.
	paths, err := c.resolveAll(ctx, namespace, helper, volumePath(fromRel), volumePath(toRel))
	if err != nil {
		return err
	}
	return c.execInPod(ctx, namespace, helper, []string{"mv", paths[0], paths[1]}, nil, nil)
}

// MakeDir creates one folder. Its parent must already exist, so a typo makes
// a folder in the wrong place impossible rather than merely unlikely.
func (c *Client) MakeDir(ctx context.Context, namespace, objectName, space, app, subPath string) error {
	rel, err := cleanRelPath(subPath)
	if err != nil {
		return err
	}
	if rel == "" {
		return fmt.Errorf("%w — name the folder you want to create", rt.ErrBadPath)
	}
	helper, err := c.filesHelper(ctx, namespace, objectName, space, app)
	if err != nil {
		return err
	}
	dir, err := c.resolveInside(ctx, namespace, helper, volumePath(parentRel(rel)))
	if err != nil {
		return err
	}
	// 775: the app shares the disk's group, so it can write in folders donkey
	// created. -p makes creating an existing folder a no-op.
	return c.execInPod(ctx, namespace, helper,
		[]string{"mkdir", "-m", "775", "-p", dir + "/" + path.Base(rel)}, nil, nil)
}

// StorageFolders reports which folder of the disk belongs to which path
// inside the app — the volume root is a list of subPath folders, and only the
// Deployment knows that "app-data" is the app's /app/data.
func (c *Client) StorageFolders(ctx context.Context, namespace, objectName, space, app string) ([]rt.StorageFolder, error) {
	dep, err := c.Clientset.AppsV1().Deployments(namespace).Get(ctx, objectName, metav1.GetOptions{})
	if apierrors.IsNotFound(err) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	if err := c.requireOwnedObject("Deployment", namespace, objectName, dep.Labels,
		manifests.Labels(space, app, manifests.RoleWorkload)); err != nil {
		return nil, err
	}
	volume := ""
	for _, v := range dep.Spec.Template.Spec.Volumes {
		if v.PersistentVolumeClaim != nil && v.PersistentVolumeClaim.ClaimName == manifests.DataClaimName(objectName) {
			volume = v.Name
			break
		}
	}
	if volume == "" {
		return nil, nil
	}
	var out []rt.StorageFolder
	seen := map[string]bool{}
	for _, ct := range dep.Spec.Template.Spec.Containers {
		for _, m := range ct.VolumeMounts {
			if m.Name != volume || m.SubPath == "" || seen[m.SubPath] {
				continue
			}
			seen[m.SubPath] = true
			out = append(out, rt.StorageFolder{Name: m.SubPath, Path: m.MountPath})
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out, nil
}

// RemoveFilesHelper stops the app's helper pod if one is running. Destroying
// an app must call this: a pod still holding the volume keeps the disk from
// being released.
func (c *Client) RemoveFilesHelper(ctx context.Context, namespace, objectName, space, app string) error {
	// No grace period of its own: the pod's own short one applies, and the
	// machine gets to release the disk properly instead of having the pod
	// yanked out from under it.
	pods := c.Clientset.CoreV1().Pods(namespace)
	name := filesHelperName(objectName)
	pod, err := pods.Get(ctx, name, metav1.GetOptions{})
	if apierrors.IsNotFound(err) {
		return nil
	}
	if err != nil {
		return err
	}
	if err := c.requireOwnedObject("Pod", namespace, name, pod.Labels,
		manifests.Labels(space, app, manifests.RoleFiles)); err != nil {
		return err
	}
	deleteOptions := metav1.DeleteOptions{}
	if uid := pod.UID; uid != "" {
		deleteOptions.Preconditions = &metav1.Preconditions{UID: &uid}
	}
	return pods.Delete(ctx, name, deleteOptions)
}

// filesHelperName is deterministic, so one pod serves every request for the
// app and a leftover is always recognizable.
func filesHelperName(app string) string { return app + "-files" }

// cleanRelPath turns a caller-supplied path into a path relative to the disk.
// Everything here is attacker-controlled, so it is checked before any command
// is built: no absolute paths, no ".." escapes, no NUL bytes. The result is
// always something that stays under the mount.
func cleanRelPath(p string) (string, error) {
	// Deliberately no TrimSpace: a leading or trailing space is part of a
	// filename, and trimming it would make an operation land on a *different*
	// file — "report.csv " deleted as "report.csv". Only an all-blank path is
	// meaningless, and path.Clean below turns that into the disk root.
	if strings.TrimSpace(p) == "" {
		return "", nil
	}
	if strings.ContainsRune(p, 0) {
		return "", fmt.Errorf("%w — it contains a character that isn't allowed in a name", rt.ErrBadPath)
	}
	if strings.HasPrefix(p, "/") {
		return "", fmt.Errorf("%w — paths are relative to the disk, so leave the leading slash off (app-data/uploads, not /app-data/uploads)", rt.ErrBadPath)
	}
	if p == "" || p == "." {
		return "", nil
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

// volumePath is where a disk-relative path lives inside the helper pod. It
// always starts with the mount point, so no argument donkey passes can ever
// look like a command option.
func volumePath(rel string) string {
	if rel == "" {
		return filesMount
	}
	return filesMount + "/" + rel
}

// parentRel is the folder holding rel ("" for the disk root).
func parentRel(rel string) string {
	dir := path.Dir(rel)
	if dir == "." || dir == "/" {
		return ""
	}
	return dir
}

// insideVolume is the last word on where an operation may touch: after the
// pod resolved every symlink, the answer has to still be on the disk.
func insideVolume(p string) bool {
	return p == filesMount || strings.HasPrefix(p, filesMount+"/")
}

// parseStatLine reads one line of statFormat output. It returns the entry and
// the full path stat was called with, so the caller can recognize the folder
// itself among its children.
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

// modeText renders a raw Unix mode the way `ls -l` does.
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

// resolveInside resolves one path in the pod and refuses anything that left
// the disk on the way — a shortcut on the volume must never become a way to
// read the machine's own files.
func (c *Client) resolveInside(ctx context.Context, namespace string, helper filesHelperRef, p string) (string, error) {
	out, err := c.resolveAll(ctx, namespace, helper, p)
	if err != nil {
		return "", err
	}
	return out[0], nil
}

// resolveAll resolves several paths in one round trip. A path that doesn't
// exist yet resolves to itself with every existing part of it followed, which
// is exactly what writing a new file needs: the folders leading to it are
// checked, the new name is not.
func (c *Client) resolveAll(ctx context.Context, namespace string, helper filesHelperRef, paths ...string) ([]string, error) {
	var out bytes.Buffer
	if err := c.execInPod(ctx, namespace, helper, append([]string{"realpath"}, paths...), nil, &out); err != nil {
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

// removeQuietly drops a leftover temporary file; the caller is already
// reporting a more interesting error. The usual reason to be here is a caller
// who walked away mid-upload, so the cleanup gets a context of its own —
// otherwise the half-written file would stay on the disk forever.
func (c *Client) removeQuietly(ctx context.Context, namespace string, helper filesHelperRef, target string) {
	ctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 30*time.Second)
	defer cancel()
	_ = c.execInPod(ctx, namespace, helper, []string{"rm", "-f", target}, nil, nil)
}

// filesHelper returns the exact running helper Pod for the app, starting one
// if needed. Its UID follows the operation so a deleted-and-recreated Pod
// cannot be substituted between path validation and a later command.
func (c *Client) filesHelper(ctx context.Context, namespace, objectName, space, app string) (filesHelperRef, error) {
	empty := filesHelperRef{}
	pvcs := c.Clientset.CoreV1().PersistentVolumeClaims(namespace)
	pvc, err := pvcs.Get(ctx, manifests.DataClaimName(objectName), metav1.GetOptions{})
	if apierrors.IsNotFound(err) {
		// Tell the two "there is nothing to browse" cases apart: no app at all
		// reads very differently from an app that simply has no disk.
		dep, derr := c.Clientset.AppsV1().Deployments(namespace).Get(ctx, objectName, metav1.GetOptions{})
		if apierrors.IsNotFound(derr) {
			return empty, rt.ErrNotDeployed
		}
		if derr != nil {
			return empty, derr
		}
		if err := c.requireOwnedObject("Deployment", namespace, objectName, dep.Labels,
			manifests.Labels(space, app, manifests.RoleWorkload)); err != nil {
			return empty, err
		}
		return empty, rt.ErrNoDisk
	}
	if err != nil {
		return empty, err
	}
	if err := c.requireOwnedObject("PersistentVolumeClaim", namespace, pvc.Name, pvc.Labels,
		manifests.Labels(space, app, manifests.RoleStorage)); err != nil {
		return empty, err
	}

	name := filesHelperName(objectName)
	pods := c.Clientset.CoreV1().Pods(namespace)
	existing, err := pods.Get(ctx, name, metav1.GetOptions{})
	expectedHelper := manifests.Labels(space, app, manifests.RoleFiles)
	switch {
	case err != nil && !apierrors.IsNotFound(err):
		return empty, err
	case err == nil && c.requireOwnedObject("Pod", namespace, name, existing.Labels, expectedHelper) != nil:
		return empty, c.requireOwnedObject("Pod", namespace, name, existing.Labels, expectedHelper)
	case err == nil && helperUsable(existing):
		return filesHelperRef{name: name, uid: existing.UID, space: space, app: app}, nil
	case err == nil:
		// Finished, expired or running the wrong image — clear it out first.
		if err := c.RemoveFilesHelper(ctx, namespace, objectName, space, app); err != nil {
			return empty, err
		}
		if err := c.waitPodGone(ctx, namespace, name); err != nil {
			return empty, err
		}
	}

	pod := filesHelperPod(namespace, space, app, objectName, c.installation())
	c.pinToAppNode(ctx, namespace, objectName, space, app, pod)
	created, err := pods.Create(ctx, pod, metav1.CreateOptions{})
	if err != nil {
		if !apierrors.IsAlreadyExists(err) {
			return empty, fmt.Errorf("opening the app's disk: %w", err)
		}
		raced, inspectErr := pods.Get(ctx, name, metav1.GetOptions{})
		if inspectErr != nil {
			return empty, inspectErr
		}
		if err := c.requireOwnedObject("Pod", namespace, name, raced.Labels, expectedHelper); err != nil {
			return empty, err
		}
		created = raced
	}
	helper := filesHelperRef{name: name, uid: created.UID, space: space, app: app}
	return helper, c.waitPodReady(ctx, namespace, helper)
}

// helperUsable reports whether an existing helper can serve another request.
func helperUsable(pod *corev1.Pod) bool {
	if pod.DeletionTimestamp != nil || pod.Status.Phase != corev1.PodRunning {
		return false
	}
	if len(pod.Spec.Containers) == 0 || pod.Spec.Containers[0].Image != FilesHelperImage {
		return false
	}
	// The pod ends its own life after a while. Reusing one with less than a
	// whole operation's worth of life left would cut that operation off
	// partway, so anything nearer than the grace window is replaced.
	if exp, err := time.Parse(time.RFC3339, pod.Annotations[annotationFilesExpires]); err == nil {
		if time.Now().After(exp.Add(-filesHelperGrace)) {
			return false
		}
	}
	for _, cs := range pod.Status.ContainerStatuses {
		if cs.Name == filesContainer {
			return cs.Ready
		}
	}
	return false
}

// pinToAppNode puts the helper on the same machine as the app. A disk of the
// usual kind attaches to one machine at a time, so anywhere else it would
// simply never start.
func (c *Client) pinToAppNode(ctx context.Context, namespace, objectName, space, app string, pod *corev1.Pod) {
	dep, err := c.Clientset.AppsV1().Deployments(namespace).Get(ctx, objectName, metav1.GetOptions{})
	if err != nil {
		return
	}
	if c.requireOwnedObject("Deployment", namespace, objectName, dep.Labels,
		manifests.Labels(space, app, manifests.RoleWorkload)) != nil {
		return
	}
	list, err := c.Clientset.CoreV1().Pods(namespace).List(ctx, metav1.ListOptions{
		LabelSelector: labels.Set(dep.Spec.Selector.MatchLabels).String(),
	})
	if err != nil {
		return
	}
	var chosen *corev1.Pod
	for i := range list.Items {
		p := &list.Items[i]
		if c.requireOwnedObject("Pod", namespace, p.Name, p.Labels,
			manifests.Labels(space, app, manifests.RoleWorkload)) != nil {
			continue
		}
		if p.DeletionTimestamp != nil || p.Spec.NodeName == "" {
			continue
		}
		if chosen == nil || p.Status.Phase == corev1.PodRunning {
			chosen = p
		}
	}
	if chosen == nil {
		return // no app pod anywhere — let the scheduler decide
	}
	pod.Spec.NodeName = chosen.Spec.NodeName
	// Naming the machine skips the scheduler, so whatever let the app run
	// there (a tainted node pool) has to be carried over by hand.
	pod.Spec.Tolerations = chosen.Spec.Tolerations
}

func filesHelperPod(namespace, space, app, objectName, installation string) *corev1.Pod {
	nonRoot := true
	user := int64(2000)
	fsGroup := int64(2000)
	automount := false
	noEscalation := false
	readOnlyRoot := true
	grace := int64(2)
	deadline := int64(filesHelperLifetime.Seconds()) + 60

	podLabels := manifests.Labels(space, app, manifests.RoleFiles)
	podLabels[manifests.LabelInstallation] = installation
	return &corev1.Pod{
		TypeMeta: metav1.TypeMeta{APIVersion: "v1", Kind: "Pod"},
		ObjectMeta: metav1.ObjectMeta{
			Name:      filesHelperName(objectName),
			Namespace: namespace,
			Labels:    podLabels,
			Annotations: map[string]string{
				annotationFilesExpires: time.Now().Add(filesHelperLifetime).UTC().Format(time.RFC3339),
			},
		},
		Spec: corev1.PodSpec{
			// The helper exits on its own; nothing restarts it and nothing
			// re-runs it, so a forgotten pod cannot hold the disk for long.
			RestartPolicy:                 corev1.RestartPolicyNever,
			ActiveDeadlineSeconds:         &deadline,
			TerminationGracePeriodSeconds: &grace,
			AutomountServiceAccountToken:  &automount,
			SecurityContext: &corev1.PodSecurityContext{
				RunAsNonRoot: &nonRoot,
				RunAsUser:    &user,
				RunAsGroup:   &fsGroup,
				// fsGroup is the group the app's own pod runs with, and the
				// default "change every file" policy is deliberate here: the
				// app writes its files as its own user, and without that pass
				// donkey could list them but never delete or replace them.
				FSGroup:        &fsGroup,
				SeccompProfile: &corev1.SeccompProfile{Type: corev1.SeccompProfileTypeRuntimeDefault},
			},
			Containers: []corev1.Container{{
				Name:    filesContainer,
				Image:   FilesHelperImage,
				Command: []string{"sleep", strconv.Itoa(int(filesHelperLifetime.Seconds()))},
				VolumeMounts: []corev1.VolumeMount{{
					Name: "data", MountPath: filesMount,
				}},
				Resources: corev1.ResourceRequirements{
					Requests: corev1.ResourceList{
						corev1.ResourceCPU:    resource.MustParse("10m"),
						corev1.ResourceMemory: resource.MustParse("16Mi"),
					},
					Limits: corev1.ResourceList{
						corev1.ResourceMemory: resource.MustParse("64Mi"),
					},
				},
				SecurityContext: &corev1.SecurityContext{
					AllowPrivilegeEscalation: &noEscalation,
					ReadOnlyRootFilesystem:   &readOnlyRoot,
					Capabilities:             &corev1.Capabilities{Drop: []corev1.Capability{"ALL"}},
				},
			}},
			Volumes: []corev1.Volume{{
				Name: "data",
				VolumeSource: corev1.VolumeSource{
					PersistentVolumeClaim: &corev1.PersistentVolumeClaimVolumeSource{
						ClaimName: manifests.DataClaimName(objectName),
					},
				},
			}},
		},
	}
}

func (c *Client) waitPodGone(ctx context.Context, namespace, name string) error {
	deadline := time.Now().Add(filesReadyTimeout)
	for {
		_, err := c.Clientset.CoreV1().Pods(namespace).Get(ctx, name, metav1.GetOptions{})
		if apierrors.IsNotFound(err) {
			return nil
		}
		if err != nil {
			return err
		}
		if time.Now().After(deadline) {
			return fmt.Errorf("the previous session on this app's disk is taking too long to close — try again in a minute")
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(time.Second):
		}
	}
}

func (c *Client) waitPodReady(ctx context.Context, namespace string, helper filesHelperRef) error {
	deadline := time.Now().Add(filesReadyTimeout)
	for {
		pod, err := c.requireFilesHelper(ctx, namespace, helper)
		if err != nil {
			return err
		}
		if helperUsable(pod) {
			return nil
		}
		for _, cs := range pod.Status.ContainerStatuses {
			if w := cs.State.Waiting; w != nil && (w.Reason == "ImagePullBackOff" || w.Reason == "ErrImagePull") {
				return fmt.Errorf("the cluster couldn't download %s, the small image donkey uses to open a disk", FilesHelperImage)
			}
		}
		if pod.Status.Phase == corev1.PodFailed || pod.Status.Phase == corev1.PodSucceeded {
			return fmt.Errorf("donkey couldn't keep a session open on the app's disk (the helper stopped right away)")
		}
		if time.Now().After(deadline) {
			return fmt.Errorf("opening the app's disk is taking too long — it may still be attached to another machine, or it holds so many files that preparing it is slow")
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(time.Second):
		}
	}
}

// requireFilesHelper rechecks both ownership labels and Kubernetes object
// identity immediately before every exec. The Pod name is deliberately not
// enough: names become reusable as soon as an old helper is deleted.
func (c *Client) requireFilesHelper(ctx context.Context, namespace string, helper filesHelperRef) (*corev1.Pod, error) {
	pod, err := c.Clientset.CoreV1().Pods(namespace).Get(ctx, helper.name, metav1.GetOptions{})
	if err != nil {
		return nil, err
	}
	if err := c.requireOwnedObject("Pod", namespace, helper.name, pod.Labels,
		manifests.Labels(helper.space, helper.app, manifests.RoleFiles)); err != nil {
		return nil, err
	}
	if pod.UID != helper.uid {
		return nil, fmt.Errorf("the file session for %s/%s was replaced while the operation was running — try again", helper.space, helper.app)
	}
	return pod, nil
}

// execInPod runs one command in the helper pod. The command is an argv slice
// and never a shell line, so no caller-supplied name can turn into another
// command.
func (c *Client) execInPod(ctx context.Context, namespace string, helper filesHelperRef, argv []string, stdin io.Reader, stdout io.Writer) error {
	if _, err := c.requireFilesHelper(ctx, namespace, helper); err != nil {
		return err
	}
	req := c.Clientset.CoreV1().RESTClient().Post().
		Resource("pods").Name(helper.name).Namespace(namespace).SubResource("exec").
		VersionedParams(&corev1.PodExecOptions{
			Container: filesContainer,
			Command:   argv,
			Stdin:     stdin != nil,
			Stdout:    stdout != nil,
			Stderr:    true,
		}, scheme.ParameterCodec)

	ex, err := c.executor(req.URL())
	if err != nil {
		return err
	}
	var stderr bytes.Buffer
	err = ex.StreamWithContext(ctx, remotecommand.StreamOptions{
		Stdin:  stdin,
		Stdout: stdout,
		Stderr: &stderr,
	})
	if err != nil {
		return execError(err, stderr.String())
	}
	return nil
}

// executor speaks WebSocket to newer clusters and falls back to the older
// SPDY upgrade, which is all a pre-1.30 API server understands.
func (c *Client) executor(u *url.URL) (remotecommand.Executor, error) {
	ws, err := remotecommand.NewWebSocketExecutor(c.Config, "GET", u.String())
	if err != nil {
		return nil, err
	}
	spdy, err := remotecommand.NewSPDYExecutor(c.Config, "POST", u)
	if err != nil {
		return nil, err
	}
	return remotecommand.NewFallbackExecutor(ws, spdy, func(err error) bool {
		return httpstream.IsUpgradeFailure(err) || httpstream.IsHTTPSProxyError(err)
	})
}

// execError turns a failed command into something a person can act on. The
// tool's own message is the only clue there is, so the few that matter are
// translated and the rest are passed on.
func execError(err error, stderr string) error {
	msg := firstLine(stderr)
	var code utilexec.CodeExitError
	if !errors.As(err, &code) {
		if msg != "" {
			return fmt.Errorf("donkey lost the connection to the app's disk: %s", msg)
		}
		return fmt.Errorf("donkey lost the connection to the app's disk: %w", err)
	}
	switch {
	case strings.Contains(msg, "No such file or directory"):
		return rt.ErrNotFound
	case strings.Contains(msg, "Is a directory"):
		return rt.ErrNotAFile
	case strings.Contains(msg, "Not a directory"):
		return rt.ErrNotAFolder
	case strings.Contains(msg, "Permission denied"):
		return fmt.Errorf("the app's own files there don't let donkey change them")
	case strings.Contains(msg, "No space left"):
		return fmt.Errorf("the disk is full — delete something before writing more")
	case msg != "":
		return fmt.Errorf("the app's disk refused that: %s", msg)
	}
	return fmt.Errorf("the app's disk refused that (the command ended with code %d)", code.ExitStatus())
}

func firstLine(s string) string {
	s = strings.TrimSpace(s)
	if i := strings.IndexByte(s, '\n'); i >= 0 {
		s = s[:i]
	}
	// Busybox prefixes its messages with the applet name ("cat: ..."), which
	// says nothing to the person reading it, and it names files by their place
	// inside the helper pod, which nobody outside donkey has ever heard of.
	if i := strings.Index(s, ": "); i >= 0 && i < 12 && !strings.Contains(s[:i], " ") {
		s = s[i+2:]
	}
	s = strings.ReplaceAll(s, filesMount+"/", "")
	s = strings.ReplaceAll(s, filesMount, "the disk")
	return strings.TrimSpace(s)
}

// errCapReached ends a command whose output nobody is going to read anyway.
var errCapReached = errors.New("output limit reached")

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

// full reports that the writer stopped the command instead of the command
// stopping itself — the output is a beginning, not the whole answer.
func (w *capWriter) full() bool { return w.buf.Len() >= w.limit }

// limitReader stops an upload that outgrows the cap instead of letting it fill
// the app's disk. It is given one byte more than the cap: reading that byte is
// what proves the cap was passed, and the upload then simply ends rather than
// tearing the connection down mid-command.
func newLimitReader(r io.Reader, cap int64) *limitReader {
	return &limitReader{r: r, left: cap + 1}
}

type limitReader struct {
	r    io.Reader
	left int64
}

func (r *limitReader) Read(p []byte) (int, error) {
	if r.left <= 0 {
		return 0, io.EOF
	}
	if int64(len(p)) > r.left {
		p = p[:r.left]
	}
	n, err := r.r.Read(p)
	r.left -= int64(n)
	return n, err
}

// exceeded reports that there was more to upload than the cap allows.
func (r *limitReader) exceeded() bool { return r.left <= 0 }
