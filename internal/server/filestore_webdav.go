package server

import (
	"context"
	"encoding/xml"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"path"
	"slices"
	"strconv"
	"strings"
	"time"
	"unicode"

	"github.com/adrianliechti/donkey/pkg/runtime"
)

// webdavStore browses a WebDAV service as the signed-in user, the way the
// Trino viewer does: every request carries the caller's IdP access token as
// Bearer, and nothing of donkey's. The client exists for one request only;
// the token never enters a binding or a pool.
//
// A WebDAV service is one tree, so it has one container, named after the
// service. Folders are real collections — made with MKCOL, removed whole
// with DELETE — and a listing is one PROPFIND of depth 1, paged here, so the
// console sees the same pages and tokens it sees from S3.
type webdavStore struct {
	base  *url.URL // WEBDAV_URL, its path ending in "/"
	name  string   // the service, and its one container
	token string
}

// Headers arrive within this long or the request fails; bodies — downloads,
// uploads — take as long as they take, bounded by the request's context.
var webdavTransport = func() *http.Transport {
	t := http.DefaultTransport.(*http.Transport).Clone()
	t.ResponseHeaderTimeout = 30 * time.Second
	return t
}()

// Never follow a redirect carrying the user's credential elsewhere.
var webdavHTTPClient = &http.Client{
	Transport:     webdavTransport,
	CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
}

// Listing a tree recursively walks it folder by folder; past this many
// entries, the folder is too big to take in one go.
const webdavListCap = 10000

func newWebDAVStore(rawURL, service, token string) (*webdavStore, error) {
	base, err := url.Parse(rawURL) // validated by serviceConnection
	if err != nil {
		return nil, runtime.UserError{Err: fmt.Errorf("%s has no usable address", service)}
	}
	if !strings.HasSuffix(base.Path, "/") {
		base.Path += "/"
		base.RawPath = ""
	}
	base.RawQuery, base.Fragment = "", ""
	return &webdavStore{base: base, name: service, token: token}, nil
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

// url is where a key lives: each segment escaped on its own, a folder with
// its trailing slash, so the service never has to redirect.
func (s *webdavStore) url(key string) *url.URL {
	trimmed := strings.TrimSuffix(key, "/")
	if trimmed == "" {
		return s.base
	}
	var escaped []string
	for part := range strings.SplitSeq(trimmed, "/") {
		escaped = append(escaped, url.PathEscape(part))
	}
	u := s.base.JoinPath(escaped...)
	if strings.HasSuffix(key, "/") {
		u.Path += "/"
		if u.RawPath != "" {
			u.RawPath += "/"
		}
	}
	return u
}

func (s *webdavStore) do(ctx context.Context, method string, u *url.URL, header http.Header, body io.Reader, size int64) (*http.Response, error) {
	req, err := http.NewRequestWithContext(ctx, method, u.String(), body)
	if err != nil {
		return nil, err
	}
	for name, values := range header {
		req.Header[name] = values
	}
	req.Header.Set("Authorization", "Bearer "+s.token)
	req.Header.Set("User-Agent", "donkey")
	if body != nil {
		req.ContentLength = size
	}
	res, err := webdavHTTPClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("%s did not answer: %w", s.name, err)
	}
	return res, nil
}

// fail turns the service's answer into the console's error and drains the
// response.
func (s *webdavStore) fail(res *http.Response, what string) error {
	defer res.Body.Close()
	_, _ = io.Copy(io.Discard, io.LimitReader(res.Body, 64<<10))
	switch res.StatusCode {
	case http.StatusUnauthorized, http.StatusForbidden:
		return &upstreamError{status: res.StatusCode, err: fmt.Errorf("%s did not accept your identity token (HTTP %d)", s.name, res.StatusCode)}
	case http.StatusNotFound:
		return &upstreamError{status: res.StatusCode, err: fmt.Errorf("%s: not found", what)}
	}
	if res.StatusCode >= 400 && res.StatusCode < 500 {
		return &upstreamError{status: res.StatusCode, err: fmt.Errorf("%s answered HTTP %d for %s", s.name, res.StatusCode, what)}
	}
	return fmt.Errorf("%s answered HTTP %d for %s", s.name, res.StatusCode, what)
}

// ─── PROPFIND ──────────────────────────────────────────────────────────

const webdavPropfindBody = `<?xml version="1.0" encoding="utf-8"?>
<D:propfind xmlns:D="DAV:"><D:prop>
<D:displayname/><D:resourcetype/><D:getcontentlength/><D:getlastmodified/><D:getcontenttype/><D:getetag/>
</D:prop></D:propfind>`

type davMultistatus struct {
	Responses []davResponse `xml:"DAV: response"`
}

type davResponse struct {
	Href     string        `xml:"DAV: href"`
	Propstat []davPropstat `xml:"DAV: propstat"`
}

type davPropstat struct {
	Status string  `xml:"DAV: status"`
	Prop   davProp `xml:"DAV: prop"`
}

type davProp struct {
	DisplayName  string `xml:"DAV: displayname"`
	ResourceType struct {
		Collection *struct{} `xml:"DAV: collection"`
	} `xml:"DAV: resourcetype"`
	ContentLength string `xml:"DAV: getcontentlength"`
	LastModified  string `xml:"DAV: getlastmodified"`
	ContentType   string `xml:"DAV: getcontenttype"`
	ETag          string `xml:"DAV: getetag"`
}

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

// relative places a response href under the service root. Hrefs come as
// absolute paths or full URLs, percent-encoded; resolved against the
// request and compared decoded, they all land in the same place.
func (s *webdavStore) relative(reqURL *url.URL, href string) (string, bool) {
	ref, err := url.Parse(strings.TrimSpace(href))
	if err != nil {
		return "", false
	}
	p := reqURL.ResolveReference(ref).Path
	if p+"/" == s.base.Path {
		return "", true
	}
	return strings.CutPrefix(p, s.base.Path)
}

func parseHTTPTime(raw string) time.Time {
	if t, err := http.ParseTime(raw); err == nil {
		return t
	}
	if t, err := time.Parse(time.RFC3339, raw); err == nil {
		return t
	}
	return time.Time{}
}

// propfind describes the resource at key (depth 0) or it and its children
// (depth 1).
func (s *webdavStore) propfind(ctx context.Context, key string, depth int) ([]davEntry, error) {
	u := s.url(key)
	header := http.Header{"Depth": {strconv.Itoa(depth)}, "Content-Type": {"application/xml; charset=utf-8"}}
	res, err := s.do(ctx, "PROPFIND", u, header, strings.NewReader(webdavPropfindBody), int64(len(webdavPropfindBody)))
	if err != nil {
		return nil, err
	}
	if res.StatusCode != http.StatusMultiStatus {
		return nil, s.fail(res, key)
	}
	defer res.Body.Close()
	var ms davMultistatus
	if err := xml.NewDecoder(io.LimitReader(res.Body, 64<<20)).Decode(&ms); err != nil {
		return nil, fmt.Errorf("%s answered PROPFIND with something other than a multistatus: %w", s.name, err)
	}
	var entries []davEntry
	for _, r := range ms.Responses {
		rel, ok := s.relative(u, r.Href)
		if !ok {
			continue
		}
		entry := davEntry{Key: strings.TrimSuffix(rel, "/")}
		for _, ps := range r.Propstat {
			// Only what the service actually has: a 404 propstat lists the
			// properties it does not.
			status := strings.Fields(ps.Status)
			if len(status) < 2 || status[1] != "200" {
				continue
			}
			p := ps.Prop
			if strings.TrimSpace(p.DisplayName) != "" {
				entry.DisplayName = p.DisplayName
			}
			if p.ResourceType.Collection != nil {
				entry.Dir = true
			}
			if p.ContentLength != "" {
				entry.Size, _ = strconv.ParseInt(strings.TrimSpace(p.ContentLength), 10, 64)
			}
			if p.LastModified != "" {
				entry.Modified = parseHTTPTime(strings.TrimSpace(p.LastModified))
			}
			if p.ContentType != "" {
				entry.ContentType = strings.TrimSpace(p.ContentType)
			}
			if p.ETag != "" {
				entry.ETag = strings.Trim(strings.TrimSpace(p.ETag), `"`)
			}
		}
		if entry.Dir {
			if entry.Key != "" {
				entry.Key += "/"
			}
			entry.Size = 0
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
	res, err := s.do(ctx, http.MethodGet, s.url(key), header, nil, 0)
	if err != nil {
		return nil, fileInfo{}, err
	}
	if res.StatusCode != http.StatusOK && res.StatusCode != http.StatusPartialContent {
		return nil, fileInfo{}, s.fail(res, key)
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
	res, err := s.do(ctx, http.MethodPut, s.url(key), http.Header{"Content-Type": {contentType}}, counted, size)
	if err != nil {
		return 0, err
	}
	switch res.StatusCode {
	case http.StatusOK, http.StatusCreated, http.StatusNoContent:
		res.Body.Close()
		return counted.n, nil
	}
	return 0, s.fail(res, key)
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

// remove is a DELETE; a folder goes with everything in it (RFC 4918 takes
// a collection's DELETE at infinite depth). What is already gone is gone.
func (s *webdavStore) remove(ctx context.Context, key string) error {
	res, err := s.do(ctx, http.MethodDelete, s.url(key), nil, nil, 0)
	if err != nil {
		return err
	}
	switch res.StatusCode {
	case http.StatusOK, http.StatusNoContent, http.StatusNotFound:
		res.Body.Close()
		return nil
	}
	return s.fail(res, key)
}

func (s *webdavStore) Delete(ctx context.Context, _ string, key string) error {
	return s.remove(ctx, key)
}

func (s *webdavStore) DeleteMany(ctx context.Context, _ string, keys, folders []string) (int, error) {
	deleted := 0
	for _, key := range slices.Concat(keys, folders) {
		if err := s.remove(ctx, key); err != nil {
			return deleted, err
		}
		deleted++
	}
	return deleted, nil
}

// MakeFolder is MKCOL. 405 means something is already called that; 409
// that the parent is missing.
func (s *webdavStore) MakeFolder(ctx context.Context, _ string, key string) error {
	res, err := s.do(ctx, "MKCOL", s.url(key), nil, nil, 0)
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
	return s.fail(res, key)
}

var _ fileStore = (*webdavStore)(nil)
