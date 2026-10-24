package server

// The console's bucket browser. donkey-server already sits in the cluster
// and already holds the binding, so it can speak S3 to a space's object
// store directly — no tunnel, no exposed port, the admin token is the only
// door.

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"mime"
	"net/http"
	"net/url"
	"path"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/minio/minio-go/v7"
	"github.com/minio/minio-go/v7/pkg/credentials"
	"github.com/minio/minio-go/v7/pkg/s3utils"

	"github.com/adrianliechti/donkey/internal/addons"
	"github.com/adrianliechti/donkey/pkg/runtime"
)

// s3Connect builds a client for the space service the request names, from
// the binding's own values — the browser sees exactly the store the apps
// see.
func (s *Server) s3Connect(ctx context.Context, r *http.Request) (*minio.Client, error) {
	service, err := s.serviceConnection(ctx, r, addons.ProtocolS3)
	if err != nil {
		return nil, err
	}
	data := service.Binding
	u, _ := url.Parse(data["AWS_ENDPOINT_URL_S3"]) // validated by serviceConnection
	client, err := minio.New(u.Host, &minio.Options{
		Creds:  credentials.NewStaticV4(data["AWS_ACCESS_KEY_ID"], data["AWS_SECRET_ACCESS_KEY"], data["AWS_SESSION_TOKEN"]),
		Secure: u.Scheme == "https",
		Region: data["AWS_REGION"],
	})
	if err != nil {
		return nil, fmt.Errorf("%s is not reachable: %w", r.PathValue("service"), err)
	}
	return client, nil
}

// s3Status translates a failure into the console's terms: the caller's
// mistakes and the store's own 4xx answers stay theirs, everything else is
// the backend's.
func s3Status(err error) int {
	var ue runtime.UserError
	if errors.As(err, &ue) {
		return http.StatusBadRequest
	}
	if resp := minio.ToErrorResponse(err); resp.StatusCode >= 400 && resp.StatusCode < 500 {
		return resp.StatusCode
	}
	return http.StatusBadGateway
}

func (s *Server) handleS3Buckets(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := context.WithCancel(r.Context())
	defer cancel()
	client, err := s.s3Connect(ctx, r)
	if err != nil {
		writeErr(w, s3Status(err), err)
		return
	}
	buckets, err := client.ListBuckets(ctx)
	if err != nil {
		writeErr(w, s3Status(err), err)
		return
	}
	type bucket struct {
		Name      string `json:"name"`
		CreatedAt string `json:"createdAt"`
	}
	out := []bucket{}
	for _, b := range buckets {
		out = append(out, bucket{Name: b.Name, CreatedAt: b.CreationDate.UTC().Format(time.RFC3339)})
	}
	writeJSON(w, http.StatusOK, map[string]any{"buckets": out})
}

func (s *Server) handleS3CreateBucket(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := context.WithCancel(r.Context())
	defer cancel()
	var req struct {
		Name string `json:"name"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeErr(w, http.StatusBadRequest, err)
		return
	}
	// Strict S3 rules up front, so the store never sees a name it would
	// answer with a cryptic XML error.
	if err := s3utils.CheckValidBucketNameStrict(req.Name); err != nil {
		writeErr(w, http.StatusBadRequest, fmt.Errorf("%q won't work as a bucket name — lowercase letters, digits and dashes, 3 to 63 characters", req.Name))
		return
	}
	client, err := s.s3Connect(ctx, r)
	if err != nil {
		writeErr(w, s3Status(err), err)
		return
	}
	if err := client.MakeBucket(ctx, req.Name, minio.MakeBucketOptions{}); err != nil {
		if code := minio.ToErrorResponse(err).Code; code == "BucketAlreadyOwnedByYou" || code == "BucketAlreadyExists" {
			writeErr(w, http.StatusConflict, fmt.Errorf("a bucket named %q already exists", req.Name))
			return
		}
		writeErr(w, s3Status(err), err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "name": req.Name})
}

func (s *Server) handleS3DeleteBucket(w http.ResponseWriter, r *http.Request) {
	// Sweeping a full bucket is many requests, not one — it gets time the
	// metadata calls don't need.
	ctx, cancel := context.WithCancel(r.Context())
	defer cancel()
	bucket := r.PathValue("bucket")
	if err := s3utils.CheckValidBucketName(bucket); err != nil {
		writeErr(w, http.StatusBadRequest, fmt.Errorf("%q is not a valid bucket name", bucket))
		return
	}
	client, err := s.s3Connect(ctx, r)
	if err != nil {
		writeErr(w, s3Status(err), err)
		return
	}
	if r.URL.Query().Get("force") == "true" {
		if err := s3EmptyBucket(ctx, client, bucket); err != nil {
			writeErr(w, s3Status(err), err)
			return
		}
	}
	if err := client.RemoveBucket(ctx, bucket); err != nil {
		if minio.ToErrorResponse(err).Code == "BucketNotEmpty" {
			writeErr(w, http.StatusConflict, fmt.Errorf("%s still holds objects — deleting it takes every one of them with it; confirm to go ahead", bucket))
			return
		}
		writeErr(w, s3Status(err), err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true})
}

// s3EmptyBucket removes every object so the bucket itself can go.
func s3EmptyBucket(ctx context.Context, client *minio.Client, bucket string) error {
	_, err := s3RemoveAll(ctx, client, bucket, nil, []string{""})
	return err
}

// s3RemoveAll removes the named objects and everything under the named
// prefixes, and says how many objects went. The listing feeds the batched
// remover directly — no key list ever sits in memory whole.
func s3RemoveAll(ctx context.Context, client *minio.Client, bucket string, keys, prefixes []string) (int, error) {
	toRemove := make(chan minio.ObjectInfo)
	listed := make(chan struct{})
	var listErr error
	sent := 0
	go func() {
		defer close(listed)
		defer close(toRemove)
		// Selected on ctx: without it, a remover that stopped reading
		// leaves this send blocked for the life of the process.
		send := func(obj minio.ObjectInfo) bool {
			select {
			case toRemove <- obj:
				sent++
				return true
			case <-ctx.Done():
				listErr = ctx.Err()
				return false
			}
		}
		for _, key := range keys {
			if !send(minio.ObjectInfo{Key: key}) {
				return
			}
		}
		for _, prefix := range prefixes {
			for obj := range client.ListObjects(ctx, bucket, minio.ListObjectsOptions{Prefix: prefix, Recursive: true}) {
				if obj.Err != nil {
					listErr = obj.Err
					return
				}
				if !send(obj) {
					return
				}
			}
		}
	}()
	// Drained to the end even after a failure. Returning early would stop the
	// remover reading toRemove, which blocks the lister above forever — and a
	// single failed multi-delete reports one error per object in the batch.
	var removeErr error
	failed := 0
	for rerr := range client.RemoveObjects(ctx, bucket, toRemove, minio.RemoveObjectsOptions{}) {
		if rerr.Err != nil {
			failed++
			if removeErr == nil {
				removeErr = rerr.Err
			}
		}
	}
	// The remover drained the channel (or ctx ended the lister's sends), so
	// the lister is done, and its count and error can be read.
	<-listed
	if removeErr != nil {
		return sent - failed, removeErr
	}
	return sent, listErr
}

// s3Object is one object as the browser lists it.
type s3Object struct {
	Key          string `json:"key"`
	Size         int64  `json:"size"`
	Modified     string `json:"modified"`
	ETag         string `json:"etag,omitempty"`
	StorageClass string `json:"storageClass,omitempty"`
}

func (s *Server) handleS3Objects(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := context.WithCancel(r.Context())
	defer cancel()
	q := r.URL.Query()
	bucket := r.PathValue("bucket")
	if err := s3utils.CheckValidBucketName(bucket); err != nil {
		writeErr(w, http.StatusBadRequest, fmt.Errorf("%q is not a valid bucket name", bucket))
		return
	}
	prefix := q.Get("prefix")
	if err := s3utils.CheckValidObjectNamePrefix(prefix); err != nil {
		writeErr(w, http.StatusBadRequest, fmt.Errorf("%q won't work as a prefix", prefix))
		return
	}
	max, _ := strconv.Atoi(q.Get("max"))
	if max < 1 || max > 1000 {
		max = 500
	}
	// Non-recursive listing folds keys at "/" — the store's flat namespace
	// comes back as the folders people put into their key names. Recursive
	// lists every object under the prefix, for downloading a folder.
	delimiter := "/"
	if q.Get("recursive") == "true" {
		delimiter = ""
	}
	client, err := s.s3Connect(ctx, r)
	if err != nil {
		writeErr(w, s3Status(err), err)
		return
	}
	// One page of the store's own listing, so its continuation token carries
	// the next request on exactly where this one stopped. The streaming API
	// hides the token, and within a page it hands out objects before folders,
	// so no "start after the last one shown" survives it. (This call takes no
	// context; the client's transport timeouts bound it.)
	page, err := minio.Core{Client: client}.ListObjectsV2(bucket, prefix, "", q.Get("token"), delimiter, max)
	if err != nil {
		writeErr(w, s3Status(err), err)
		return
	}
	prefixes := []string{}
	for _, p := range page.CommonPrefixes {
		prefixes = append(prefixes, p.Prefix)
	}
	objects := []s3Object{}
	for _, info := range page.Contents {
		// A zero-byte marker named exactly like the prefix would list the
		// folder inside itself.
		if info.Key == prefix {
			continue
		}
		objects = append(objects, s3Object{
			Key:          info.Key,
			Size:         info.Size,
			Modified:     info.LastModified.UTC().Format(time.RFC3339),
			ETag:         strings.Trim(info.ETag, `"`),
			StorageClass: info.StorageClass,
		})
	}
	next := ""
	if page.IsTruncated {
		next = page.NextContinuationToken
	}
	writeJSON(w, http.StatusOK, map[string]any{"prefixes": prefixes, "objects": objects, "truncated": page.IsTruncated, "next": next})
}

// The preview pane reads up to this much of an object as text.
const s3PreviewBytes = 256 << 10

// s3MayBeText rules out the content types that are never text, so their
// bytes are not fetched only to be found binary.
func s3MayBeText(contentType string) bool {
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

// s3Text is the bytes as text when they are: UTF-8 without NULs. A preview
// cut mid-rune drops the partial rune rather than declaring the file binary.
func s3Text(data []byte, cut bool) (string, bool) {
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

// handleS3Info is the preview pane: an object's metadata, and its first
// s3PreviewBytes as text when it is text.
func (s *Server) handleS3Info(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := context.WithCancel(r.Context())
	defer cancel()
	q := r.URL.Query()
	bucket, key := r.PathValue("bucket"), q.Get("key")
	if err := s3utils.CheckValidBucketName(bucket); err != nil {
		writeErr(w, http.StatusBadRequest, fmt.Errorf("%q is not a valid bucket name", bucket))
		return
	}
	if key == "" {
		writeErr(w, http.StatusBadRequest, fmt.Errorf("which object?"))
		return
	}
	client, err := s.s3Connect(ctx, r)
	if err != nil {
		writeErr(w, s3Status(err), err)
		return
	}
	st, err := client.StatObject(ctx, bucket, key, minio.StatObjectOptions{})
	if err != nil {
		writeErr(w, s3Status(err), err)
		return
	}
	metadata := map[string]string{}
	for k, v := range st.UserMetadata {
		metadata[k] = v
	}
	out := map[string]any{
		"key":          st.Key,
		"size":         st.Size,
		"modified":     st.LastModified.UTC().Format(time.RFC3339),
		"contentType":  st.ContentType,
		"etag":         strings.Trim(st.ETag, `"`),
		"storageClass": st.StorageClass,
		"versionId":    st.VersionID,
		"metadata":     metadata,
	}
	if q.Get("text") != "false" && st.Size > 0 && s3MayBeText(st.ContentType) {
		var opts minio.GetObjectOptions
		_ = opts.SetRange(0, s3PreviewBytes-1)
		obj, err := client.GetObject(ctx, bucket, key, opts)
		if err != nil {
			writeErr(w, s3Status(err), err)
			return
		}
		defer obj.Close()
		data, err := io.ReadAll(io.LimitReader(obj, s3PreviewBytes))
		if err != nil {
			writeErr(w, s3Status(err), err)
			return
		}
		cut := st.Size > int64(len(data))
		if text, ok := s3Text(data, cut); ok {
			out["text"] = text
			out["truncated"] = cut
		}
	}
	writeJSON(w, http.StatusOK, out)
}

// handleS3DeleteMany removes a selection: objects by key, folders with
// everything in them. Emptying the whole bucket is the bucket's own delete.
func (s *Server) handleS3DeleteMany(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := context.WithCancel(r.Context())
	defer cancel()
	bucket := r.PathValue("bucket")
	if err := s3utils.CheckValidBucketName(bucket); err != nil {
		writeErr(w, http.StatusBadRequest, fmt.Errorf("%q is not a valid bucket name", bucket))
		return
	}
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
	for _, key := range req.Keys {
		if err := s3utils.CheckValidObjectName(key); err != nil {
			writeErr(w, http.StatusBadRequest, fmt.Errorf("%q won't work as an object key", key))
			return
		}
	}
	for _, prefix := range req.Prefixes {
		if !strings.HasSuffix(prefix, "/") || s3utils.CheckValidObjectName(strings.TrimSuffix(prefix, "/")) != nil {
			writeErr(w, http.StatusBadRequest, fmt.Errorf("%q is not a folder", prefix))
			return
		}
	}
	client, err := s.s3Connect(ctx, r)
	if err != nil {
		writeErr(w, s3Status(err), err)
		return
	}
	deleted, err := s3RemoveAll(ctx, client, bucket, req.Keys, req.Prefixes)
	if err != nil {
		writeErr(w, s3Status(err), fmt.Errorf("%d deleted, then: %w", deleted, err))
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "deleted": deleted})
}

// s3FolderKey checks a folder's marker key: "a/b/", no empty or dot
// segments — those are legal in S3 and confusing everywhere else.
func s3FolderKey(key string) error {
	if !strings.HasSuffix(key, "/") || strings.HasPrefix(key, "/") || s3utils.CheckValidObjectName(key) != nil {
		return fmt.Errorf("%q won't work as a folder", key)
	}
	for part := range strings.SplitSeq(strings.TrimSuffix(key, "/"), "/") {
		if part == "" || part == "." || part == ".." {
			return fmt.Errorf("%q won't work as a folder", key)
		}
	}
	return nil
}

// handleS3CreateFolder writes the zero-byte marker that makes an empty
// folder visible — S3 has no folders, only keys with slashes in them.
func (s *Server) handleS3CreateFolder(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := context.WithCancel(r.Context())
	defer cancel()
	bucket := r.PathValue("bucket")
	if err := s3utils.CheckValidBucketName(bucket); err != nil {
		writeErr(w, http.StatusBadRequest, fmt.Errorf("%q is not a valid bucket name", bucket))
		return
	}
	var req struct {
		Key string `json:"key"`
	}
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 8<<10)).Decode(&req); err != nil {
		writeErr(w, http.StatusBadRequest, err)
		return
	}
	if err := s3FolderKey(req.Key); err != nil {
		writeErr(w, http.StatusBadRequest, err)
		return
	}
	client, err := s.s3Connect(ctx, r)
	if err != nil {
		writeErr(w, s3Status(err), err)
		return
	}
	if _, err := client.PutObject(ctx, bucket, req.Key, bytes.NewReader(nil), 0, minio.PutObjectOptions{ContentType: "application/x-directory"}); err != nil {
		writeErr(w, s3Status(err), err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "key": req.Key})
}

func (s *Server) handleS3Upload(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	bucket := r.PathValue("bucket")
	if err := s3utils.CheckValidBucketName(bucket); err != nil {
		writeErr(w, http.StatusBadRequest, fmt.Errorf("%q is not a valid bucket name", bucket))
		return
	}
	client, err := s.s3Connect(r.Context(), r)
	if err != nil {
		writeErr(w, s3Status(err), err)
		return
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
		if err := s3utils.CheckValidObjectName(key); err != nil {
			writeErr(w, http.StatusBadRequest, fmt.Errorf("%q won't work as an object key", key))
			return
		}
		// Size -1 streams the part straight through in bounded chunks — the
		// server never holds the file, and PartSize bounds the buffer.
		info, err := client.PutObject(r.Context(), bucket, key, part, -1, minio.PutObjectOptions{
			ContentType: part.Header.Get("Content-Type"),
			PartSize:    16 << 20,
		})
		if err != nil {
			writeErr(w, s3Status(err), err)
			return
		}
		writeJSON(w, http.StatusOK, map[string]any{"ok": true, "key": key, "size": info.Size})
		return
	}
	writeErr(w, http.StatusBadRequest, fmt.Errorf("the upload carries no file — send it as the multipart field %q", "file"))
}

func (s *Server) handleS3Download(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	bucket, key := r.PathValue("bucket"), q.Get("key")
	if err := s3utils.CheckValidBucketName(bucket); err != nil {
		writeErr(w, http.StatusBadRequest, fmt.Errorf("%q is not a valid bucket name", bucket))
		return
	}
	if key == "" {
		writeErr(w, http.StatusBadRequest, fmt.Errorf("which object?"))
		return
	}
	client, err := s.s3Connect(r.Context(), r)
	if err != nil {
		writeErr(w, s3Status(err), err)
		return
	}
	obj, err := client.GetObject(r.Context(), bucket, key, minio.GetObjectOptions{})
	if err != nil {
		writeErr(w, s3Status(err), err)
		return
	}
	defer obj.Close()
	// GetObject is lazy — a missing key only surfaces here, while headers
	// can still say 404 instead of a broken download.
	st, err := obj.Stat()
	if err != nil {
		writeErr(w, s3Status(err), err)
		return
	}
	contentType := st.ContentType
	if contentType == "" {
		contentType = "application/octet-stream"
	}
	w.Header().Set("Content-Type", contentType)
	w.Header().Set("Content-Length", strconv.FormatInt(st.Size, 10))
	w.Header().Set("Content-Disposition", mime.FormatMediaType("attachment", map[string]string{"filename": path.Base(key)}))
	w.WriteHeader(http.StatusOK)
	_, _ = io.Copy(w, obj)
}

func (s *Server) handleS3DeleteObject(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := context.WithCancel(r.Context())
	defer cancel()
	q := r.URL.Query()
	bucket, key := r.PathValue("bucket"), q.Get("key")
	if err := s3utils.CheckValidBucketName(bucket); err != nil {
		writeErr(w, http.StatusBadRequest, fmt.Errorf("%q is not a valid bucket name", bucket))
		return
	}
	if key == "" {
		writeErr(w, http.StatusBadRequest, fmt.Errorf("which object?"))
		return
	}
	client, err := s.s3Connect(ctx, r)
	if err != nil {
		writeErr(w, s3Status(err), err)
		return
	}
	if err := client.RemoveObject(ctx, bucket, key, minio.RemoveObjectOptions{}); err != nil {
		writeErr(w, s3Status(err), err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true})
}
