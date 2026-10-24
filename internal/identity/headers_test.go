package identity

import (
	"net/http"
	"testing"
)

func TestStripHeadersDropsEveryIdentityName(t *testing.T) {
	h := http.Header{"X-Auth-Token": {"keep"}, "Authorization": {"keep"}, "X-Forwarded-For": {"keep"}, "X-Forwarded-Proto": {"keep"}}
	h.Set("X-Auth-Request-User", "forged")
	h.Set("X-Auth-Request-Something-Else", "forged")
	h.Set("X-Forwarded-User", "forged")
	h.Set("X-Forwarded-Access-Token", "forged")
	h["x-auth-request-redirect"] = []string{"forged"}
	h["x-forwarded-email"] = []string{"forged"}
	StripHeaders(h)
	if len(h) != 4 || h.Get("X-Auth-Token") != "keep" || h.Get("X-Forwarded-For") != "keep" {
		t.Fatalf("headers = %v", h)
	}
}

func TestSetHeaderWritesBothFamiliesSafely(t *testing.T) {
	h := http.Header{}
	SetHeader(h, "Name", "Ada\r\nX-Injected: 1\tLovelace")
	SetHeader(h, "Groups", "")
	if h.Get("X-Forwarded-Name") != "AdaX-Injected: 1Lovelace" || h.Get("X-Auth-Request-Name") != h.Get("X-Forwarded-Name") {
		t.Fatalf("name = %v", h)
	}
	if _, ok := h["X-Forwarded-Groups"]; ok {
		t.Error("an empty value must not become a header")
	}
	SetHeader(h, "User", "sub")
	SetForwardedHeader(h, "User", "oid")
	SetForwardedHeader(h, "User", "")
	if h.Get("X-Forwarded-User") != "oid" || h.Get("X-Auth-Request-User") != "sub" {
		t.Errorf("user = %q / %q", h.Get("X-Forwarded-User"), h.Get("X-Auth-Request-User"))
	}
}
