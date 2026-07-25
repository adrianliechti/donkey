package server

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log"
	"mime/multipart"
	"net/http"
	"net/url"
	"path"
	"strings"
	"time"

	"github.com/adrianliechti/donkey/internal/k8s"
)

// The app's disk as a file browser. Every operation opens a short-lived
// helper pod on the volume (see internal/k8s/files.go) — the handlers here
// only turn HTTP into that, and turn its errors into something a person can
// act on.

const (
	// filesOpTimeout covers listing, deleting and moving: the slow part is
	// starting the helper pod, not the operation itself.
	filesOpTimeout = 3 * time.Minute
	// filesTransferTimeout is for uploads and downloads, which are as slow as
	// the file is large.
	filesTransferTimeout = 20 * time.Minute
)

type FileInfo struct {
	Name string `json:"name"`
	Size int64  `json:"size"`
	// Mode is the ls-style spelling, e.g. "-rw-rw-r--".
	Mode string `json:"mode"`
	// ModTime is RFC 3339 UTC.
	ModTime string `json:"modTime"`
	IsDir   bool   `json:"isDir"`
	// IsLink marks a shortcut — neither a file to download nor a folder to
	// open.
	IsLink bool `json:"isLink,omitempty"`
}

// StorageFolderInfo says which folder of the disk is which path in the app.
type StorageFolderInfo struct {
	Name string `json:"name"`
	Path string `json:"path"`
}

type ListFilesResponse struct {
	Path    string     `json:"path"`
	Entries []FileInfo `json:"entries"`
	// Truncated says the folder holds more than one listing shows.
	Truncated bool `json:"truncated,omitempty"`
	// Folders is only filled for the disk's root, where the folder names are
	// donkey's own (one per storage path).
	Folders []StorageFolderInfo `json:"folders,omitempty"`
}

func (s *Server) handleListFiles(w http.ResponseWriter, r *http.Request) {
	ns, app, err := s.spaceApp(r)
	if err != nil {
		writeErr(w, http.StatusBadRequest, err)
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), filesOpTimeout)
	defer cancel()

	p := r.URL.Query().Get("path")
	entries, err := s.k8s.ListFiles(ctx, ns, app, p)
	if err != nil {
		writeFilesErr(w, err)
		return
	}
	resp := ListFilesResponse{
		Path:      p,
		Entries:   make([]FileInfo, 0, len(entries)),
		Truncated: len(entries) >= k8s.MaxListEntries,
	}
	for _, e := range entries {
		resp.Entries = append(resp.Entries, FileInfo{
			Name:    e.Name,
			Size:    e.Size,
			Mode:    e.Mode,
			ModTime: e.ModTime.UTC().Format(time.RFC3339),
			IsDir:   e.IsDir,
			IsLink:  e.IsLink,
		})
	}
	// At the root, "app-data" means nothing on its own — the Deployment knows
	// it is the app's /app/data, so the listing says so.
	if p == "" {
		if folders, ferr := s.k8s.StorageFolders(ctx, ns, app); ferr == nil {
			for _, f := range folders {
				resp.Folders = append(resp.Folders, StorageFolderInfo{Name: f.Name, Path: f.Path})
			}
		}
	}
	writeJSON(w, http.StatusOK, resp)
}

// handleReadFile streams one file out. Nothing is buffered, so downloading a
// multi-gigabyte file costs the server nothing but time.
func (s *Server) handleReadFile(w http.ResponseWriter, r *http.Request) {
	ns, app, err := s.spaceApp(r)
	if err != nil {
		writeErr(w, http.StatusBadRequest, err)
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), filesTransferTimeout)
	defer cancel()

	p := r.URL.Query().Get("path")
	out := &downloadWriter{w: w, name: path.Base(p), inline: r.URL.Query().Get("inline") == "true"}
	if err := s.k8s.ReadFile(ctx, ns, app, p, out); err != nil {
		if out.started {
			// The response is already on its way, so there is no status left
			// to send: the client sees a download that stops short.
			log.Printf("files: download of %s %s stopped early: %v", app, p, err)
			return
		}
		writeFilesErr(w, err)
		return
	}
	// An empty file still arrives as a download rather than as a blank page.
	out.finish()
}

// handleWriteFile stores an upload. It takes either a multipart form (what a
// browser sends) or the raw request body, so `curl --data-binary` works too.
func (s *Server) handleWriteFile(w http.ResponseWriter, r *http.Request) {
	ns, app, err := s.spaceApp(r)
	if err != nil {
		writeErr(w, http.StatusBadRequest, err)
		return
	}
	if r.ContentLength > k8s.MaxUploadBytes {
		writeErr(w, http.StatusRequestEntityTooLarge,
			fmt.Errorf("that file is too large to upload — the limit is %d MB", k8s.MaxUploadBytes>>20))
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), filesTransferTimeout)
	defer cancel()

	p := r.URL.Query().Get("path")
	body := io.Reader(r.Body)
	if strings.HasPrefix(r.Header.Get("Content-Type"), "multipart/form-data") {
		mr, merr := r.MultipartReader()
		if merr != nil {
			writeErr(w, http.StatusBadRequest, merr)
			return
		}
		part, name, perr := firstFilePart(mr)
		if perr != nil {
			writeErr(w, http.StatusBadRequest, perr)
			return
		}
		defer part.Close()
		body = part
		// Dropping a file into a folder names it after the file that was
		// dropped; an explicit path still wins.
		if p == "" || strings.HasSuffix(p, "/") {
			p += path.Base(name)
		}
	}

	counted := &countingReader{r: body}
	if err := s.k8s.WriteFile(ctx, ns, app, p, counted); err != nil {
		writeFilesErr(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "path": p, "size": counted.n})
}

func (s *Server) handleDeleteFile(w http.ResponseWriter, r *http.Request) {
	ns, app, err := s.spaceApp(r)
	if err != nil {
		writeErr(w, http.StatusBadRequest, err)
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), filesOpTimeout)
	defer cancel()

	p := r.URL.Query().Get("path")
	if err := s.k8s.RemoveFile(ctx, ns, app, p); err != nil {
		writeFilesErr(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "path": p})
}

// handleMoveFile renames or moves something on the disk. A destination that
// is an existing folder means "move it in there".
func (s *Server) handleMoveFile(w http.ResponseWriter, r *http.Request) {
	ns, app, err := s.spaceApp(r)
	if err != nil {
		writeErr(w, http.StatusBadRequest, err)
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), filesOpTimeout)
	defer cancel()

	q := r.URL.Query()
	from, to := q.Get("from"), q.Get("to")
	if err := s.k8s.MoveFile(ctx, ns, app, from, to); err != nil {
		writeFilesErr(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "from": from, "to": to})
}

// handleMakeFolder creates one folder; its parent has to exist already.
func (s *Server) handleMakeFolder(w http.ResponseWriter, r *http.Request) {
	ns, app, err := s.spaceApp(r)
	if err != nil {
		writeErr(w, http.StatusBadRequest, err)
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), filesOpTimeout)
	defer cancel()

	p := r.URL.Query().Get("path")
	if err := s.k8s.MakeDir(ctx, ns, app, p); err != nil {
		writeFilesErr(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "path": p})
}

// writeFilesErr keeps the status codes honest: a missing app, a bad path and
// a cluster that misbehaved are three different things, and only the last one
// is donkey's fault.
func writeFilesErr(w http.ResponseWriter, err error) {
	switch {
	case errors.Is(err, k8s.ErrNoDisk), errors.Is(err, k8s.ErrNotDeployed), errors.Is(err, k8s.ErrNotFound):
		writeErr(w, http.StatusNotFound, err)
	case errors.Is(err, k8s.ErrBadPath), errors.Is(err, k8s.ErrNotAFile), errors.Is(err, k8s.ErrNotAFolder):
		writeErr(w, http.StatusBadRequest, err)
	case errors.Is(err, k8s.ErrTooLarge):
		writeErr(w, http.StatusRequestEntityTooLarge, err)
	case errors.Is(err, context.DeadlineExceeded):
		writeErr(w, http.StatusGatewayTimeout, fmt.Errorf("the app's disk didn't answer in time — try again"))
	default:
		writeErr(w, http.StatusBadGateway, err)
	}
}

// firstFilePart picks the uploaded file out of a multipart form, whatever the
// field is called.
func firstFilePart(mr *multipart.Reader) (*multipart.Part, string, error) {
	for {
		part, err := mr.NextPart()
		if err == io.EOF {
			return nil, "", fmt.Errorf("the upload contained no file")
		}
		if err != nil {
			return nil, "", err
		}
		if name := part.FileName(); name != "" {
			return part, name, nil
		}
		part.Close()
	}
}

// downloadWriter writes the response headers only once the first byte of the
// file arrives — until then an error can still become a proper status code.
type downloadWriter struct {
	w       http.ResponseWriter
	name    string
	inline  bool
	started bool
}

// finish sends the headers for a file that turned out to be empty.
func (d *downloadWriter) finish() {
	if !d.started {
		d.start()
	}
}

func (d *downloadWriter) start() {
	d.started = true
	h := d.w.Header()
	// An app's files are not donkey's: served as something a browser
	// executes, they would run on the console's own address. So they are
	// handed out as a download, or as plain text when asked to be shown.
	h.Set("X-Content-Type-Options", "nosniff")
	if d.inline {
		h.Set("Content-Type", "text/plain; charset=utf-8")
		h.Set("Content-Disposition", "inline")
	} else {
		h.Set("Content-Type", "application/octet-stream")
		h.Set("Content-Disposition", contentDisposition(d.name))
	}
	d.w.WriteHeader(http.StatusOK)
}

func (d *downloadWriter) Write(p []byte) (int, error) {
	if !d.started {
		d.start()
	}
	n, err := d.w.Write(p)
	if f, ok := d.w.(http.Flusher); ok {
		f.Flush()
	}
	return n, err
}

// contentDisposition names the download safely: quotes and control characters
// out of the plain form, the real name repeated in the encoded one.
func contentDisposition(name string) string {
	if name == "" || name == "." || name == "/" {
		name = "download"
	}
	ascii := strings.Map(func(r rune) rune {
		if r < 32 || r > 126 || r == '"' || r == '\\' {
			return '_'
		}
		return r
	}, name)
	return fmt.Sprintf("attachment; filename=%q; filename*=UTF-8''%s", ascii, url.PathEscape(name))
}

// countingReader reports how much was actually uploaded.
type countingReader struct {
	r io.Reader
	n int64
}

func (c *countingReader) Read(p []byte) (int, error) {
	n, err := c.r.Read(p)
	c.n += int64(n)
	return n, err
}
