package apiclient

import (
	"bytes"
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestUploadFileSendsRawContentAndPreservesPath(t *testing.T) {
	for _, path := range []string{"original.pdf", "app-data/renamed.pdf", "app-data/é #?&+%25.pdf "} {
		t.Run(path, func(t *testing.T) {
			original := []byte("%PDF-1.6\n\x00\xff\x80\r\n%%EOF\n")
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.Method != http.MethodPut || r.URL.Path != "/v1/spaces/team/apps/api/files" {
					t.Errorf("unexpected upload route: %s %s", r.Method, r.URL)
				}
				if r.URL.Query().Get("path") != path {
					t.Errorf("path=%q, want %q", r.URL.Query().Get("path"), path)
				}
				if got := r.Header.Get("Content-Type"); got != "application/octet-stream" {
					t.Errorf("content type=%q", got)
				}
				stored, err := io.ReadAll(r.Body)
				if err != nil || !bytes.Equal(stored, original) {
					t.Errorf("upload content differs: %v", err)
				}
				w.WriteHeader(http.StatusOK)
			}))
			defer server.Close()
			client, err := New(server.URL, "token", "")
			if err != nil {
				t.Fatal(err)
			}
			if err := client.UploadFile(context.Background(), "team", "api", path, bytes.NewReader(original)); err != nil {
				t.Fatal(err)
			}
		})
	}
}
