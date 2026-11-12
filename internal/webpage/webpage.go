// Package webpage is the shell for the few HTML pages donkey renders itself,
// outside the console: the gateway's sign-in errors and notices, the OAuth
// consent form, and the CLI's "signed in" tab. One layout keeps them looking
// like the console they belong to — its colours, its mark, its favicon, in
// light and dark alike. The markup is html/template, so every value a page
// shows is escaped for where it lands.
package webpage

import (
	"bytes"
	_ "embed"
	"html/template"
	"net/http"
)

//go:embed layout.html
var layoutHTML string

//go:embed notice.html
var noticeHTML string

// layout is only ever cloned by Parse, never executed, so it stays clonable.
var layout = template.Must(template.New("layout").Parse(layoutHTML))

// Parse returns the layout with body, a template fragment, as the card's
// content. The fragment sees the data the page is executed with; the layout
// itself reads .Title for the tab.
func Parse(body string) *template.Template {
	t := template.Must(layout.Clone())
	template.Must(t.New("body").Parse(body))
	return t
}

// Notice is a page that says one thing: a one-line title, a sentence of
// detail and a small footer naming who speaks (for example "donkey gateway").
type Notice struct {
	Title  string
	Detail string
	Foot   string
}

var notice = Parse(noticeHTML)

// WriteNotice renders a Notice as the whole response.
func WriteNotice(w http.ResponseWriter, status int, n Notice) {
	Write(w, status, notice, n)
}

// Write renders a page parsed with Parse as the whole response. The page is
// rendered before anything is sent, so a template error becomes a plain 500
// rather than half a page. These pages are moments, not documents: none is
// cacheable.
func Write(w http.ResponseWriter, status int, page *template.Template, data any) {
	var buf bytes.Buffer
	if err := page.Execute(&buf, data); err != nil {
		http.Error(w, "the page could not be rendered", http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(status)
	_, _ = w.Write(buf.Bytes())
}
