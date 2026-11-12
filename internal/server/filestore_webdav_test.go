package server

import (
	"bytes"
	"context"
	"encoding/json"
	"encoding/xml"
	"fmt"
	"io"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path"
	"reflect"
	"slices"
	"strings"
	"sync"
	"testing"
	"unicode/utf8"

	"golang.org/x/net/webdav"

	"github.com/adrianliechti/donkey/internal/ai"
	"github.com/adrianliechti/donkey/pkg/runtime"
)

// davFake is a WebDAV service that knows its users by their IdP token, as a
// donkey add-on does: it takes "entra-u1" and nothing else, and remembers
// every request it saw.
type davFake struct {
	fs       webdav.FileSystem
	mu       sync.Mutex
	requests []davRequest
}

type davRequest struct {
	method, path   string
	header         http.Header
	contentLength  int64
	transferChunks bool
}

func newDAVFake(t *testing.T) (*davFake, *httptest.Server) {
	t.Helper()
	f := &davFake{fs: webdav.NewMemFS()}
	handler := &webdav.Handler{Prefix: "/remote/dav", FileSystem: f.fs, LockSystem: webdav.NewMemLS()}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		f.mu.Lock()
		f.requests = append(f.requests, davRequest{
			method: r.Method, path: r.URL.Path, header: r.Header.Clone(),
			contentLength: r.ContentLength, transferChunks: slices.Contains(r.TransferEncoding, "chunked"),
		})
		f.mu.Unlock()
		if r.Header.Get("Authorization") != "Bearer entra-u1" {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		handler.ServeHTTP(w, r)
	}))
	t.Cleanup(srv.Close)
	return f, srv
}

// write puts a file in the tree, making the folders above it.
func (f *davFake) write(t *testing.T, name, content string) {
	t.Helper()
	ctx := context.Background()
	f.mkdir(t, path.Dir(name))
	file, err := f.fs.OpenFile(ctx, name, os.O_RDWR|os.O_CREATE|os.O_TRUNC, 0o644)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := file.Write([]byte(content)); err != nil {
		t.Fatal(err)
	}
	if err := file.Close(); err != nil {
		t.Fatal(err)
	}
}

func (f *davFake) mkdir(t *testing.T, dir string) {
	t.Helper()
	ctx := context.Background()
	current := "/"
	for part := range strings.SplitSeq(strings.Trim(dir, "/"), "/") {
		if part == "" {
			continue
		}
		current = path.Join(current, part)
		if _, err := f.fs.Stat(ctx, current); err == nil {
			continue
		}
		if err := f.fs.Mkdir(ctx, current, 0o755); err != nil {
			t.Fatal(err)
		}
	}
}

func (f *davFake) read(t *testing.T, name string) (string, bool) {
	t.Helper()
	file, err := f.fs.OpenFile(context.Background(), name, os.O_RDONLY, 0)
	if err != nil {
		return "", false
	}
	defer file.Close()
	data, err := io.ReadAll(file)
	if err != nil {
		t.Fatal(err)
	}
	return string(data), true
}

func (f *davFake) exists(name string) bool {
	_, err := f.fs.Stat(context.Background(), name)
	return err == nil
}

func (f *davFake) seen(method string) []davRequest {
	f.mu.Lock()
	defer f.mu.Unlock()
	var out []davRequest
	for _, r := range f.requests {
		if method == "" || r.method == method {
			out = append(out, r)
		}
	}
	return out
}

// webdavTestServer has auth.scope configured and a service named "store"
// speaking WebDAV at an address with a path and no trailing slash.
func webdavTestServer(t *testing.T) (*Server, *davFake) {
	t.Helper()
	fake, upstream := newDAVFake(t)
	s := planServer(consoleRuntime{connection: runtime.ServiceConnection{Type: "sharepoint", Protocol: "webdav", Binding: map[string]string{
		"WEBDAV_URL": upstream.URL + "/remote/dav",
	}}}, &ai.Analysis{})
	s.cfg.OIDCScope = "api://donkey/.default"
	return s, fake
}

// davCall is a console request by user u1, whose IdP token is "entra-u1",
// with a session cookie that must never reach the service.
func davCall(t *testing.T, s *Server, method, suffix string, body any, status int) *httptest.ResponseRecorder {
	t.Helper()
	return davCallAs(t, s, "entra-u1", method, suffix, body, status)
}

func davCallAs(t *testing.T, s *Server, token, method, suffix string, body any, status int) *httptest.ResponseRecorder {
	t.Helper()
	var reader io.Reader
	contentType := ""
	switch b := body.(type) {
	case nil:
	case *bytes.Buffer:
		reader, contentType = b, "multipart/form-data; boundary="+davBoundary
	default:
		data, err := json.Marshal(body)
		if err != nil {
			t.Fatal(err)
		}
		reader = bytes.NewReader(data)
	}
	req := httptest.NewRequest(method, "/v1/spaces/default/services/store/buckets"+suffix, reader)
	if contentType != "" {
		req.Header.Set("Content-Type", contentType)
	}
	req.Header.Set("X-Access-Token", trinoIdentityToken(t, "u1", token))
	req.Header.Set("Cookie", "session=private")
	rec := httptest.NewRecorder()
	s.routes().ServeHTTP(rec, req)
	if rec.Code != status {
		t.Fatalf("%s %s: %d, want %d: %s", method, suffix, rec.Code, status, rec.Body.String())
	}
	return rec
}

const davBoundary = "donkey-test-boundary"

func davUpload(t *testing.T, name, content string) *bytes.Buffer {
	t.Helper()
	var buf bytes.Buffer
	mw := multipart.NewWriter(&buf)
	if err := mw.SetBoundary(davBoundary); err != nil {
		t.Fatal(err)
	}
	part, err := mw.CreateFormFile("file", name)
	if err != nil {
		t.Fatal(err)
	}
	_, _ = part.Write([]byte(content))
	_ = mw.Close()
	return &buf
}

func davList(t *testing.T, s *Server, query url.Values) s3Listing {
	t.Helper()
	var page s3Listing
	if err := json.Unmarshal(davCall(t, s, "GET", "/store/objects?"+query.Encode(), nil, 200).Body.Bytes(), &page); err != nil {
		t.Fatal(err)
	}
	return page
}

func TestWebDAVDisplayNamesAreLabelsForOriginalKeys(t *testing.T) {
	const root = `<D:multistatus xmlns:D="DAV:" xmlns:other="urn:other">
<D:response><D:href>/remote/dav/</D:href><D:propstat><D:prop><D:resourcetype><D:collection/></D:resourcetype><D:displayname>Shared files</D:displayname></D:prop><D:status>HTTP/1.1 200 OK</D:status></D:propstat></D:response>
<D:response><D:href>/remote/dav/dir-1/</D:href><D:propstat><D:prop><D:resourcetype><D:collection/></D:resourcetype><D:displayname>Reports</D:displayname></D:prop><D:status>HTTP/1.1 200 OK</D:status></D:propstat></D:response>
<D:response><D:href>/remote/dav/dir-2/</D:href><D:propstat><D:prop><D:resourcetype><D:collection/></D:resourcetype></D:prop><D:status>HTTP/1.1 200 OK</D:status></D:propstat><D:propstat><D:prop><D:displayname>Reports</D:displayname></D:prop><D:status>HTTP/1.1 200 OK</D:status></D:propstat><D:propstat><D:prop><D:displayname>Wrong name</D:displayname></D:prop><D:status>HTTP/1.1 404 Not Found</D:status></D:propstat></D:response>
<D:response><D:href>/remote/dav/file-1</D:href><D:propstat><D:prop><D:displayname>Résumé &lt;img src=x onerror=alert(1)&gt; &amp; notes</D:displayname><D:getcontentlength>3</D:getcontentlength></D:prop><D:status>HTTP/1.1 200 OK</D:status></D:propstat></D:response>
<D:response><D:href>/remote/dav/file-2</D:href><D:propstat><D:prop><D:displayname>Reports</D:displayname></D:prop><D:status>HTTP/2 200</D:status></D:propstat></D:response>
<D:response><D:href>/remote/dav/fallback%20%C3%BC.txt</D:href><D:propstat><D:prop><D:getcontentlength>1</D:getcontentlength></D:prop><D:status>HTTP/1.1 200 OK</D:status></D:propstat><D:propstat><D:prop><D:displayname>Unavailable</D:displayname></D:prop><D:status>HTTP/1.1 404 Not Found</D:status></D:propstat></D:response>
<D:response><D:href>/remote/dav/100%2520.txt</D:href><D:propstat><D:prop><D:displayname/></D:prop><D:status>HTTP/1.1 200 OK</D:status></D:propstat></D:response>
<D:response><D:href>/remote/dav/blank.txt</D:href><D:propstat><D:prop><D:displayname>  </D:displayname></D:prop><D:status>HTTP/1.1 200 OK</D:status></D:propstat></D:response>
<D:response><D:href>/remote/dav/no-status.txt</D:href><D:propstat><D:prop><D:displayname>No status</D:displayname></D:prop></D:propstat></D:response>
<D:response><D:href>/remote/dav/non-200.txt</D:href><D:propstat><D:prop><D:displayname>Created</D:displayname></D:prop><D:status>HTTP/1.1 201 Created</D:status></D:propstat></D:response>
<D:response><D:href>/remote/dav/wrong-namespace.txt</D:href><D:propstat><D:prop><other:displayname>Other namespace</other:displayname></D:prop><D:status>HTTP/1.1 200 OK</D:status></D:propstat></D:response>
</D:multistatus>`
	const folder = `<D:multistatus xmlns:D="DAV:"><D:response><D:href>/remote/dav/dir-1/</D:href><D:propstat><D:prop><D:resourcetype><D:collection/></D:resourcetype><D:displayname>Reports</D:displayname></D:prop><D:status>HTTP/1.1 200 OK</D:status></D:propstat></D:response></D:multistatus>`
	var mu sync.Mutex
	var operations []string
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != "PROPFIND" {
			mu.Lock()
			operations = append(operations, r.Method+" "+r.URL.EscapedPath())
			mu.Unlock()
			if r.Method == "GET" {
				_, _ = io.WriteString(w, "abc")
			} else {
				w.WriteHeader(http.StatusNoContent)
			}
			return
		}
		var request struct {
			Prop struct {
				DisplayName *struct{} `xml:"DAV: displayname"`
			} `xml:"DAV: prop"`
		}
		if err := xml.NewDecoder(r.Body).Decode(&request); err != nil || request.Prop.DisplayName == nil {
			t.Errorf("PROPFIND must request DAV:displayname: %+v, %v", request, err)
		}
		w.WriteHeader(http.StatusMultiStatus)
		if r.URL.Path == "/remote/dav/dir-1/" {
			_, _ = io.WriteString(w, folder)
		} else {
			_, _ = io.WriteString(w, root)
		}
	}))
	defer upstream.Close()
	s := planServer(consoleRuntime{connection: runtime.ServiceConnection{Type: "sharepoint", Protocol: "webdav", Binding: map[string]string{
		"WEBDAV_URL": upstream.URL + "/remote/dav",
	}}}, &ai.Analysis{})
	s.cfg.OIDCScope = "api://donkey/.default"
	list := func(query url.Values) (s3Listing, map[string]string) {
		t.Helper()
		var page struct {
			s3Listing
			DisplayNames map[string]string `json:"displayNames"`
		}
		if err := json.Unmarshal(davCall(t, s, "GET", "/store/objects?"+query.Encode(), nil, 200).Body.Bytes(), &page); err != nil {
			t.Fatal(err)
		}
		return page.s3Listing, page.DisplayNames
	}
	page, names := list(nil)
	wantNames := map[string]string{
		"": "Shared files", "dir-1/": "Reports", "dir-2/": "Reports", "file-2": "Reports",
		"file-1": "Résumé <img src=x onerror=alert(1)> & notes",
	}
	if !reflect.DeepEqual(names, wantNames) {
		t.Fatalf("display names: %v, want %v", names, wantNames)
	}
	if !slices.Equal(page.Prefixes, []string{"dir-1/", "dir-2/"}) || len(page.Objects) != 8 {
		t.Fatalf("duplicate names lost entries: %+v", page)
	}
	if !slices.ContainsFunc(page.Objects, func(o listedObject) bool { return o.Key == "fallback ü.txt" }) ||
		!slices.ContainsFunc(page.Objects, func(o listedObject) bool { return o.Key == "100%20.txt" }) {
		t.Fatalf("hrefs must be decoded exactly once: %+v", page.Objects)
	}
	var keys []string
	for token := ""; ; {
		page, names := list(url.Values{"max": {"2"}, "token": {token}})
		pageKeys := append([]string{}, page.Prefixes...)
		for _, o := range page.Objects {
			pageKeys = append(pageKeys, o.Key)
		}
		keys = append(keys, pageKeys...)
		for key := range names {
			if key != "" && !slices.Contains(pageKeys, key) {
				t.Fatalf("name for an entry outside the page: %q", key)
			}
		}
		if page.Next == "" {
			break
		}
		if page.Next <= token {
			t.Fatal("pagination must advance by key")
		}
		token = page.Next
	}
	if len(keys) != 10 || len(slices.Compact(keys)) != 10 {
		t.Fatalf("pagination lost entries with duplicate display names: %v", keys)
	}
	page, names = list(url.Values{"prefix": {"dir-1/"}})
	if names["dir-1/"] != "Reports" || len(page.Objects)+len(page.Prefixes) != 0 {
		t.Fatalf("an empty folder must describe itself for breadcrumbs: %+v, %v", page, names)
	}
	_, names = list(url.Values{"recursive": {"true"}})
	if names["file-1"] != wantNames["file-1"] || names["file-2"] != "Reports" {
		t.Fatalf("recursive listing lost file display names: %v", names)
	}
	davCall(t, s, "GET", "/store/objects/download?key=file-1", nil, 200)
	davCall(t, s, "POST", "/store/objects/delete", map[string]any{"keys": []string{"file-1"}, "prefixes": []string{"dir-2/"}}, 200)
	mu.Lock()
	defer mu.Unlock()
	if want := []string{"GET /remote/dav/file-1", "DELETE /remote/dav/file-1", "DELETE /remote/dav/dir-2/"}; !slices.Equal(operations, want) {
		t.Fatalf("operations must use keys: %v, want %v", operations, want)
	}
}

func TestWebDAVBrowserIsOneTreeNamedAfterTheService(t *testing.T) {
	s, fake := webdavTestServer(t)
	body := davCall(t, s, "GET", "", nil, 200).Body.String()
	if body != `{"buckets":[{"name":"store"}]}`+"\n" {
		t.Fatalf("containers: %s", body)
	}
	// The one root is asked about, so a token the service will not take
	// shows up here, not as an empty tree.
	if len(fake.seen("PROPFIND")) != 1 || fake.seen("PROPFIND")[0].header.Get("Depth") != "0" {
		t.Fatalf("expected one PROPFIND of depth 0, saw %+v", fake.seen(""))
	}
	for _, call := range []struct {
		method, suffix string
		body           any
	}{
		{"POST", "", map[string]string{"name": "other"}},
		{"DELETE", "/store", nil},
		{"GET", "/other/objects", nil},
	} {
		rec := davCallAs(t, s, "entra-u1", call.method, call.suffix, call.body, 400)
		if strings.Contains(rec.Body.String(), "bucket") {
			t.Errorf("%s %s spoke S3 to a WebDAV tree: %s", call.method, call.suffix, rec.Body.String())
		}
	}
}

func TestWebDAVBrowserListsFoldersPageByPage(t *testing.T) {
	s, fake := webdavTestServer(t)
	for _, name := range []string{"/a.txt", "/b/1.txt", "/b/2.txt", "/c/d/x", "/e f.txt", "/100%/x.txt", "/ü/n.txt", "/zz"} {
		fake.write(t, name, "x")
	}
	fake.mkdir(t, "/g")
	// Two entries a page, folders and files alike, in key order, until the
	// token runs out — every entry exactly once.
	var seen []string
	token := ""
	for pages := 0; ; pages++ {
		if pages > 10 {
			t.Fatal("the listing never ended")
		}
		page := davList(t, s, url.Values{"max": {"2"}, "token": {token}})
		if len(page.Prefixes)+len(page.Objects) > 2 {
			t.Fatalf("page too long: %+v", page)
		}
		seen = append(seen, page.Prefixes...)
		for _, obj := range page.Objects {
			seen = append(seen, obj.Key)
		}
		if page.Next == "" {
			if page.Truncated {
				t.Fatal("truncated without a token")
			}
			break
		}
		token = page.Next
	}
	slices.Sort(seen)
	if want := []string{"100%/", "a.txt", "b/", "c/", "e f.txt", "g/", "zz", "ü/"}; !slices.Equal(seen, want) {
		t.Fatalf("listed %v, want %v", seen, want)
	}
	// Folders are collections: a folder's own entry is not listed inside it,
	// and names needing escaping come back as they were given.
	if page := davList(t, s, url.Values{"prefix": {"c/"}}); !slices.Equal(page.Prefixes, []string{"c/d/"}) || len(page.Objects) != 0 {
		t.Fatalf("inside c/: %+v", page)
	}
	for _, folder := range []string{"100%/", "ü/", "b/"} {
		page := davList(t, s, url.Values{"prefix": {folder}})
		if len(page.Objects) == 0 || !strings.HasPrefix(page.Objects[0].Key, folder) || page.Objects[0].Size != 1 || page.Objects[0].Modified == "" {
			t.Fatalf("inside %s: %+v", folder, page)
		}
	}
	page := davList(t, s, url.Values{"prefix": {"b/"}, "recursive": {"true"}})
	if len(page.Objects) != 2 || page.Objects[0].Key != "b/1.txt" || page.Objects[1].Key != "b/2.txt" || len(page.Prefixes) != 0 {
		t.Fatalf("recursive: %+v", page)
	}
	if page := davList(t, s, url.Values{"recursive": {"true"}}); len(page.Objects) != 8 || len(page.Prefixes) != 0 {
		t.Fatalf("recursive root: %+v", page)
	}
	for _, bad := range []string{"../", "/abs/", "a//b/", "no-slash", "./"} {
		davCall(t, s, "GET", "/store/objects?prefix="+url.QueryEscape(bad), nil, 400)
	}
	davCall(t, s, "GET", "/store/objects?prefix=missing/", nil, 404)
}

func TestWebDAVBrowserPreviewsDownloadsAndUploads(t *testing.T) {
	s, fake := webdavTestServer(t)
	fake.write(t, "/docs/notes.txt", "# Hello\n")
	fake.write(t, "/big.txt", "x"+strings.Repeat("é", filePreviewBytes))
	fake.write(t, "/blob.bin", "ab\x00cd")
	fake.write(t, "/photo.png", "\x89PNG")
	type info struct {
		Key, ContentType, Modified string
		Size                       int64
		Text                       *string
		Truncated                  bool
	}
	read := func(key string, status int) info {
		var out info
		_ = json.Unmarshal(davCall(t, s, "GET", "/store/objects/info?key="+url.QueryEscape(key), nil, status).Body.Bytes(), &out)
		return out
	}
	notes := read("docs/notes.txt", 200)
	if notes.Text == nil || *notes.Text != "# Hello\n" || notes.Truncated || !strings.HasPrefix(notes.ContentType, "text/plain") || notes.Size != 8 || notes.Modified == "" {
		t.Fatalf("notes: %+v", notes)
	}
	// Cut at the preview size, mid-rune: the partial rune goes, the rest is text.
	big := read("big.txt", 200)
	if big.Text == nil || !big.Truncated || !utf8.ValidString(*big.Text) || len(*big.Text) != filePreviewBytes-1 {
		t.Fatalf("big: truncated %v, %d bytes", big.Truncated, len(*big.Text))
	}
	if gets := fake.seen("GET"); len(gets) != 2 || gets[1].header.Get("Range") != fmt.Sprintf("bytes=0-%d", filePreviewBytes-1) {
		t.Fatalf("the preview did not ask for the start of the file only: %+v", gets)
	}
	if blob := read("blob.bin", 200); blob.Text != nil {
		t.Fatal("binary data previewed as text")
	}
	if photo := read("photo.png", 200); photo.Text != nil || len(fake.seen("GET")) != 3 {
		t.Fatal("an image was read for a text preview")
	}
	read("missing.txt", 404)
	read("docs", 404)
	davCall(t, s, "GET", "/store/objects/info", nil, 400)

	rec := davCall(t, s, "GET", "/store/objects/download?key=docs/notes.txt", nil, 200)
	if rec.Body.String() != "# Hello\n" || rec.Header().Get("Content-Length") != "8" || !strings.Contains(rec.Header().Get("Content-Disposition"), "notes.txt") {
		t.Fatalf("download: %q %v", rec.Body.String(), rec.Header())
	}
	davCall(t, s, "GET", "/store/objects/download?key=missing.txt", nil, 404)

	// The console says the size, so the PUT carries a Content-Length; a
	// client that does not gets a chunked upload.
	var out struct {
		Key  string
		Size int64
	}
	_ = json.Unmarshal(davCall(t, s, "POST", "/store/objects?key=docs/up.txt&size=5", davUpload(t, "up.txt", "hello"), 200).Body.Bytes(), &out)
	if out.Key != "docs/up.txt" || out.Size != 5 {
		t.Fatalf("upload: %+v", out)
	}
	if got, ok := fake.read(t, "/docs/up.txt"); !ok || got != "hello" {
		t.Fatalf("uploaded %q %v", got, ok)
	}
	_ = json.Unmarshal(davCall(t, s, "POST", "/store/objects", davUpload(t, "e f.txt", "spaced"), 200).Body.Bytes(), &out)
	if out.Key != "e f.txt" || out.Size != 6 {
		t.Fatalf("upload without size: %+v", out)
	}
	if got, ok := fake.read(t, "/e f.txt"); !ok || got != "spaced" {
		t.Fatalf("uploaded %q %v", got, ok)
	}
	puts := fake.seen("PUT")
	if len(puts) != 2 || puts[0].contentLength != 5 || puts[0].transferChunks || puts[1].contentLength != -1 || !puts[1].transferChunks || puts[1].path != "/remote/dav/e f.txt" {
		t.Fatalf("PUTs: %+v", puts)
	}
	davCall(t, s, "POST", "/store/objects?key=../up.txt", davUpload(t, "up.txt", "x"), 400)
	davCall(t, s, "POST", "/store/objects?key=docs/up.txt&size=many", davUpload(t, "up.txt", "x"), 400)

	// The service saw this user's token and nothing of the console's session.
	for _, r := range fake.seen("") {
		if r.header.Get("Authorization") != "Bearer entra-u1" {
			t.Fatalf("%s %s without the user's token: %v", r.method, r.path, r.header)
		}
		for _, h := range []string{"Cookie", "X-Access-Token", "X-Auth-Request-User"} {
			if r.header.Get(h) != "" {
				t.Fatalf("%s %s leaked %s", r.method, r.path, h)
			}
		}
	}
}

func TestWebDAVBrowserDeletesAndMakesFolders(t *testing.T) {
	s, fake := webdavTestServer(t)
	for _, name := range []string{"/keep.txt", "/x.txt", "/dir/a", "/dir/sub/b", "/dirx/c"} {
		fake.write(t, name, "x")
	}
	var out struct{ Deleted int }
	_ = json.Unmarshal(davCall(t, s, "POST", "/store/objects/delete", map[string]any{"keys": []string{"x.txt"}, "prefixes": []string{"dir/"}}, 200).Body.Bytes(), &out)
	// A folder goes whole, in one DELETE: two entries went.
	if out.Deleted != 2 || fake.exists("/x.txt") || fake.exists("/dir") || fake.exists("/dir/sub/b") || !fake.exists("/dirx/c") || !fake.exists("/keep.txt") {
		t.Fatalf("deleted %d; left x.txt=%v dir=%v dirx/c=%v", out.Deleted, fake.exists("/x.txt"), fake.exists("/dir"), fake.exists("/dirx/c"))
	}
	for _, bad := range []map[string]any{
		{},
		{"prefixes": []string{""}},
		{"prefixes": []string{"/"}},
		{"prefixes": []string{"no-slash"}},
		{"prefixes": []string{"../"}},
		{"keys": []string{"a/"}},
		{"keys": []string{"a/../b"}},
	} {
		davCall(t, s, "POST", "/store/objects/delete", bad, 400)
	}
	// What is already gone is gone.
	davCall(t, s, "DELETE", "/store/objects?key=keep.txt", nil, 200)
	davCall(t, s, "DELETE", "/store/objects?key=keep.txt", nil, 200)
	if fake.exists("/keep.txt") {
		t.Fatal("keep.txt survived")
	}

	davCall(t, s, "POST", "/store/folders", map[string]string{"key": "new/"}, 200)
	davCall(t, s, "POST", "/store/folders", map[string]string{"key": "new/sub/"}, 200)
	if !fake.exists("/new/sub") {
		t.Fatal("MKCOL did not make the folder")
	}
	if page := davList(t, s, url.Values{"prefix": {"new/"}}); !slices.Equal(page.Prefixes, []string{"new/sub/"}) {
		t.Fatalf("new folder not listed: %+v", page)
	}
	davCall(t, s, "POST", "/store/folders", map[string]string{"key": "new/"}, 409)
	davCall(t, s, "POST", "/store/folders", map[string]string{"key": "missing/parent/"}, 409)
	for _, bad := range []string{"no-slash", "/abs/", "a//b/", "../up/", "a/./b/", "/"} {
		davCall(t, s, "POST", "/store/folders", map[string]string{"key": bad}, 400)
	}
}

func TestWebDAVBrowserNeedsTheUsersToken(t *testing.T) {
	s, fake := webdavTestServer(t)
	// A token the service will not take is the service's refusal, never the
	// console's own 401, which would end the session.
	rec := davCallAs(t, s, "entra-other", "GET", "", nil, 403)
	if !strings.Contains(rec.Body.String(), "identity token") {
		t.Fatalf("refusal: %s", rec.Body.String())
	}
	// A sign-in without the IdP token, or an installation without auth.scope,
	// says so before anything reaches the service.
	rec = davCallAs(t, s, "", "GET", "", nil, 400)
	if !strings.Contains(rec.Body.String(), "identity provider token") {
		t.Fatalf("no token: %s", rec.Body.String())
	}
	s.cfg.OIDCScope = ""
	rec = davCall(t, s, "GET", "", nil, 400)
	if !strings.Contains(rec.Body.String(), "auth.scope") {
		t.Fatalf("no scope: %s", rec.Body.String())
	}
	if n := len(fake.seen("")); n != 1 {
		t.Fatalf("the service was reached %d times, want once (the refusal)", n)
	}
	// The admin token alone opens no WebDAV tree: the service needs a user.
	req := httptest.NewRequest("GET", "/v1/spaces/default/services/store/buckets", nil)
	asAdmin(t, req)
	rec = httptest.NewRecorder()
	s.routes().ServeHTTP(rec, req)
	if rec.Code != 400 {
		t.Fatalf("admin token: %d %s", rec.Code, rec.Body.String())
	}
}

// The tunnel carries a WebDAV client's requests as written — names with
// spaces and percent signs included — and points MOVE and COPY destinations,
// which the client names by the tunnel's own address, at the service.
func TestWebDAVTunnelCarriesDestinationsAndEscapes(t *testing.T) {
	type seen struct{ method, uri, destination, overwrite, auth string }
	var calls []seen
	var mu sync.Mutex
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		calls = append(calls, seen{r.Method, r.URL.RequestURI(), r.Header.Get("Destination"), r.Header.Get("Overwrite"), r.Header.Get("Authorization")})
		mu.Unlock()
		w.Header().Set("Location", "http://"+r.Host+"/remote/dav/moved/")
		w.WriteHeader(http.StatusCreated)
	}))
	defer upstream.Close()
	s := planServer(consoleRuntime{connection: runtime.ServiceConnection{Type: "sharepoint", Protocol: "webdav", Binding: map[string]string{
		"WEBDAV_URL": upstream.URL + "/remote/dav",
	}}}, &ai.Analysis{})
	s.cfg.OIDCScope = "api://donkey/.default"
	send := func(method, path string, header http.Header, status int) *httptest.ResponseRecorder {
		t.Helper()
		req := httptest.NewRequest(method, "/v1/spaces/default/services/store/tunnel/"+path, nil)
		req.Header.Set("X-Access-Token", trinoIdentityToken(t, "u1", "entra-u1"))
		for k, v := range header {
			req.Header[k] = v
		}
		rec := httptest.NewRecorder()
		s.routes().ServeHTTP(rec, req)
		if rec.Code != status {
			t.Fatalf("%s %s: %d, want %d: %s", method, path, rec.Code, status, rec.Body.String())
		}
		return rec
	}
	send("MOVE", "a/e%20f.txt", http.Header{"Destination": {"http://127.0.0.1:8083/b/100%25.txt"}, "Overwrite": {"F"}}, 201)
	send("COPY", "folder/", http.Header{"Destination": {"/copies/folder/"}}, 201)
	send("MKCOL", "100%25/", nil, 201)
	send("PROPFIND", "", http.Header{"Depth": {"1"}}, 201)
	for _, bad := range []string{"http://127.0.0.1:8083/../../etc/passwd", "/..%2F..%2Fetc", "%zz"} {
		send("MOVE", "a.txt", http.Header{"Destination": {bad}}, 400)
	}
	send("GET", "..%2F..%2Fsecret", nil, 400)
	want := []seen{
		{"MOVE", "/remote/dav/a/e%20f.txt", upstream.URL + "/remote/dav/b/100%25.txt", "F", "Bearer entra-u1"},
		{"COPY", "/remote/dav/folder/", upstream.URL + "/remote/dav/copies/folder/", "", "Bearer entra-u1"},
		{"MKCOL", "/remote/dav/100%25/", "", "", "Bearer entra-u1"},
		{"PROPFIND", "/remote/dav/", "", "", "Bearer entra-u1"},
	}
	mu.Lock()
	defer mu.Unlock()
	if !slices.Equal(calls, want) {
		t.Fatalf("the service saw\n%+v\nwant\n%+v", calls, want)
	}
}
