package gateway

import (
	"net/http/httptest"
	"strings"
	"testing"
)

func TestConsentPageEscapesItsValues(t *testing.T) {
	rec := httptest.NewRecorder()
	renderOAuthConsent(rec, `Acme "CLI"`, "https://api.example.com/v1", "http://127.0.0.1:1234/callback?x=1&y=2", `sealed"request`)
	body := rec.Body.String()
	for _, want := range []string{
		"<title>Authorize API client · Donkey</title>",
		`class="mark"`,
		"<h1>Allow Acme &#34;CLI&#34; to use this API?</h1>",
		"<code>http://127.0.0.1:1234/callback?x=1&amp;y=2</code>",
		`value="sealed&#34;request"`,
		`<button class="primary" name="decision" value="approve">`,
	} {
		if !strings.Contains(body, want) {
			t.Errorf("consent page lacks %q\n%s", want, body)
		}
	}
	if strings.Contains(body, "ZgotmplZ") {
		t.Errorf("html/template rejected a value:\n%s", body)
	}
	if cc := rec.Header().Get("Cache-Control"); cc != "no-store" {
		t.Errorf("Cache-Control = %q, want no-store", cc)
	}
}
