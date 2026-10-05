package docker

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"net/http"
	"strings"
	"testing"

	"github.com/distribution/reference"
	"github.com/docker/docker/api/types/container"
	"github.com/docker/docker/api/types/volume"
	"github.com/docker/docker/client"

	"github.com/adrianliechti/donkey/internal/config"
)

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
