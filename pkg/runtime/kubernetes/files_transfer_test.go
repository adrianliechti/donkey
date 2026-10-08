package kubernetes

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/gorilla/websocket"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	k8sclient "k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"

	"github.com/adrianliechti/donkey/pkg/runtime/kubernetes/manifests"
)

type uploadFailureReader struct{}

func (uploadFailureReader) Read([]byte) (int, error) { return 0, errors.New("upload connection lost") }

// Exercise the real client-go WebSocket executor. It logs stdin errors while
// returning nil when dd exits zero, which used to commit a truncated upload.
func TestWriteFileVerifiesUploadBeforeReplacingDestination(t *testing.T) {
	data := bytes.Repeat([]byte{0, 255, 1, '\n', '\r'}, 40000)
	for _, scenario := range []string{"complete", "empty", "source error", "disk truncated", "same size corruption"} {
		t.Run(scenario, func(t *testing.T) {
			const namespace, target = "workloads", "/data/app-data/file.bin"
			var mu sync.Mutex
			disk := map[string][]byte{target: []byte("original")}
			client := &Client{}
			pod := client.filesHelperPod(namespace, "team", "site", "team-site", client.installation())
			pod.UID = "helper-uid"
			pod.Status.Phase = corev1.PodRunning
			pod.Status.ContainerStatuses = []corev1.ContainerStatus{{Name: filesContainer, Ready: true}}
			labels := manifests.Labels("team", "site", manifests.RoleStorage)
			labels[manifests.LabelInstallation] = client.installation()
			pvc := &corev1.PersistentVolumeClaim{ObjectMeta: metav1.ObjectMeta{Name: manifests.DataClaimName("team-site"), Namespace: namespace, Labels: labels}}
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if strings.HasSuffix(r.URL.Path, "/exec") {
					upgrader := websocket.Upgrader{Subprotocols: []string{"v5.channel.k8s.io"}}
					conn, err := upgrader.Upgrade(w, r, nil)
					if err != nil {
						t.Error(err)
						return
					}
					defer conn.Close()
					argv := r.URL.Query()["command"]
					var output string
					if argv[0] == "sh" {
						var incoming bytes.Buffer
						for {
							_, message, err := conn.ReadMessage()
							if err != nil {
								t.Error(err)
								return
							}
							if len(message) > 0 && message[0] == 0 {
								incoming.Write(message[1:])
							}
							if len(message) == 2 && message[0] == 255 && message[1] == 0 {
								break
							}
						}
						stored := bytes.Clone(incoming.Bytes())
						if scenario == "disk truncated" {
							stored = stored[:len(stored)/2]
						}
						if scenario == "same size corruption" {
							stored[0] ^= 255
						}
						mu.Lock()
						disk[argv[4]] = stored
						mu.Unlock()
						output = fmt.Sprintf("%x  %s\n", sha256.Sum256(stored), argv[4])
					} else {
						mu.Lock()
						switch argv[0] {
						case "realpath":
							output = strings.Join(argv[1:], "\n") + "\n"
						case "chmod":
						case "mv":
							if len(argv) != 4 || argv[1] != "-fT" {
								t.Errorf("unexpected mv: %v", argv)
							} else {
								disk[argv[3]] = disk[argv[2]]
								delete(disk, argv[2])
							}
						case "rm":
							delete(disk, argv[len(argv)-1])
						default:
							t.Errorf("unexpected exec: %v", argv)
						}
						mu.Unlock()
					}
					if output != "" {
						conn.WriteMessage(websocket.BinaryMessage, append([]byte{1}, []byte(output)...))
					}
					conn.WriteMessage(websocket.BinaryMessage, append([]byte{3}, []byte(`{"status":"Success"}`)...))
					conn.WriteMessage(websocket.CloseMessage, websocket.FormatCloseMessage(websocket.CloseNormalClosure, ""))
					return
				}
				w.Header().Set("Content-Type", "application/json")
				switch {
				case strings.HasSuffix(r.URL.Path, "/persistentvolumeclaims/"+pvc.Name):
					json.NewEncoder(w).Encode(pvc)
				case strings.HasSuffix(r.URL.Path, "/pods/"+pod.Name):
					json.NewEncoder(w).Encode(pod)
				default:
					t.Errorf("unexpected request: %s %s", r.Method, r.URL)
					http.NotFound(w, r)
				}
			}))
			defer server.Close()
			client.Config = &rest.Config{Host: server.URL}
			var err error
			client.Clientset, err = k8sclient.NewForConfig(client.Config)
			if err != nil {
				t.Fatal(err)
			}
			want := data
			if scenario == "empty" {
				want = nil
			}
			var source io.Reader = bytes.NewReader(want)
			if scenario == "source error" {
				source = io.MultiReader(source, uploadFailureReader{})
			}
			ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
			defer cancel()
			err = client.WriteFile(ctx, namespace, "team-site", "team", "site", "app-data/file.bin", source)
			wantErr := scenario != "complete" && scenario != "empty"
			if (err != nil) != wantErr {
				t.Fatalf("WriteFile error = %v, want error %v", err, wantErr)
			}
			mu.Lock()
			defer mu.Unlock()
			if wantErr {
				want = []byte("original")
			}
			if !bytes.Equal(disk[target], want) {
				t.Fatalf("destination changed incorrectly: got %d bytes, want %d", len(disk[target]), len(want))
			}
			if len(disk) != 1 {
				t.Fatalf("temporary upload wasn't cleaned up: %d files remain", len(disk))
			}
		})
	}
}
