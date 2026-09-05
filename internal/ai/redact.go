package ai

import (
	"regexp"
	"slices"
	"strings"
)

// Failure evidence is the one place real credentials would reach the model:
// donkey puts a full DATABASE_URL into every app with a postgres addon, and
// most frameworks print it when they cannot connect — the very condition that
// starts a diagnosis or repair. Scrubbing also lives at the model boundary,
// so a caller cannot accidentally send unredacted runtime evidence.

const redacted = "***"

// Below this length a known value ("app", "5432") appears in ordinary log text
// too often to blank safely.
const minKnownSecret = 8

var (
	// Password matched greedily to the last "@" of the authority, so one
	// containing "@" leaves no tail; "/" and whitespace end it.
	credentialsInURL = regexp.MustCompile(`(?i)\b(postgresql|postgres|mysql|mongodb\+srv|mongodb|rediss|redis|amqps|amqp|https|http)://([^\s:/?#@]+):([^\s/?#]*)@`)

	// Takes the rest of the line: the value is scheme plus token, and half a
	// token is still the half that matters.
	authorizationHeader = regexp.MustCompile(`(?i)\b(authorization)(["']?\s*[:=]\s*)[^\r\n]+`)

	bearerToken = regexp.MustCompile(`(?i)\b(bearer)\s+[A-Za-z0-9\-._~+/]{8,}=*`)

	secretAssignment = regexp.MustCompile(`(?i)\b([\w.-]*(?:pgpassword|password|passwd|secret|api[_-]?key|apikey|token|authorization|bearer)[\w.-]*)(["']?\s*[:=]\s*)("[^"]*"|'[^']*'|[^\s,;]+)`)
)

// Redact strips credentials from text. Connection strings keep their host and
// lose their user and password; anything assigned to a credential-shaped key
// loses its value. Text holding no secret comes back unchanged.
func Redact(s string) string {
	if s == "" {
		return s
	}
	s = credentialsInURL.ReplaceAllString(s, "${1}://"+redacted+":"+redacted+"@")
	s = authorizationHeader.ReplaceAllString(s, "${1}${2}"+redacted)
	s = bearerToken.ReplaceAllString(s, "${1} "+redacted)
	s = secretAssignment.ReplaceAllString(s, "${1}${2}"+redacted)
	return s
}

// RedactEvidence is the shared boundary for all model-bound runtime evidence,
// including automatic repair attempts, rather than just final diagnosis.
func RedactEvidence(s string, known ...string) string {
	return Redact(replaceKnown(s, usableSecrets(known)))
}

// usableSecrets drops values too short to replace safely, longest first so a
// value containing another is replaced whole rather than cut into pieces.
func usableSecrets(known []string) []string {
	out := make([]string, 0, len(known))
	for _, v := range known {
		if len(v) >= minKnownSecret {
			out = append(out, v)
		}
	}
	slices.SortFunc(out, func(a, b string) int { return len(b) - len(a) })
	return out
}

func replaceKnown(s string, known []string) string {
	for _, v := range known {
		s = strings.ReplaceAll(s, v, redacted)
	}
	return s
}

// redact copies rather than rewriting in place: the caller shows the same
// evidence to the user, who is entitled to read their own logs in full.
func (fc *FailureContext) redact() *FailureContext {
	known := usableSecrets(fc.KnownSecrets)
	scrub := func(s string) string { return Redact(replaceKnown(s, known)) }

	out := *fc
	out.KnownSecrets = nil
	out.Logs = scrub(fc.Logs)
	out.DonkeyYAML = scrub(fc.DonkeyYAML)
	out.Events = nil
	for _, e := range fc.Events {
		out.Events = append(out.Events, scrub(e))
	}
	// What the assistant reads through its own tools is deliberately NOT
	// scrubbed: it answers with a file's complete new content, which donkey
	// may write back, so `SECRET_KEY = ***` would come back and destroy the
	// real value while looking like a clean edit. Source is protected instead
	// by deniedFile, which keeps credentials unreadable in the first place.
	return &out
}
