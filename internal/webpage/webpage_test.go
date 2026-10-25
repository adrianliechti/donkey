package webpage

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestNoticeRendersInTheShell(t *testing.T) {
	rec := httptest.NewRecorder()
	WriteNotice(rec, http.StatusForbidden, Notice{Title: "This app is restricted.", Detail: `Signed in as <ada@example.com> & "friends"`, Foot: "donkey gateway"})

	if rec.Code != http.StatusForbidden {
		t.Fatalf("status = %d, want 403", rec.Code)
	}
	if ct := rec.Header().Get("Content-Type"); !strings.HasPrefix(ct, "text/html") {
		t.Fatalf("content type = %q", ct)
	}
	body := rec.Body.String()
	for _, want := range []string{
		"<title>This app is restricted. · Donkey</title>",
		`href="data:image/svg+xml,`, // the favicon survives html/template's URL filter
		`class="mark"`,
		"<h1>This app is restricted.</h1>",
		"Signed in as &lt;ada@example.com&gt; &amp; &#34;friends&#34;",
		`<div class="foot">donkey gateway</div>`,
		"color-scheme: light dark",
	} {
		if !strings.Contains(body, want) {
			t.Errorf("page lacks %q\n%s", want, body)
		}
	}
	if strings.Contains(body, "ZgotmplZ") {
		t.Errorf("html/template rejected a value:\n%s", body)
	}
}

func TestParseAddsABodyToTheLayout(t *testing.T) {
	page := Parse(`<p id="x">{{.Extra}}</p>`)
	rec := httptest.NewRecorder()
	Write(rec, http.StatusOK, page, map[string]string{"Title": "Custom", "Extra": "<b>"})
	body := rec.Body.String()
	if !strings.Contains(body, "<title>Custom · Donkey</title>") || !strings.Contains(body, `<p id="x">&lt;b&gt;</p>`) {
		t.Fatalf("unexpected page:\n%s", body)
	}
	// The layout itself stays clonable: a second page must not fail.
	Parse(`second`)
}
