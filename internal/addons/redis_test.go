package addons

import (
	"net/url"
	"strings"
	"testing"
)

func TestRedisBindingAndAuthentication(t *testing.T) {
	r := redis{}
	in := input
	in.Name, in.Version, in.Password = "cache", r.DefaultVersion(), "p@ss/word,with punctuation"
	values := r.Binding(in)
	u, err := url.Parse(values["REDIS_URL"])
	if err != nil {
		t.Fatal(err)
	}
	password, _ := u.User.Password()
	if password != in.Password || values["REDIS_ADDR"] != "cache.donkey-prod.svc:6379" || values["REDIS_DB"] != "0" || values["REDIS_TLS"] != "false" {
		t.Fatal("Redis binding lost its address, credentials or defaults")
	}
	w, err := r.Workload(in)
	if err != nil {
		t.Fatal(err)
	}
	if len(w.Command) == 0 || !strings.Contains(strings.Join(w.Command, " "), "$REDIS_PASSWORD") || strings.Contains(strings.Join(w.Command, " "), in.Password) {
		t.Fatal("the Redis command must authenticate through its secret environment")
	}
	if w.SecretEnv["REDIS_PASSWORD"] != r.CredentialKey() || w.DataPath != "/data" || !strings.Contains(strings.Join(w.Command, " "), "--appendonly yes") {
		t.Fatal("Redis must keep authenticated, persistent data")
	}
}

func TestRedisExternalBinding(t *testing.T) {
	r := redis{}
	for _, raw := range []string{"redis://cache", "rediss://user:p%40ss@[::1]:6380/2"} {
		values, err := r.ExternalBinding(raw)
		if err != nil {
			t.Fatal(err)
		}
		for key := range r.Binding(input) {
			if _, exists := values[key]; !exists {
				t.Errorf("external Redis is missing %s", key)
			}
		}
		if raw == "redis://cache" {
			if values["REDIS_ADDR"] != "cache:6379" || values["REDIS_DB"] != "0" || values["REDIS_USERNAME"] != "default" {
				t.Fatal("missing external defaults")
			}
		} else if values["REDIS_ADDR"] != "[::1]:6380" || values["REDIS_PASSWORD"] != "p@ss" || values["REDIS_TLS"] != "true" || values["REDIS_DB"] != "2" {
			t.Fatal("external Redis lost IPv6, TLS, credentials or database")
		}
	}
	for _, raw := range []string{"http://cache", "redis:///0", "redis://cache/-1", "redis://cache/abc", "redis://cache:0", "redis://cache:65536", "redis://cache/0#fragment"} {
		if _, err := r.ExternalBinding(raw); err == nil {
			t.Errorf("invalid Redis URL accepted: %s", raw)
		}
	}
}
