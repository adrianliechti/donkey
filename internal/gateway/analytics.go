package gateway

import (
	"encoding/json"
	"io"
	"net"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/adrianliechti/donkey/internal/analytics"
	"github.com/adrianliechti/donkey/internal/config"
	"github.com/adrianliechti/donkey/internal/telemetry"
)

// Web analytics: on a web app's address the gateway adds analytics.ScriptTag
// to HTML pages, serves the script, and takes its events — same-origin, so
// there is no third party, no CORS and no cookie. The events become
// telemetry.WebEvent here, with every dimension already derived; the request's
// address and user agent never leave this process.

// maxEventBody bounds one beacon. A page view is a few hundred bytes; a
// custom event with its properties stays well below this.
const maxEventBody = 8 << 10

// countsVisits says the route's pages get the script: an app's own web route,
// not opted out, while there is a telemetry service to send to.
func (s *Server) countsVisits(rt route) bool {
	return s.telemetry.collectsWeb() && rt.Space != "" && rt.App != "" &&
		rt.Mode != config.AppModeAPI && rt.Analytics != config.AnalyticsOff
}

// serveAnalytics answers the script and its events on any host where some
// route counts visits. It returns false for everything else, which is the
// app's to serve.
func (s *Server) serveAnalytics(w http.ResponseWriter, r *http.Request) bool {
	switch {
	case r.URL.Path == analytics.ScriptPath && (r.Method == http.MethodGet || r.Method == http.MethodHead):
	case r.URL.Path == analytics.EventPath && r.Method == http.MethodPost:
	default:
		return false
	}
	host := requestHost(r)
	if !s.hostCountsVisits(host) {
		return false
	}
	if r.URL.Path == analytics.ScriptPath {
		body, etag := analytics.Script()
		w.Header().Set("Content-Type", "text/javascript; charset=utf-8")
		w.Header().Set("Cache-Control", "public, max-age=3600")
		w.Header().Set("ETag", etag)
		w.Header().Set("X-Content-Type-Options", "nosniff")
		if strings.Contains(r.Header.Get("If-None-Match"), etag) {
			w.WriteHeader(http.StatusNotModified)
			return true
		}
		if r.Method == http.MethodGet {
			_, _ = w.Write(body)
		}
		return true
	}
	// A beacon learns nothing from the answer, whatever happened to it.
	if event, ok := s.webEvent(r, host); ok {
		s.telemetry.RecordWeb(event)
	}
	w.WriteHeader(http.StatusNoContent)
	return true
}

func (s *Server) hostCountsVisits(host string) bool {
	s.mu.RLock()
	defer s.mu.RUnlock()
	for _, rt := range s.routes[host] {
		if s.countsVisits(rt) {
			return true
		}
	}
	return false
}

// webEvent turns one beacon into an event, or refuses it: sent from another
// site, by a bot, for a page whose route doesn't count visits, or — behind a
// login — by someone the page would not have been shown to.
func (s *Server) webEvent(r *http.Request, host string) (telemetry.WebEvent, bool) {
	if site := r.Header.Get("Sec-Fetch-Site"); site != "" && site != "same-origin" {
		return telemetry.WebEvent{}, false
	}
	if origin := r.Header.Get("Origin"); origin != "" {
		if u, err := url.Parse(origin); err != nil || !strings.EqualFold(u.Hostname(), host) {
			return telemetry.WebEvent{}, false
		}
	}
	agent := analytics.ParseUserAgent(r.UserAgent())
	if agent.Bot {
		return telemetry.WebEvent{}, false
	}
	var p analytics.Payload
	if err := json.NewDecoder(io.LimitReader(r.Body, maxEventBody)).Decode(&p); err != nil {
		return telemetry.WebEvent{}, false
	}
	page, err := url.Parse(p.URL)
	if err != nil || !strings.EqualFold(page.Hostname(), host) {
		return telemetry.WebEvent{}, false
	}
	// The page decides which app this is: on a shared address, /api may be
	// another app than /, and only the page's own route counts.
	rt, ok := s.tableLookup(host, page.Path)
	if !ok || !s.countsVisits(rt) {
		return telemetry.WebEvent{}, false
	}
	sess, _ := s.identityFrom(r)
	if rt.Login && (!s.cfg.authConfigured() || sess == nil || !rt.allows(sess.Subject, sess.Email, sess.Groups)) {
		return telemetry.WebEvent{}, false
	}

	now := time.Now()
	event := telemetry.WebEvent{
		TimeUnixMilli: now.UnixMilli(),
		Space:         rt.Space,
		App:           rt.App,
		Path:          analytics.PagePath(page),
		Language:      analytics.Language(p.Language),
		Country:       analytics.Country(p.TimeZone),
	}
	agent = agent.WithScreen(p.Width)
	event.Browser, event.OS, event.Device = agent.Browser, agent.OS, agent.Device
	subject := ""
	if sess != nil {
		subject = sess.Subject
	}
	address, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		address = r.RemoteAddr
	}
	event.Visitor = telemetry.WebVisitor(s.cfg.TelemetryKey, rt.Space, rt.App, subject, address, r.UserAgent(), now)

	switch p.Kind {
	case telemetry.WebKindPageview:
		event.Kind = telemetry.WebKindPageview
		event.Referrer = analytics.ReferrerHost(p.Referrer, host)
		event.UTMSource, event.UTMMedium, event.UTMCampaign = analytics.Campaign(page.Query())
		event.Channel = analytics.Channel(event.Referrer, event.UTMSource, event.UTMMedium)
	case telemetry.WebKindEvent:
		name := strings.TrimSpace(p.Name)
		if name == "" || len(p.Data) > 10 {
			return telemetry.WebEvent{}, false
		}
		event.Kind, event.Name = telemetry.WebKindEvent, analytics.Clip(name, 100)
		if len(p.Data) > 0 {
			event.Props = make(map[string]string, len(p.Data))
			for key, value := range p.Data {
				if key = strings.TrimSpace(key); key != "" {
					event.Props[analytics.Clip(key, 50)] = analytics.Clip(value, 200)
				}
			}
		}
	case telemetry.WebKindEngagement:
		if p.Engaged <= 0 {
			return telemetry.WebEvent{}, false
		}
		event.Kind, event.EngagedMS = telemetry.WebKindEngagement, min(p.Engaged, (24*time.Hour).Milliseconds())
	default:
		return telemetry.WebEvent{}, false
	}
	return event, true
}

// wantsDocument says the browser is navigating to a page: the one kind of
// request whose HTML the script is added to. Fetches and frames are not, and
// neither is a byte range — its slice of a gzip stream cannot be decoded.
func wantsDocument(r *http.Request) bool {
	if r.Method != http.MethodGet || r.Header.Get("Range") != "" {
		return false
	}
	if dest := r.Header.Get("Sec-Fetch-Dest"); dest != "" {
		return dest == "document"
	}
	return strings.Contains(r.Header.Get("Accept"), "text/html")
}

// injectScript adds the script to a page on its way out. The request went
// upstream without the client's Accept-Encoding, so Go's transport asked for
// gzip itself and decoded it; the page is compressed again here when the
// client takes gzip, and streams either way.
func injectScript(res *http.Response, acceptsGzip bool) {
	injectable := analytics.Injectable(res)
	if injectable {
		res.Body = analytics.Inject(res.Body)
		res.ContentLength = -1
		res.Header.Del("Content-Length")
		// The bytes differ from what the app sent, so the validator is only
		// weakly the app's — and a weak If-None-Match still matches upstream.
		if etag := res.Header.Get("ETag"); etag != "" && !strings.HasPrefix(etag, "W/") {
			res.Header.Set("ETag", "W/"+etag)
		}
	}
	// A page left alone (a strict CSP, say) was still fetched decoded; it
	// goes back compressed like any other.
	if acceptsGzip && (injectable || res.Uncompressed) && res.Header.Get("Content-Encoding") == "" {
		res.Body = analytics.Gzip(res.Body)
		res.ContentLength = -1
		res.Header.Del("Content-Length")
		res.Header.Set("Content-Encoding", "gzip")
		res.Header.Add("Vary", "Accept-Encoding")
	}
}
