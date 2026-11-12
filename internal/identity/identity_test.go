package identity

import (
	"bytes"
	"testing"
	"time"
)

func TestTokenRoundTrip(t *testing.T) {
	key := bytes.Repeat([]byte{7}, 32)
	in := Identity{Subject: "u1", Email: "ada@example.com", Groups: []string{"staff"}, Expires: time.Now().Add(time.Hour).Unix()}
	tok, err := SealToken(key, in)
	if err != nil {
		t.Fatal(err)
	}
	out, err := VerifyToken(key, tok)
	if err != nil {
		t.Fatal(err)
	}
	if out.Subject != in.Subject || out.Email != in.Email || len(out.Groups) != 1 {
		t.Errorf("round trip lost data: %+v", out)
	}
	if _, err := VerifyToken(bytes.Repeat([]byte{9}, 32), tok); err == nil {
		t.Error("a different key must not verify")
	}
	if _, err := VerifyToken(key, tok[:len(tok)-2]+"xx"); err == nil {
		t.Error("a tampered token must not verify")
	}
}

func TestExpiredTokenIsRejected(t *testing.T) {
	key := bytes.Repeat([]byte{7}, 32)
	tok, err := SealToken(key, Identity{Subject: "u1", Expires: time.Now().Add(-time.Minute).Unix()})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := VerifyToken(key, tok); err == nil {
		t.Error("an expired token must not verify")
	}
}
