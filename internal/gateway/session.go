package gateway

import (
	"net/http"
	"strings"
	"time"

	"github.com/adrianliechti/donkey/internal/identity"
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
	Return string `json:"return"`
	// CLIPort is set when `donkey login` started this round trip: the
	// callback then mints a sealed CLI token and hands it to the CLI
	// waiting on 127.0.0.1:<CLIPort>.
	CLIPort int   `json:"cli,omitempty"`
	Expires int64 `json:"exp"`
}

const (
	sessionCookie = "donkey_session"
	stateCookie   = "donkey_login"

	sessionTTL = 12 * time.Hour
	loginTTL   = 15 * time.Minute
)

// Every sealed value (identity.Seal) carries the purpose it was minted for
// as the AEAD's associated data, so nothing can stand in for anything else.
// Both cookies are sealed with the same key and both are plain JSON, and
// the two structs share the "exp" field while every other field of each is
// optional to the other — so without this binding a sealed loginState opens
// as a perfectly valid (if blank) session, and a sealed session opens as a
// loginState whose State and Nonce are empty. The gateway hands the
// loginState cookie to every anonymous visitor before they sign in, which
// made that first direction an unauthenticated way into any protected app.
// (The CLI tokens carry their own purpose inside the identity package.)
const (
	purposeSession = "donkey/session/v1"
	purposeLogin   = "donkey/login/v1"
)

// sessionFrom reads and validates the visitor's session; nil means "not
// signed in" (missing, tampered or expired — all the same to the caller).
func (s *Server) sessionFrom(r *http.Request) *session {
	c, err := r.Cookie(sessionCookie)
	if err != nil {
		return nil
	}
	var sess session
	if identity.Unseal(s.cfg.SessionKey, purposeSession, c.Value, &sess) != nil {
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
	value, err := identity.Seal(s.cfg.SessionKey, purposeSession, sess)
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

// identityFrom is who the request is: the gateway's session cookie (a
// browser), or a sealed CLI token in the Authorization header (`donkey
// login`). The bool reports the token case — the header was the gateway's
// to consume then, and must not travel on to the app.
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
	return &session{
		Subject: id.Subject, Email: id.Email, Name: id.Name,
		Groups: id.Groups, Roles: id.Roles, Expires: id.Expires,
	}, true
}
