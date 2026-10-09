package docker

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"net/http"
	"strings"
	"testing"

	"github.com/distribution/reference"
	"github.com/docker/docker/api/types/container"
	"github.com/docker/docker/api/types/volume"
	"github.com/docker/docker/client"

	"github.com/adrianliechti/donkey/internal/config"
	rt "github.com/adrianliechti/donkey/pkg/runtime"
)

func TestCleanRelPath(t *testing.T) {
	for _, tc := range []struct {
		in, want string
		bad      bool
	}{
		{"", "", false},
		{".", "", false},
		{"   ", "", false},
		{"original.pdf", "original.pdf", false},
		{"app-data//./uploads/", "app-data/uploads", false},
		{"app-data/x/../report.pdf", "app-data/report.pdf", false},
		{"app-data/é #?&+%25.pdf ", "app-data/é #?&+%25.pdf ", false},
		{"app-data/ report.pdf", "app-data/ report.pdf", false},
		{"/etc/passwd", "", true},
		{"/data/app-data/file.pdf", "", true},
		{"..", "", true},
		{"../../etc/passwd", "", true},
		{"app-data/../../etc/passwd", "", true},
		{"app-data/a\x00b", "", true},
	} {
		t.Run(tc.in, func(t *testing.T) {
			got, err := cleanRelPath(tc.in)
			if tc.bad {
				if !errors.Is(err, rt.ErrBadPath) {
					t.Fatalf("path %q: got %q, %v, want ErrBadPath", tc.in, got, err)
				}
				return
			}
			if err != nil || got != tc.want || !insideVolume(volumePath(got)) {
				t.Fatalf("path %q: got %q, %v, want %q inside the disk", tc.in, got, err, tc.want)
			}
		})
	}
}

func TestWriteFileRejectsInvalidPathBeforeContactingDocker(t *testing.T) {
	for _, path := range []string{"", ".", "app-data/..", "/data/file.pdf", "../file.pdf", "app-data/../../file.pdf", "a\x00b"} {
		t.Run(path, func(t *testing.T) {
			// No Docker client: any attempt to acquire a helper would panic.
			err := (&Runtime{}).WriteFile(context.Background(), "team", "api", path, strings.NewReader("data"))
			if !errors.Is(err, rt.ErrBadPath) {
				t.Fatalf("WriteFile(%q): %v, want ErrBadPath", path, err)
			}
		})
	}
}

func TestInsideVolume(t *testing.T) {
	for _, path := range []string{"/data", "/data/app-data", "/data/app-data/report.pdf"} {
		if !insideVolume(path) {
			t.Errorf("%q should be inside the disk", path)
		}
	}
	for _, path := range []string{"", "/", "/etc/passwd", "/database/file.pdf", "/data-other/file.pdf", "data/file.pdf"} {
		if insideVolume(path) {
			t.Errorf("%q should be outside the disk", path)
		}
	}
}

func TestBusyboxImageUsedForFilesAndVolumeInitialization(t *testing.T) {
	t.Setenv("DONKEY_REGISTRY_PASSWORD", "registry-secret")
	for _, image := range []string{"", "jfrog.corp/dockerhub/library/busybox:1.37"} {
		for _, operation := range []string{"files", "init"} {
			t.Run(image+"/"+operation, func(t *testing.T) {
				wantImage := image
				if wantImage == "" {
					wantImage = config.DefaultBusyboxImage
				}
				var created container.Config
				pulled := false
				transport := dockerRoundTripFunc(func(request *http.Request) (*http.Response, error) {
					path := strings.TrimPrefix(request.URL.Path, "/v1.48")
					switch {
					case request.Method == http.MethodGet && strings.HasPrefix(path, "/volumes/"):
						return dockerJSONResponse(http.StatusOK, volume.Volume{
							Name: volName("team", "site"), Labels: appLabels("team", "site", roleStorage),
						}), nil
					case request.Method == http.MethodGet && strings.HasSuffix(path, "/json"):
						if created.Image == "" {
							return dockerJSONResponse(http.StatusNotFound, map[string]string{"message": "not found"}), nil
						}
						return dockerJSONResponse(http.StatusOK, map[string]any{
							"Id": "helper-id", "Config": created, "State": map[string]any{"Running": true},
						}), nil
					case request.Method == http.MethodPost && path == "/images/create":
						named, _ := reference.ParseNormalizedNamed(wantImage)
						if request.URL.Query().Get("fromImage") != named.Name() || request.URL.Query().Get("tag") != "1.37" {
							t.Fatalf("pulled wrong image: %s", request.URL)
						}
						if image != "" {
							data, err := base64.URLEncoding.DecodeString(request.Header.Get("X-Registry-Auth"))
							if err != nil {
								t.Fatal(err)
							}
							var auth map[string]string
							if err := json.Unmarshal(data, &auth); err != nil || auth["serveraddress"] != "jfrog.corp" || auth["password"] != "registry-secret" {
								t.Fatalf("private helper image has wrong pull credentials: %v, %v", auth, err)
							}
						}
						pulled = true
						return dockerJSONResponse(http.StatusOK, map[string]string{"status": "done"}), nil
					case request.Method == http.MethodPost && path == "/containers/create":
						if err := json.NewDecoder(request.Body).Decode(&created); err != nil {
							t.Fatal(err)
						}
						return dockerJSONResponse(http.StatusCreated, map[string]string{"Id": "helper-id"}), nil
					case request.Method == http.MethodPost && strings.HasSuffix(path, "/start"):
						return dockerJSONResponse(http.StatusNoContent, nil), nil
					case request.Method == http.MethodPost && strings.HasSuffix(path, "/wait"):
						return dockerJSONResponse(http.StatusOK, map[string]int{"StatusCode": 0}), nil
					case request.Method == http.MethodDelete:
						return dockerJSONResponse(http.StatusNoContent, nil), nil
					default:
						t.Fatalf("unexpected Docker request: %s %s", request.Method, request.URL)
						return nil, nil
					}
				})
				cli, err := client.NewClientWithOpts(
					client.WithHost("http://docker.test"), client.WithVersion("1.48"),
					client.WithHTTPClient(&http.Client{Transport: transport}),
				)
				if err != nil {
					t.Fatal(err)
				}
				r := &Runtime{cli: cli, opts: Options{
					BusyboxImage: image, Registry: config.Registry{URL: "jfrog.corp/donkey", Username: "reader"},
				}}
				if operation == "files" {
					_, err = r.filesHelper(context.Background(), "team", "site")
				} else {
					err = r.initVolumeDirs(context.Background(), volName("team", "site"), []string{"app-data"}, appLabels("team", "site", roleInit))
				}
				if err != nil {
					t.Fatal(err)
				}
				if !pulled || created.Image != wantImage {
					t.Fatalf("pulled=%v, created image=%q, want %q", pulled, created.Image, wantImage)
				}
			})
		}
	}
}
