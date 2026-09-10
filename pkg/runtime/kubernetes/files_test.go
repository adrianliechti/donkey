package kubernetes

import (
	"context"
	"errors"
	"testing"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/kubernetes/fake"

	rt "github.com/adrianliechti/donkey/pkg/runtime"
	"github.com/adrianliechti/donkey/pkg/runtime/kubernetes/manifests"
)

// Every path a file browser sends is attacker-controlled, so this is the test
// that matters most: nothing may resolve to a place outside the app's disk.
func TestCleanRelPath(t *testing.T) {
	cases := []struct {
		name    string
		in      string
		want    string
		wantErr bool
	}{
		{"empty is the disk root", "", "", false},
		{"dot is the disk root", ".", "", false},
		{"plain name", "notes.txt", "notes.txt", false},
		{"nested", "app-data/uploads/a.png", "app-data/uploads/a.png", false},
		{"tidied up", "app-data//./uploads/", "app-data/uploads", false},
		{"inner traversal that stays inside", "app-data/x/../y", "app-data/y", false},
		{"trailing slash", "app-data/", "app-data", false},
		{"spaces in names are fine", "app-data/my file.txt", "app-data/my file.txt", false},
		// A trailing or leading space is part of the name. Trimming it would
		// make delete/move/upload act on the neighbouring file instead.
		{"a trailing space is kept", "app-data/report.csv ", "app-data/report.csv ", false},
		{"a leading space is kept", "app-data/ report.csv", "app-data/ report.csv", false},
		{"blank means the disk root", "   ", "", false},
		{"absolute", "/etc/passwd", "", true},
		{"absolute to the mount", "/data/app-data", "", true},
		{"traversal", "../../etc/passwd", "", true},
		{"traversal after a folder", "app-data/../../etc/passwd", "", true},
		{"bare dotdot", "..", "", true},
		{"nul byte", "app-data/a\x00b", "", true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := cleanRelPath(tc.in)
			if tc.wantErr {
				if err == nil {
					t.Fatalf("cleanRelPath(%q) = %q, want an error", tc.in, got)
				}
				if !errors.Is(err, rt.ErrBadPath) {
					t.Fatalf("cleanRelPath(%q) error %v, want ErrBadPath", tc.in, err)
				}
				return
			}
			if err != nil {
				t.Fatalf("cleanRelPath(%q): %v", tc.in, err)
			}
			if got != tc.want {
				t.Fatalf("cleanRelPath(%q) = %q, want %q", tc.in, got, tc.want)
			}
			// Whatever comes out has to land under the mount, always.
			if !insideVolume(volumePath(got)) {
				t.Fatalf("%q leaves the volume as %q", tc.in, volumePath(got))
			}
		})
	}
}

func TestVolumePathAndParent(t *testing.T) {
	if got := volumePath(""); got != "/data" {
		t.Fatalf("volumePath(\"\") = %q", got)
	}
	if got := volumePath("app-data/x"); got != "/data/app-data/x" {
		t.Fatalf("volumePath = %q", got)
	}
	if got := parentRel("a.txt"); got != "" {
		t.Fatalf("parentRel(\"a.txt\") = %q, want the disk root", got)
	}
	if got := parentRel("app-data/uploads/a.txt"); got != "app-data/uploads" {
		t.Fatalf("parentRel = %q", got)
	}
}

func TestInsideVolume(t *testing.T) {
	inside := []string{"/data", "/data/app-data", "/data/a/b/c"}
	outside := []string{"/", "/etc/passwd", "/database", "/data-other/x", "data/x", ""}
	for _, p := range inside {
		if !insideVolume(p) {
			t.Fatalf("%q should be inside the volume", p)
		}
	}
	for _, p := range outside {
		if insideVolume(p) {
			t.Fatalf("%q should be outside the volume", p)
		}
	}
}

func TestParseStatLine(t *testing.T) {
	cases := []struct {
		name    string
		line    string
		wantOK  bool
		entry   rt.FileEntry
		wantDir bool
	}{
		{
			name:   "regular file",
			line:   "81b4|1234|1700000000|/data/app-data/notes.txt",
			wantOK: true,
			entry:  rt.FileEntry{Name: "notes.txt", Size: 1234, Mode: "-rw-rw-r--"},
		},
		{
			name:    "folder",
			line:    "41ed|4096|1700000000|/data/app-data",
			wantOK:  true,
			entry:   rt.FileEntry{Name: "app-data", Size: 4096, Mode: "drwxr-xr-x"},
			wantDir: true,
		},
		{"garbage", "not a stat line", false, rt.FileEntry{}, false},
		{"empty", "", false, rt.FileEntry{}, false},
		{"short", "81b4|1234", false, rt.FileEntry{}, false},
		{"bad mode", "zz|1|1|/data/x", false, rt.FileEntry{}, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, _, ok := parseStatLine(tc.line)
			if ok != tc.wantOK {
				t.Fatalf("parseStatLine(%q) ok=%v, want %v", tc.line, ok, tc.wantOK)
			}
			if !ok {
				return
			}
			if got.Name != tc.entry.Name || got.Size != tc.entry.Size || got.Mode != tc.entry.Mode {
				t.Fatalf("parseStatLine(%q) = %+v, want %+v", tc.line, got, tc.entry)
			}
			if got.IsDir != tc.wantDir {
				t.Fatalf("IsDir = %v, want %v", got.IsDir, tc.wantDir)
			}
			if got.ModTime.Unix() != 1700000000 {
				t.Fatalf("ModTime = %v", got.ModTime)
			}
		})
	}
}

// A name with the separator in it must still parse: only the name may contain
// one, so splitting from the left is safe.
func TestParseStatLineNameWithSeparator(t *testing.T) {
	got, full, ok := parseStatLine("81b4|7|1700000000|/data/app-data/a|b.txt")
	if !ok {
		t.Fatal("a name containing the separator should still parse")
	}
	if got.Name != "a|b.txt" {
		t.Fatalf("Name = %q", got.Name)
	}
	if full != "/data/app-data/a|b.txt" {
		t.Fatalf("full = %q", full)
	}
}

func TestModeText(t *testing.T) {
	cases := map[uint32]string{
		0x81b4: "-rw-rw-r--",
		0x41ed: "drwxr-xr-x",
		0xa1ff: "Lrwxrwxrwx",
		0x8180: "-rw-------",
	}
	for raw, want := range cases {
		if got := modeText(raw); got != want {
			t.Fatalf("modeText(%#x) = %q, want %q", raw, got, want)
		}
	}
}

func TestFirstLine(t *testing.T) {
	cases := map[string]string{
		"cat: /data/app-data/x: No such file or directory\n": "app-data/x: No such file or directory",
		"realpath: /data/nope: No such file or directory":    "nope: No such file or directory",
		"": "",
		"mv: can't rename '/data/a': Permission denied\nsecond line": "can't rename 'a': Permission denied",
	}
	for in, want := range cases {
		if got := firstLine(in); got != want {
			t.Fatalf("firstLine(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestCapWriter(t *testing.T) {
	w := &capWriter{limit: 8}
	// Everything up to the limit is kept, and the writer then asks for the
	// command to be stopped rather than reading a folder that never ends.
	n, err := w.Write([]byte("0123456789"))
	if n != 10 || !errors.Is(err, errCapReached) {
		t.Fatalf("Write = %d, %v", n, err)
	}
	if got := w.buf.String(); got != "01234567" {
		t.Fatalf("buffered %q", got)
	}
	if !w.full() {
		t.Fatal("full() should report that the output was cut short")
	}
	if _, err := w.Write([]byte("more")); !errors.Is(err, errCapReached) {
		t.Fatalf("second Write err = %v", err)
	}
}

func TestFilesHelperName(t *testing.T) {
	if got := filesHelperName("shop"); got != "shop-files" {
		t.Fatalf("filesHelperName = %q", got)
	}
}

func TestFileOperationsRequireRequestedAppIdentity(t *testing.T) {
	const namespace = "workloads"
	labels := manifests.Labels("other-space", "other-app", manifests.RoleStorage)
	labels[manifests.LabelInstallation] = "donkey-system"
	pvc := &corev1.PersistentVolumeClaim{ObjectMeta: metav1.ObjectMeta{
		Name: manifests.DataClaimName("team-site"), Namespace: namespace, Labels: labels,
	}}
	client := (&Client{Clientset: fake.NewSimpleClientset(pvc)}).WithInstallation("donkey-system")

	if _, err := client.ListFiles(context.Background(), namespace, "team-site", "team", "site", ""); err == nil {
		t.Fatal("a same-named disk carrying another app's identity was accepted")
	}
	if _, err := client.Clientset.CoreV1().Pods(namespace).Get(
		context.Background(), filesHelperName("team-site"), metav1.GetOptions{},
	); err == nil {
		t.Fatal("a helper was created for a disk outside the requested app identity")
	}
}

func TestRemoveFilesHelperRequiresRequestedAppIdentity(t *testing.T) {
	const namespace = "workloads"
	labels := manifests.Labels("other-space", "other-app", manifests.RoleFiles)
	labels[manifests.LabelInstallation] = "donkey-system"
	pod := &corev1.Pod{ObjectMeta: metav1.ObjectMeta{
		Name: filesHelperName("team-site"), Namespace: namespace, Labels: labels,
	}}
	client := (&Client{Clientset: fake.NewSimpleClientset(pod)}).WithInstallation("donkey-system")

	if err := client.RemoveFilesHelper(context.Background(), namespace, "team-site", "team", "site"); err == nil {
		t.Fatal("a same-named helper carrying another app's identity was accepted")
	}
	if _, err := client.Clientset.CoreV1().Pods(namespace).Get(
		context.Background(), pod.Name, metav1.GetOptions{},
	); err != nil {
		t.Fatalf("foreign helper was deleted: %v", err)
	}
}

func TestFilesHelperCommandsRequireLabelsAndOriginalUID(t *testing.T) {
	const namespace = "workloads"
	labels := manifests.Labels("team", "site", manifests.RoleFiles)
	labels[manifests.LabelInstallation] = "donkey-system"
	pod := &corev1.Pod{ObjectMeta: metav1.ObjectMeta{
		Name:      filesHelperName("team-site"),
		Namespace: namespace,
		Labels:    labels,
		UID:       types.UID("current-helper"),
	}}
	client := (&Client{Clientset: fake.NewSimpleClientset(pod)}).WithInstallation("donkey-system")

	replaced := filesHelperRef{name: pod.Name, uid: types.UID("previous-helper"), space: "team", app: "site"}
	if _, err := client.requireFilesHelper(context.Background(), namespace, replaced); err == nil {
		t.Fatal("a replacement Pod with the same name was accepted")
	}

	wrongApp := filesHelperRef{name: pod.Name, uid: pod.UID, space: "team", app: "other"}
	if _, err := client.requireFilesHelper(context.Background(), namespace, wrongApp); err == nil {
		t.Fatal("a helper carrying another app's identity was accepted")
	}

	exact := filesHelperRef{name: pod.Name, uid: pod.UID, space: "team", app: "site"}
	if _, err := client.requireFilesHelper(context.Background(), namespace, exact); err != nil {
		t.Fatalf("exact helper identity rejected: %v", err)
	}
}
