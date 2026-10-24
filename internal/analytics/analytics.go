// Package analytics is the browser end of donkey's web analytics: the script
// donkey-gateway adds to web apps' HTML pages, and the classification that
// turns what a request carries into report dimensions before anything leaves
// the gateway — browser, OS and device from the user agent, a country from
// the browser's time zone, a channel from the referrer and campaign. The raw
// values stay in the gateway; donkey-telemetry only ever sees the results.
package analytics

import (
	"crypto/sha256"
	_ "embed"
	"encoding/hex"
	"net/url"
	"strings"
	"unicode/utf8"
)

const (
	// The script and its events are the gateway's on every host whose app
	// counts visits; with analytics off they are the app's like any other
	// path. Both carry a dot, which no app name can: /_donkey/<app> is where
	// browser connections reach a sibling, and these never shadow one.
	Prefix     = "/_donkey/"
	ScriptPath = Prefix + "s.js"
	EventPath  = Prefix + "e.json"

	// ScriptTag is what the gateway adds to <head>. defer keeps it off the
	// page's critical path.
	ScriptTag = `<script defer src="` + ScriptPath + `"></script>`
)

//go:embed script.js
var script []byte

// Script is the browser script and its ETag.
func Script() ([]byte, string) { return script, scriptETag }

var scriptETag = func() string {
	sum := sha256.Sum256(script)
	return `"` + hex.EncodeToString(sum[:8]) + `"`
}()

// Payload is what the script sends: short keys, since every page view
// carries one.
type Payload struct {
	// Kind is pageview, event or engagement.
	Kind string `json:"k"`
	// URL is the page the event belongs to; Referrer is document.referrer on
	// a visit's first page view.
	URL      string `json:"u"`
	Referrer string `json:"r,omitempty"`
	// Name and Data are a custom event and its properties.
	Name string            `json:"n,omitempty"`
	Data map[string]string `json:"d,omitempty"`
	// Engaged is how long the page stayed visible, in milliseconds.
	Engaged  int64  `json:"e,omitempty"`
	Language string `json:"l,omitempty"`
	TimeZone string `json:"z,omitempty"`
	// Width is the screen's, in CSS pixels.
	Width int `json:"w,omitempty"`
}

// PagePath is a page's identity: its path, and the route of a hash router
// (#/… or #!/…). Query strings never count — they are where tokens and
// e-mail addresses end up.
func PagePath(u *url.URL) string {
	path := u.Path
	if path == "" {
		path = "/"
	}
	if fragment := u.Fragment; strings.HasPrefix(fragment, "/") || strings.HasPrefix(fragment, "!/") {
		fragment, _, _ = strings.Cut(fragment, "?")
		path += "#" + fragment
	}
	return clip(path, 512)
}

// Campaign reads the UTM parameters, with the common ?ref= as the source
// when utm_source is absent.
func Campaign(query url.Values) (source, medium, campaign string) {
	source = query.Get("utm_source")
	if source == "" {
		source = query.Get("ref")
	}
	return clip(strings.TrimSpace(source), 100), clip(strings.TrimSpace(query.Get("utm_medium")), 100),
		clip(strings.TrimSpace(query.Get("utm_campaign")), 100)
}

// ReferrerHost is where a visit came from: the referring host, without a
// leading www., or "" for none, for a link within the same site, or for
// anything that is not a web or app address. Paths are dropped — a search
// referrer's path can carry what the visitor searched for.
func ReferrerHost(referrer, host string) string {
	u, err := url.Parse(referrer)
	if err != nil || u.Hostname() == "" {
		return ""
	}
	switch u.Scheme {
	case "http", "https", "android-app":
	default:
		return ""
	}
	ref := strings.TrimPrefix(strings.ToLower(u.Hostname()), "www.")
	if ref == strings.TrimPrefix(strings.ToLower(host), "www.") || len(ref) > 253 {
		return ""
	}
	return ref
}

// Language is a BCP 47 tag cut to language and region ("de-CH"): enough to
// tell audiences apart, without the long tail of variants.
func Language(tag string) string {
	parts := strings.Split(strings.ReplaceAll(tag, "_", "-"), "-")
	language := strings.ToLower(parts[0])
	if len(language) < 2 || len(language) > 3 || !letters(language) {
		return ""
	}
	for _, part := range parts[1:] {
		if len(part) == 2 && letters(part) || len(part) == 3 && digits(part) {
			return language + "-" + strings.ToUpper(part)
		}
	}
	return language
}

func letters(s string) bool {
	for _, r := range s {
		if (r < 'a' || r > 'z') && (r < 'A' || r > 'Z') {
			return false
		}
	}
	return true
}

func digits(s string) bool {
	for _, r := range s {
		if r < '0' || r > '9' {
			return false
		}
	}
	return true
}

// clip cuts s to at most n bytes without splitting a character.
func clip(s string, n int) string {
	if len(s) <= n {
		return s
	}
	for n > 0 && !utf8.RuneStart(s[n]) {
		n--
	}
	return s[:n]
}

// Clip is clip for the gateway's free-form values (event names, properties).
func Clip(s string, n int) string { return clip(s, n) }
