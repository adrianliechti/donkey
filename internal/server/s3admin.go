package server

// The console's bucket browser. donkey-server already sits in the cluster
// and already holds the binding, so it can speak S3 to a space's object
// store directly — no tunnel, no exposed port, the admin token is the only
// door.

import (
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

	"github.com/minio/minio-go/v7"
	"github.com/minio/minio-go/v7/pkg/credentials"
	"github.com/minio/minio-go/v7/pkg/s3utils"

	"github.com/adrianliechti/donkey/internal/addons"
	"github.com/adrianliechti/donkey/internal/config"
	"github.com/adrianliechti/donkey/pkg/runtime"
)

const s3Timeout = 30 * time.Second

// s3MaxUpload caps one upload — big enough for real assets, small enough
// that a stray request can't fill the server's disk buffers or the store.
const s3MaxUpload = 512 << 20

// s3Connect builds a client for the space service the request names, from
// the binding's own values — the browser sees exactly the store the apps
// see.
func (s *Server) s3Connect(ctx context.Context, r *http.Request) (*minio.Client, error) {
	space, err := s.space(r)
	if err != nil {
		return nil, runtime.UserError{Err: err}
	}
	name := r.PathValue("service")
	if !config.ValidName(name) {
		return nil, runtime.UserError{Err: fmt.Errorf("%q is not a valid service name", name)}
	}
	data, err := s.rt.ServiceBinding(ctx, space, addons.SharedOwner, name)
	if err != nil {
		return nil, err
	}
	if len(data) == 0 {
		return nil, runtime.UserError{Err: fmt.Errorf("the space has no service named %q", name)}
	}
	endpoint, ok := data["AWS_ENDPOINT_URL_S3"]
	if !ok {
		return nil, runtime.UserError{Err: fmt.Errorf("%s doesn't speak S3 — only services with an AWS_ENDPOINT_URL_S3 binding can be browsed", name)}
	}
	u, err := url.Parse(endpoint)
	if err != nil || u.Host == "" {
		return nil, runtime.UserError{Err: fmt.Errorf("the service's endpoint %q is not a usable URL", endpoint)}
	}
	client, err := minio.New(u.Host, &minio.Options{
		Creds:  credentials.NewStaticV4(data["AWS_ACCESS_KEY_ID"], data["AWS_SECRET_ACCESS_KEY"], ""),
		Secure: u.Scheme == "https",
		Region: data["AWS_REGION"],
	})
	if err != nil {
		return nil, fmt.Errorf("%s is not reachable: %w", name, err)
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
	ctx, cancel := context.WithTimeout(r.Context(), s3Timeout)
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
	ctx, cancel := context.WithTimeout(r.Context(), s3Timeout)
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
	ctx, cancel := context.WithTimeout(r.Context(), 5*time.Minute)
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

// s3EmptyBucket removes every object so the bucket itself can go. The
// listing feeds the batched remover directly — no key list ever sits in
// memory whole.
func s3EmptyBucket(ctx context.Context, client *minio.Client, bucket string) error {
	listed := client.ListObjects(ctx, bucket, minio.ListObjectsOptions{Recursive: true})
	toRemove := make(chan minio.ObjectInfo)
	var listErr error
	go func() {
		defer close(toRemove)
		for obj := range listed {
			if obj.Err != nil {
				listErr = obj.Err
				return
			}
			// Selected on ctx: without it, a remover that stopped reading
			// leaves this send blocked for the life of the process.
			select {
			case toRemove <- obj:
			case <-ctx.Done():
				listErr = ctx.Err()
				return
			}
		}
	}()
	// Drained to the end even after a failure. Returning early would stop the
	// remover reading toRemove, which blocks the lister above forever — and a
	// single failed multi-delete reports one error per object in the batch.
	var removeErr error
	for rerr := range client.RemoveObjects(ctx, bucket, toRemove, minio.RemoveObjectsOptions{}) {
		if rerr.Err != nil && removeErr == nil {
			removeErr = rerr.Err
		}
	}
	if removeErr != nil {
		return removeErr
	}
	// The remover drains the channel, so the lister goroutine is done by the
	// time its error is read.
	return listErr
}

func (s *Server) handleS3Objects(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := context.WithTimeout(r.Context(), s3Timeout)
	defer cancel()
	q := r.URL.Query()
	bucket := r.PathValue("bucket")
	if err := s3utils.CheckValidBucketName(bucket); err != nil {
		writeErr(w, http.StatusBadRequest, fmt.Errorf("%q is not a valid bucket name", bucket))
		return
	}
	prefix := q.Get("prefix")
	max, _ := strconv.Atoi(q.Get("max"))
	if max < 1 || max > 500 {
		max = 500
	}
	client, err := s.s3Connect(ctx, r)
	if err != nil {
		writeErr(w, s3Status(err), err)
		return
	}
	type object struct {
		Key      string `json:"key"`
		Size     int64  `json:"size"`
		Modified string `json:"modified"`
	}
	prefixes := []string{}
	objects := []object{}
	truncated := false
	// Non-recursive listing folds keys at "/" — the store's flat namespace
	// comes back as the folders people put into their key names.
	for info := range client.ListObjects(ctx, bucket, minio.ListObjectsOptions{Prefix: prefix, Recursive: false}) {
		if info.Err != nil {
			writeErr(w, s3Status(info.Err), info.Err)
			return
		}
		// A zero-byte marker named exactly like the prefix would list the
		// folder inside itself.
		if info.Key == prefix {
			continue
		}
		if len(prefixes)+len(objects) >= max {
			truncated = true
			break
		}
		if strings.HasSuffix(info.Key, "/") {
			prefixes = append(prefixes, info.Key)
			continue
		}
		objects = append(objects, object{Key: info.Key, Size: info.Size, Modified: info.LastModified.UTC().Format(time.RFC3339)})
	}
	writeJSON(w, http.StatusOK, map[string]any{"prefixes": prefixes, "objects": objects, "truncated": truncated})
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
	// The cap sits on the raw body, so an oversized upload fails while
	// streaming instead of after filling anything up.
	r.Body = http.MaxBytesReader(w, r.Body, s3MaxUpload)
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
			var tooLarge *http.MaxBytesError
			if errors.As(err, &tooLarge) {
				writeErr(w, http.StatusRequestEntityTooLarge, fmt.Errorf("that file is bigger than the console's %dMB upload limit", s3MaxUpload>>20))
				return
			}
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
	ctx, cancel := context.WithTimeout(r.Context(), s3Timeout)
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
