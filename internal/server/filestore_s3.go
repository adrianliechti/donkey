package server

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"net/url"
	"strings"

	"github.com/minio/minio-go/v7"
	"github.com/minio/minio-go/v7/pkg/credentials"
	"github.com/minio/minio-go/v7/pkg/s3utils"
)

// s3Store browses an S3-compatible store with the binding's own keys — the
// browser sees exactly the store the apps see. Buckets are the containers;
// folders are what the flat key space folds into at "/".
type s3Store struct {
	client *minio.Client
}

func newS3Store(binding map[string]string, service string) (*s3Store, error) {
	u, _ := url.Parse(binding["AWS_ENDPOINT_URL_S3"]) // validated by serviceConnection
	client, err := minio.New(u.Host, &minio.Options{
		Creds:  credentials.NewStaticV4(binding["AWS_ACCESS_KEY_ID"], binding["AWS_SECRET_ACCESS_KEY"], binding["AWS_SESSION_TOKEN"]),
		Secure: u.Scheme == "https",
		Region: binding["AWS_REGION"],
	})
	if err != nil {
		return nil, fmt.Errorf("%s is not reachable: %w", service, err)
	}
	return &s3Store{client: client}, nil
}

// s3Err keeps the store's own 4xx answers the caller's, and names the two
// conflicts the browser asks about.
func s3Err(err error) error {
	if err == nil {
		return nil
	}
	resp := minio.ToErrorResponse(err)
	switch resp.Code {
	case "BucketAlreadyOwnedByYou", "BucketAlreadyExists":
		return fmt.Errorf("%w: %v", errExists, err)
	case "BucketNotEmpty":
		return fmt.Errorf("%w: %v", errNotEmpty, err)
	}
	if resp.StatusCode >= 400 && resp.StatusCode < 500 {
		return &upstreamError{status: resp.StatusCode, err: err}
	}
	return err
}

func (s *s3Store) CheckContainer(name string, creating bool) error {
	if creating {
		// Strict S3 rules up front, so the store never sees a name it would
		// answer with a cryptic XML error.
		if err := s3utils.CheckValidBucketNameStrict(name); err != nil {
			return fmt.Errorf("%q won't work as a bucket name — lowercase letters, digits and dashes, 3 to 63 characters", name)
		}
		return nil
	}
	if err := s3utils.CheckValidBucketName(name); err != nil {
		return fmt.Errorf("%q is not a valid bucket name", name)
	}
	return nil
}

func (s *s3Store) CheckKey(key string) error {
	if err := s3utils.CheckValidObjectName(key); err != nil {
		return fmt.Errorf("%q won't work as an object key", key)
	}
	return nil
}

// CheckFolder is a folder's marker key: "a/b/", no empty or dot segments —
// those are legal in S3 and confusing everywhere else.
func (s *s3Store) CheckFolder(key string) error {
	if !strings.HasSuffix(key, "/") || strings.HasPrefix(key, "/") || s3utils.CheckValidObjectName(strings.TrimSuffix(key, "/")) != nil {
		return fmt.Errorf("%q won't work as a folder", key)
	}
	for part := range strings.SplitSeq(strings.TrimSuffix(key, "/"), "/") {
		if part == "" || part == "." || part == ".." {
			return fmt.Errorf("%q won't work as a folder", key)
		}
	}
	return nil
}

func (s *s3Store) CheckPrefix(prefix string) error {
	if err := s3utils.CheckValidObjectNamePrefix(prefix); err != nil {
		return fmt.Errorf("%q won't work as a prefix", prefix)
	}
	return nil
}

func (s *s3Store) Containers(ctx context.Context) ([]fileContainer, error) {
	buckets, err := s.client.ListBuckets(ctx)
	if err != nil {
		return nil, s3Err(err)
	}
	out := make([]fileContainer, 0, len(buckets))
	for _, b := range buckets {
		out = append(out, fileContainer{Name: b.Name, Created: b.CreationDate})
	}
	return out, nil
}

func (s *s3Store) CreateContainer(ctx context.Context, name string) error {
	return s3Err(s.client.MakeBucket(ctx, name, minio.MakeBucketOptions{}))
}

func (s *s3Store) DeleteContainer(ctx context.Context, name string, force bool) error {
	if force {
		// Sweeping a full bucket is many requests, not one.
		if _, err := s.removeAll(ctx, name, nil, []string{""}); err != nil {
			return s3Err(err)
		}
	}
	return s3Err(s.client.RemoveBucket(ctx, name))
}

func (s *s3Store) List(ctx context.Context, bucket, prefix, token string, max int, recursive bool) (fileListing, error) {
	// Non-recursive listing folds keys at "/" — the store's flat namespace
	// comes back as the folders people put into their key names. Recursive
	// lists every object under the prefix, for downloading a folder.
	delimiter := "/"
	if recursive {
		delimiter = ""
	}
	// One page of the store's own listing, so its continuation token carries
	// the next request on exactly where this one stopped. The streaming API
	// hides the token, and within a page it hands out objects before folders,
	// so no "start after the last one shown" survives it. (This call takes no
	// context; the client's transport timeouts bound it.)
	page, err := minio.Core{Client: s.client}.ListObjectsV2(bucket, prefix, "", token, delimiter, max)
	if err != nil {
		return fileListing{}, s3Err(err)
	}
	out := fileListing{Truncated: page.IsTruncated}
	for _, p := range page.CommonPrefixes {
		out.Prefixes = append(out.Prefixes, p.Prefix)
	}
	for _, info := range page.Contents {
		// A zero-byte marker named exactly like the prefix would list the
		// folder inside itself.
		if info.Key == prefix {
			continue
		}
		out.Objects = append(out.Objects, fileEntry{
			Key:          info.Key,
			Size:         info.Size,
			Modified:     info.LastModified,
			ETag:         strings.Trim(info.ETag, `"`),
			StorageClass: info.StorageClass,
		})
	}
	if page.IsTruncated {
		out.Next = page.NextContinuationToken
	}
	return out, nil
}

func s3FileInfo(st minio.ObjectInfo) fileInfo {
	metadata := map[string]string{}
	for k, v := range st.UserMetadata {
		metadata[k] = v
	}
	return fileInfo{
		Key:          st.Key,
		Size:         st.Size,
		Modified:     st.LastModified,
		ContentType:  st.ContentType,
		ETag:         strings.Trim(st.ETag, `"`),
		StorageClass: st.StorageClass,
		VersionID:    st.VersionID,
		Metadata:     metadata,
	}
}

func (s *s3Store) Stat(ctx context.Context, bucket, key string) (fileInfo, error) {
	st, err := s.client.StatObject(ctx, bucket, key, minio.StatObjectOptions{})
	if err != nil {
		return fileInfo{}, s3Err(err)
	}
	return s3FileInfo(st), nil
}

func (s *s3Store) Open(ctx context.Context, bucket, key string, limit int64) (io.ReadCloser, fileInfo, error) {
	var opts minio.GetObjectOptions
	if limit > 0 {
		_ = opts.SetRange(0, limit-1)
	}
	obj, err := s.client.GetObject(ctx, bucket, key, opts)
	if err != nil {
		return nil, fileInfo{}, s3Err(err)
	}
	// GetObject is lazy — a missing key only surfaces on the first read.
	st, err := obj.Stat()
	if err != nil {
		obj.Close()
		return nil, fileInfo{}, s3Err(err)
	}
	return obj, s3FileInfo(st), nil
}

func (s *s3Store) Put(ctx context.Context, bucket, key string, body io.Reader, size int64, contentType string) (int64, error) {
	// An unknown size streams straight through in bounded parts — the
	// server never holds the file, and PartSize bounds the buffer.
	info, err := s.client.PutObject(ctx, bucket, key, body, size, minio.PutObjectOptions{
		ContentType: contentType,
		PartSize:    16 << 20,
	})
	if err != nil {
		return 0, s3Err(err)
	}
	return info.Size, nil
}

func (s *s3Store) Delete(ctx context.Context, bucket, key string) error {
	return s3Err(s.client.RemoveObject(ctx, bucket, key, minio.RemoveObjectOptions{}))
}

func (s *s3Store) DeleteMany(ctx context.Context, bucket string, keys, folders []string) (int, error) {
	n, err := s.removeAll(ctx, bucket, keys, folders)
	return n, s3Err(err)
}

// MakeFolder writes the zero-byte marker that makes an empty folder
// visible — S3 has no folders, only keys with slashes in them.
func (s *s3Store) MakeFolder(ctx context.Context, bucket, key string) error {
	_, err := s.client.PutObject(ctx, bucket, key, bytes.NewReader(nil), 0, minio.PutObjectOptions{ContentType: "application/x-directory"})
	return s3Err(err)
}

// removeAll removes the named objects and everything under the named
// prefixes, and says how many objects went. The listing feeds the batched
// remover directly — no key list ever sits in memory whole.
func (s *s3Store) removeAll(ctx context.Context, bucket string, keys, prefixes []string) (int, error) {
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
			for obj := range s.client.ListObjects(ctx, bucket, minio.ListObjectsOptions{Prefix: prefix, Recursive: true}) {
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
	for rerr := range s.client.RemoveObjects(ctx, bucket, toRemove, minio.RemoveObjectsOptions{}) {
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

var _ fileStore = (*s3Store)(nil)
