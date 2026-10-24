package server

// Viewers get only operational summaries. Even GET endpoints can expose
// secrets, logs, build contents, files or service data, so use an allowlist.
func viewerRoute(pattern string) bool {
	switch pattern {
	case "GET /v1/spaces/{space}/apps/{app}/status",
		"GET /v1/spaces/{space}/traffic",
		"GET /v1/spaces/{space}/apps/{app}/traffic",
		"GET /v1/spaces/{space}/apps/{app}/resources",
		"GET /v1/spaces/{space}/apps/{app}/analytics",
		"GET /v1/spaces/{space}/apps/{app}/analytics/live":
		return true
	}
	return false
}
