package server

import (
	"bufio"
	"bytes"
	"encoding/json"
	"encoding/xml"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"slices"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"
	"unicode/utf8"

	"github.com/adrianliechti/donkey/internal/ai"
	"github.com/adrianliechti/donkey/pkg/runtime"
)

// s3Fake is just enough of an S3 store for the bucket browser: buckets,
// objects with metadata, ListObjectsV2 with delimiters and continuation
// tokens, ranged reads and batch deletes.
type s3Fake struct {
	mu      sync.Mutex
	buckets map[string]map[string]s3FakeObject
	gets    []string // object keys read with GET
}

type s3FakeObject struct {
	data        []byte
	contentType string
	meta        map[string]string
	modified    time.Time
}

func (f *s3Fake) put(bucket, key, contentType string, data []byte, meta map[string]string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.buckets[bucket] == nil {
		f.buckets[bucket] = map[string]s3FakeObject{}
	}
	f.buckets[bucket][key] = s3FakeObject{data: data, contentType: contentType, meta: meta, modified: time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC)}
}

func (f *s3Fake) keys(bucket string) []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	var keys []string
	for key := range f.buckets[bucket] {
		keys = append(keys, key)
	}
	slices.Sort(keys)
	return keys
}

func s3FakeError(w http.ResponseWriter, status int, code string) {
	w.Header().Set("Content-Type", "application/xml")
	w.WriteHeader(status)
	fmt.Fprintf(w, `<?xml version="1.0" encoding="UTF-8"?><Error><Code>%s</Code><Message>%s</Message><RequestId>1</RequestId></Error>`, code, code)
}

func s3FakeXML(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/xml")
	_ = xml.NewEncoder(w).Encode(v)
}

// unchunk decodes the aws-chunked body minio-go streams over plain HTTP.
func unchunk(r *http.Request) []byte {
	if !strings.HasPrefix(r.Header.Get("X-Amz-Content-Sha256"), "STREAMING-") {
		data, _ := io.ReadAll(r.Body)
		return data
	}
	var out []byte
	reader := bufio.NewReader(r.Body)
	for {
		line, err := reader.ReadString('\n')
		if err != nil {
			return out
		}
		size, _ := strconv.ParseInt(strings.SplitN(strings.TrimSpace(line), ";", 2)[0], 16, 64)
		if size == 0 {
			return out
		}
		chunk := make([]byte, size)
		_, _ = io.ReadFull(reader, chunk)
		out = append(out, chunk...)
		_, _ = reader.ReadString('\n')
	}
}

func (f *s3Fake) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	bucket, key, _ := strings.Cut(strings.TrimPrefix(r.URL.Path, "/"), "/")
	q := r.URL.Query()
	f.mu.Lock()
	objects, exists := f.buckets[bucket]
	f.mu.Unlock()
	if bucket != "" && !exists && !(key == "" && r.Method == http.MethodPut) {
		s3FakeError(w, http.StatusNotFound, "NoSuchBucket")
		return
	}
	switch {
	case bucket == "":
		type entry struct {
			Name         string
			CreationDate string
		}
		var list struct {
			XMLName xml.Name `xml:"ListAllMyBucketsResult"`
			Buckets []entry  `xml:"Buckets>Bucket"`
		}
		f.mu.Lock()
		for name := range f.buckets {
			list.Buckets = append(list.Buckets, entry{name, "2026-01-01T00:00:00Z"})
		}
		f.mu.Unlock()
		s3FakeXML(w, list)
	case key == "" && r.Method == http.MethodPut:
		f.mu.Lock()
		f.buckets[bucket] = map[string]s3FakeObject{}
		f.mu.Unlock()
	case key == "" && r.Method == http.MethodDelete:
		if len(objects) > 0 {
			s3FakeError(w, http.StatusConflict, "BucketNotEmpty")
			return
		}
		f.mu.Lock()
		delete(f.buckets, bucket)
		f.mu.Unlock()
		w.WriteHeader(http.StatusNoContent)
	case key == "" && r.Method == http.MethodPost && q.Has("delete"):
		var req struct {
			Objects []struct{ Key string } `xml:"Object"`
		}
		_ = xml.NewDecoder(r.Body).Decode(&req)
		f.mu.Lock()
		for _, obj := range req.Objects {
			delete(f.buckets[bucket], obj.Key)
		}
		f.mu.Unlock()
		s3FakeXML(w, struct {
			XMLName xml.Name `xml:"DeleteResult"`
		}{})
	case key == "" && r.Method == http.MethodGet:
		f.list(w, bucket, q)
	case r.Method == http.MethodPut:
		meta := map[string]string{}
		for name, values := range r.Header {
			if rest, ok := strings.CutPrefix(name, "X-Amz-Meta-"); ok {
				meta[rest] = values[0]
			}
		}
		f.put(bucket, key, r.Header.Get("Content-Type"), unchunk(r), meta)
		w.Header().Set("ETag", `"etag"`)
	case r.Method == http.MethodHead || r.Method == http.MethodGet:
		obj, ok := objects[key]
		if !ok {
			s3FakeError(w, http.StatusNotFound, "NoSuchKey")
			return
		}
		h := w.Header()
		h.Set("Content-Type", obj.contentType)
		h.Set("ETag", `"etag-`+key+`"`)
		h.Set("Last-Modified", obj.modified.Format(http.TimeFormat))
		for k, v := range obj.meta {
			h.Set("X-Amz-Meta-"+k, v)
		}
		data, status := obj.data, http.StatusOK
		if r.Method == http.MethodGet {
			f.mu.Lock()
			f.gets = append(f.gets, key)
			f.mu.Unlock()
			var start, end int
			if _, err := fmt.Sscanf(r.Header.Get("Range"), "bytes=%d-%d", &start, &end); err == nil {
				end = min(end, len(data)-1)
				h.Set("Content-Range", fmt.Sprintf("bytes %d-%d/%d", start, end, len(data)))
				data, status = data[start:end+1], http.StatusPartialContent
			}
		}
		h.Set("Content-Length", strconv.Itoa(len(data)))
		w.WriteHeader(status)
		if r.Method == http.MethodGet {
			_, _ = w.Write(data)
		}
	case r.Method == http.MethodDelete:
		f.mu.Lock()
		delete(f.buckets[bucket], key)
		f.mu.Unlock()
		w.WriteHeader(http.StatusNoContent)
	default:
		s3FakeError(w, http.StatusNotImplemented, "NotImplemented")
	}
}

// list is ListObjectsV2: keys folded at the delimiter, a page of max-keys
// entries, and a token naming the last entry handed out.
func (f *s3Fake) list(w http.ResponseWriter, bucket string, q url.Values) {
	prefix, delimiter, token := q.Get("prefix"), q.Get("delimiter"), q.Get("continuation-token")
	max, err := strconv.Atoi(q.Get("max-keys"))
	if err != nil || max < 1 {
		max = 1000
	}
	type content struct {
		Key          string
		LastModified string
		ETag         string
		Size         int
		StorageClass string
	}
	type common struct{ Prefix string }
	var page struct {
		XMLName               xml.Name `xml:"ListBucketResult"`
		Name                  string
		Prefix                string
		MaxKeys               int
		IsTruncated           bool
		NextContinuationToken string    `xml:",omitempty"`
		Contents              []content `xml:"Contents"`
		CommonPrefixes        []common  `xml:"CommonPrefixes"`
	}
	page.Name, page.Prefix, page.MaxKeys = bucket, prefix, max
	f.mu.Lock()
	objects := f.buckets[bucket]
	f.mu.Unlock()
	last, count := "", 0
	for _, key := range f.keys(bucket) {
		if !strings.HasPrefix(key, prefix) {
			continue
		}
		name, folded := key, false
		if i := strings.Index(key[len(prefix):], delimiter); delimiter != "" && i >= 0 {
			name, folded = key[:len(prefix)+i+len(delimiter)], true
		}
		if name <= token || name == last {
			continue
		}
		if count == max {
			page.IsTruncated, page.NextContinuationToken = true, last
			break
		}
		last, count = name, count+1
		if folded {
			page.CommonPrefixes = append(page.CommonPrefixes, common{name})
		} else {
			obj := objects[key]
			page.Contents = append(page.Contents, content{key, obj.modified.Format(time.RFC3339), `"etag"`, len(obj.data), "STANDARD"})
		}
	}
	s3FakeXML(w, page)
}

func s3TestServer(t *testing.T) (*Server, *s3Fake) {
	t.Helper()
	fake := &s3Fake{buckets: map[string]map[string]s3FakeObject{"data": {}}}
	upstream := httptest.NewServer(fake)
	t.Cleanup(upstream.Close)
	s := planServer(consoleRuntime{connection: runtime.ServiceConnection{Type: "s3", Protocol: "s3", Binding: map[string]string{
		"AWS_ENDPOINT_URL_S3": upstream.URL, "AWS_ACCESS_KEY_ID": "access", "AWS_SECRET_ACCESS_KEY": "secret", "AWS_REGION": "us-east-1",
	}}}, &ai.Analysis{})
	return s, fake
}

func s3Call(t *testing.T, s *Server, method, suffix string, body any, status int) []byte {
	t.Helper()
	var reader io.Reader
	if body != nil {
		data, err := json.Marshal(body)
		if err != nil {
			t.Fatal(err)
		}
		reader = bytes.NewReader(data)
	}
	req := httptest.NewRequest(method, "/v1/spaces/default/services/store/buckets"+suffix, reader)
	asAdmin(t, req)
	rec := httptest.NewRecorder()
	s.routes().ServeHTTP(rec, req)
	if rec.Code != status {
		t.Fatalf("%s %s: %d, want %d: %s", method, suffix, rec.Code, status, rec.Body.String())
	}
	return rec.Body.Bytes()
}

// listedObject is an object as the console API lists it.
type listedObject struct {
	Key, Modified, ETag, StorageClass string
	Size                              int64
}

type s3Listing struct {
	Prefixes  []string
	Objects   []listedObject
	Truncated bool
	Next      string
}

func s3List(t *testing.T, s *Server, query url.Values) s3Listing {
	t.Helper()
	var page s3Listing
	if err := json.Unmarshal(s3Call(t, s, "GET", "/data/objects?"+query.Encode(), nil, 200), &page); err != nil {
		t.Fatal(err)
	}
	return page
}

func TestS3BrowserListsFoldersPageByPage(t *testing.T) {
	s, fake := s3TestServer(t)
	for _, key := range []string{"a.txt", "b/1.txt", "b/2.txt", "c/", "c/d/x", "e f.txt", "g/", "zz"} {
		fake.put("data", key, "text/plain", []byte("x"), nil)
	}
	// Two entries a page, folders and objects alike, until the token runs
	// out — every entry exactly once.
	var seen []string
	token := ""
	for pages := 0; ; pages++ {
		if pages > 10 {
			t.Fatal("the listing never ended")
		}
		page := s3List(t, s, url.Values{"max": {"2"}, "token": {token}})
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
	if want := []string{"a.txt", "b/", "c/", "e f.txt", "g/", "zz"}; !slices.Equal(seen, want) {
		t.Fatalf("listed %v, want %v", seen, want)
	}
	// A folder's own marker is not listed inside it.
	page := s3List(t, s, url.Values{"prefix": {"c/"}})
	if !slices.Equal(page.Prefixes, []string{"c/d/"}) || len(page.Objects) != 0 {
		t.Fatalf("inside c/: %+v", page)
	}
	page = s3List(t, s, url.Values{"prefix": {"b/"}, "recursive": {"true"}})
	if len(page.Objects) != 2 || page.Objects[0].Key != "b/1.txt" || page.Objects[0].Modified != "2026-01-02T03:04:05Z" {
		t.Fatalf("recursive: %+v", page)
	}
	s3Call(t, s, "GET", "/ab/objects", nil, 400)
}

func TestS3BrowserPreviewsTextAndMetadata(t *testing.T) {
	s, fake := s3TestServer(t)
	fake.put("data", "docs/notes.md", "text/markdown", []byte("# Hello\n"), map[string]string{"Owner": "ada"})
	fake.put("data", "big.txt", "", []byte("x"+strings.Repeat("é", filePreviewBytes)), nil)
	fake.put("data", "blob.bin", "application/octet-stream", []byte("ab\x00cd"), nil)
	fake.put("data", "photo.png", "image/png", []byte("\x89PNG"), nil)
	type info struct {
		Key, ContentType, ETag, Modified string
		Size                             int64
		Metadata                         map[string]string
		Text                             *string
		Truncated                        bool
	}
	read := func(key string, status int) info {
		var out info
		_ = json.Unmarshal(s3Call(t, s, "GET", "/data/objects/info?key="+url.QueryEscape(key), nil, status), &out)
		return out
	}
	notes := read("docs/notes.md", 200)
	if notes.Text == nil || *notes.Text != "# Hello\n" || notes.Truncated || notes.ContentType != "text/markdown" || notes.Metadata["Owner"] != "ada" || notes.Size != 8 || notes.ETag != "etag-docs/notes.md" {
		t.Fatalf("notes: %+v", notes)
	}
	// Cut at the preview size, mid-rune: the partial rune goes, the rest is text.
	big := read("big.txt", 200)
	if big.Text == nil || !big.Truncated || !utf8.ValidString(*big.Text) || len(*big.Text) != filePreviewBytes-1 {
		t.Fatalf("big: truncated %v, %d bytes", big.Truncated, len(*big.Text))
	}
	if blob := read("blob.bin", 200); blob.Text != nil {
		t.Fatal("binary data previewed as text")
	}
	if photo := read("photo.png", 200); photo.Text != nil || slices.Contains(fake.gets, "photo.png") {
		t.Fatal("an image was read for a text preview")
	}
	read("missing.txt", 404)
	s3Call(t, s, "GET", "/data/objects/info", nil, 400)
}

func TestS3BrowserDeletesSelectionsAndFolders(t *testing.T) {
	s, fake := s3TestServer(t)
	for _, key := range []string{"keep.txt", "x.txt", "dir/", "dir/a", "dir/sub/b", "dirx/c"} {
		fake.put("data", key, "", []byte("x"), nil)
	}
	var out struct{ Deleted int }
	_ = json.Unmarshal(s3Call(t, s, "POST", "/data/objects/delete", map[string]any{"keys": []string{"x.txt"}, "prefixes": []string{"dir/"}}, 200), &out)
	if out.Deleted != 4 {
		t.Fatalf("deleted %d", out.Deleted)
	}
	if got := fake.keys("data"); !slices.Equal(got, []string{"dirx/c", "keep.txt"}) {
		t.Fatalf("left %v", got)
	}
	for _, bad := range []map[string]any{
		{},
		{"prefixes": []string{""}},
		{"prefixes": []string{"/"}},
		{"prefixes": []string{"no-slash"}},
		{"keys": []string{" "}},
	} {
		s3Call(t, s, "POST", "/data/objects/delete", bad, 400)
	}
	// A full bucket asks before it goes, then goes with everything in it.
	s3Call(t, s, "DELETE", "/data", nil, 409)
	s3Call(t, s, "DELETE", "/data?force=true", nil, 200)
	if _, ok := fake.buckets["data"]; ok {
		t.Fatal("bucket survived a forced delete")
	}
}

func TestS3BrowserCreatesFolderMarkers(t *testing.T) {
	s, fake := s3TestServer(t)
	s3Call(t, s, "POST", "/data/folders", map[string]string{"key": "new/sub/"}, 200)
	obj, ok := fake.buckets["data"]["new/sub/"]
	if !ok || len(obj.data) != 0 || obj.contentType != "application/x-directory" {
		t.Fatalf("marker: %v %+v", ok, obj)
	}
	if page := s3List(t, s, url.Values{"prefix": {"new/"}}); !slices.Equal(page.Prefixes, []string{"new/sub/"}) {
		t.Fatalf("new folder not listed: %+v", page)
	}
	for _, bad := range []string{"no-slash", "/abs/", "a//b/", "../up/", "a/./b/", "/"} {
		s3Call(t, s, "POST", "/data/folders", map[string]string{"key": bad}, 400)
	}
}
