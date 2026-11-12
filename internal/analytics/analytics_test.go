package analytics

import (
	"bytes"
	"compress/gzip"
	"io"
	"net/http"
	"net/url"
	"strings"
	"testing"
	"testing/iotest"
)

func TestParseUserAgent(t *testing.T) {
	for _, tc := range []struct {
		ua   string
		want Agent
	}{
		{"Mozilla/5.0 (Macintosh; Intel Mac OS X 10_15_7) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/141.0.0.0 Safari/537.36",
			Agent{Browser: "Chrome", OS: "macOS", Device: DeviceDesktop}},
		{"Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/141.0.0.0 Safari/537.36 Edg/141.0.0.0",
			Agent{Browser: "Edge", OS: "Windows", Device: DeviceDesktop}},
		{"Mozilla/5.0 (iPhone; CPU iPhone OS 18_5 like Mac OS X) AppleWebKit/605.1.15 (KHTML, like Gecko) Version/18.5 Mobile/15E148 Safari/604.1",
			Agent{Browser: "Safari", OS: "iOS", Device: DeviceMobile}},
		{"Mozilla/5.0 (iPhone; CPU iPhone OS 18_5 like Mac OS X) AppleWebKit/605.1.15 (KHTML, like Gecko) CriOS/141.0 Mobile/15E148 Safari/604.1",
			Agent{Browser: "Chrome", OS: "iOS", Device: DeviceMobile}},
		{"Mozilla/5.0 (Linux; Android 15; Pixel 9) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/141.0.0.0 Mobile Safari/537.36",
			Agent{Browser: "Chrome", OS: "Android", Device: DeviceMobile}},
		{"Mozilla/5.0 (Linux; Android 14; SM-X710) AppleWebKit/537.36 (KHTML, like Gecko) SamsungBrowser/27.0 Chrome/125.0.0.0 Safari/537.36",
			Agent{Browser: "Samsung Internet", OS: "Android", Device: DeviceTablet}},
		{"Mozilla/5.0 (X11; Ubuntu; Linux x86_64; rv:143.0) Gecko/20100101 Firefox/143.0",
			Agent{Browser: "Firefox", OS: "Linux", Device: DeviceDesktop}},
		{"Mozilla/5.0 (compatible; Googlebot/2.1; +http://www.google.com/bot.html)", Agent{Bot: true}},
		{"Mozilla/5.0 (X11; Linux x86_64) AppleWebKit/537.36 (KHTML, like Gecko) HeadlessChrome/141.0.0.0 Safari/537.36", Agent{Bot: true}},
		{"curl/8.7.1", Agent{Bot: true}},
		{"", Agent{Bot: true}},
	} {
		if got := ParseUserAgent(tc.ua); got != tc.want {
			t.Errorf("%s\n  = %+v, want %+v", tc.ua, got, tc.want)
		}
	}
	// iPadOS asks for desktop sites as a Mac; its screen gives it away.
	mac := ParseUserAgent("Mozilla/5.0 (Macintosh; Intel Mac OS X 10_15_7) AppleWebKit/605.1.15 (KHTML, like Gecko) Version/18.5 Safari/605.1.15")
	if got := mac.WithScreen(820); got.Device != DeviceTablet || got.OS != "iOS" {
		t.Errorf("an iPad-sized Mac = %+v", got)
	}
	if got := mac.WithScreen(1512); got.Device != DeviceDesktop || got.OS != "macOS" {
		t.Errorf("a MacBook = %+v", got)
	}
}

func TestDimensions(t *testing.T) {
	for zone, want := range map[string]string{
		"Europe/Zurich": "CH", "America/New_York": "US", "Asia/Calcutta": "IN", "Asia/Kolkata": "IN", "UTC": "", "Mars/Olympus": "",
	} {
		if got := Country(zone); got != want {
			t.Errorf("Country(%q) = %q, want %q", zone, got, want)
		}
	}
	for tag, want := range map[string]string{
		"de-CH": "de-CH", "en": "en", "zh-Hant-TW": "zh-TW", "es-419": "es-419", "EN_us": "en-US", "x": "", "<script>": "",
	} {
		if got := Language(tag); got != want {
			t.Errorf("Language(%q) = %q, want %q", tag, got, want)
		}
	}
	for _, tc := range []struct{ referrer, source, medium, want string }{
		{"", "", "", ChannelDirect},
		{"google.co.uk", "", "", ChannelSearch},
		{"gemini.google.com", "", "", ChannelAI},
		{"chatgpt.com", "", "", ChannelAI},
		{"mail.google.com", "", "", ChannelEmail},
		{"news.ycombinator.com", "", "", ChannelSocial},
		{"blog.example.org", "", "", ChannelReferral},
		{"google.com", "google", "cpc", ChannelPaid},
		{"", "newsletter", "email", ChannelEmail},
		{"", "producthunt.com", "", ChannelReferral},
		{"", "linkedin.com", "", ChannelSocial},
	} {
		if got := Channel(tc.referrer, tc.source, tc.medium); got != tc.want {
			t.Errorf("Channel(%q, %q, %q) = %q, want %q", tc.referrer, tc.source, tc.medium, got, tc.want)
		}
	}
	for raw, want := range map[string]string{
		"https://www.google.com/search?q=secret": "google.com",
		"https://shop.acme.example.com/cart":     "",
		"android-app://com.google.android.gm/":   "com.google.android.gm",
		"javascript:alert(1)":                    "",
		"":                                       "",
	} {
		if got := ReferrerHost(raw, "shop.acme.example.com"); got != want {
			t.Errorf("ReferrerHost(%q) = %q, want %q", raw, got, want)
		}
	}
	for raw, want := range map[string]string{
		"https://x.example.com/pricing?token=secret#plans": "/pricing",
		"https://x.example.com":                            "/",
		"https://x.example.com/#/app/acme/shop?tab=1":      "/#/app/acme/shop",
		"https://x.example.com/docs#!/intro":               "/docs#!/intro",
	} {
		u, _ := url.Parse(raw)
		if got := PagePath(u); got != want {
			t.Errorf("PagePath(%q) = %q, want %q", raw, got, want)
		}
	}
	query, _ := url.ParseQuery("ref=producthunt&utm_campaign=launch")
	if source, medium, campaign := Campaign(query); source != "producthunt" || medium != "" || campaign != "launch" {
		t.Errorf("Campaign = %q %q %q", source, medium, campaign)
	}
}

func TestPolicyAllows(t *testing.T) {
	for policy, want := range map[string]bool{
		"":                                      true,
		"default-src 'self'":                    true,
		"default-src 'none'; script-src 'self'": false, // no connect-src: the beacon is blocked
		"default-src 'none'; script-src 'self'; connect-src 'self'": true,
		"script-src 'nonce-abc' 'strict-dynamic'":                   false,
		"script-src 'self' https://cdn.example.com":                 true,
		"script-src https://cdn.example.com":                        false,
		"img-src *":                                                 true,
		"default-src 'self', script-src 'none'":                     false, // two policies, both enforced
		"frame-ancestors 'none'":                                    true,
	} {
		header := http.Header{}
		if policy != "" {
			header.Set("Content-Security-Policy", policy)
		}
		if got := PolicyAllows(header); got != want {
			t.Errorf("%q = %v, want %v", policy, got, want)
		}
	}
	reportOnly := http.Header{"Content-Security-Policy-Report-Only": {"script-src 'none'"}}
	if !PolicyAllows(reportOnly) {
		t.Error("a report-only policy blocks nothing")
	}
}

func TestInjectable(t *testing.T) {
	response := func(status int, header ...string) *http.Response {
		res := &http.Response{StatusCode: status, Header: http.Header{}}
		for i := 0; i+1 < len(header); i += 2 {
			res.Header.Add(header[i], header[i+1])
		}
		return res
	}
	for name, tc := range map[string]struct {
		res  *http.Response
		want bool
	}{
		"html":         {response(200, "Content-Type", "text/html; charset=utf-8"), true},
		"json":         {response(200, "Content-Type", "application/json"), false},
		"not found":    {response(404, "Content-Type", "text/html"), false},
		"compressed":   {response(200, "Content-Type", "text/html", "Content-Encoding", "br"), false},
		"no-transform": {response(200, "Content-Type", "text/html", "Cache-Control", "public, no-transform"), false},
		"strict csp":   {response(200, "Content-Type", "text/html", "Content-Security-Policy", "script-src 'none'"), false},
	} {
		if got := Injectable(tc.res); got != tc.want {
			t.Errorf("%s: %v, want %v", name, got, tc.want)
		}
	}
}

func TestInject(t *testing.T) {
	for _, tc := range []struct{ name, in, want string }{
		{"head", "<!doctype html><html><head><title>x</title></head><body>hi</body></html>",
			"<!doctype html><html><head>" + ScriptTag + "<title>x</title></head><body>hi</body></html>"},
		{"head with attributes", `<HTML lang="en"><HEAD data-x="1"><meta charset="utf-8">`,
			`<HTML lang="en"><HEAD data-x="1">` + ScriptTag + `<meta charset="utf-8">`},
		{"header is not head", "<html><body><header>x</header></body></html>",
			"<html>" + ScriptTag + "<body><header>x</header></body></html>"},
		{"no head", `<html lang="de"><p>hi`, `<html lang="de">` + ScriptTag + `<p>hi`},
		{"fragment", "<div>partial</div>", "<div>partial</div>"},
		{"already there", `<head><script defer src="/_donkey/s.js"></script></head>`, `<head><script defer src="/_donkey/s.js"></script></head>`},
		{"empty", "", ""},
	} {
		// One byte per read: the tag must be found across read boundaries.
		got, err := io.ReadAll(Inject(io.NopCloser(iotest.OneByteReader(strings.NewReader(tc.in)))))
		if err != nil {
			t.Fatalf("%s: %v", tc.name, err)
		}
		if string(got) != tc.want {
			t.Errorf("%s:\n got %s\nwant %s", tc.name, got, tc.want)
		}
	}

	// A long document streams past the scan; the tail is never held back.
	long := "<html><head></head><body>" + strings.Repeat("x", 3*scanLimit) + "</body></html>"
	got, err := io.ReadAll(Inject(io.NopCloser(strings.NewReader(long))))
	if err != nil || !strings.HasPrefix(string(got), "<html><head>"+ScriptTag) || len(got) != len(long)+len(ScriptTag) {
		t.Fatalf("long document: %d bytes, %v", len(got), err)
	}
	// Without any tag in the scan window, the document is left as it is.
	plain := strings.Repeat("y", scanLimit+10) + "<head>"
	got, _ = io.ReadAll(Inject(io.NopCloser(strings.NewReader(plain))))
	if string(got) != plain {
		t.Fatal("a document without an early <head> must pass unchanged")
	}
	// A read error surfaces after the bytes read before it.
	broken := io.MultiReader(strings.NewReader("<head>"), iotest.ErrReader(io.ErrUnexpectedEOF))
	got, err = io.ReadAll(Inject(io.NopCloser(broken)))
	if err != io.ErrUnexpectedEOF || string(got) != "<head>"+ScriptTag {
		t.Fatalf("broken upstream: %q %v", got, err)
	}
}

func TestGzip(t *testing.T) {
	doc := strings.Repeat("<p>donkey</p>", 1000)
	zr, err := gzip.NewReader(Gzip(io.NopCloser(strings.NewReader(doc))))
	if err != nil {
		t.Fatal(err)
	}
	got, err := io.ReadAll(zr)
	if err != nil || string(got) != doc {
		t.Fatalf("round trip: %d bytes, %v", len(got), err)
	}

	// Each upstream write is flushed: the first piece decodes before the
	// rest exists.
	upstream, write := io.Pipe()
	body := Gzip(upstream)
	go func() { _, _ = write.Write([]byte("<head>first")) }()
	zr, err = gzip.NewReader(body)
	if err != nil {
		t.Fatal(err)
	}
	first := make([]byte, len("<head>first"))
	if _, err := io.ReadFull(zr, first); err != nil || !bytes.Equal(first, []byte("<head>first")) {
		t.Fatalf("first piece: %q %v", first, err)
	}
	_ = write.Close()
	_ = body.Close()

	for header, want := range map[string]bool{
		"gzip, deflate, br": true, "br": false, "gzip;q=0": false, "GZIP; q=0.5": true, "": false,
	} {
		r := &http.Request{Header: http.Header{"Accept-Encoding": {header}}}
		if got := AcceptsGzip(r); got != want {
			t.Errorf("AcceptsGzip(%q) = %v, want %v", header, got, want)
		}
	}
}
