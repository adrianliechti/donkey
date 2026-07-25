package ai

import (
	"strings"
	"testing"
)

// The failure that starts a diagnosis is usually the failure that prints the
// credential: donkey injects DATABASE_URL with the addon password into every
// app, and the common frameworks echo it back when they cannot connect.
func TestRedactCredentials(t *testing.T) {
	cases := []struct {
		name    string
		in      string
		leaked  []string
		wantSub string
	}{{
		name:    "postgres url in a log line",
		in:      `django.db.OperationalError: could not connect to postgres://donkey:hunter2@shop-db.donkey-acme.svc:5432/app?sslmode=disable`,
		leaked:  []string{"hunter2", "donkey:hunter2"},
		wantSub: "postgres://***:***@shop-db.donkey-acme.svc:5432/app",
	}, {
		name:    "password with an @ in it leaves no tail",
		in:      `connection failed: postgresql://app:p@ssw0rd@db.example.com:5432/app`,
		leaked:  []string{"p@ssw0rd", "ssw0rd"},
		wantSub: "postgresql://***:***@db.example.com:5432/app",
	}, {
		name:    "env assignment",
		in:      "env: PGPASSWORD=hunter2 PGHOST=shop-db",
		leaked:  []string{"hunter2"},
		wantSub: "PGPASSWORD=***",
	}, {
		name:    "yaml-style assignment",
		in:      "  api_key: sk-live-4f2a9c1b03de",
		leaked:  []string{"sk-live-4f2a9c1b03de"},
		wantSub: "api_key: ***",
	}, {
		name:    "json log line",
		in:      `{"level":"error","token":"eyJhbGciOiJIUzI1NiJ9.abc","msg":"login failed"}`,
		leaked:  []string{"eyJhbGciOiJIUzI1NiJ9.abc"},
		wantSub: `"msg":"login failed"`,
	}, {
		name:    "bearer token header",
		in:      `curl failed: 401 for Authorization: Bearer eyJhbGciOiJIUzI1NiJ9.abcdefghij.KLMNOP`,
		leaked:  []string{"eyJhbGciOiJIUzI1NiJ9", "abcdefghij", "KLMNOP"},
		wantSub: "Authorization: ***",
	}, {
		name:    "bare bearer token without its header",
		in:      `retrying with bearer sk-proj-0123456789abcdef`,
		leaked:  []string{"sk-proj-0123456789abcdef"},
		wantSub: "bearer ***",
	}}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := Redact(tc.in)
			for _, leak := range tc.leaked {
				if strings.Contains(got, leak) {
					t.Errorf("Redact(%q) still contains %q:\n%s", tc.in, leak, got)
				}
			}
			if !strings.Contains(got, tc.wantSub) {
				t.Errorf("Redact(%q) = %q, want it to contain %q", tc.in, got, tc.wantSub)
			}
		})
	}
}

// Redaction must not eat the evidence. A diagnosis is only useful while the
// stack trace, the host names and the identifiers are still readable.
func TestRedactLeavesOrdinaryLogsAlone(t *testing.T) {
	logs := strings.Join([]string{
		`2026-07-25T14:30:22Z INFO starting gunicorn on http://0.0.0.0:8080`,
		`GET /healthz 200 in 3ms`,
		`Back-off restarting failed container shop in pod shop-7d4b8c6f9-2xkzl`,
		`Traceback (most recent call last):`,
		`  File "/app/manage.py", line 22, in <module>`,
		`ImportError: No module named 'psycopg2'`,
		`image: registry.example.com/donkey/shop:4f2a9c1b03de-20260724143022`,
		`connection to shop-db.donkey-acme.svc:5432 refused`,
	}, "\n")

	if got := Redact(logs); got != logs {
		t.Errorf("ordinary log text was rewritten:\n--- before ---\n%s\n--- after ---\n%s", logs, got)
	}
}

// KnownSecrets is what catches the password no pattern recognizes — here one
// that looks like a word, printed in the middle of a sentence.
func TestRedactKnownSecrets(t *testing.T) {
	fc := &FailureContext{
		Stage:  "rollout",
		Logs:   "FATAL: password authentication failed for user \"donkey\" (tried correcthorsebattery)",
		Events: []string{`Error: secret "shop-db" has value correcthorsebattery`, "BackOff: restarting"},
		// Short values must be ignored: blanking "app" everywhere would
		// destroy the logs.
		KnownSecrets: []string{"correcthorsebattery", "app"},
	}

	got := fc.redact()
	if strings.Contains(got.Logs, "correcthorsebattery") {
		t.Errorf("known secret survived in logs: %s", got.Logs)
	}
	if !strings.Contains(got.Logs, "password authentication failed") {
		t.Errorf("the useful part of the line was lost: %s", got.Logs)
	}
	if strings.Contains(strings.Join(got.Events, "\n"), "correcthorsebattery") {
		t.Errorf("known secret survived in events: %v", got.Events)
	}
	if !strings.Contains(got.Events[1], "BackOff: restarting") {
		t.Errorf("an event without secrets was rewritten: %q", got.Events[1])
	}
	if strings.Contains(got.Events[0], "***-db") || !strings.Contains(got.Events[0], "shop-db") {
		t.Errorf("a short known value should be ignored, got %q", got.Events[0])
	}
	if got.KnownSecrets != nil {
		t.Error("the redacted copy must not carry the secret list along")
	}

	// The caller shows the same evidence to the user, so its own copy stays
	// untouched.
	if !strings.Contains(fc.Logs, "correcthorsebattery") || !strings.Contains(fc.Events[0], "correcthorsebattery") {
		t.Error("redact() must not rewrite the caller's context")
	}
}

// The whole point of doing this inside DiagnoseFailure: the context that
// reaches the prompt builder is already clean, whatever the caller passed.
func TestRedactCoversEveryFreeTextField(t *testing.T) {
	fc := &FailureContext{
		Stage:      "build",
		Logs:       "DATABASE_URL=postgres://donkey:hunter2@db.svc:5432/app",
		Events:     []string{"Failed to pull image: authorization: Basic ZG9ua2V5Omh1bnRlcjI="},
		DonkeyYAML: "env:\n  API_TOKEN: t0psecret-value\n",
	}
	got := fc.redact()
	for field, s := range map[string]string{"logs": got.Logs, "events": got.Events[0], "donkey.yaml": got.DonkeyYAML} {
		for _, leak := range []string{"hunter2", "ZG9ua2V5Omh1bnRlcjI=", "t0psecret-value"} {
			if strings.Contains(s, leak) {
				t.Errorf("%s still contains %q: %s", field, leak, s)
			}
		}
	}
}
