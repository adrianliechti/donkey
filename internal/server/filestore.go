package server

// The console's file browser. donkey-server already sits in the cluster
// and already holds what a store needs — the binding, or the caller's own
// identity — so it reaches a space's store directly: no tunnel, no exposed
// port, the admin token is the only door. What a store is made of (S3
// buckets and keys, a WebDAV tree) stays inside its fileStore; the handlers
// know containers, folders and files, and the console sees one API.

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"mime"
	"net/http"
	"path"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/adrianliechti/donkey/internal/addons"
	"github.com/adrianliechti/donkey/pkg/runtime"
)

// fileStore is a store as the browser sees it: containers at the top,
// folders and files below. Keys are slash-separated paths relative to the
// container; a folder's key ends in "/". Each protocol implements it in its
// own terms and checks names by its own rules, before the store is asked
// anything it would answer with a cryptic error.
type fileStore interface {
	// Containers is the top level: S3's buckets, a WebDAV service's one root.
	Containers(ctx context.Context) ([]fileContainer, error)
	// CreateContainer makes a new one; errExists when the name is taken.
	CreateContainer(ctx context.Context, name string) error
	// DeleteContainer removes an empty container, or with force everything in
	// it first; errNotEmpty otherwise.
	DeleteContainer(ctx context.Context, name string, force bool) error
	// List is one page of a folder — its subfolders and files — or, with
	// recursive, of every file below the prefix. token continues an earlier
	// page where it stopped; the page's Next is the token for the one after.
	List(ctx context.Context, container, prefix, token string, max int, recursive bool) (fileListing, error)
	// Stat describes a file.
	Stat(ctx context.Context, container, key string) (fileInfo, error)
	// Open streams a file from its start, or with limit > 0 at most that
	// many bytes of it, with what the store says about the whole file.
	Open(ctx context.Context, container, key string, limit int64) (io.ReadCloser, fileInfo, error)
	// Put writes a file, replacing one of the same key. size is -1 when
	// unknown. It reports the bytes written.
	Put(ctx context.Context, container, key string, body io.Reader, size int64, contentType string) (int64, error)
	// Delete removes one file; one that is already gone is no error.
	Delete(ctx context.Context, container, key string) error
	// DeleteMany removes files by key and folders with everything in them,
	// and says how many entries went before any error.
	DeleteMany(ctx context.Context, container string, keys, folders []string) (int, error)
	// MakeFolder creates an empty folder; errExists when something is
	// already called that.
	MakeFolder(ctx context.Context, container, key string) error

	// The store's naming rules. creating is a stricter check for a name
	// about to be made than for one merely referred to.
	CheckContainer(name string, creating bool) error
	CheckKey(key string) error
	// CheckFolder is a folder's key: "a/b/". CheckPrefix allows "" as well.
	CheckFolder(key string) error
	CheckPrefix(prefix string) error
}

type fileContainer struct {
	Name    string
	Created time.Time // zero when the store does not say
}

type fileEntry struct {
	Key          string
	Size         int64
	Modified     time.Time
	ETag         string
	StorageClass string
}

type fileListing struct {
	Prefixes     []string
	Objects      []fileEntry
	DisplayNames map[string]string // optional labels by key, including the listed folder itself
	Truncated    bool
	Next         string
}

type fileInfo struct {
	Key          string
	Size         int64
	Modified     time.Time
	ContentType  string
	ETag         string
	StorageClass string
	VersionID    string
	Metadata     map[string]string
}

var (
	errExists   = errors.New("already exists")
	errNotEmpty = errors.New("not empty")
)

// upstreamError is the store's own answer: a 4xx is the caller's (not
// there, not allowed), anything else the backend's.
type upstreamError struct {
	status int
	err    error
}

func (e *upstreamError) Error() string { return e.err.Error() }
func (e *upstreamError) Unwrap() error { return e.err }

// fileStatus translates a failure into the console's terms. An upstream 401
// is reported as 403: to the console, 401 means its own session ended.
func fileStatus(err error) int {
	var ue runtime.UserError
	if errors.As(err, &ue) {
		return http.StatusBadRequest
	}
	if errors.Is(err, errExists) || errors.Is(err, errNotEmpty) {
		return http.StatusConflict
	}
	var up *upstreamError
	if errors.As(err, &up) && up.status >= 400 && up.status < 500 {
		if up.status == http.StatusUnauthorized {
			return http.StatusForbidden
		}
		return up.status
	}
	return http.StatusBadGateway
}

// fileStore opens the store of the service the request names, in the way
// its protocol is reached: S3 with the binding's keys, WebDAV as the caller
// — the user's IdP token, as the Trino viewer does, never donkey's own.
func (s *Server) fileStore(ctx context.Context, r *http.Request) (fileStore, error) {
	connection, err := s.serviceConnection(ctx, r, addons.ProtocolS3, addons.ProtocolWebDAV)
	if err != nil {
		return nil, err
	}
	service := r.PathValue("service")
	switch connection.Protocol {
	case addons.ProtocolS3:
		return newS3Store(connection.Binding, service)
	case addons.ProtocolWebDAV:
		token, err := s.userToken(ctx)
		if err != nil {
			return nil, err
		}
		return newWebDAVStore(connection.Binding[addons.ConnectionKey(addons.ProtocolWebDAV)], service, token)
	}
	return nil, runtime.UserError{Err: fmt.Errorf("no file browser for protocol %s", connection.Protocol)}
}

func formatTime(t time.Time) string {
	if t.IsZero() {
		return ""
	}
	return t.UTC().Format(time.RFC3339)
}

func (s *Server) handleFileContainers(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := context.WithCancel(r.Context())
	defer cancel()
	store, err := s.fileStore(ctx, r)
	if err != nil {
		writeErr(w, fileStatus(err), err)
		return
	}
	containers, err := store.Containers(ctx)
	if err != nil {
		writeErr(w, fileStatus(err), err)
		return
	}
	type container struct {
		Name      string `json:"name"`
		CreatedAt string `json:"createdAt,omitempty"`
	}
	out := []container{}
	for _, c := range containers {
		out = append(out, container{Name: c.Name, CreatedAt: formatTime(c.Created)})
	}
	writeJSON(w, http.StatusOK, map[string]any{"buckets": out})
}

func (s *Server) handleFileCreateContainer(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := context.WithCancel(r.Context())
	defer cancel()
	var req struct {
		Name string `json:"name"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeErr(w, http.StatusBadRequest, err)
		return
	}
	store, err := s.fileStore(ctx, r)
	if err != nil {
		writeErr(w, fileStatus(err), err)
		return
	}
	if err := store.CheckContainer(req.Name, true); err != nil {
		writeErr(w, http.StatusBadRequest, err)
		return
	}
	if err := store.CreateContainer(ctx, req.Name); err != nil {
		status := fileStatus(err)
		if errors.Is(err, errExists) {
			err = fmt.Errorf("a bucket named %q already exists", req.Name)
		}
		writeErr(w, status, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "name": req.Name})
}

func (s *Server) handleFileDeleteContainer(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := context.WithCancel(r.Context())
	defer cancel()
	container := r.PathValue("bucket")
	store, err := s.fileStore(ctx, r)
	if err != nil {
		writeErr(w, fileStatus(err), err)
		return
	}
	if err := store.CheckContainer(container, false); err != nil {
		writeErr(w, http.StatusBadRequest, err)
		return
	}
	if err := store.DeleteContainer(ctx, container, r.URL.Query().Get("force") == "true"); err != nil {
		status := fileStatus(err)
		if errors.Is(err, errNotEmpty) {
			err = fmt.Errorf("%s still holds objects — deleting it takes every one of them with it; confirm to go ahead", container)
		}
		writeErr(w, status, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true})
}

func (s *Server) handleFileList(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := context.WithCancel(r.Context())
	defer cancel()
	q := r.URL.Query()
	container, prefix := r.PathValue("bucket"), q.Get("prefix")
	max, _ := strconv.Atoi(q.Get("max"))
	if max < 1 || max > 1000 {
		max = 500
	}
	store, err := s.fileStore(ctx, r)
	if err != nil {
		writeErr(w, fileStatus(err), err)
		return
	}
	if err := store.CheckContainer(container, false); err != nil {
		writeErr(w, http.StatusBadRequest, err)
		return
	}
	if err := store.CheckPrefix(prefix); err != nil {
		writeErr(w, http.StatusBadRequest, err)
		return
	}
	page, err := store.List(ctx, container, prefix, q.Get("token"), max, q.Get("recursive") == "true")
	if err != nil {
		writeErr(w, fileStatus(err), err)
		return
	}
	type object struct {
		Key          string `json:"key"`
		Size         int64  `json:"size"`
		Modified     string `json:"modified"`
		ETag         string `json:"etag,omitempty"`
		StorageClass string `json:"storageClass,omitempty"`
	}
	prefixes, objects := []string{}, []object{}
	prefixes = append(prefixes, page.Prefixes...)
	for _, e := range page.Objects {
		objects = append(objects, object{Key: e.Key, Size: e.Size, Modified: formatTime(e.Modified), ETag: e.ETag, StorageClass: e.StorageClass})
	}
	out := map[string]any{"prefixes": prefixes, "objects": objects, "truncated": page.Truncated, "next": page.Next}
	if len(page.DisplayNames) > 0 {
		out["displayNames"] = page.DisplayNames
	}
	writeJSON(w, http.StatusOK, out)
}

// The preview pane reads up to this much of a file as text.
const filePreviewBytes = 256 << 10

// fileMayBeText rules out the content types that are never text, so their
// bytes are not fetched only to be found binary.
func fileMayBeText(contentType string) bool {
	media, _, _ := mime.ParseMediaType(contentType)
	top, sub, _ := strings.Cut(media, "/")
	switch top {
	case "image", "video", "audio", "font":
		return false
	case "application":
		switch sub {
		case "zip", "gzip", "x-gzip", "x-tar", "x-7z-compressed", "x-bzip2", "x-xz", "zstd", "pdf", "wasm", "vnd.apache.parquet", "x-parquet":
			return false
		}
	}
	return true
}

// fileText is the bytes as text when they are: UTF-8 without NULs. A
// preview cut mid-rune drops the partial rune rather than declaring the file
// binary.
func fileText(data []byte, cut bool) (string, bool) {
	if bytes.IndexByte(data, 0) >= 0 {
		return "", false
	}
	for drop := 0; drop < 4 && drop <= len(data); drop++ {
		if utf8.Valid(data[:len(data)-drop]) {
			return string(data[:len(data)-drop]), true
		}
		if !cut {
			break
		}
	}
	return "", false
}

// handleFileInfo is the preview pane: a file's metadata, and its first
// filePreviewBytes as text when it is text.
func (s *Server) handleFileInfo(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := context.WithCancel(r.Context())
	defer cancel()
	q := r.URL.Query()
	container, key := r.PathValue("bucket"), q.Get("key")
	if key == "" {
		writeErr(w, http.StatusBadRequest, fmt.Errorf("which object?"))
		return
	}
	store, err := s.fileStore(ctx, r)
	if err != nil {
		writeErr(w, fileStatus(err), err)
		return
	}
	if err := store.CheckContainer(container, false); err != nil {
		writeErr(w, http.StatusBadRequest, err)
		return
	}
	if err := store.CheckKey(key); err != nil {
		writeErr(w, http.StatusBadRequest, err)
		return
	}
	st, err := store.Stat(ctx, container, key)
	if err != nil {
		writeErr(w, fileStatus(err), err)
		return
	}
	metadata := map[string]string{}
	for k, v := range st.Metadata {
		metadata[k] = v
	}
	out := map[string]any{
		"key":          st.Key,
		"size":         st.Size,
		"modified":     formatTime(st.Modified),
		"contentType":  st.ContentType,
		"etag":         st.ETag,
		"storageClass": st.StorageClass,
		"versionId":    st.VersionID,
		"metadata":     metadata,
	}
	if q.Get("text") != "false" && st.Size > 0 && fileMayBeText(st.ContentType) {
		body, _, err := store.Open(ctx, container, key, filePreviewBytes)
		if err != nil {
			writeErr(w, fileStatus(err), err)
			return
		}
		defer body.Close()
		data, err := io.ReadAll(io.LimitReader(body, filePreviewBytes))
		if err != nil {
			writeErr(w, fileStatus(err), err)
			return
		}
		cut := st.Size > int64(len(data))
		if text, ok := fileText(data, cut); ok {
			out["text"] = text
			out["truncated"] = cut
		}
	}
	writeJSON(w, http.StatusOK, out)
}

// handleFileDeleteMany removes a selection: files by key, folders with
// everything in them. Emptying a whole container is the container's own
// delete.
func (s *Server) handleFileDeleteMany(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := context.WithCancel(r.Context())
	defer cancel()
	container := r.PathValue("bucket")
	var req struct {
		Keys     []string `json:"keys"`
		Prefixes []string `json:"prefixes"`
	}
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 4<<20)).Decode(&req); err != nil {
		writeErr(w, http.StatusBadRequest, err)
		return
	}
	if len(req.Keys)+len(req.Prefixes) == 0 || len(req.Keys) > 1000 || len(req.Prefixes) > 100 {
		writeErr(w, http.StatusBadRequest, fmt.Errorf("delete 1 to 1000 objects and up to 100 folders at once"))
		return
	}
	store, err := s.fileStore(ctx, r)
	if err != nil {
		writeErr(w, fileStatus(err), err)
		return
	}
	if err := store.CheckContainer(container, false); err != nil {
		writeErr(w, http.StatusBadRequest, err)
		return
	}
	for _, key := range req.Keys {
		if err := store.CheckKey(key); err != nil {
			writeErr(w, http.StatusBadRequest, err)
			return
		}
	}
	for _, prefix := range req.Prefixes {
		if err := store.CheckFolder(prefix); err != nil {
			writeErr(w, http.StatusBadRequest, fmt.Errorf("%q is not a folder", prefix))
			return
		}
	}
	deleted, err := store.DeleteMany(ctx, container, req.Keys, req.Prefixes)
	if err != nil {
		writeErr(w, fileStatus(err), fmt.Errorf("%d deleted, then: %w", deleted, err))
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "deleted": deleted})
}

func (s *Server) handleFileCreateFolder(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := context.WithCancel(r.Context())
	defer cancel()
	container := r.PathValue("bucket")
	var req struct {
		Key string `json:"key"`
	}
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 8<<10)).Decode(&req); err != nil {
		writeErr(w, http.StatusBadRequest, err)
		return
	}
	store, err := s.fileStore(ctx, r)
	if err != nil {
		writeErr(w, fileStatus(err), err)
		return
	}
	if err := store.CheckContainer(container, false); err != nil {
		writeErr(w, http.StatusBadRequest, err)
		return
	}
	if err := store.CheckFolder(req.Key); err != nil {
		writeErr(w, http.StatusBadRequest, fmt.Errorf("%q won't work as a folder", req.Key))
		return
	}
	if err := store.MakeFolder(ctx, container, req.Key); err != nil {
		status := fileStatus(err)
		if errors.Is(err, errExists) {
			err = fmt.Errorf("something named %q is already there", strings.TrimSuffix(req.Key, "/"))
		}
		writeErr(w, status, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "key": req.Key})
}

// handleFileUpload takes one file as the multipart field "file" and streams
// it to the store — the server never holds the file. The console says the
// size up front (`size`), so a store that needs a Content-Length has one.
func (s *Server) handleFileUpload(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	container := r.PathValue("bucket")
	store, err := s.fileStore(r.Context(), r)
	if err != nil {
		writeErr(w, fileStatus(err), err)
		return
	}
	if err := store.CheckContainer(container, false); err != nil {
		writeErr(w, http.StatusBadRequest, err)
		return
	}
	size := int64(-1)
	if raw := q.Get("size"); raw != "" {
		n, err := strconv.ParseInt(raw, 10, 64)
		if err != nil || n < 0 {
			writeErr(w, http.StatusBadRequest, fmt.Errorf("size must be a byte count"))
			return
		}
		size = n
	}
	mr, err := r.MultipartReader()
	if err != nil {
		writeErr(w, http.StatusBadRequest, err)
		return
	}
	for {
		part, err := mr.NextPart()
		if err == io.EOF {
			break
		}
		if err != nil {
			writeErr(w, http.StatusBadRequest, err)
			return
		}
		if part.FormName() != "file" {
			continue
		}
		key := q.Get("key")
		if key == "" {
			key = part.FileName()
		}
		key = strings.TrimPrefix(key, "/")
		if err := store.CheckKey(key); err != nil {
			writeErr(w, http.StatusBadRequest, err)
			return
		}
		written, err := store.Put(r.Context(), container, key, part, size, part.Header.Get("Content-Type"))
		if err != nil {
			writeErr(w, fileStatus(err), err)
			return
		}
		writeJSON(w, http.StatusOK, map[string]any{"ok": true, "key": key, "size": written})
		return
	}
	writeErr(w, http.StatusBadRequest, fmt.Errorf("the upload carries no file — send it as the multipart field %q", "file"))
}

func (s *Server) handleFileDownload(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	container, key := r.PathValue("bucket"), q.Get("key")
	if key == "" {
		writeErr(w, http.StatusBadRequest, fmt.Errorf("which object?"))
		return
	}
	store, err := s.fileStore(r.Context(), r)
	if err != nil {
		writeErr(w, fileStatus(err), err)
		return
	}
	if err := store.CheckContainer(container, false); err != nil {
		writeErr(w, http.StatusBadRequest, err)
		return
	}
	if err := store.CheckKey(key); err != nil {
		writeErr(w, http.StatusBadRequest, err)
		return
	}
	// A missing file surfaces here, while the headers can still say 404
	// instead of a broken download.
	body, st, err := store.Open(r.Context(), container, key, 0)
	if err != nil {
		writeErr(w, fileStatus(err), err)
		return
	}
	defer body.Close()
	contentType := st.ContentType
	if contentType == "" {
		contentType = "application/octet-stream"
	}
	w.Header().Set("Content-Type", contentType)
	if st.Size >= 0 {
		w.Header().Set("Content-Length", strconv.FormatInt(st.Size, 10))
	}
	w.Header().Set("Content-Disposition", mime.FormatMediaType("attachment", map[string]string{"filename": path.Base(key)}))
	w.WriteHeader(http.StatusOK)
	_, _ = io.Copy(w, body)
}

func (s *Server) handleFileDelete(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := context.WithCancel(r.Context())
	defer cancel()
	q := r.URL.Query()
	container, key := r.PathValue("bucket"), q.Get("key")
	if key == "" {
		writeErr(w, http.StatusBadRequest, fmt.Errorf("which object?"))
		return
	}
	store, err := s.fileStore(ctx, r)
	if err != nil {
		writeErr(w, fileStatus(err), err)
		return
	}
	if err := store.CheckContainer(container, false); err != nil {
		writeErr(w, http.StatusBadRequest, err)
		return
	}
	if err := store.CheckKey(key); err != nil {
		writeErr(w, http.StatusBadRequest, err)
		return
	}
	if err := store.Delete(ctx, container, key); err != nil {
		writeErr(w, fileStatus(err), err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true})
}
