package server

import (
	"cmp"
	"context"
	"encoding/xml"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/adrianliechti/donkey/internal/addons"
	"github.com/adrianliechti/donkey/pkg/runtime"
)

// davClient is a WebDAV service — a file tree, address books, calendars —
// as the signed-in user reaches it, the way the Trino viewer does: every
// request carries the caller's IdP access token as Bearer, and nothing of
// donkey's. A client exists for one request only; the token never enters a
// binding or a pool. Decoded keys identify resources in the current mount.
// Collection keys carry a trailing slash.
type davClient struct {
	base      *url.URL // request URL of the binding or mounted collection
	name      string   // the service, for messages
	token     string
	published *url.URL // canonical root advertised by a reverse-proxied DAV service
	keyPrefix string   // public key of a discovered collection mounted outside the binding path
}

// Headers arrive within this long or the request fails; bodies — downloads,
// uploads — take as long as they take, bounded by the request's context.
var davTransport = func() *http.Transport {
	t := http.DefaultTransport.(*http.Transport).Clone()
	t.ResponseHeaderTimeout = 30 * time.Second
	return t
}()

// Never follow a redirect carrying the user's credential elsewhere.
var davHTTPClient = &http.Client{
	Transport:     davTransport,
	CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
}

// A multistatus is read up to this size: a big address book's cards, or a
// calendar's events, arrive in one.
const davMultistatusBytes = 64 << 20

// serveDAV answers a console call on a DAV service: it opens the service
// the request names, which speaks protocol, as the caller — their IdP
// token, never donkey's own — and writes what call makes of it, or the
// error as the file browser would.
func (s *Server) serveDAV(w http.ResponseWriter, r *http.Request, kind davDiscovery, key string, call func(context.Context, *davClient) (any, error)) {
	ctx := r.Context()
	dav, err := s.openDAV(ctx, r, kind.protocol)
	if err != nil {
		writeErr(w, fileStatus(err), err)
		return
	}
	if strings.HasPrefix(key, "/") {
		if err := dav.mountDiscovered(ctx, kind, key); err != nil {
			writeErr(w, fileStatus(err), err)
			return
		}
	}
	out, err := call(ctx, dav)
	if err != nil {
		writeErr(w, fileStatus(err), err)
		return
	}
	writeJSON(w, http.StatusOK, out)
}

func (s *Server) openDAV(ctx context.Context, r *http.Request, protocol string) (*davClient, error) {
	connection, err := s.serviceConnection(ctx, r, protocol)
	if err != nil {
		return nil, err
	}
	token, err := s.userToken(ctx)
	if err != nil {
		return nil, err
	}
	return newDAVClient(connection.Binding[addons.ConnectionKey(protocol)], r.PathValue("service"), token)
}

func newDAVClient(rawURL, service, token string) (*davClient, error) {
	base, err := url.Parse(rawURL) // validated by serviceConnection
	if err != nil {
		return nil, runtime.UserError{Err: fmt.Errorf("%s has no usable address", service)}
	}
	if !strings.HasSuffix(base.Path, "/") {
		base.Path += "/"
		base.RawPath = ""
	}
	base.RawQuery, base.Fragment = "", ""
	return &davClient{base: base, name: service, token: token}, nil
}

// url is where a key lives: each segment escaped on its own, a collection
// with its trailing slash, so the service never has to redirect.
func (c *davClient) url(key string) *url.URL {
	trimmed := strings.TrimSuffix(strings.TrimPrefix(key, c.keyPrefix), "/")
	if trimmed == "" {
		return c.base
	}
	var escaped []string
	for part := range strings.SplitSeq(trimmed, "/") {
		escaped = append(escaped, url.PathEscape(part))
	}
	u := c.base.JoinPath(escaped...)
	if strings.HasSuffix(key, "/") {
		u.Path += "/"
		if u.RawPath != "" {
			u.RawPath += "/"
		}
	}
	return u
}

func (c *davClient) do(ctx context.Context, method string, u *url.URL, header http.Header, body io.Reader, size int64) (*http.Response, error) {
	req, err := http.NewRequestWithContext(ctx, method, u.String(), body)
	if err != nil {
		return nil, err
	}
	for name, values := range header {
		req.Header[name] = values
	}
	req.Header.Set("Authorization", "Bearer "+c.token)
	req.Header.Set("User-Agent", "donkey")
	if body != nil {
		req.ContentLength = size
	}
	res, err := davHTTPClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("%s did not answer: %w", c.name, err)
	}
	return res, nil
}

// fail turns the service's answer into the console's error and drains the
// response.
func (c *davClient) fail(res *http.Response, what string) error {
	defer res.Body.Close()
	_, _ = io.Copy(io.Discard, io.LimitReader(res.Body, 64<<10))
	switch res.StatusCode {
	case http.StatusUnauthorized, http.StatusForbidden:
		return &upstreamError{status: res.StatusCode, err: fmt.Errorf("%s did not accept your identity token (HTTP %d)", c.name, res.StatusCode)}
	case http.StatusNotFound:
		return &upstreamError{status: res.StatusCode, err: fmt.Errorf("%s: not found", what)}
	}
	return &upstreamError{status: res.StatusCode, err: fmt.Errorf("%s answered HTTP %d for %s", c.name, res.StatusCode, what)}
}

// relative places a response href under the service root. Hrefs come as
// absolute paths or full URLs, percent-encoded; resolved against the
// request and compared decoded, they all land in the same place. One
// outside the root is not the service's to hand out.
func (c *davClient) relative(reqURL *url.URL, href string) (string, bool) {
	ref, err := url.Parse(strings.TrimSpace(href))
	if err != nil {
		return "", false
	}
	path := reqURL.ResolveReference(ref).Path
	suffix, ok := davPathSuffix(c.base.Path, path)
	if !ok && c.keyPrefix != "" {
		suffix, ok = davPathSuffix(c.keyPrefix, path)
	}
	if !ok || suffix != "" && webdavSegments(strings.TrimSuffix(suffix, "/")) != nil {
		return "", false
	}
	return c.keyPrefix + suffix, true
}

// davPathSuffix returns a path relative to root, treating either spelling
// of the root itself (with or without a trailing slash) as the empty key.
func davPathSuffix(root, path string) (string, bool) {
	root = strings.TrimSuffix(root, "/")
	if path == root {
		return "", true
	}
	return strings.CutPrefix(path, root+"/")
}

// ─── Multistatus ───────────────────────────────────────────────────────

const (
	nsDAV     = "DAV:"
	nsCardDAV = "urn:ietf:params:xml:ns:carddav"
	nsCalDAV  = "urn:ietf:params:xml:ns:caldav"
	nsApple   = "http://apple.com/ns/ical/"
)

type davMultistatus struct {
	Responses []davResponse `xml:"DAV: response"`
}

type davResponse struct {
	Status   string        `xml:"DAV: status"`
	Href     string        `xml:"DAV: href"`
	Propstat []davPropstat `xml:"DAV: propstat"`
}

type davPropstat struct {
	Status string  `xml:"DAV: status"`
	Prop   davProp `xml:"DAV: prop"`
}

// davProp is every property donkey asks a DAV service about; a request
// names the ones it wants and the rest stay empty.
type davProp struct {
	DisplayName  string `xml:"DAV: displayname"`
	ResourceType struct {
		Collection     *struct{} `xml:"DAV: collection"`
		AddressBook    *struct{} `xml:"urn:ietf:params:xml:ns:carddav addressbook"`
		Calendar       *struct{} `xml:"urn:ietf:params:xml:ns:caldav calendar"`
		Principal      *struct{} `xml:"DAV: principal"`
		ScheduleInbox  *struct{} `xml:"urn:ietf:params:xml:ns:caldav schedule-inbox"`
		ScheduleOutbox *struct{} `xml:"urn:ietf:params:xml:ns:caldav schedule-outbox"`
	} `xml:"DAV: resourcetype"`
	ContentLength string `xml:"DAV: getcontentlength"`
	LastModified  string `xml:"DAV: getlastmodified"`
	ContentType   string `xml:"DAV: getcontenttype"`
	ETag          string `xml:"DAV: getetag"`

	// Where a user's address books and calendars are (RFC 6352, RFC 4791).
	CurrentUserPrincipal *davHrefs `xml:"DAV: current-user-principal"`
	AddressBookHomeSet   *davHrefs `xml:"urn:ietf:params:xml:ns:carddav addressbook-home-set"`
	CalendarHomeSet      *davHrefs `xml:"urn:ietf:params:xml:ns:caldav calendar-home-set"`

	AddressBookDescription string `xml:"urn:ietf:params:xml:ns:carddav addressbook-description"`
	CalendarDescription    string `xml:"urn:ietf:params:xml:ns:caldav calendar-description"`
	CalendarColor          string `xml:"http://apple.com/ns/ical/ calendar-color"`
	// Absent means the calendar takes every kind of component.
	CalendarComponents *struct {
		Comps []struct {
			Name string `xml:"name,attr"`
		} `xml:"urn:ietf:params:xml:ns:caldav comp"`
	} `xml:"urn:ietf:params:xml:ns:caldav supported-calendar-component-set"`

	// A report's answer: the card or the calendar object itself.
	AddressData  string `xml:"urn:ietf:params:xml:ns:carddav address-data"`
	CalendarData string `xml:"urn:ietf:params:xml:ns:caldav calendar-data"`
}

type davHrefs struct {
	Hrefs []string `xml:"DAV: href"`
}

// davItem is one resource of a multistatus, keyed relative to the service
// root, with the properties it actually has — a 404 propstat lists the ones
// it does not.
type davItem struct {
	Key      string // a collection's ends in "/"
	Props    []davProp
	location *url.URL // request URL for a collection with an absolute public key
}

func (i davItem) collection() bool {
	return i.has(func(p davProp) bool { return p.ResourceType.Collection != nil })
}

// has says whether any of the item's properties passes test.
func (i davItem) has(test func(davProp) bool) bool {
	return slices.ContainsFunc(i.Props, test)
}

// prop is the first non-empty value a getter finds among the item's
// properties.
func (i davItem) prop(get func(davProp) string) string {
	for _, p := range i.Props {
		if v := strings.TrimSpace(get(p)); v != "" {
			return v
		}
	}
	return ""
}

// multistatus sends a PROPFIND or a REPORT at key and reads the answer's
// resources. Hrefs outside the service root are dropped.
func (c *davClient) multistatus(ctx context.Context, method, key string, depth int, query string) ([]davItem, error) {
	u := c.url(key)
	responses, err := c.requestMultistatus(ctx, method, u, depth, query)
	if err != nil {
		return nil, err
	}
	items := make([]davItem, 0, len(responses))
	for _, response := range responses {
		if key, ok := c.relative(u, response.Href); ok {
			items = append(items, response.item(key))
		}
	}
	return items, nil
}

func (r davResponse) item(key string) davItem {
	item := davItem{Key: key}
	for _, ps := range r.Propstat {
		if davStatus(ps.Status) == http.StatusOK {
			item.Props = append(item.Props, ps.Prop)
		}
	}
	if item.collection() && item.Key != "" && !strings.HasSuffix(item.Key, "/") {
		item.Key += "/"
	}
	return item
}

func (c *davClient) requestMultistatus(ctx context.Context, method string, u *url.URL, depth int, query string) ([]davResponse, error) {
	header := http.Header{"Depth": {strconv.Itoa(depth)}, "Content-Type": {"application/xml; charset=utf-8"}}
	res, err := c.do(ctx, method, u, header, strings.NewReader(query), int64(len(query)))
	if err != nil {
		return nil, err
	}
	if res.StatusCode != http.StatusMultiStatus {
		return nil, c.fail(res, cmp.Or(u.EscapedPath(), c.name))
	}
	defer res.Body.Close()
	body := &io.LimitedReader{R: res.Body, N: davMultistatusBytes}
	var ms davMultistatus
	if err := xml.NewDecoder(body).Decode(&ms); err != nil {
		if body.N == 0 {
			return nil, runtime.UserError{Err: fmt.Errorf("%s answered %s with more than %d MB — too much to take in one go", c.name, method, davMultistatusBytes>>20)}
		}
		return nil, fmt.Errorf("%s answered %s with something other than a multistatus: %w", c.name, method, err)
	}
	return ms.Responses, nil
}

// remove is a DELETE, conditional on the ETag the caller saw — as the
// service wrote it — when there is one. A weak ETag never matches (RFC 9110
// compares If-Match strongly), so it cannot guard a delete. What is already
// gone is gone.
func (c *davClient) remove(ctx context.Context, key, etag string) error {
	header := http.Header{}
	if etag != "" && !strings.HasPrefix(etag, "W/") {
		header.Set("If-Match", etag)
	}
	res, err := c.do(ctx, http.MethodDelete, c.url(key), header, nil, 0)
	if err != nil {
		return err
	}
	switch res.StatusCode {
	case http.StatusOK, http.StatusNoContent, http.StatusNotFound:
		res.Body.Close()
		return nil
	case http.StatusPreconditionFailed:
		res.Body.Close()
		return &upstreamError{status: http.StatusConflict, err: fmt.Errorf("%s changed since it was opened — look again before deleting it", key)}
	}
	return c.fail(res, key)
}

// get reads one resource whole, up to limit bytes, and its ETag as the
// service wrote it.
func (c *davClient) get(ctx context.Context, key string, limit int64) ([]byte, string, error) {
	res, err := c.do(ctx, http.MethodGet, c.url(key), nil, nil, 0)
	if err != nil {
		return nil, "", err
	}
	if res.StatusCode != http.StatusOK {
		return nil, "", c.fail(res, key)
	}
	defer res.Body.Close()
	data, err := io.ReadAll(io.LimitReader(res.Body, limit+1))
	if err != nil {
		return nil, "", fmt.Errorf("%s: %w", key, err)
	}
	if int64(len(data)) > limit {
		return nil, "", runtime.UserError{Err: fmt.Errorf("%s is larger than %d MB — too large to show", key, limit>>20)}
	}
	return data, res.Header.Get("ETag"), nil
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

// name is what to call a collection: its display name, else the last
// segment of its key, else the service's name.
func (c *davClient) collectionName(i davItem) string {
	if name := i.prop(func(p davProp) string { return p.DisplayName }); name != "" {
		return name
	}
	if key := strings.TrimSuffix(i.Key, "/"); key != "" {
		return key[strings.LastIndex(key, "/")+1:]
	}
	return c.name
}

// checkDAVCollection checks a collection's key, "" being the service root.
func checkDAVCollection(key string) error {
	if key == "" || key == "/" {
		return nil
	}
	if !strings.HasSuffix(key, "/") || webdavSegments(strings.TrimSuffix(strings.TrimPrefix(key, "/"), "/")) != nil {
		return runtime.UserError{Err: fmt.Errorf("%q is not a collection", key)}
	}
	return nil
}

// checkDAVResource checks the key of a card or a calendar object.
func checkDAVResource(key string) error {
	if key == "" || strings.HasSuffix(key, "/") || webdavSegments(strings.TrimPrefix(key, "/")) != nil {
		return runtime.UserError{Err: fmt.Errorf("%q is not a resource", key)}
	}
	return nil
}
