//go:build e2e

package server

import (
	"bufio"
	"bytes"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	k8sclient "k8s.io/client-go/kubernetes"
	"k8s.io/client-go/tools/clientcmd"

	"github.com/adrianliechti/donkey/internal/config"
	"github.com/adrianliechti/donkey/internal/identity"
	runtime "github.com/adrianliechti/donkey/pkg/runtime"
	"github.com/adrianliechti/donkey/pkg/runtime/kubernetes"
	"github.com/adrianliechti/donkey/pkg/runtime/kubernetes/manifests"
)

type kubernetesFilesRuntime struct {
	planRuntime
	client    *kubernetes.Client
	namespace string
	failAfter atomic.Int64
}

func (r *kubernetesFilesRuntime) WriteFile(ctx context.Context, space, app, path string, in io.Reader) error {
	if after := r.failAfter.Load(); after > 0 {
		in = io.MultiReader(io.LimitReader(in, after), interruptedPDFReader{})
	}
	return r.client.WriteFile(ctx, r.namespace, app, space, app, path, in)
}

func (r *kubernetesFilesRuntime) ReadFile(ctx context.Context, space, app, path string, out io.Writer) error {
	return r.client.ReadFile(ctx, r.namespace, app, space, app, path, out)
}

func (r *kubernetesFilesRuntime) ListFiles(ctx context.Context, space, app, path string) ([]runtime.FileEntry, error) {
	return r.client.ListFiles(ctx, r.namespace, app, space, app, path)
}

type interruptedPDFReader struct{}

func (interruptedPDFReader) Read([]byte) (int, error) {
	return 0, errors.New("injected upload read failure")
}

// Produce a valid PDF with an uncompressed RGB image, including binary bytes.
// Each requested size is approximate because the PDF objects add framing.
func uploadTestPDF(kib int) []byte {
	const width = 256
	height := kib * 1024 / (width * 3)
	image := bytes.Repeat([]byte{0, 255, 128}, width*height)
	var out bytes.Buffer
	out.WriteString("%PDF-1.4\n%\xe2\xe3\xcf\xd3\n")
	offsets := make([]int, 6)
	object := func(n int, body string) {
		offsets[n] = out.Len()
		fmt.Fprintf(&out, "%d 0 obj\n%s\nendobj\n", n, body)
	}
	object(1, "<< /Type /Catalog /Pages 2 0 R >>")
	object(2, "<< /Type /Pages /Kids [3 0 R] /Count 1 >>")
	object(3, "<< /Type /Page /Parent 2 0 R /MediaBox [0 0 256 256] /Resources << /XObject << /Im0 5 0 R >> >> /Contents 4 0 R >>")
	content := "q 256 0 0 256 0 0 cm /Im0 Do Q\n"
	object(4, fmt.Sprintf("<< /Length %d >>\nstream\n%sendstream", len(content), content))
	offsets[5] = out.Len()
	fmt.Fprintf(&out, "5 0 obj\n<< /Type /XObject /Subtype /Image /Width %d /Height %d /ColorSpace /DeviceRGB /BitsPerComponent 8 /Length %d >>\nstream\n", width, height, len(image))
	out.Write(image)
	out.WriteString("\nendstream\nendobj\n")
	xref := out.Len()
	out.WriteString("xref\n0 6\n0000000000 65535 f \n")
	for _, offset := range offsets[1:] {
		fmt.Fprintf(&out, "%010d 00000 n \n", offset)
	}
	fmt.Fprintf(&out, "trailer\n<< /Size 6 /Root 1 0 R >>\nstartxref\n%d\n%%%%EOF\n", xref)
	return out.Bytes()
}

// Runs the actual CLI -> HTTP server -> client-go -> helper pod -> PVC path.
// A fault is injected only in the server's upload reader; exec and disk I/O
// use the real cluster. The preservation assertion fails on the original code.
// DONKEY_E2E_FILES_KUBECONFIG=/path/to/kubeconfig go test -tags e2e ./internal/server -run TestPDFUploadsKubernetes -v
func TestPDFUploadsKubernetes(t *testing.T) {
	kubeconfig := os.Getenv("DONKEY_E2E_FILES_KUBECONFIG")
	if kubeconfig == "" {
		t.Skip("set DONKEY_E2E_FILES_KUBECONFIG to test PDFs through the CLI and a real Kubernetes helper")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()
	cfg, err := clientcmd.BuildConfigFromFlags("", kubeconfig)
	if err != nil {
		t.Fatal(err)
	}
	cs, err := k8sclient.NewForConfig(cfg)
	if err != nil {
		t.Fatal(err)
	}
	client := &kubernetes.Client{Clientset: cs, Config: cfg}
	namespace := "files-repro-" + strings.ToLower(rand.Text()[:8])
	if _, err := cs.CoreV1().Namespaces().Create(ctx, &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: namespace}}, metav1.CreateOptions{}); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		cleanup, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		if err := cs.CoreV1().Namespaces().Delete(cleanup, namespace, metav1.DeleteOptions{}); err != nil {
			t.Error(err)
		}
	})
	const space, app = "team", "pdf-upload"
	labels := manifests.Labels(space, app, manifests.RoleStorage)
	labels[manifests.LabelInstallation] = manifests.DefaultSystemNamespace
	pvc := &corev1.PersistentVolumeClaim{
		ObjectMeta: metav1.ObjectMeta{Name: manifests.DataClaimName(app), Namespace: namespace, Labels: labels},
		Spec: corev1.PersistentVolumeClaimSpec{
			AccessModes: []corev1.PersistentVolumeAccessMode{corev1.ReadWriteOnce},
			Resources:   corev1.VolumeResourceRequirements{Requests: corev1.ResourceList{corev1.ResourceStorage: resource.MustParse("1Gi")}},
		},
	}
	// An isolated single-node cluster can use a pre-created hostPath instead
	// of downloading a storage provisioner just for this test.
	if hostPath := os.Getenv("DONKEY_E2E_FILES_HOSTPATH"); hostPath != "" {
		emptyClass := ""
		pvc.Spec.StorageClassName = &emptyClass
		pvc.Spec.VolumeName = namespace
		pv := &corev1.PersistentVolume{
			ObjectMeta: metav1.ObjectMeta{Name: namespace},
			Spec: corev1.PersistentVolumeSpec{
				Capacity:               corev1.ResourceList{corev1.ResourceStorage: resource.MustParse("1Gi")},
				AccessModes:            []corev1.PersistentVolumeAccessMode{corev1.ReadWriteOnce},
				PersistentVolumeSource: corev1.PersistentVolumeSource{HostPath: &corev1.HostPathVolumeSource{Path: hostPath}},
				ClaimRef:               &corev1.ObjectReference{Namespace: namespace, Name: pvc.Name},
			},
		}
		if _, err := cs.CoreV1().PersistentVolumes().Create(ctx, pv, metav1.CreateOptions{}); err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() {
			cleanup, cancel := context.WithTimeout(context.Background(), 30*time.Second)
			defer cancel()
			if err := cs.CoreV1().PersistentVolumes().Delete(cleanup, pv.Name, metav1.DeleteOptions{}); err != nil {
				t.Error(err)
			}
		})
	}
	if _, err := cs.CoreV1().PersistentVolumeClaims(namespace).Create(ctx, pvc, metav1.CreateOptions{}); err != nil {
		t.Fatal(err)
	}
	if err := client.MakeDir(ctx, namespace, app, space, app, "app-data"); err != nil {
		t.Fatal(err)
	}
	rt := &kubernetesFilesRuntime{client: client, namespace: namespace}
	s := &Server{cfg: &Config{Domain: "example.com", SessionKey: testSessionKey}, rt: rt}
	server := httptest.NewServer(s.routes())
	defer server.Close()
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "donkey.yaml"), []byte("name: "+app+"\nport: 8080\n"), 0600); err != nil {
		t.Fatal(err)
	}
	token, err := identity.SealToken(testSessionKey, identity.Identity{Subject: "u1", Email: "u1@example.com", Expires: time.Now().Add(time.Hour).Unix()})
	if err != nil {
		t.Fatal(err)
	}
	global := &config.GlobalConfig{Server: config.ServerConfig{URL: server.URL, Token: token}}
	if err := global.Save(filepath.Join(dir, ".config", "donkey", "config.yaml")); err != nil {
		t.Fatal(err)
	}
	binary := filepath.Join(dir, "donkey")
	build := exec.CommandContext(ctx, "go", "build", "-o", binary, "./cmd/donkey")
	build.Dir = "../.."
	if out, err := build.CombinedOutput(); err != nil {
		t.Fatalf("build CLI: %v\n%s", err, out)
	}
	runCLI := func(args ...string) ([]byte, error) {
		cmd := exec.CommandContext(ctx, binary, append([]string{"--app-dir", dir, "--space", space, "files"}, args...)...)
		cmd.Env = append(os.Environ(), "HOME="+dir, "DONKEY_TOKEN="+token)
		return cmd.CombinedOutput()
	}
	upload := func(local, remote string) ([]byte, error) {
		return runCLI("put", local, remote)
	}
	readDisk := func(remote string) []byte {
		t.Helper()
		var out bytes.Buffer
		if err := client.ReadFile(ctx, namespace, app, space, app, remote, &out); err != nil {
			t.Fatal(err)
		}
		return out.Bytes()
	}
	for _, kib := range []int{170, 800, 1100, 1400} {
		t.Run(fmt.Sprintf("PDF_%d_KiB", kib), func(t *testing.T) {
			pdf := uploadTestPDF(kib)
			local := filepath.Join(dir, "document.pdf")
			if err := os.WriteFile(local, pdf, 0600); err != nil {
				t.Fatal(err)
			}
			const remote = "app-data/document.pdf"
			for attempt := 0; attempt < 3; attempt++ {
				out, err := upload(local, "app-data/")
				if err != nil {
					t.Fatalf("CLI upload: %v\n%s", err, out)
				}
				stored := readDisk(remote)
				t.Logf("attempt=%d local=%d bytes disk=%d bytes SHA256=%x", attempt+1, len(pdf), len(stored), sha256.Sum256(stored))
				if !bytes.Equal(stored, pdf) {
					t.Fatalf("PDF differs: local SHA256=%x disk SHA256=%x", sha256.Sum256(pdf), sha256.Sum256(stored))
				}
				download := filepath.Join(dir, "download.pdf")
				if out, err := runCLI("get", "-o", download, remote); err != nil {
					t.Fatalf("CLI download: %v\n%s", err, out)
				}
				got, err := os.ReadFile(download)
				if err != nil || !bytes.Equal(got, pdf) {
					t.Fatalf("CLI download differs: size=%d SHA256=%x error=%v", len(got), sha256.Sum256(got), err)
				}
			}
			if out, err := runCLI("ls", "app-data"); err != nil || !bytes.Contains(out, []byte("document.pdf")) {
				t.Fatalf("CLI listing: %v\n%s", err, out)
			}
		})
	}
	t.Run("directory_destination_does_not_claim_success", func(t *testing.T) {
		local := filepath.Join(dir, "document.pdf")
		out, err := upload(local, "app-data")
		entries, listErr := client.ListFiles(ctx, namespace, app, space, app, "app-data")
		if listErr != nil {
			t.Fatal(listErr)
		}
		var hidden []string
		for _, entry := range entries {
			if strings.HasPrefix(entry.Name, ".donkey-upload-") {
				hidden = append(hidden, entry.Name)
			}
		}
		t.Logf("upload to bare directory: CLI error=%v output=%q hidden uploads=%q", err, out, hidden)
		if err == nil {
			t.Error("CLI reported success after uploading under a hidden temporary filename")
		}
		if len(hidden) > 0 {
			t.Errorf("upload left hidden temporary files: %q", hidden)
		}
	})
	t.Run("read_failure_keeps_original", func(t *testing.T) {
		original := uploadTestPDF(800)
		const remote = "app-data/protected.pdf"
		if err := client.WriteFile(ctx, namespace, app, space, app, remote, bytes.NewReader(original)); err != nil {
			t.Fatal(err)
		}
		local := filepath.Join(dir, "replacement.pdf")
		if err := os.WriteFile(local, uploadTestPDF(1400), 0600); err != nil {
			t.Fatal(err)
		}
		rt.failAfter.Store(128 * 1024)
		defer rt.failAfter.Store(0)
		out, err := upload(local, remote)
		stored := readDisk(remote)
		t.Logf("injected read failure at 131072 bytes; CLI error=%v output=%q; original=%d bytes disk=%d bytes; original SHA256=%x disk SHA256=%x", err, out, len(original), len(stored), sha256.Sum256(original), sha256.Sum256(stored))
		if !bytes.Equal(stored, original) {
			t.Errorf("interrupted upload replaced the original with %d bytes", len(stored))
		}
		if err == nil {
			t.Error("CLI reported success for an interrupted upload")
		}
	})
	t.Run("HTTP_disconnect_keeps_original", func(t *testing.T) {
		original := uploadTestPDF(800)
		const remote = "app-data/disconnected.pdf"
		if err := client.WriteFile(ctx, namespace, app, space, app, remote, bytes.NewReader(original)); err != nil {
			t.Fatal(err)
		}
		conn, err := net.DialTimeout("tcp", server.Listener.Addr().String(), 5*time.Second)
		if err != nil {
			t.Fatal(err)
		}
		defer conn.Close()
		if err := conn.SetDeadline(time.Now().Add(30 * time.Second)); err != nil {
			t.Fatal(err)
		}
		pdf := uploadTestPDF(1400)
		fmt.Fprintf(conn, "PUT /v1/spaces/%s/apps/%s/files?path=%s HTTP/1.1\r\nHost: localhost\r\nAuthorization: Bearer %s\r\nContent-Type: application/octet-stream\r\nContent-Length: %d\r\nConnection: close\r\n\r\n", space, app, remote, token, len(pdf))
		if _, err := conn.Write(pdf[:128*1024]); err != nil {
			t.Fatal(err)
		}
		if err := conn.(*net.TCPConn).CloseWrite(); err != nil {
			t.Fatal(err)
		}
		response, responseErr := http.ReadResponse(bufio.NewReader(conn), nil)
		status := 0
		if response != nil {
			status = response.StatusCode
			response.Body.Close()
		}
		stored := readDisk(remote)
		t.Logf("real TCP half-close at 131072/%d bytes; HTTP status=%d response error=%v; original=%d bytes disk=%d bytes", len(pdf), status, responseErr, len(original), len(stored))
		if responseErr == nil && status < 400 {
			t.Error("HTTP disconnect was reported as a successful upload")
		}
		if !bytes.Equal(stored, original) {
			t.Errorf("HTTP disconnect replaced the original with %d bytes", len(stored))
		}
	})
}
