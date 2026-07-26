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
	// Host is the hostname the visitor was on. The central callback serves
	// every hostname of the base domain, so the path alone no longer says
	// where to return.
	Host string `json:"host,omitempty"`
	// Return is a path (+query) on Host — never a full URL, so the callback
	// can't be turned into an open redirect.
	Return  string `json:"return"`
	Expires int64  `json:"exp"`
}

const (
	sessionCookie = "donkey_session"
	stateCookie   = "donkey_login"

	sessionTTL = 12 * time.Hour
	loginTTL   = 15 * time.Minute
)

// Sealed values carry the purpose they were minted for as the AEAD's
// associated data, so one cookie can never be replayed as the other. Both
// cookies are sealed with the same key and both are plain JSON, and the two
// structs share the "exp" field while every other field of each is optional
// to the other — so without this binding a sealed loginState opens as a
// perfectly valid (if blank) session, and a sealed session opens as a
// loginState whose State and Nonce are empty. The gateway hands the
// loginState cookie to every anonymous visitor before they sign in, which
// made that first direction an unauthenticated way into any protected app.
const (
	purposeSession = "donkey/session/v1"
	purposeLogin   = "donkey/login/v1"
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

func (s *sealer) seal(v any, purpose string) (string, error) {
	plain, err := json.Marshal(v)
	if err != nil {
		return "", err
	}
	nonce := make([]byte, s.aead.NonceSize())
	if _, err := rand.Read(nonce); err != nil {
		return "", err
	}
	sealed := s.aead.Seal(nonce, nonce, plain, []byte(purpose))
	return base64.RawURLEncoding.EncodeToString(sealed), nil
}

// unseal opens a value sealed for exactly this purpose; one sealed for
// another fails authentication here rather than decoding into the wrong type.
func (s *sealer) unseal(value, purpose string, v any) error {
	raw, err := base64.RawURLEncoding.DecodeString(value)
	if err != nil {
		return err
	}
	if len(raw) < s.aead.NonceSize() {
		return fmt.Errorf("sealed value too short")
	}
	plain, err := s.aead.Open(nil, raw[:s.aead.NonceSize()], raw[s.aead.NonceSize():], []byte(purpose))
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
	if s.sealer.unseal(c.Value, purposeSession, &sess) != nil {
		return nil
	}
	// Every session this gateway mints carries the ID token's subject. A
	// blank one is not a visitor the IdP ever vouched for.
	if sess.Subject == "" {
		return nil
	}
	if time.Now().Unix() >= sess.Expires {
		return nil
	}
	return &sess
}

// cookieDomain picks the Domain attribute for the given hostname: hosts of
// the base domain share their cookies — that is what makes one login cover
// every app — while custom domains keep theirs to themselves.
func (s *Server) cookieDomain(host string) string {
	if s.cfg.ssoHost(host) {
		return s.cfg.Domain
	}
	return ""
}

func (s *Server) setSession(w http.ResponseWriter, sess *session, domain string) error {
	value, err := s.sealer.seal(sess, purposeSession)
	if err != nil {
		return err
	}
	http.SetCookie(w, &http.Cookie{
		Name:   sessionCookie,
		Value:  value,
		Path:   "/",
		Domain: domain,
		// Secure: the browser only ever talks to the shared Gateway over
		// https; this hop is behind it. Lax keeps the IdP redirect working.
		Secure:   true,
		HttpOnly: true,
		SameSite: http.SameSiteLaxMode,
		MaxAge:   int(sessionTTL / time.Second),
	})
	return nil
}

// clearCookie needs the same Path and Domain the cookie was set with — a
// Set-Cookie under different attributes creates a sibling instead of
// deleting anything.
func clearCookie(w http.ResponseWriter, name, path, domain string) {
	http.SetCookie(w, &http.Cookie{
		Name: name, Value: "", Path: path, Domain: domain,
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
