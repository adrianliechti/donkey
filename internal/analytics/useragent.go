package analytics

import "strings"

// Agent is what the reports keep of a user agent. The string itself is
// never exported.
type Agent struct {
	Browser string
	OS      string
	Device  string
	// Bot is a crawler, monitor, headless browser or HTTP library; its
	// events are dropped.
	Bot bool
}

const (
	DeviceDesktop = "desktop"
	DeviceMobile  = "mobile"
	DeviceTablet  = "tablet"
)

// botMarkers are lowercase substrings no browser a person uses sends.
var botMarkers = []string{
	"bot", "crawl", "spider", "slurp", "headless", "lighthouse", "pagespeed", "pingdom", "uptime", "monitor",
	"preview", "facebookexternalhit", "embedly", "curl/", "wget/", "python-", "go-http-client", "okhttp",
	"java/", "axios/", "node-fetch", "httpclient", "phantomjs", "selenium", "puppeteer", "playwright",
}

// ParseUserAgent classifies a user agent the way analytics needs it: a
// handful of families, not versions. Order matters — most browsers claim to
// be Chrome and Safari as well.
func ParseUserAgent(ua string) Agent {
	lower := strings.ToLower(ua)
	if !strings.HasPrefix(lower, "mozilla/") && !strings.HasPrefix(lower, "opera/") {
		return Agent{Bot: true}
	}
	for _, marker := range botMarkers {
		if strings.Contains(lower, marker) {
			return Agent{Bot: true}
		}
	}
	a := Agent{Browser: "Other", OS: "Other", Device: DeviceDesktop}
	switch {
	case strings.Contains(ua, "Edg/"), strings.Contains(ua, "EdgA/"), strings.Contains(ua, "EdgiOS/"):
		a.Browser = "Edge"
	case strings.Contains(ua, "OPR/"), strings.Contains(ua, "Opera"):
		a.Browser = "Opera"
	case strings.Contains(ua, "SamsungBrowser/"):
		a.Browser = "Samsung Internet"
	case strings.Contains(ua, "YaBrowser/"):
		a.Browser = "Yandex"
	case strings.Contains(ua, "Firefox/"), strings.Contains(ua, "FxiOS/"):
		a.Browser = "Firefox"
	case strings.Contains(ua, "CriOS/"), strings.Contains(ua, "Chrome/"), strings.Contains(ua, "Chromium/"):
		a.Browser = "Chrome"
	case strings.Contains(ua, "Safari/") && strings.Contains(ua, "Version/"):
		a.Browser = "Safari"
	}
	switch {
	case strings.Contains(ua, "iPhone"), strings.Contains(ua, "iPad"), strings.Contains(ua, "iPod"):
		a.OS = "iOS"
	case strings.Contains(ua, "Android"):
		a.OS = "Android"
	case strings.Contains(ua, "Windows"):
		a.OS = "Windows"
	case strings.Contains(ua, "CrOS"):
		a.OS = "ChromeOS"
	case strings.Contains(ua, "Macintosh"), strings.Contains(ua, "Mac OS X"):
		a.OS = "macOS"
	case strings.Contains(ua, "Linux"):
		a.OS = "Linux"
	}
	switch {
	case strings.Contains(ua, "iPad"), strings.Contains(ua, "Tablet"),
		a.OS == "Android" && !strings.Contains(ua, "Mobile"):
		a.Device = DeviceTablet
	case strings.Contains(ua, "Mobi"), strings.Contains(ua, "iPhone"), strings.Contains(ua, "iPod"):
		a.Device = DeviceMobile
	}
	return a
}

// WithScreen corrects the device by the screen the script measured: iPads
// ask for desktop sites and say Macintosh, but a Mac's screen is never a
// tablet's. Widths are CSS pixels.
func (a Agent) WithScreen(width int) Agent {
	if width <= 0 {
		return a
	}
	if a.Device == DeviceDesktop && a.OS == "macOS" && isIPadWidth(width) {
		a.Device = DeviceTablet
		a.OS = "iOS"
	} else if a.Device == DeviceDesktop && width < 600 {
		a.Device = DeviceMobile
	}
	return a
}

// isIPadWidth: the portrait and landscape CSS widths iPads report and no Mac
// display does.
func isIPadWidth(width int) bool {
	switch width {
	case 744, 768, 810, 820, 834, 1024, 1112, 1180, 1194, 1366:
		return true
	}
	return false
}
