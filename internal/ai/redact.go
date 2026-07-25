package ai

import (
	"regexp"
	"sort"
	"strings"
)

// Failure evidence is the one place where real credentials would reach the
// model. donkey puts a full connection string into every app with a postgres
// addon (DATABASE_URL, password included), and Django, Rails, SQLAlchemy and
// node-postgres all print that string when they cannot connect — which is
// exactly the condition that starts a diagnosis. So logs and events are
// scrubbed inside DiagnoseFailure rather than at its callers: a caller that
// forgets is a leak, and there is nothing about a call site that makes the
// scrubbing optional.
//
// Two passes run, cheapest trust first. Values donkey already knows are
// secret (env values, addon bindings) are replaced literally — that catches
// what no pattern can, including a password that looks like a word. The
// regexes then cover the rest: credentials in URLs, and the assignment forms
// (`KEY=value`, `key: value`) whose key names a credential.
//
// Deliberately absent: a rule for "long high-entropy strings". Build logs and
// cluster events are full of git shas, image digests, container ids and
// Kubernetes UIDs — the very identifiers that make a diagnosis useful — and
// such a rule would blank them all while adding nothing the two passes above
// don't already catch. KnownSecrets is the answer to what regexes miss.

const redacted = "***"

// minKnownSecret is the length below which a known value is left alone. A
// short one ("app", "5432", a one-character password) appears in ordinary log
// text all the time, and asterisking every occurrence would destroy the
// evidence the diagnosis is made of.
const minKnownSecret = 8

var (
	// credentialsInURL covers the shape that leaks in practice: a connection
	// string carrying user and password. Only schemes that really carry
	// credentials are listed — a rule for any "scheme://" would rewrite half
	// of a build log. The password part is matched greedily up to the last
	// "@" of the authority, so a password containing "@" leaves no tail
	// behind; "/" and whitespace end it, so a second URL on the same line is
	// its own match.
	credentialsInURL = regexp.MustCompile(`(?i)\b(postgresql|postgres|mysql|mongodb\+srv|mongodb|rediss|redis|amqps|amqp|https|http)://([^\s:/?#@]+):([^\s/?#]*)@`)

	// authorizationHeader takes the rest of the line, because the value is a
	// scheme plus a token ("Bearer eyJ…") and keeping half of it would keep
	// the half that matters.
	authorizationHeader = regexp.MustCompile(`(?i)\b(authorization)(["']?\s*[:=]\s*)[^\r\n]+`)

	// bearerToken catches the token where it appears without its header, e.g.
	// in a curl command line echoed by a build.
	bearerToken = regexp.MustCompile(`(?i)\b(bearer)\s+[A-Za-z0-9\-._~+/]{8,}=*`)

	// secretAssignment matches "key=value" and "key: value" wherever the key
	// names a credential, with or without decoration around it: PGPASSWORD,
	// DB_PASSWORD, api-key, "token" in a JSON log line. The value ends at
	// whitespace (or its closing quote), so the rest of the line survives.
	secretAssignment = regexp.MustCompile(`(?i)\b([\w.-]*(?:pgpassword|password|passwd|secret|api[_-]?key|apikey|token|authorization|bearer)[\w.-]*)(["']?\s*[:=]\s*)("[^"]*"|'[^']*'|[^\s,;]+)`)
)

// Redact removes credentials from a piece of text: connection strings keep
// their host (which is what a diagnosis needs) and lose their user and
// password, and anything assigned to a credential-shaped key loses its value.
// It is safe to run over text that holds no secret at all — ordinary log
// lines come back unchanged.
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

// usableSecrets drops the known values that are too short to replace safely
// and sorts the rest longest first, so a value that contains another one is
// replaced whole instead of being cut into pieces.
func usableSecrets(known []string) []string {
	out := make([]string, 0, len(known))
	for _, v := range known {
		if len(v) >= minKnownSecret {
			out = append(out, v)
		}
	}
	sort.Slice(out, func(i, j int) bool { return len(out[i]) > len(out[j]) })
	return out
}

// replaceKnown blanks every occurrence of a value donkey knows is secret.
func replaceKnown(s string, known []string) string {
	for _, v := range known {
		s = strings.ReplaceAll(s, v, redacted)
	}
	return s
}

// redact returns a scrubbed copy of the context. It copies instead of
// rewriting in place because the caller shows the same evidence to the user,
// who is entitled to read their own logs in full — only the trip to the model
// endpoint is redacted.
func (fc *FailureContext) redact() *FailureContext {
	known := usableSecrets(fc.KnownSecrets)
	scrub := func(s string) string { return Redact(replaceKnown(s, known)) }

	out := *fc
	out.KnownSecrets = nil
	out.Logs = scrub(fc.Logs)
	// donkey.yaml can carry an env block, so it gets the same treatment.
	out.DonkeyYAML = scrub(fc.DonkeyYAML)
	out.Events = nil
	for _, e := range fc.Events {
		out.Events = append(out.Events, scrub(e))
	}
	return &out
}
