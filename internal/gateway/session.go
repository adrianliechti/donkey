package gateway

import (
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/adrianliechti/donkey/internal/identity"
)

// session lives sealed inside the cookie, so any replica can answer any
// request. It is the identity a CLI or OAuth token carries too. Expires gets
// no silent refresh: the next navigation goes through the IdP again. A
// rotated IdP refresh token re-seals it.
type session = identity.Identity

// loginState rides in a short-lived cookie between the IdP redirect and the
// callback.
type loginState struct {
	State string `json:"state"`
	Nonce string `json:"nonce"`
	// Host is where the visitor was: the central callback serves every
	// hostname of the base domain, so the path alone no longer says.
	Host string `json:"host,omitempty"`
	// Return is a path on Host, never a full URL — no open redirect.
	Return string `json:"return"`
	// CLIPort: the callback mints a sealed token for the CLI waiting on
	// 127.0.0.1:<CLIPort>.
	CLIPort  int    `json:"cli,omitempty"`
	Expires  int64  `json:"exp"`
	MaxAge   *int64 `json:"max_age,omitempty"`
	Verifier string `json:"verifier,omitempty"`
}

const (
	sessionCookie = "donkey_session"
	stateCookie   = "donkey_login"

	sessionTTL = 12 * time.Hour
	loginTTL   = 15 * time.Minute
)

// Each sealed value carries its purpose as the AEAD's associated data, so
// one cannot stand in for another. Both cookies use the same key and are
// plain JSON, and every field of each is optional to the other — so without
// this binding a sealed loginState opens as a valid blank session. The
// gateway hands that cookie to every anonymous visitor, which made it an
// unauthenticated way into any protected app.
const (
	purposeSession = "donkey/session/v1"
	purposeLogin   = "donkey/login/v1"
)

// A session carrying the IdP's refresh token outgrows the ~4 KB browsers keep
// per cookie, so the sealed value is split: donkey_session, donkey_session_1,
// and so on. Small sessions stay one cookie, exactly as before.
const (
	sessionChunkSize = 3800
	sessionChunks    = 6
)

// sessionCookieNames are every name a session may occupy.
func sessionCookieNames() []string {
	names := []string{sessionCookie}
	for i := 1; i < sessionChunks; i++ {
		names = append(names, sessionCookie+"_"+strconv.Itoa(i))
	}
	return names
}

// sessionFrom returns nil for missing, tampered or expired alike.
func (s *Server) sessionFrom(r *http.Request) *session {
	var value strings.Builder
	for _, name := range sessionCookieNames() {
		c, err := r.Cookie(name)
		if err != nil {
			break
		}
		value.WriteString(c.Value)
	}
	if value.Len() == 0 {
		return nil
	}
	var sess session
	if identity.Unseal(s.cfg.SessionKey, purposeSession, value.String(), &sess) != nil {
		return nil
	}
	// A blank subject is not a visitor the IdP ever vouched for.
	if sess.Subject == "" {
		return nil
	}
	if time.Now().Unix() >= sess.Expires {
		return nil
	}
	return &sess
}

// cookieDomain: hosts of the base domain share cookies, which is what makes
// one login cover every app. Custom domains keep theirs to themselves.
func (s *Server) cookieDomain(host string) string {
	if s.cfg.ssoHost(host) {
		return s.cfg.Domain
	}
	return ""
}

func (s *Server) setSession(w http.ResponseWriter, r *http.Request, sess *session, domain string) error {
	value, err := identity.Seal(s.cfg.SessionKey, purposeSession, sess)
	if err != nil {
		return err
	}
	names := sessionCookieNames()
	if len(value) > sessionChunkSize*len(names) {
		return fmt.Errorf("the session is too large for its cookies (%d bytes)", len(value))
	}
	for _, name := range names {
		if value == "" {
			// A larger session before this one may have left chunks behind;
			// a stale tail would corrupt the next read.
			if _, err := r.Cookie(name); err == nil {
				clearCookie(w, name, "/", domain)
			}
			continue
		}
		chunk := value[:min(len(value), sessionChunkSize)]
		value = value[len(chunk):]
		http.SetCookie(w, &http.Cookie{
			Name:   name,
			Value:  chunk,
			Path:   "/",
			Domain: domain,
			// Lax keeps the IdP redirect working.
			Secure:   true,
			HttpOnly: true,
			SameSite: http.SameSiteLaxMode,
			MaxAge:   int(sessionTTL / time.Second),
		})
	}
	return nil
}

// clearSession removes every chunk the browser sent.
func clearSession(w http.ResponseWriter, r *http.Request, domain string) {
	for _, name := range sessionCookieNames() {
		if _, err := r.Cookie(name); err == nil || name == sessionCookie {
			clearCookie(w, name, "/", domain)
		}
	}
}

// clearCookie needs the Path and Domain the cookie was set with: different
// attributes create a sibling instead of deleting anything.
func clearCookie(w http.ResponseWriter, name, path, domain string) {
	http.SetCookie(w, &http.Cookie{
		Name: name, Value: "", Path: path, Domain: domain,
		Secure: true, HttpOnly: true, SameSite: http.SameSiteLaxMode, MaxAge: -1,
	})
}

// identityFrom: the session cookie (a browser) or a sealed CLI token. The
// bool reports the token case — that header is the gateway's to consume and
// must not travel on to the app.
func (s *Server) identityFrom(r *http.Request) (*session, bool) {
	if sess := s.sessionFrom(r); sess != nil {
		return sess, false
	}
	tok, ok := strings.CutPrefix(r.Header.Get("Authorization"), "Bearer ")
	if !ok || tok == "" {
		return nil, false
	}
	id, err := identity.VerifyToken(s.cfg.SessionKey, tok)
	if err != nil {
		return nil, false
	}
	return id, true
}
