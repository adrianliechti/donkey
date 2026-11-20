package server

// The calendar console, for services that speak CalDAV: the user's
// calendars, the events of one in a window, and one occurrence read in
// full. Like the WebDAV browser it reaches the service as the caller
// (davClient). The service is asked to expand series into occurrences
// (CalDAV's expand); a series it sends whole is expanded here (pim).

import (
	"cmp"
	"context"
	"errors"
	"fmt"
	"net/http"
	"regexp"
	"slices"
	"strings"
	"time"

	"github.com/adrianliechti/donkey/internal/addons"
	"github.com/adrianliechti/donkey/internal/pim"
	"github.com/adrianliechti/donkey/pkg/runtime"
)

type calDAV struct {
	dav *davClient
}

const (
	// A window holds at most this many events for the console.
	eventsMax = 2000
	// and spans at most this long: a year, and a bit for a month view's
	// edges.
	eventsWindowMax = 400 * 24 * time.Hour
	// A calendar object read in full is at most this large.
	calendarObjectBytes = 4 << 20
)

const calDAVDiscover = `<?xml version="1.0" encoding="utf-8"?>
<D:propfind xmlns:D="DAV:" xmlns:C="urn:ietf:params:xml:ns:caldav" xmlns:A="http://apple.com/ns/ical/"><D:prop>
<D:resourcetype/><D:displayname/><D:current-user-principal/><C:calendar-home-set/><C:calendar-description/>
<A:calendar-color/><C:supported-calendar-component-set/>
</D:prop></D:propfind>`

// calDAVQuery asks for the events overlapping a window, each series
// expanded to its occurrences there unless expand is false.
func calDAVQuery(from, to time.Time, expand bool) string {
	start, end := from.UTC().Format("20060102T150405Z"), to.UTC().Format("20060102T150405Z")
	data := `<C:calendar-data/>`
	if expand {
		data = `<C:calendar-data><C:expand start="` + start + `" end="` + end + `"/></C:calendar-data>`
	}
	return `<?xml version="1.0" encoding="utf-8"?>
<C:calendar-query xmlns:D="DAV:" xmlns:C="urn:ietf:params:xml:ns:caldav">
<D:prop><D:getetag/>` + data + `</D:prop>
<C:filter><C:comp-filter name="VCALENDAR"><C:comp-filter name="VEVENT">
<C:time-range start="` + start + `" end="` + end + `"/>
</C:comp-filter></C:comp-filter></C:filter>
</C:calendar-query>`
}

type calendar struct {
	Key         string `json:"key"`
	Name        string `json:"name"`
	Description string `json:"description,omitempty"`
	Color       string `json:"color,omitempty"`
}

type eventSummary struct {
	Key       string `json:"key"`
	ETag      string `json:"etag,omitempty"`
	UID       string `json:"uid,omitempty"`
	Instance  string `json:"instance,omitempty"`
	Summary   string `json:"summary"`
	Start     string `json:"start"`
	End       string `json:"end"`
	AllDay    bool   `json:"allDay"`
	Location  string `json:"location,omitempty"`
	Status    string `json:"status,omitempty"`
	Recurring bool   `json:"recurring"`
	Free      bool   `json:"free,omitempty"`

	at time.Time // the start, for ordering
}

type eventDetail struct {
	eventSummary
	Description string         `json:"description,omitempty"`
	URL         string         `json:"url,omitempty"`
	Organizer   *pim.Person    `json:"organizer,omitempty"`
	Attendees   []pim.Attendee `json:"attendees"`
	Categories  []string       `json:"categories"`
	Rule        string         `json:"rule,omitempty"`
	Created     string         `json:"created,omitempty"`
	Modified    string         `json:"modified,omitempty"`
	Class       string         `json:"class,omitempty"`
	Data        string         `json:"data"`
}

var calendarColor = regexp.MustCompile(`^#[0-9a-fA-F]{6}([0-9a-fA-F]{2})?$`)

var calendarDiscovery = davDiscovery{
	protocol: addons.ProtocolCalDAV,
	query:    calDAVDiscover,
	homes:    func(p davProp) *davHrefs { return p.CalendarHomeSet },
	matches:  holdsEvents,
}

// Calendars are the user's calendars that hold events, by name.
func (c calDAV) Calendars(ctx context.Context) ([]calendar, error) {
	items, err := c.dav.collections(ctx, calendarDiscovery)
	if err != nil {
		return nil, err
	}
	out := []calendar{}
	for _, item := range items {
		cal := calendar{
			Key:         item.Key,
			Name:        c.dav.collectionName(item),
			Description: item.prop(func(p davProp) string { return p.CalendarDescription }),
		}
		// Apple writes #RRGGBBAA; the console takes the colour, not its
		// opacity.
		if color := item.prop(func(p davProp) string { return p.CalendarColor }); calendarColor.MatchString(color) {
			cal.Color = strings.ToLower(color[:7])
		}
		out = append(out, cal)
	}
	slices.SortFunc(out, func(a, b calendar) int { return cmp.Compare(strings.ToLower(a.Name), strings.ToLower(b.Name)) })
	return out, nil
}

// holdsEvents is a calendar that takes VEVENTs: one that names no
// components takes them all.
func holdsEvents(i davItem) bool {
	if !i.has(func(p davProp) bool { return p.ResourceType.Calendar != nil }) {
		return false
	}
	named := false
	for _, p := range i.Props {
		if p.CalendarComponents == nil {
			continue
		}
		for _, comp := range p.CalendarComponents.Comps {
			named = true
			if strings.EqualFold(comp.Name, "VEVENT") {
				return true
			}
		}
	}
	return !named
}

// Events are a calendar's occurrences overlapping [from, to), in order;
// floating times are in loc. truncated says there were more than
// eventsMax.
func (c calDAV) Events(ctx context.Context, key string, from, to time.Time, loc *time.Location) ([]eventSummary, bool, error) {
	// The service reads floating times and dates in the calendar's zone, or
	// UTC, not the reader's: it is asked for a day more on either side, and
	// the window is cut exactly here.
	wideFrom, wideTo := from.Add(-24*time.Hour), to.Add(24*time.Hour)
	items, err := c.dav.multistatus(ctx, "REPORT", key, 1, calDAVQuery(wideFrom, wideTo, true))
	// A service that will not expand is asked again for the series whole.
	var up *upstreamError
	if errors.As(err, &up) && (up.status == http.StatusBadRequest || up.status == http.StatusNotImplemented || up.status == http.StatusUnsupportedMediaType) {
		items, err = c.dav.multistatus(ctx, "REPORT", key, 1, calDAVQuery(wideFrom, wideTo, false))
	}
	if err != nil {
		return nil, false, err
	}
	out := []eventSummary{}
	for _, item := range items {
		data := item.prop(func(p davProp) string { return p.CalendarData })
		if item.collection() || data == "" {
			continue
		}
		cal, ok := pim.ParseCalendar([]byte(data), loc)
		if !ok {
			continue
		}
		etag := item.prop(func(p davProp) string { return p.ETag })
		for _, e := range cal.Occurrences(from, to) {
			out = append(out, summarizeEvent(item.Key, etag, e, loc))
		}
	}
	// By start; on the same day all-day events first, then by title.
	slices.SortFunc(out, func(a, b eventSummary) int {
		if a.AllDay != b.AllDay && a.at.Equal(b.at) {
			if a.AllDay {
				return -1
			}
			return 1
		}
		return cmp.Or(a.at.Compare(b.at), cmp.Compare(strings.ToLower(a.Summary), strings.ToLower(b.Summary)), cmp.Compare(a.Key, b.Key))
	})
	truncated := len(out) > eventsMax
	if truncated {
		out = out[:eventsMax]
	}
	return out, truncated, nil
}

func summarizeEvent(key, etag string, e pim.Event, loc *time.Location) eventSummary {
	out := eventSummary{
		Key: key, ETag: etag, UID: e.UID, Instance: e.Instance, Summary: e.Summary,
		Start: e.Start.String(), End: e.End.String(), AllDay: e.Start.Date,
		Location: e.Location, Recurring: e.Recurring, Free: e.Free,
	}
	switch e.Status {
	case "tentative", "confirmed", "cancelled":
		out.Status = e.Status
	}
	out.at = e.Start.Time
	if e.Start.Date {
		y, m, d := e.Start.Time.Date()
		out.at = time.Date(y, m, d, 0, 0, 0, 0, loc)
	}
	return out
}

// Event is one occurrence read in full — instance as eventSummary names
// it, empty for the series or the one-off — and the object as it is
// stored.
func (c calDAV) Event(ctx context.Context, key, instance string, loc *time.Location) (eventDetail, error) {
	data, etag, err := c.dav.get(ctx, key, calendarObjectBytes)
	if err != nil {
		return eventDetail{}, err
	}
	cal, ok := pim.ParseCalendar(data, loc)
	if !ok {
		return eventDetail{}, &upstreamError{status: http.StatusNotFound, err: fmt.Errorf("%s holds no calendar", key)}
	}
	e, ok := cal.Occurrence(instance)
	if !ok {
		return eventDetail{}, &upstreamError{status: http.StatusNotFound, err: fmt.Errorf("%s holds no such event", key)}
	}
	out := eventDetail{
		eventSummary: summarizeEvent(key, etag, e, loc),
		Description:  e.Description, URL: e.URL, Organizer: e.Organizer,
		Attendees: nonNil(e.Attendees), Categories: nonNil(e.Categories), Rule: e.Rule,
		Created: formatTime(e.Created), Modified: formatTime(e.Modified), Class: e.Class,
		Data: string(data),
	}
	switch out.Class {
	case "public", "private", "confidential":
	default:
		out.Class = ""
	}
	// An answer the console has no word for is no answer it shows.
	for i, a := range out.Attendees {
		switch a.Status {
		case "needs-action", "accepted", "declined", "tentative", "delegated":
		default:
			out.Attendees[i].Status = ""
		}
	}
	return out, nil
}

// Delete removes a calendar object — a series with every occurrence —
// unless it changed since etag (as the service wrote it).
func (c calDAV) Delete(ctx context.Context, key, etag string) error {
	return c.dav.remove(ctx, key, etag)
}

// ─── Handlers ──────────────────────────────────────────────────────────

// readerZone is where the reader is (tz), for floating times; UTC when it
// does not say or names no zone.
func readerZone(r *http.Request) *time.Location {
	if name := r.URL.Query().Get("tz"); name != "" && len(name) <= 64 {
		if loc, err := time.LoadLocation(name); err == nil {
			return loc
		}
	}
	return time.UTC
}

func (s *Server) handleCalendars(w http.ResponseWriter, r *http.Request) {
	s.serveDAV(w, r, calendarDiscovery, "", func(ctx context.Context, dav *davClient) (any, error) {
		calendars, err := calDAV{dav}.Calendars(ctx)
		return map[string]any{"calendars": calendars}, err
	})
}

func (s *Server) handleEvents(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	key := q.Get("calendar")
	if err := checkDAVCollection(key); err != nil {
		writeErr(w, http.StatusBadRequest, err)
		return
	}
	from, err1 := time.Parse(time.RFC3339, q.Get("start"))
	to, err2 := time.Parse(time.RFC3339, q.Get("end"))
	if err1 != nil || err2 != nil || !to.After(from) || to.Sub(from) > eventsWindowMax {
		writeErr(w, http.StatusBadRequest, runtime.UserError{Err: fmt.Errorf("start and end must be RFC 3339 times, end after start, at most %d days apart", int(eventsWindowMax.Hours()/24))})
		return
	}
	s.serveDAV(w, r, calendarDiscovery, key, func(ctx context.Context, dav *davClient) (any, error) {
		events, truncated, err := calDAV{dav}.Events(ctx, key, from, to, readerZone(r))
		return map[string]any{"events": events, "truncated": truncated}, err
	})
}

func (s *Server) handleEvent(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	key := q.Get("key")
	if err := checkDAVResource(key); err != nil {
		writeErr(w, http.StatusBadRequest, err)
		return
	}
	s.serveDAV(w, r, calendarDiscovery, key, func(ctx context.Context, dav *davClient) (any, error) {
		return calDAV{dav}.Event(ctx, key, q.Get("instance"), readerZone(r))
	})
}

func (s *Server) handleDeleteEvent(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	key := q.Get("key")
	if err := checkDAVResource(key); err != nil {
		writeErr(w, http.StatusBadRequest, err)
		return
	}
	s.serveDAV(w, r, calendarDiscovery, key, func(ctx context.Context, dav *davClient) (any, error) {
		return map[string]any{"ok": true}, calDAV{dav}.Delete(ctx, key, q.Get("etag"))
	})
}
