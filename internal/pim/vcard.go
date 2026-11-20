package pim

import (
	"encoding/base64"
	"regexp"
	"slices"
	"strings"
	"time"
)

// Contact is a vCard as a person reads it.
type Contact struct {
	Name       string // what to call it: FN, else N, else ORG, else an email
	Names      *Names
	Kind       string // individual (empty), group, org, location
	Nickname   string
	Org        string
	Department string
	Title      string
	Role       string
	Emails     []Typed
	Phones     []Typed
	Addresses  []Typed // lines joined by "\n"
	URLs       []Typed
	Birthday   string // YYYY-MM-DD, --MM-DD without a year, or as written
	// Anniversary is written like Birthday.
	Anniversary string
	Note        string
	Categories  []string
	Photo       string   // a data: URI; only photos the card carries itself
	Members     []string // a group's, as written
	UID         string
	Revision    string // RFC 3339 when readable
	Version     string
}

// Names are N's parts.
type Names struct {
	Prefix string `json:"prefix,omitempty"`
	Given  string `json:"given,omitempty"`
	Middle string `json:"middle,omitempty"`
	Family string `json:"family,omitempty"`
	Suffix string `json:"suffix,omitempty"`
}

// Typed is a value with what kind it is: work, home, cell…
type Typed struct {
	Value     string   `json:"value"`
	Types     []string `json:"types,omitempty"`
	Preferred bool     `json:"preferred,omitempty"`
}

// A photo larger than this stays out of the contact: it would be a long
// wait for a thumbnail.
const photoMaxBytes = 2 << 20

// ParseContact reads the first card of a vCard text. ok is false when
// there is none.
func ParseContact(data []byte) (Contact, bool) {
	for _, c := range Parse(data) {
		if c.Name == "VCARD" {
			return contactFrom(c), true
		}
	}
	return Contact{}, false
}

func contactFrom(card *Component) Contact {
	c := Contact{
		Version:  card.Text("VERSION"),
		UID:      card.Text("UID"),
		Nickname: strings.Join(listOf(card, "NICKNAME"), ", "),
		Title:    card.Text("TITLE"),
		Role:     card.Text("ROLE"),
		Note:     card.Text("NOTE"),
	}
	c.Kind = strings.ToLower(card.Text("KIND"))
	if c.Kind == "" {
		c.Kind = strings.ToLower(card.Text("X-ADDRESSBOOKSERVER-KIND"))
	}
	if c.Kind == "individual" {
		c.Kind = ""
	}
	if p, ok := card.Get("N"); ok {
		parts := append(p.Parts(), "", "", "", "", "")
		n := Names{Family: trim(parts[0]), Given: trim(parts[1]), Middle: trim(parts[2]), Prefix: trim(parts[3]), Suffix: trim(parts[4])}
		if n != (Names{}) {
			c.Names = &n
		}
	}
	if p, ok := card.Get("ORG"); ok {
		parts := slices.DeleteFunc(p.Parts(), func(s string) bool { return trim(s) == "" })
		if len(parts) > 0 {
			c.Org = trim(parts[0])
			c.Department = strings.Join(parts[1:], " · ")
		}
	}
	labels := groupLabels(card)
	for _, p := range card.All("EMAIL") {
		c.Emails = appendTyped(c.Emails, strings.TrimPrefix(trim(p.Text()), "mailto:"), p, labels)
	}
	for _, p := range card.All("TEL") {
		c.Phones = appendTyped(c.Phones, strings.TrimPrefix(trim(p.Text()), "tel:"), p, labels)
	}
	for _, p := range card.All("ADR") {
		c.Addresses = appendTyped(c.Addresses, address(p), p, labels)
	}
	for _, p := range card.All("URL") {
		c.URLs = appendTyped(c.URLs, trim(p.Text()), p, labels)
	}
	c.Categories = listOf(card, "CATEGORIES")
	if p, ok := card.Get("BDAY"); ok {
		c.Birthday = day(p)
	}
	for _, name := range []string{"ANNIVERSARY", "X-ANNIVERSARY"} {
		if p, ok := card.Get(name); ok && c.Anniversary == "" {
			c.Anniversary = day(p)
		}
	}
	for _, name := range []string{"MEMBER", "X-ADDRESSBOOKSERVER-MEMBER"} {
		for _, p := range card.All(name) {
			if v := trim(p.Text()); v != "" {
				c.Members = append(c.Members, v)
			}
		}
	}
	if p, ok := card.Get("REV"); ok {
		c.Revision = trim(p.Text())
		if t, ok := parseStamp(c.Revision); ok {
			c.Revision = t.UTC().Format(time.RFC3339)
		}
	}
	if p, ok := card.Get("PHOTO"); ok {
		c.Photo = photo(p)
	}
	c.Name = card.Text("FN")
	if c.Name == "" && c.Names != nil {
		c.Name = strings.Join(slices.DeleteFunc([]string{c.Names.Prefix, c.Names.Given, c.Names.Middle, c.Names.Family, c.Names.Suffix}, func(s string) bool { return s == "" }), " ")
	}
	if c.Name == "" {
		c.Name = c.Org
	}
	if c.Name == "" && len(c.Emails) > 0 {
		c.Name = c.Emails[0].Value
	}
	return c
}

func trim(s string) string { return strings.TrimSpace(s) }

func listOf(c *Component, name string) []string {
	var out []string
	for _, p := range c.All(name) {
		out = append(out, p.List()...)
	}
	return out
}

// groupLabels are Apple's custom labels: item1.X-ABLabel names what
// item1.EMAIL is, wrapped as _$!<Mobile>!$_ when it is a stock one.
func groupLabels(card *Component) map[string]string {
	labels := map[string]string{}
	for _, p := range card.All("X-ABLABEL") {
		if p.Group == "" {
			continue
		}
		label := trim(p.Text())
		label = strings.TrimSuffix(strings.TrimPrefix(label, "_$!<"), ">!$_")
		if label != "" {
			labels[p.Group] = strings.ToLower(label)
		}
	}
	return labels
}

// Types that say nothing to a reader: every email is "internet".
var noiseTypes = []string{"internet", "x400", "pref", "voice"}

func appendTyped(list []Typed, value string, p Property, labels map[string]string) []Typed {
	if value == "" {
		return list
	}
	t := Typed{Value: value}
	for _, raw := range p.Params["TYPE"] {
		for _, v := range strings.Split(raw, ",") {
			v = strings.ToLower(trim(v))
			if v == "pref" {
				t.Preferred = true
			}
			if v == "" || slices.Contains(noiseTypes, v) || slices.Contains(t.Types, v) {
				continue
			}
			t.Types = append(t.Types, v)
		}
	}
	if p.Param("PREF") == "1" {
		t.Preferred = true
	}
	if label := labels[p.Group]; label != "" && p.Group != "" && !slices.Contains(t.Types, label) {
		t.Types = append(t.Types, label)
	}
	return append(list, t)
}

// address is an ADR as lines: its LABEL when it brings one, else street,
// extended address and post box, postal code and locality, region and
// country, each on a line of its own.
func address(p Property) string {
	if label := trim(p.Param("LABEL")); label != "" {
		return label
	}
	parts := append(p.Parts(), "", "", "", "", "", "", "")
	pobox, ext, street, locality, region, code, country := trim(parts[0]), trim(parts[1]), trim(parts[2]), trim(parts[3]), trim(parts[4]), trim(parts[5]), trim(parts[6])
	var lines []string
	for _, line := range []string{street, ext, pobox, trim(code + " " + locality), region, country} {
		if line != "" {
			lines = append(lines, line)
		}
	}
	return strings.Join(lines, "\n")
}

var (
	dateDigits   = regexp.MustCompile(`^(\d{4})-?(\d{2})-?(\d{2})`)
	noYearDigits = regexp.MustCompile(`^--(\d{2})-?(\d{2})`)
	imageType    = regexp.MustCompile(`^image/[a-z0-9.+-]+$`)
)

// day reads BDAY and ANNIVERSARY: 19800501, 1980-05-01, --0501, a
// timestamp, or text. Apple writes a year it does not know as 1604.
func day(p Property) string {
	v := trim(p.Text())
	if m := noYearDigits.FindStringSubmatch(v); m != nil {
		return "--" + m[1] + "-" + m[2]
	}
	if m := dateDigits.FindStringSubmatch(v); m != nil {
		if m[1] == "1604" || p.Param("X-APPLE-OMIT-YEAR") == m[1] {
			return "--" + m[2] + "-" + m[3]
		}
		return m[1] + "-" + m[2] + "-" + m[3]
	}
	return v
}

// photo is a card's own picture as a data: URI: vCard 4 writes one, 3.0
// writes base64 with ENCODING=b and the TYPE. A photo at an address is left
// there: fetching it would reach wherever the card points.
func photo(p Property) string {
	v := trim(p.Value)
	if strings.HasPrefix(strings.ToLower(v), "data:image/") {
		if len(v) > photoMaxBytes*4/3 {
			return ""
		}
		return v
	}
	encoding := strings.ToLower(p.Param("ENCODING"))
	if encoding != "b" && encoding != "base64" {
		return ""
	}
	data := strings.Map(func(r rune) rune {
		if r == ' ' || r == '\t' || r == '\n' || r == '\r' {
			return -1
		}
		return r
	}, v)
	if len(data) > photoMaxBytes*4/3 {
		return ""
	}
	if _, err := base64.StdEncoding.DecodeString(data); err != nil {
		return ""
	}
	media := "image/jpeg"
	for _, t := range p.Params["TYPE"] {
		t = strings.ToLower(trim(t))
		switch {
		case strings.HasPrefix(t, "image/"):
			media = t
		case t == "png" || t == "gif" || t == "webp" || t == "bmp" || t == "tiff":
			media = "image/" + t
		}
	}
	if !imageType.MatchString(media) {
		return ""
	}
	return "data:" + media + ";base64," + data
}

// parseStamp reads a REV or a DTSTAMP: 20261120T101010Z, its extended form,
// or RFC 3339.
func parseStamp(v string) (time.Time, bool) {
	for _, layout := range []string{"20060102T150405Z", "2006-01-02T15:04:05Z", time.RFC3339, "20060102T150405", "20060102"} {
		if t, err := time.Parse(layout, v); err == nil {
			return t, true
		}
	}
	return time.Time{}, false
}
