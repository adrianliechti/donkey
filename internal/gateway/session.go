package gateway

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/adrianliechti/donkey/internal/config"
)

// session is what a completed login learned about the visitor. It lives
// sealed inside the cookie — the gateway keeps no state, so any replica can
// answer any request.
type session struct {
	Subject string   `json:"sub"`
	Email   string   `json:"email,omitempty"`
	Name    string   `json:"name,omitempty"`
	Groups  []string `json:"groups,omitempty"`
	Roles   []string `json:"roles,omitempty"`
	// Expires is Unix seconds. Sessions don't refresh silently — after this,
	// the next navigation goes through the IdP again (which is instant while
	// the IdP session is alive).
	Expires int64 `json:"exp"`
}

// loginState rides in a short-lived cookie between the redirect to the IdP
// and the callback: the CSRF state, the ID-token nonce, and where to land.
type loginState struct {
	State string `json:"state"`
	Nonce string `json:"nonce"`
	// Return is a same-host path (+query) — never a full URL, so the
	// callback can't be turned into an open redirect.
	Return  string `json:"return"`
	Expires int64  `json:"exp"`
}

const (
	sessionCookie = "donkey_session"
	stateCookie   = "donkey_login"

	sessionTTL = 12 * time.Hour
	loginTTL   = 15 * time.Minute
)

// sealer authenticates-and-encrypts small JSON payloads into cookie values.
type sealer struct {
	aead cipher.AEAD
}

func newSealer(key []byte) *sealer {
	block, err := aes.NewCipher(key)
	if err != nil {
		panic(err) // key length is validated by FromEnv
	}
	aead, err := cipher.NewGCM(block)
	if err != nil {
		panic(err)
	}
	return &sealer{aead: aead}
}

func (s *sealer) seal(v any) (string, error) {
	plain, err := json.Marshal(v)
	if err != nil {
		return "", err
	}
	nonce := make([]byte, s.aead.NonceSize())
	if _, err := rand.Read(nonce); err != nil {
		return "", err
	}
	sealed := s.aead.Seal(nonce, nonce, plain, nil)
	return base64.RawURLEncoding.EncodeToString(sealed), nil
}

func (s *sealer) unseal(value string, v any) error {
	raw, err := base64.RawURLEncoding.DecodeString(value)
	if err != nil {
		return err
	}
	if len(raw) < s.aead.NonceSize() {
		return fmt.Errorf("sealed value too short")
	}
	plain, err := s.aead.Open(nil, raw[:s.aead.NonceSize()], raw[s.aead.NonceSize():], nil)
	if err != nil {
		return err
	}
	return json.Unmarshal(plain, v)
}

// sessionFrom reads and validates the visitor's session; nil means "not
// signed in" (missing, tampered or expired — all the same to the caller).
func (s *Server) sessionFrom(r *http.Request) *session {
	c, err := r.Cookie(sessionCookie)
	if err != nil {
		return nil
	}
	var sess session
	if s.sealer.unseal(c.Value, &sess) != nil {
		return nil
	}
	if time.Now().Unix() >= sess.Expires {
		return nil
	}
	return &sess
}

func (s *Server) setSession(w http.ResponseWriter, sess *session) error {
	value, err := s.sealer.seal(sess)
	if err != nil {
		return err
	}
	http.SetCookie(w, &http.Cookie{
		Name:  sessionCookie,
		Value: value,
		Path:  "/",
		// Secure: the browser only ever talks to the shared Gateway over
		// https; this hop is behind it. Lax keeps the IdP redirect working.
		Secure:   true,
		HttpOnly: true,
		SameSite: http.SameSiteLaxMode,
		MaxAge:   int(sessionTTL / time.Second),
	})
	return nil
}

func clearCookie(w http.ResponseWriter, name string) {
	http.SetCookie(w, &http.Cookie{
		Name: name, Value: "", Path: "/",
		Secure: true, HttpOnly: true, SameSite: http.SameSiteLaxMode, MaxAge: -1,
	})
}

// allowedAccess reports whether the session's identity satisfies the route's
// access rules — by matching any of them. Emails compare case-insensitively
// (IdPs disagree on casing); groups and roles compare exactly (they are
// identifiers, often GUIDs).
func allowedAccess(access *config.AccessConfig, sess *session) bool {
	if access.Empty() {
		return true
	}
	for _, u := range access.Users {
		if sess.Email != "" && strings.EqualFold(u, sess.Email) {
			return true
		}
	}
	return anyOverlap(access.Groups, sess.Groups) || anyOverlap(access.Roles, sess.Roles)
}

func anyOverlap(wanted, have []string) bool {
	if len(wanted) == 0 || len(have) == 0 {
		return false
	}
	mine := make(map[string]bool, len(have))
	for _, v := range have {
		mine[v] = true
	}
	for _, v := range wanted {
		if mine[v] {
			return true
		}
	}
	return false
}
