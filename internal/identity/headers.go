package identity

import (
	"net/http"
	"slices"
	"strings"
)

// A gateway speaks for the visitor under oauth2-proxy's names: its
// proxy-mode X-Forwarded-*, which apps read first, and its auth-request
// X-Auth-Request-*, kept as a fallback. Apps may trust names donkey never
// sets in either family, so no client gets to send any of them.
const (
	ForwardedPrefix   = "X-Forwarded-"
	AuthRequestPrefix = "X-Auth-Request-"
)

// Strip identity header names even when donkey never sets them: apps may
// trust them, so accepting client-supplied values would allow impersonation.
var identityHeaderNames = []string{"User", "Email", "Preferred-Username", "Name", "Groups", "Roles", "Access-Token"}

// SetHeader sets one identity value under both prefixes. Control characters
// are dropped: a display name with a line break must not fail the request.
func SetHeader(h http.Header, name, value string) {
	if value = headerValue(value); value != "" {
		h.Set(ForwardedPrefix+name, value)
		h.Set(AuthRequestPrefix+name, value)
	}
}

// SetForwardedHeader overrides one value under X-Forwarded-* only, where the
// families differ: the fallback keeps what it always carried.
func SetForwardedHeader(h http.Header, name, value string) {
	if value = headerValue(value); value != "" {
		h.Set(ForwardedPrefix+name, value)
	}
}

func headerValue(value string) string {
	return strings.Map(func(r rune) rune {
		if r < ' ' || r == 0x7f {
			return -1
		}
		return r
	}, value)
}

// StripHeaders removes every X-Auth-Request-* and the X-Forwarded-* identity
// names; X-Forwarded-For, -Host and -Proto are not identity and stay.
func StripHeaders(h http.Header) {
	for name := range h {
		canonical := http.CanonicalHeaderKey(name)
		if strings.HasPrefix(canonical, AuthRequestPrefix) ||
			strings.HasPrefix(canonical, ForwardedPrefix) && slices.Contains(identityHeaderNames, strings.TrimPrefix(canonical, ForwardedPrefix)) {
			delete(h, name)
		}
	}
}
