package server

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"slices"
	"strings"
	"sync"
	"testing"

	"github.com/adrianliechti/donkey/internal/ai"
	"github.com/adrianliechti/donkey/pkg/runtime"
)

// pimFake is a CardDAV and CalDAV service under /dav, as an add-on serves
// one: it knows its user by the IdP token "entra-u1", names a principal
// whose homes hold the collections, and answers reports from what it holds.
type pimFake struct {
	mu           sync.Mutex
	requests     []pimRequest
	resources    map[string]pimResource // by path
	expandStatus int                    // optional HTTP error for a REPORT requesting expansion
}

type pimRequest struct {
	method, path, depth, ifMatch, auth, cookie, body string
}

type pimResource struct {
	etag, data string
}

const pimMultistatus = `<?xml version="1.0" encoding="utf-8"?><d:multistatus xmlns:d="DAV:" xmlns:card="urn:ietf:params:xml:ns:carddav" xmlns:cal="urn:ietf:params:xml:ns:caldav" xmlns:ic="http://apple.com/ns/ical/">`

func pimProps(href, props string) string {
	return `<d:response><d:href>` + href + `</d:href><d:propstat><d:prop>` + props + `</d:prop><d:status>HTTP/1.1 200 OK</d:status></d:propstat>` +
		`<d:propstat><d:prop><d:getcontentlength/></d:prop><d:status>HTTP/1.1 404 Not Found</d:status></d:propstat></d:response>`
}

func newPIMFake(t *testing.T) (*pimFake, *httptest.Server) {
	t.Helper()
	f := &pimFake{resources: map[string]pimResource{}}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		f.mu.Lock()
		f.requests = append(f.requests, pimRequest{r.Method, r.URL.EscapedPath(), r.Header.Get("Depth"), r.Header.Get("If-Match"), r.Header.Get("Authorization"), r.Header.Get("Cookie"), string(body)})
		f.mu.Unlock()
		if r.Header.Get("Authorization") != "Bearer entra-u1" {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		f.serve(w, r, string(body))
	}))
	t.Cleanup(srv.Close)
	return f, srv
}

func (f *pimFake) serve(w http.ResponseWriter, r *http.Request, body string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	p := r.URL.Path
	multistatus := func(responses ...string) {
		w.Header().Set("Content-Type", "application/xml; charset=utf-8")
		w.WriteHeader(http.StatusMultiStatus)
		_, _ = io.WriteString(w, pimMultistatus+strings.Join(responses, "")+`</d:multistatus>`)
	}
	switch {
	case r.Method == "PROPFIND" && p == "/dav/":
		multistatus(pimProps("/dav/", `<d:resourcetype><d:collection/></d:resourcetype><d:current-user-principal><d:href>/dav/principals/u1/</d:href></d:current-user-principal>`))
	case r.Method == "PROPFIND" && p == "/dav/principals/u1/":
		multistatus(pimProps(p, `<card:addressbook-home-set><d:href>/dav/addressbooks/u1/</d:href></card:addressbook-home-set><cal:calendar-home-set><d:href>http://elsewhere.test/dav/calendars/u1/</d:href></cal:calendar-home-set>`))
	case r.Method == "PROPFIND" && p == "/dav/addressbooks/u1/":
		multistatus(
			pimProps(p, `<d:resourcetype><d:collection/></d:resourcetype>`),
			pimProps(p+"contacts/", `<d:resourcetype><d:collection/><card:addressbook/></d:resourcetype><d:displayname>Contacts</d:displayname><card:addressbook-description>Everyone</card:addressbook-description>`),
			pimProps(p+"inbox/", `<d:resourcetype><d:collection/></d:resourcetype>`),
			pimProps(p+"a%20team/", `<d:resourcetype><d:collection/><card:addressbook/></d:resourcetype>`),
		)
	case r.Method == "PROPFIND" && p == "/dav/addressbooks/u1/inbox/":
		multistatus(pimProps(p, `<d:resourcetype><d:collection/></d:resourcetype>`))
	case r.Method == "PROPFIND" && p == "/dav/calendars/u1/":
		multistatus(
			pimProps(p+"work/", `<d:resourcetype><d:collection/><cal:calendar/></d:resourcetype><d:displayname>Work</d:displayname><ic:calendar-color>#3366FFCC</ic:calendar-color><cal:supported-calendar-component-set><cal:comp name="VEVENT"/><cal:comp name="VTODO"/></cal:supported-calendar-component-set>`),
			pimProps(p+"tasks/", `<d:resourcetype><d:collection/><cal:calendar/></d:resourcetype><d:displayname>Tasks</d:displayname><cal:supported-calendar-component-set><cal:comp name="VTODO"/></cal:supported-calendar-component-set>`),
			pimProps(p+"holidays/", `<d:resourcetype><d:collection/><cal:calendar/></d:resourcetype><d:displayname>Holidays</d:displayname><ic:calendar-color>blue</ic:calendar-color>`),
		)
	case r.Method == "REPORT":
		if f.expandStatus != 0 && strings.Contains(body, "expand") {
			w.WriteHeader(f.expandStatus)
			return
		}
		// An empty filter matches every card only when it tests "allof":
		// Radicale reads the default "anyof" of nothing as nothing.
		if strings.Contains(body, "addressbook-query") && strings.Contains(body, "<C:filter") && !strings.Contains(body, `test="allof"`) {
			multistatus()
			return
		}
		var out []string
		for _, key := range slices.Sorted(func(yield func(string) bool) {
			for k := range f.resources {
				if !yield(k) {
					return
				}
			}
		}) {
			if !strings.HasPrefix(key, p) {
				continue
			}
			res := f.resources[key]
			data := `<card:address-data>` + xmlText(res.data) + `</card:address-data>`
			if strings.Contains(body, "calendar-query") {
				data = `<cal:calendar-data><![CDATA[` + res.data + `]]></cal:calendar-data>`
			}
			out = append(out, pimProps(key, `<d:getetag>"`+res.etag+`"</d:getetag>`+data))
		}
		multistatus(out...)
	case r.Method == http.MethodGet:
		res, ok := f.resources[p]
		if !ok {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		w.Header().Set("ETag", `"`+res.etag+`"`)
		_, _ = io.WriteString(w, res.data)
	case r.Method == http.MethodDelete:
		res, ok := f.resources[p]
		if !ok {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		if m := r.Header.Get("If-Match"); m != "" && m != `"`+res.etag+`"` {
			w.WriteHeader(http.StatusPreconditionFailed)
			return
		}
		delete(f.resources, p)
		w.WriteHeader(http.StatusNoContent)
	default:
		w.WriteHeader(http.StatusMethodNotAllowed)
	}
}

func xmlText(s string) string {
	return strings.NewReplacer("&", "&amp;", "<", "&lt;", ">", "&gt;").Replace(s)
}

func (f *pimFake) put(path, etag, data string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.resources[path] = pimResource{etag, strings.ReplaceAll(strings.TrimPrefix(data, "\n"), "\n", "\r\n")}
}

func (f *pimFake) seen(method string) []pimRequest {
	f.mu.Lock()
	defer f.mu.Unlock()
	var out []pimRequest
	for _, r := range f.requests {
		if method == "" || r.method == method {
			out = append(out, r)
		}
	}
	return out
}

// pimTestServer has a service "people" speaking protocol at the fake's
// /dav, written without its trailing slash.
func pimTestServer(t *testing.T, protocol, key string) (*Server, *pimFake) {
	t.Helper()
	fake, upstream := newPIMFake(t)
	s := planServer(consoleRuntime{connection: runtime.ServiceConnection{Type: "groupware", Protocol: protocol, Binding: map[string]string{
		key: upstream.URL + "/dav",
	}}}, &ai.Analysis{})
	s.cfg.OIDCScope = "api://donkey/.default"
	return s, fake
}

func pimCall(t *testing.T, s *Server, method, suffix string, status int, out any) {
	t.Helper()
	req := httptest.NewRequest(method, "/v1/spaces/default/services/people"+suffix, nil)
	req.Header.Set("X-Access-Token", trinoIdentityToken(t, "u1", "entra-u1"))
	req.Header.Set("Cookie", "session=private")
	rec := httptest.NewRecorder()
	s.routes().ServeHTTP(rec, req)
	if rec.Code != status {
		t.Fatalf("%s %s: %d, want %d: %s", method, suffix, rec.Code, status, rec.Body.String())
	}
	if out != nil {
		if err := json.Unmarshal(rec.Body.Bytes(), out); err != nil {
			t.Fatalf("%s %s: %v: %s", method, suffix, err, rec.Body.String())
		}
	}
}

// The address books are found the way CardDAV clients find them — the
// principal, its home, the home's address books — as the user, with
// nothing of the console's session reaching the service.
func TestCardDAVFindsTheUsersAddressBooks(t *testing.T) {
	s, fake := pimTestServer(t, "carddav", "CARDDAV_URL")
	var books struct{ AddressBooks []addressBook }
	pimCall(t, s, "GET", "/addressbooks", 200, &books)
	want := []addressBook{{Key: "addressbooks/u1/a team/", Name: "a team"}, {Key: "addressbooks/u1/contacts/", Name: "Contacts", Description: "Everyone"}}
	if !slices.Equal(books.AddressBooks, want) {
		t.Fatalf("address books\n%+v\nwant\n%+v", books.AddressBooks, want)
	}
	for _, r := range fake.seen("") {
		if r.auth != "Bearer entra-u1" || r.cookie != "" {
			t.Fatalf("%s %s carried %q / cookie %q", r.method, r.path, r.auth, r.cookie)
		}
	}
	if got := fake.seen("PROPFIND"); len(got) != 4 || got[3].path != "/dav/addressbooks/u1/inbox/" || got[2].path != "/dav/addressbooks/u1/" || got[2].depth != "1" {
		t.Fatalf("discovery: %+v", got)
	}
	// Not a CardDAV service: the console says so before asking anything.
	pimCall(t, s, "GET", "/calendars", 400, nil)
}

func TestCardDAVListsReadsAndDeletesContacts(t *testing.T) {
	s, fake := pimTestServer(t, "carddav", "CARDDAV_URL")
	book := "/dav/addressbooks/u1/contacts/"
	fake.put(book+"b.vcf", "e-b", `
BEGIN:VCARD
VERSION:3.0
FN:bob Builder
EMAIL;TYPE=WORK:bob@acme.test
EMAIL;TYPE=HOME;TYPE=PREF:bob@home.test
END:VCARD
`)
	fake.put(book+"a.vcf", "e-a", `
BEGIN:VCARD
VERSION:4.0
UID:alice
N:Liddell;Alice;;;
ORG:Wonderland
TEL;TYPE=cell:+41 79 000 00 00
NOTE:Curious & late
PHOTO:data:image/png;base64,iVBORw0KGgo=
END:VCARD
`)
	fake.put(book+"nameless.vcf", "e-n", "\nBEGIN:VCARD\nVERSION:3.0\nEND:VCARD\n")
	fake.put(book+"broken.vcf", "e-x", "not a card")

	var list struct {
		Contacts  []contactSummary
		Truncated bool
	}
	pimCall(t, s, "GET", "/contacts?"+url.Values{"book": {"addressbooks/u1/contacts/"}}.Encode(), 200, &list)
	want := []contactSummary{
		{Key: "addressbooks/u1/contacts/a.vcf", ETag: `"e-a"`, Name: "Alice Liddell", Org: "Wonderland", Phone: "+41 79 000 00 00"},
		{Key: "addressbooks/u1/contacts/b.vcf", ETag: `"e-b"`, Name: "bob Builder", Email: "bob@home.test"},
		{Key: "addressbooks/u1/contacts/nameless.vcf", ETag: `"e-n"`},
	}
	if !slices.Equal(list.Contacts, want) || list.Truncated {
		t.Fatalf("contacts\n%+v\nwant\n%+v", list.Contacts, want)
	}
	report := fake.seen("REPORT")
	if len(report) != 1 || report[0].depth != "1" || !strings.Contains(report[0].body, "addressbook-query") || strings.Contains(report[0].body, `name="PHOTO"`) {
		t.Fatalf("report: %+v", report)
	}

	var card contactDetail
	pimCall(t, s, "GET", "/contacts/item?"+url.Values{"key": {"addressbooks/u1/contacts/a.vcf"}}.Encode(), 200, &card)
	if card.Name != "Alice Liddell" || card.Names.Given != "Alice" || card.Photo != "data:image/png;base64,iVBORw0KGgo=" || card.Note != "Curious & late" || card.ETag != `"e-a"` || !strings.HasPrefix(card.Data, "BEGIN:VCARD\r\n") || card.Emails == nil {
		t.Fatalf("card: %+v", card)
	}

	// A delete is conditional on what was seen: a changed card stays.
	pimCall(t, s, "DELETE", "/contacts/item?"+url.Values{"key": {"addressbooks/u1/contacts/b.vcf"}, "etag": {"stale"}}.Encode(), 409, nil)
	pimCall(t, s, "DELETE", "/contacts/item?"+url.Values{"key": {"addressbooks/u1/contacts/b.vcf"}, "etag": {`"e-b"`}}.Encode(), 200, nil)
	if _, ok := fake.resources[book+"b.vcf"]; ok {
		t.Fatal("the card is still there")
	}
	pimCall(t, s, "GET", "/contacts/item?"+url.Values{"key": {"addressbooks/u1/contacts/b.vcf"}}.Encode(), 404, nil)
	// A weak ETag never passes If-Match, so it guards nothing and is not sent.
	pimCall(t, s, "DELETE", "/contacts/item?"+url.Values{"key": {"addressbooks/u1/contacts/nameless.vcf"}, "etag": {`W/"e-n"`}}.Encode(), 200, nil)
	if deletes := fake.seen("DELETE"); len(deletes) != 3 || deletes[1].ifMatch != `"e-b"` || deletes[2].ifMatch != "" {
		t.Fatalf("deletes: %+v", deletes)
	}

	// Keys stay inside the service and name what they claim to.
	for _, bad := range []string{"/contacts?book=addressbooks", "/contacts?book=../x/", "/contacts/item?key=a/../../x.vcf", "/contacts/item?key=book/", "/contacts/item"} {
		pimCall(t, s, "GET", bad, 400, nil)
	}
}

// Calendars that hold events, with their colour; a window's occurrences in
// order, a series expanded here when the service will not; one occurrence
// read in full.
func TestCalDAVShowsAWindowOfEvents(t *testing.T) {
	for _, status := range []int{http.StatusBadRequest, http.StatusUnsupportedMediaType, http.StatusNotImplemented} {
		t.Run(http.StatusText(status), func(t *testing.T) {
			testCalDAVWindow(t, status)
		})
	}
}

func testCalDAVWindow(t *testing.T, expandStatus int) {
	t.Helper()
	s, fake := pimTestServer(t, "caldav", "CALDAV_URL")
	var cals struct{ Calendars []calendar }
	pimCall(t, s, "GET", "/calendars", 200, &cals)
	// The home's href names another host: only its path counts, and the
	// service is still asked at its own address.
	want := []calendar{{Key: "calendars/u1/holidays/", Name: "Holidays"}, {Key: "calendars/u1/work/", Name: "Work", Color: "#3366ff"}}
	if !slices.Equal(cals.Calendars, want) {
		t.Fatalf("calendars\n%+v\nwant\n%+v", cals.Calendars, want)
	}

	work := "/dav/calendars/u1/work/"
	fake.put(work+"standup.ics", "e-s", `
BEGIN:VCALENDAR
BEGIN:VEVENT
UID:standup
DTSTART;TZID=Europe/Zurich:20261116T090000
DTEND;TZID=Europe/Zurich:20261116T091500
RRULE:FREQ=DAILY;BYDAY=MO,TU,WE,TH,FR
SUMMARY:Standup
END:VEVENT
END:VCALENDAR
`)
	fake.put(work+"offsite.ics", "e-o", `
BEGIN:VCALENDAR
BEGIN:VEVENT
UID:offsite
DTSTART;VALUE=DATE:20261124
DTEND;VALUE=DATE:20261126
SUMMARY:Offsite
LOCATION:Lake
ORGANIZER;CN=Ann:mailto:ann@acme.test
ATTENDEE;CN=Bob;PARTSTAT=DECLINED:mailto:bob@acme.test
STATUS:CONFIRMED
CLASS:PRIVATE
END:VEVENT
END:VCALENDAR
`)
	fake.expandStatus = expandStatus
	window := url.Values{"calendar": {"calendars/u1/work/"}, "start": {"2026-11-23T00:00:00+01:00"}, "end": {"2026-11-25T00:00:00+01:00"}, "tz": {"Europe/Zurich"}}
	var list struct {
		Events    []eventSummary
		Truncated bool
	}
	pimCall(t, s, "GET", "/events?"+window.Encode(), 200, &list)
	type brief struct{ summary, start, end, instance string }
	var got []brief
	for _, e := range list.Events {
		got = append(got, brief{e.Summary, e.Start, e.End, e.Instance})
	}
	wantEvents := []brief{
		{"Standup", "2026-11-23T08:00:00Z", "2026-11-23T08:15:00Z", "2026-11-23T08:00:00Z"},
		{"Offsite", "2026-11-24", "2026-11-26", ""},
		{"Standup", "2026-11-24T08:00:00Z", "2026-11-24T08:15:00Z", "2026-11-24T08:00:00Z"},
	}
	if !slices.Equal(got, wantEvents) {
		t.Fatalf("events\n%+v\nwant\n%+v", got, wantEvents)
	}
	if e := list.Events[1]; !e.AllDay || e.Recurring || e.Location != "Lake" || e.Status != "confirmed" || e.ETag != `"e-o"` {
		t.Errorf("offsite: %+v", e)
	}
	// Asked to expand first; asked again for the series whole — both a day
	// wider than the window, which is cut here.
	reports := fake.seen("REPORT")
	if len(reports) != 2 || !strings.Contains(reports[0].body, `<C:expand start="20261121T230000Z" end="20261125T230000Z"/>`) || strings.Contains(reports[1].body, "expand") || !strings.Contains(reports[1].body, `<C:time-range start="20261121T230000Z"`) {
		t.Fatalf("reports: %+v", reports)
	}

	var one eventDetail
	pimCall(t, s, "GET", "/events/item?"+url.Values{"key": {"calendars/u1/work/standup.ics"}, "instance": {"2026-11-24T08:00:00Z"}}.Encode(), 200, &one)
	if one.Start != "2026-11-24T08:00:00Z" || !one.Recurring || one.Rule != "FREQ=DAILY;BYDAY=MO,TU,WE,TH,FR" || one.Attendees == nil || !strings.Contains(one.Data, "RRULE") {
		t.Fatalf("occurrence: %+v", one)
	}
	var offsite eventDetail
	pimCall(t, s, "GET", "/events/item?"+url.Values{"key": {"calendars/u1/work/offsite.ics"}}.Encode(), 200, &offsite)
	if offsite.Organizer == nil || offsite.Organizer.Email != "ann@acme.test" || len(offsite.Attendees) != 1 || offsite.Attendees[0].Status != "declined" || offsite.Class != "private" {
		t.Fatalf("offsite: %+v", offsite)
	}
	pimCall(t, s, "GET", "/events/item?"+url.Values{"key": {"calendars/u1/work/standup.ics"}, "instance": {"never"}}.Encode(), 404, nil)

	pimCall(t, s, "DELETE", "/events/item?"+url.Values{"key": {"calendars/u1/work/standup.ics"}, "etag": {`"e-s"`}}.Encode(), 200, nil)
	if _, ok := fake.resources[work+"standup.ics"]; ok {
		t.Fatal("the series is still there")
	}

	// A window is bounded: in order, and no longer than a year and a bit.
	for _, bad := range []url.Values{
		{"calendar": {"calendars/u1/work/"}, "start": {"2026-11-25T00:00:00Z"}, "end": {"2026-11-23T00:00:00Z"}},
		{"calendar": {"calendars/u1/work/"}, "start": {"2026-01-01T00:00:00Z"}, "end": {"2028-01-01T00:00:00Z"}},
		{"calendar": {"calendars/u1/work/"}, "start": {"tomorrow"}, "end": {"2028-01-01T00:00:00Z"}},
	} {
		pimCall(t, s, "GET", "/events?"+bad.Encode(), 400, nil)
	}
}

// A service URL that is itself the one address book is the only one, at
// key "", and a token the service will not take is its refusal — never the
// console's own 401.
func TestCardDAVURLThatIsTheAddressBook(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer entra-u1" {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		w.WriteHeader(http.StatusMultiStatus)
		_, _ = io.WriteString(w, pimMultistatus+pimProps("/book", `<d:resourcetype><d:collection/><card:addressbook/></d:resourcetype>`)+`</d:multistatus>`)
	}))
	defer upstream.Close()
	s := planServer(consoleRuntime{connection: runtime.ServiceConnection{Type: "groupware", Protocol: "carddav", Binding: map[string]string{
		"CARDDAV_URL": upstream.URL + "/book",
	}}}, &ai.Analysis{})
	s.cfg.OIDCScope = "api://donkey/.default"
	var books struct{ AddressBooks []addressBook }
	pimCall(t, s, "GET", "/addressbooks", 200, &books)
	if !slices.Equal(books.AddressBooks, []addressBook{{Key: "", Name: "people"}}) {
		t.Fatalf("books: %+v", books.AddressBooks)
	}

	req := httptest.NewRequest("GET", "/v1/spaces/default/services/people/addressbooks", nil)
	req.Header.Set("X-Access-Token", trinoIdentityToken(t, "u1", "entra-other"))
	rec := httptest.NewRecorder()
	s.routes().ServeHTTP(rec, req)
	if rec.Code != 403 || !strings.Contains(rec.Body.String(), "identity token") {
		t.Fatalf("refusal: %d %s", rec.Code, rec.Body.String())
	}
}

// An href is placed under the service root as plain segments; a
// percent-encoded dot segment would climb out once the key is joined back.
func TestDAVHrefsStayUnderTheRoot(t *testing.T) {
	c, err := newDAVClient("https://dav.example.test/dav/", "contacts", "token")
	if err != nil {
		t.Fatal(err)
	}
	for href, want := range map[string]string{
		"/dav/":                               "",
		"/dav":                                "",
		"https://dav.example.test/dav/a%20b/": "a b/",
		"/dav/books/card.vcf":                 "books/card.vcf",
		"/dav/%2e%2e/other-tenant/principal/": "!",
		"/dav/a%2F..%2F..%2Fx/":               "!",
		"/dav/a/%2e/b":                        "!",
		"/other/":                             "!",
	} {
		got, ok := c.relative(c.base, href)
		if !ok {
			got = "!"
		}
		if got != want {
			t.Errorf("%s: %q, want %q", href, got, want)
		}
	}
}

// The binding is an entry point. The principal URL has no trailing slash,
// and both homes live beside the binding. Only the second home holds data.
func TestPIMDiscoveryAndAccessOutsideBinding(t *testing.T) {
	const folder = "<d:resourcetype><d:collection/></d:resourcetype>"
	const group = "/homes/shared/group/"
	const book = group + "people/"
	const calendar = group + "events/"
	fake := &pimFake{resources: map[string]pimResource{}}
	handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		p := r.URL.Path
		if r.Header.Get("Authorization") != "Bearer entra-u1" {
			t.Error("missing user token")
		}
		if r.Method != "PROPFIND" {
			if !strings.HasPrefix(p, book) && !strings.HasPrefix(p, calendar) {
				t.Errorf("data request outside discovered collections: %s %s", r.Method, p)
				w.WriteHeader(404)
				return
			}
			body, _ := io.ReadAll(r.Body)
			fake.serve(w, r, string(body))
			return
		}
		var rows string
		switch p {
		case "/entry/", "/":
			rows = pimProps(p, folder+`<d:current-user-principal><d:href>/principals/alice</d:href></d:current-user-principal>`)
		case "/principals/alice":
			homes := "<d:href>/homes/empty/</d:href><d:href>/homes/shared/</d:href><d:href>/homes/shared/</d:href>"
			rows = pimProps(p, "<cal:calendar-home-set>"+homes+"</cal:calendar-home-set><card:addressbook-home-set>"+homes+"</card:addressbook-home-set>")
		case "/homes/empty/":
			rows = pimProps(p, folder)
		case "/homes/shared/":
			rows = pimProps(p, folder) + pimProps(group, folder)
		case group:
			rows = pimProps(p, folder) + pimProps(book, "<d:resourcetype><d:collection/><card:addressbook/></d:resourcetype>") +
				pimProps(calendar, "<d:resourcetype><d:collection/><cal:calendar/></d:resourcetype>")
		default:
			t.Errorf("unexpected discovery request: %s %s", r.Method, p)
			w.WriteHeader(404)
			return
		}
		w.WriteHeader(207)
		_, _ = io.WriteString(w, pimMultistatus+rows+"</d:multistatus>")
	})
	direct := httptest.NewServer(handler)
	defer direct.Close()
	// The same server behind a proxy that strips its mount prefix.
	mux := http.NewServeMux()
	mux.Handle("/gateway/", http.StripPrefix("/gateway", handler))
	proxy := httptest.NewServer(mux)
	defer proxy.Close()
	for _, site := range []struct{ name, bindingURL string }{
		{"sibling", direct.URL + "/entry/"},
		{"proxy", proxy.URL + "/gateway/"},
	} {
		fake.put(book+"a.vcf", "v1", "BEGIN:VCARD\nVERSION:3.0\nFN:Alice\nEND:VCARD")
		fake.put(calendar+"a.ics", "v1", "BEGIN:VCALENDAR\nBEGIN:VEVENT\nUID:a\nDTSTART:20261124T080000Z\nSUMMARY:Work\nEND:VEVENT\nEND:VCALENDAR")
		for _, tc := range []struct{ protocol, binding, collections, list, item, key, resource string }{
			{"carddav", "CARDDAV_URL", "addressBooks", "/contacts?book=" + url.QueryEscape(book), "/contacts/item", book, "a.vcf"},
			{"caldav", "CALDAV_URL", "calendars", "/events?start=2026-11-23T00:00:00Z&end=2026-11-25T00:00:00Z&calendar=" + url.QueryEscape(calendar), "/events/item", calendar, "a.ics"},
		} {
			t.Run(site.name+"/"+tc.protocol, func(t *testing.T) {
				s := planServer(consoleRuntime{connection: runtime.ServiceConnection{Type: "groupware", Protocol: tc.protocol, Binding: map[string]string{tc.binding: site.bindingURL}}}, &ai.Analysis{})
				s.cfg.OIDCScope = "scope"
				var entries map[string][]addressBook
				pimCall(t, s, "GET", "/"+strings.ToLower(tc.collections), 200, &entries)
				if got := entries[tc.collections]; len(got) != 1 || got[0].Key != tc.key {
					t.Fatalf("collections: %+v", entries)
				}
				var list struct {
					Contacts []contactSummary
					Events   []eventSummary
				}
				pimCall(t, s, "GET", tc.list, 200, &list)
				resource := tc.key + tc.resource
				if tc.protocol == "carddav" && (len(list.Contacts) != 1 || list.Contacts[0].Key != resource) ||
					tc.protocol == "caldav" && (len(list.Events) != 1 || list.Events[0].Key != resource) {
					t.Fatalf("list: %+v", list)
				}
				item := tc.item + "?key=" + url.QueryEscape(resource)
				pimCall(t, s, "GET", item, 200, nil)
				pimCall(t, s, "DELETE", tc.item+"?key=/unadvertised/item", 404, nil)
				pimCall(t, s, "DELETE", item+"&etag=%22v1%22", 200, nil)
				pimCall(t, s, "GET", item, 404, nil)
			})
		}
	}
}

func TestPIMPrincipalLookupErrors(t *testing.T) {
	for _, tc := range []struct{ protocol, binding, endpoint string }{
		{"carddav", "CARDDAV_URL", "/addressbooks"}, {"caldav", "CALDAV_URL", "/calendars"},
	} {
		for _, status := range []int{http.StatusForbidden, http.StatusNotFound} {
			t.Run(tc.protocol+"/"+http.StatusText(status), func(t *testing.T) {
				upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					switch r.URL.Path {
					case "/dav/":
						if r.Header.Get("Depth") != "0" {
							t.Error("fell back to listing the root after a lookup failure")
						}
						w.WriteHeader(207)
						_, _ = io.WriteString(w, pimMultistatus+pimProps("/dav/", "<d:current-user-principal><d:href>/dav/u</d:href></d:current-user-principal>")+"</d:multistatus>")
					case "/dav/u":
						w.WriteHeader(status)
					default:
						t.Errorf("unexpected path %s", r.URL.Path)
						w.WriteHeader(500)
					}
				}))
				defer upstream.Close()
				s := planServer(consoleRuntime{connection: runtime.ServiceConnection{Type: "groupware", Protocol: tc.protocol, Binding: map[string]string{tc.binding: upstream.URL + "/dav/"}}}, &ai.Analysis{})
				s.cfg.OIDCScope = "scope"
				var response struct{ Error string }
				pimCall(t, s, "GET", tc.endpoint, status, &response)
				if !strings.Contains(response.Error, "/dav/u") {
					t.Fatalf("missing lookup context: %+v", response)
				}
			})
		}
	}
}

func TestPIMDiscoveryWithoutAdvertisedHome(t *testing.T) {
	const folder = "<d:resourcetype><d:collection/></d:resourcetype>"
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var rows string
		switch r.URL.Path {
		case "/dav/":
			rows = pimProps("/dav/", folder)
			if r.Header.Get("Depth") == "1" {
				rows += pimProps("/dav/users/", folder)
			}
		case "/dav/users/":
			rows = pimProps("/dav/users/", folder) + pimProps("/dav/users/shared/", "<d:resourcetype><d:collection/><cal:calendar/></d:resourcetype>")
		default:
			t.Errorf("unexpected lookup of %s", r.URL.Path)
			w.WriteHeader(404)
			return
		}
		w.WriteHeader(207)
		_, _ = io.WriteString(w, pimMultistatus+rows+"</d:multistatus>")
	}))
	defer server.Close()
	c, err := newDAVClient(server.URL+"/dav/", "groupware", "token")
	if err != nil {
		t.Fatal(err)
	}
	items, err := c.collections(t.Context(), calendarDiscovery)
	if err != nil || len(items) != 1 || items[0].Key != "users/shared/" {
		t.Fatalf("collections: %+v, %v", items, err)
	}
	items, err = c.collections(t.Context(), addressBookDiscovery)
	if err != nil || len(items) != 0 {
		t.Fatalf("empty address books: %+v, %v", items, err)
	}
}

// go-webdav v0.7.0 uses the principal href in the root PROPFIND response.
// Only a request to the principal itself advertises the home.
func TestPIMDiscoveryWithPrincipalHrefAtRoot(t *testing.T) {
	const principal = "/me/"
	const owner = "<d:current-user-principal><d:href>/me/</d:href></d:current-user-principal>"
	const folder = "<d:resourcetype><d:collection/></d:resourcetype>"
	for _, tc := range []struct {
		kind                               davDiscovery
		home, leaf, property, resourceType string
	}{
		{addressBookDiscovery, "/me/addressbooks/", "directory/", "card:addressbook-home-set", "card:addressbook"},
		{calendarDiscovery, "/me/calendars/", "default/", "cal:calendar-home-set", "cal:calendar"},
	} {
		handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			var rows string
			switch r.URL.Path {
			case "/":
				rows = pimProps(principal, folder+owner)
			case principal:
				rows = pimProps(principal, owner+"<d:resourcetype><d:principal/><d:collection/></d:resourcetype><"+tc.property+"><d:href>"+tc.home+"</d:href></"+tc.property+">")
			case tc.home:
				rows = pimProps(tc.home, folder+owner)
				if r.Header.Get("Depth") == "1" {
					rows += pimProps(tc.home+tc.leaf, "<d:resourcetype><d:collection/><"+tc.resourceType+"/></d:resourcetype>")
				}
			default:
				http.NotFound(w, r)
				return
			}
			w.WriteHeader(http.StatusMultiStatus)
			_, _ = io.WriteString(w, pimMultistatus+rows+"</d:multistatus>")
		})
		for _, mount := range []string{"", "/proxy"} {
			srv := httptest.NewServer(http.StripPrefix(mount, handler))
			t.Cleanup(srv.Close)
			for _, entry := range []string{"/", tc.home} {
				t.Run(tc.kind.protocol+mount+entry, func(t *testing.T) {
					dav, err := newDAVClient(srv.URL+mount+entry, "groupware", "test")
					if err != nil {
						t.Fatal(err)
					}
					items, err := dav.collections(t.Context(), tc.kind)
					wantKey := tc.home + tc.leaf
					if mount == "" {
						wantKey = strings.TrimPrefix(wantKey, entry)
					}
					if err != nil || len(items) != 1 || items[0].Key != wantKey {
						t.Fatalf("discover %s: got %+v, %v; want %q", entry, items, err, wantKey)
					}
				})
			}
		}
	}
}
