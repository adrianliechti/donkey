package server

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"path"
	"slices"
	"strconv"
	"strings"
	"time"
	"unicode"

	"github.com/adrianliechti/donkey/pkg/runtime"
)

// webdavStore browses a WebDAV service as the signed-in user (davClient).
// A WebDAV service is one tree, so it has one container, named after the
// service. Folders are real collections — made with MKCOL, removed whole
// with DELETE — and a listing is one PROPFIND of depth 1, paged here, so the
// console sees the same pages and tokens it sees from S3.
type webdavStore struct {
	dav  *davClient
	name string // the service, and its one container
}

// Listing a tree recursively walks it folder by folder; past this many
// entries, the folder is too big to take in one go.
const webdavListCap = 10000

func newWebDAVStore(rawURL, service, token string) (*webdavStore, error) {
	dav, err := newDAVClient(rawURL, service, token)
	if err != nil {
		return nil, err
	}
	return &webdavStore{dav: dav, name: service}, nil
}

// webdavSegments checks a slash-separated path the way a file system would:
// no empty, dot or control-character segments.
func webdavSegments(p string) error {
	if p == "" || len(p) > 1024 || strings.HasPrefix(p, "/") {
		return fmt.Errorf("invalid path")
	}
	for part := range strings.SplitSeq(p, "/") {
		if part == "" || part == "." || part == ".." || strings.ContainsFunc(part, unicode.IsControl) || strings.ContainsAny(part, `\`) {
			return fmt.Errorf("invalid path")
		}
	}
	return nil
}

func (s *webdavStore) CheckContainer(name string, creating bool) error {
	if creating {
		return runtime.UserError{Err: fmt.Errorf("%s is one tree — make folders inside it instead", s.name)}
	}
	if name != s.name {
		return fmt.Errorf("%q is not a folder root of %s", name, s.name)
	}
	return nil
}

func (s *webdavStore) CheckKey(key string) error {
	if strings.HasSuffix(key, "/") || webdavSegments(key) != nil {
		return fmt.Errorf("%q won't work as a file path", key)
	}
	return nil
}

func (s *webdavStore) CheckFolder(key string) error {
	if !strings.HasSuffix(key, "/") || webdavSegments(strings.TrimSuffix(key, "/")) != nil {
		return fmt.Errorf("%q won't work as a folder", key)
	}
	return nil
}

func (s *webdavStore) CheckPrefix(prefix string) error {
	if prefix == "" {
		return nil
	}
	return s.CheckFolder(prefix)
}

// ─── PROPFIND ──────────────────────────────────────────────────────────

const webdavPropfindBody = `<?xml version="1.0" encoding="utf-8"?>
<D:propfind xmlns:D="DAV:"><D:prop>
<D:displayname/><D:resourcetype/><D:getcontentlength/><D:getlastmodified/><D:getcontenttype/><D:getetag/>
</D:prop></D:propfind>`

// davEntry is one resource as PROPFIND describes it, keyed relative to the
// service root — a folder with its trailing slash.
type davEntry struct {
	Key         string
	DisplayName string
	Dir         bool
	Size        int64
	Modified    time.Time
	ContentType string
	ETag        string
}

// propfind describes the resource at key (depth 0) or it and its children
// (depth 1).
func (s *webdavStore) propfind(ctx context.Context, key string, depth int) ([]davEntry, error) {
	items, err := s.dav.multistatus(ctx, "PROPFIND", key, depth, webdavPropfindBody)
	if err != nil {
		return nil, err
	}
	var entries []davEntry
	for _, item := range items {
		entry := davEntry{
			Key:         item.Key, // a collection's already ends in "/"
			Dir:         item.collection(),
			DisplayName: item.prop(func(p davProp) string { return p.DisplayName }),
			Modified:    parseHTTPTime(item.prop(func(p davProp) string { return p.LastModified })),
			ContentType: item.prop(func(p davProp) string { return p.ContentType }),
			ETag:        strings.Trim(item.prop(func(p davProp) string { return p.ETag }), `"`),
		}
		if !entry.Dir {
			entry.Key = strings.TrimSuffix(entry.Key, "/")
			entry.Size, _ = strconv.ParseInt(item.prop(func(p davProp) string { return p.ContentLength }), 10, 64)
		}
		entries = append(entries, entry)
	}
	return entries, nil
}

// children lists a folder: what PROPFIND returns at depth 1, less the
// folder itself.
func (s *webdavStore) children(ctx context.Context, folder string) ([]davEntry, string, error) {
	entries, err := s.propfind(ctx, folder, 1)
	if err != nil {
		return nil, "", err
	}
	var displayName string
	out := entries[:0]
	for _, e := range entries {
		if strings.TrimSuffix(e.Key, "/") == strings.TrimSuffix(folder, "/") {
			displayName = e.DisplayName
			continue
		}
		// A child is one segment below the folder; anything else is a
		// service answering more than it was asked.
		if !strings.HasPrefix(e.Key, folder) || strings.Contains(strings.TrimSuffix(e.Key[len(folder):], "/"), "/") {
			continue
		}
		out = append(out, e)
	}
	return out, displayName, nil
}

// ─── fileStore ─────────────────────────────────────────────────────────

// Containers asks the service for its root first, so a token it will not
// take, or an address that does not answer, shows up as such rather than
// as an empty tree.
func (s *webdavStore) Containers(ctx context.Context) ([]fileContainer, error) {
	if _, err := s.propfind(ctx, "", 0); err != nil {
		return nil, err
	}
	return []fileContainer{{Name: s.name}}, nil
}

func (s *webdavStore) CreateContainer(context.Context, string) error {
	return runtime.UserError{Err: fmt.Errorf("%s is one tree — make folders inside it instead", s.name)}
}

func (s *webdavStore) DeleteContainer(context.Context, string, bool) error {
	return runtime.UserError{Err: fmt.Errorf("%s is the service's own tree — remove the service to let go of it", s.name)}
}

// List pages a folder's children — or, recursively, every file below it —
// in key order, the token naming the last entry handed out, so the console
// walks a WebDAV tree exactly as it walks a bucket.
func (s *webdavStore) List(ctx context.Context, _ string, prefix, token string, max int, recursive bool) (fileListing, error) {
	var entries []davEntry
	out := fileListing{DisplayNames: map[string]string{}}
	if recursive {
		pending := []string{prefix}
		for len(pending) > 0 {
			folder := pending[0]
			pending = pending[1:]
			kids, name, err := s.children(ctx, folder)
			if err != nil {
				return fileListing{}, err
			}
			if folder == prefix && name != "" {
				out.DisplayNames[prefix] = name
			}
			for _, e := range kids {
				if e.Dir {
					pending = append(pending, e.Key)
					continue
				}
				entries = append(entries, e)
			}
			if len(entries)+len(pending) > webdavListCap {
				return fileListing{}, runtime.UserError{Err: fmt.Errorf("more than %d files below %q — too many to take in one go", webdavListCap, prefix)}
			}
		}
	} else {
		var err error
		var name string
		if entries, name, err = s.children(ctx, prefix); err != nil {
			return fileListing{}, err
		}
		if name != "" {
			out.DisplayNames[prefix] = name
		}
	}
	slices.SortFunc(entries, func(a, b davEntry) int { return strings.Compare(a.Key, b.Key) })
	for i, e := range entries {
		if token != "" && e.Key <= token {
			continue
		}
		if len(out.Prefixes)+len(out.Objects) == max {
			out.Truncated, out.Next = true, entries[i-1].Key
			break
		}
		if e.DisplayName != "" {
			out.DisplayNames[e.Key] = e.DisplayName
		}
		if e.Dir {
			out.Prefixes = append(out.Prefixes, e.Key)
			continue
		}
		out.Objects = append(out.Objects, fileEntry{Key: e.Key, Size: e.Size, Modified: e.Modified, ETag: e.ETag})
	}
	return out, nil
}

func (s *webdavStore) Stat(ctx context.Context, _ string, key string) (fileInfo, error) {
	entries, err := s.propfind(ctx, key, 0)
	if err != nil {
		return fileInfo{}, err
	}
	for _, e := range entries {
		if strings.TrimSuffix(e.Key, "/") != key {
			continue
		}
		if e.Dir {
			return fileInfo{}, &upstreamError{status: http.StatusNotFound, err: fmt.Errorf("%s is a folder", key)}
		}
		return fileInfo{Key: key, Size: e.Size, Modified: e.Modified, ContentType: e.ContentType, ETag: e.ETag, Metadata: map[string]string{}}, nil
	}
	return fileInfo{}, &upstreamError{status: http.StatusNotFound, err: fmt.Errorf("%s: not found", key)}
}

// Open is a GET, ranged when only the start is wanted. A service that
// ignores the range sends everything; the reader stops at the limit either
// way, and the size reported is the whole file's.
func (s *webdavStore) Open(ctx context.Context, _ string, key string, limit int64) (io.ReadCloser, fileInfo, error) {
	header := http.Header{}
	if limit > 0 {
		header.Set("Range", fmt.Sprintf("bytes=0-%d", limit-1))
	}
	res, err := s.dav.do(ctx, http.MethodGet, s.dav.url(key), header, nil, 0)
	if err != nil {
		return nil, fileInfo{}, err
	}
	if res.StatusCode != http.StatusOK && res.StatusCode != http.StatusPartialContent {
		return nil, fileInfo{}, s.dav.fail(res, key)
	}
	info := fileInfo{Key: key, Size: -1, ContentType: res.Header.Get("Content-Type"), ETag: strings.Trim(res.Header.Get("ETag"), `"`), Metadata: map[string]string{}}
	if lm := res.Header.Get("Last-Modified"); lm != "" {
		info.Modified = parseHTTPTime(lm)
	}
	if res.StatusCode == http.StatusPartialContent {
		if _, total, ok := strings.Cut(res.Header.Get("Content-Range"), "/"); ok {
			info.Size, _ = strconv.ParseInt(total, 10, 64)
		}
	} else if res.ContentLength >= 0 {
		info.Size = res.ContentLength
	}
	body := io.ReadCloser(res.Body)
	if limit > 0 {
		body = struct {
			io.Reader
			io.Closer
		}{io.LimitReader(res.Body, limit), res.Body}
	}
	return body, info, nil
}

// Put is a PUT with the size as Content-Length when the console said it —
// many services refuse a chunked upload — and chunked otherwise.
func (s *webdavStore) Put(ctx context.Context, _ string, key string, body io.Reader, size int64, contentType string) (int64, error) {
	if contentType == "" {
		contentType = "application/octet-stream"
	}
	counted := &webdavCounter{r: body}
	res, err := s.dav.do(ctx, http.MethodPut, s.dav.url(key), http.Header{"Content-Type": {contentType}}, counted, size)
	if err != nil {
		return 0, err
	}
	switch res.StatusCode {
	case http.StatusOK, http.StatusCreated, http.StatusNoContent:
		res.Body.Close()
		return counted.n, nil
	}
	return 0, s.dav.fail(res, key)
}

// webdavCounter counts what the service took when the size was unknown.
type webdavCounter struct {
	r io.Reader
	n int64
}

func (c *webdavCounter) Read(p []byte) (int, error) {
	n, err := c.r.Read(p)
	c.n += int64(n)
	return n, err
}

// Delete is a DELETE; a folder goes with everything in it (RFC 4918 takes
// a collection's DELETE at infinite depth).
func (s *webdavStore) Delete(ctx context.Context, _ string, key string) error {
	return s.dav.remove(ctx, key, "")
}

func (s *webdavStore) DeleteMany(ctx context.Context, _ string, keys, folders []string) (int, error) {
	deleted := 0
	for _, key := range slices.Concat(keys, folders) {
		if err := s.dav.remove(ctx, key, ""); err != nil {
			return deleted, err
		}
		deleted++
	}
	return deleted, nil
}

// MakeFolder is MKCOL. 405 means something is already called that; 409
// that the parent is missing.
func (s *webdavStore) MakeFolder(ctx context.Context, _ string, key string) error {
	res, err := s.dav.do(ctx, "MKCOL", s.dav.url(key), nil, nil, 0)
	if err != nil {
		return err
	}
	switch res.StatusCode {
	case http.StatusCreated, http.StatusOK, http.StatusNoContent:
		res.Body.Close()
		return nil
	case http.StatusMethodNotAllowed:
		res.Body.Close()
		return fmt.Errorf("%w: %s", errExists, key)
	case http.StatusConflict:
		res.Body.Close()
		return &upstreamError{status: http.StatusConflict, err: fmt.Errorf("the folder above %q is missing", path.Dir(strings.TrimSuffix(key, "/")))}
	}
	return s.dav.fail(res, key)
}

var _ fileStore = (*webdavStore)(nil)
