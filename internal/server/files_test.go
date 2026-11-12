package server

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"

	"github.com/adrianliechti/donkey/pkg/runtime"
)

type fileUploadRuntime struct {
	planRuntime
	path   string
	data   []byte
	writes int
	err    error
}

func (r *fileUploadRuntime) WriteFile(_ context.Context, space, app, path string, in io.Reader) error {
	if space != "team" || app != "api" {
		return fmt.Errorf("unexpected upload destination: %s/%s", space, app)
	}
	r.writes++
	if r.err != nil {
		return r.err
	}
	data, err := io.ReadAll(in)
	if err != nil {
		return err
	}
	r.path, r.data = path, data
	return nil
}

func callFileUpload(t *testing.T, rt *fileUploadRuntime, method, path, contentType string, body []byte) *httptest.ResponseRecorder {
	t.Helper()
	s := &Server{cfg: &Config{Domain: "example.com", SessionKey: testSessionKey}, rt: rt}
	req := httptest.NewRequest(method, "https://donkey.example.com/v1/spaces/team/apps/api/files?"+url.Values{"path": {path}}.Encode(), bytes.NewReader(body))
	asAdmin(t, req)
	req.Header.Set("Content-Type", contentType)
	rec := httptest.NewRecorder()
	s.routes().ServeHTTP(rec, req)
	return rec
}

func assertFileUpload(t *testing.T, rec *httptest.ResponseRecorder, rt *fileUploadRuntime, path string, original []byte) {
	t.Helper()
	if rec.Code != http.StatusOK {
		t.Fatalf("upload returned %d: %s", rec.Code, rec.Body.String())
	}
	if rt.writes != 1 || rt.path != path {
		t.Fatalf("writes=%d, path=%q, want one write at %q", rt.writes, rt.path, path)
	}
	if !bytes.Equal(rt.data, original) {
		t.Fatalf("stored %d bytes (%x), want %d bytes (%x)", len(rt.data), sha256.Sum256(rt.data), len(original), sha256.Sum256(original))
	}
	var result struct {
		OK   bool   `json:"ok"`
		Path string `json:"path"`
		Size int    `json:"size"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &result); err != nil {
		t.Fatal(err)
	}
	if !result.OK || result.Path != path || result.Size != len(original) {
		t.Fatalf("wrong upload response: %+v", result)
	}
}

// Reproduce the reported browser framing, including its exact 192-byte overhead.
func TestWriteFileBrowserPDFs(t *testing.T) {
	const boundary = "----WebKitFormBoundaryCAaDNNCIABeUmnV1"
	for _, size := range []int{0, 170 << 10, 800 << 10, 1100 << 10, 1400 << 10, 2 << 20} {
		t.Run(fmt.Sprint(size), func(t *testing.T) {
			original := append([]byte("%PDF-1.6\n"), bytes.Repeat([]byte{0, 255, 128, '\r', '\n'}, size/5)...)
			original = append(original, []byte("\n%%EOF\n")...)
			body := []byte("--" + boundary + "\r\nContent-Disposition: form-data; name=\"file\"; filename=\"736333792.pdf\"\r\nContent-Type: application/pdf\r\n\r\n")
			body = append(body, original...)
			body = append(body, []byte("\r\n--"+boundary+"--\r\n")...)
			if len(body)-len(original) != 192 {
				t.Fatalf("fixture framing has %d bytes, want 192", len(body)-len(original))
			}
			rt := &fileUploadRuntime{}
			rec := callFileUpload(t, rt, http.MethodPost, "app-data/736333792.pdf", "multipart/form-data; boundary="+boundary, body)
			assertFileUpload(t, rec, rt, "app-data/736333792.pdf", original)
		})
	}
}

func TestWriteFileMultipartPaths(t *testing.T) {
	for _, tc := range []struct{ name, path, filename, want string }{
		{"disk root", "", "original.pdf", "original.pdf"},
		{"folder", "app-data/", "original.pdf", "app-data/original.pdf"},
		{"explicit rename", "app-data/renamed.pdf", "original.pdf", "app-data/renamed.pdf"},
		{"bare folder stays explicit", "app-data", "original.pdf", "app-data"},
		{"special characters", "app-data/é #?&+%25.pdf ", "original.pdf", "app-data/é #?&+%25.pdf "},
		{"filename cannot choose a parent", "app-data/", "../../original.pdf", "app-data/original.pdf"},
	} {
		for _, method := range []string{http.MethodPost, http.MethodPut} {
			t.Run(tc.name+"/"+method, func(t *testing.T) {
				original := []byte("%PDF-1.6\n\x00\xff\x80\n%%EOF\n")
				var body bytes.Buffer
				form := multipart.NewWriter(&body)
				if err := form.WriteField("description", "metadata must not reach the disk"); err != nil {
					t.Fatal(err)
				}
				part, err := form.CreateFormFile("file", tc.filename)
				if err != nil {
					t.Fatal(err)
				}
				if _, err := part.Write(original); err != nil {
					t.Fatal(err)
				}
				if err := form.Close(); err != nil {
					t.Fatal(err)
				}
				rt := &fileUploadRuntime{}
				rec := callFileUpload(t, rt, method, tc.path, form.FormDataContentType(), body.Bytes())
				assertFileUpload(t, rec, rt, tc.want, original)
			})
		}
	}
}

func TestWriteFileRawBody(t *testing.T) {
	for _, method := range []string{http.MethodPut, http.MethodPost} {
		for _, original := range [][]byte{nil, []byte("%PDF-1.6\n\x00\xff\x80\n%%EOF\n"), []byte("{\"example\":true}")} {
			t.Run(fmt.Sprintf("%s/%d", method, len(original)), func(t *testing.T) {
				rt := &fileUploadRuntime{}
				const path = "app-data/é #?&+%25.pdf "
				rec := callFileUpload(t, rt, method, path, "application/octet-stream", original)
				assertFileUpload(t, rec, rt, path, original)
			})
		}
	}
}

func TestWriteFileRejectsMalformedMultipart(t *testing.T) {
	const boundary = "upload-test"
	for _, tc := range []struct{ name, contentType, body string }{
		{"missing boundary", "multipart/form-data", "unparseable"},
		{"no file", "multipart/form-data; boundary=" + boundary, "--" + boundary + "\r\nContent-Disposition: form-data; name=\"description\"\r\n\r\nmetadata\r\n--" + boundary + "--\r\n"},
		{"truncated headers", "multipart/form-data; boundary=" + boundary, "--" + boundary + "\r\nContent-Disposition:"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			rt := &fileUploadRuntime{}
			rec := callFileUpload(t, rt, http.MethodPost, "app-data/file.pdf", tc.contentType, []byte(tc.body))
			if rec.Code != http.StatusBadRequest || rt.writes != 0 {
				t.Fatalf("status=%d, writes=%d: %s", rec.Code, rt.writes, rec.Body.String())
			}
		})
	}
}

func TestWriteFileDoesNotReportSuccessOnFailure(t *testing.T) {
	for _, tc := range []struct {
		name   string
		err    error
		status int
	}{
		{"bad path", runtime.ErrBadPath, http.StatusBadRequest},
		{"directory destination", runtime.ErrNotAFile, http.StatusBadRequest},
		{"missing destination", runtime.ErrNotFound, http.StatusNotFound},
		{"disk failure", errors.New("disk unavailable"), http.StatusBadGateway},
	} {
		t.Run(tc.name, func(t *testing.T) {
			rt := &fileUploadRuntime{err: tc.err}
			rec := callFileUpload(t, rt, http.MethodPut, "app-data/file.pdf", "application/octet-stream", []byte("data"))
			if rec.Code != tc.status || strings.Contains(rec.Body.String(), `"ok":true`) {
				t.Fatalf("status=%d: %s", rec.Code, rec.Body.String())
			}
		})
	}

	// EOF before the terminating boundary must not be treated as a complete file.
	rt := &fileUploadRuntime{data: []byte("original")}
	body := []byte("--upload-test\r\nContent-Disposition: form-data; name=\"file\"; filename=\"file.pdf\"\r\n\r\n%PDF-1.6")
	rec := callFileUpload(t, rt, http.MethodPost, "app-data/file.pdf", "multipart/form-data; boundary=upload-test", body)
	if rec.Code != http.StatusBadGateway || string(rt.data) != "original" {
		t.Fatalf("truncated upload: status=%d, data=%q", rec.Code, rt.data)
	}
}
