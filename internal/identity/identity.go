// Package identity is the sealed credential the browser login mints for the
// CLI: what the IdP said, encrypted-and-authenticated under the session key.
// One key, one format, no state — rotating the key revokes every token.
package identity

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"time"
)

// Identity is what the IdP said about a visitor, in every sealed form donkey
// keeps it: the gateway's session cookie, the CLI's token, OAuth clients'
// tokens and the short-lived hop to donkey's API all carry this one shape.
type Identity struct {
	Subject       string `json:"sub"`
	Email         string `json:"email,omitempty"`
	EmailVerified *bool  `json:"email_verified,omitempty"`
	Name          string `json:"name,omitempty"`
	// PreferredUsername is the ID token's preferred_username claim, ObjectID
	// Entra's oid: the tenant-wide user ID, where sub is pairwise per app.
	PreferredUsername string   `json:"preferred_username,omitempty"`
	ObjectID          string   `json:"oid,omitempty"`
	Groups            []string `json:"groups,omitempty"`
	// Expires (Unix seconds); a token never outlives it.
	Expires int64 `json:"exp"`
	// AuthTime is the upstream's auth_time, when it supplied one. It stays
	// absent when the provider did not assert when authentication occurred.
	AuthTime int64 `json:"auth_time,omitempty"`
	// LoginTime is when the gateway finished the IdP round trip this identity
	// descends from (Unix seconds). Every credential derived from a sign-in
	// keeps it, which is what lets a credential minted from another one —
	// the sign-in handed to a personal machine — expire when a fresh login
	// at that moment would have, instead of renewing itself forever. Absent
	// on sign-ins older than the field.
	LoginTime int64 `json:"lt,omitempty"`
	// RefreshToken is the IdP's, kept when the installation names a token
	// scope so the gateway can mint access tokens for it (see Upstream), and
	// Grant names that sign-in across rotations. Sealed like the rest: it
	// never reaches an app, or the CLI's disk, in the clear.
	RefreshToken string `json:"rt,omitempty"`
	Grant        string `json:"grant,omitempty"`
	// AccessToken is the IdP's access token for auth.scope, minted by the
	// gateway into the short-lived token it forwards to donkey's API — the
	// server hands it on (Trino) and never mints one itself.
	AccessToken string `json:"at,omitempty"`
}

// Long on purpose (no daily browser round trip), bounded because sealed
// tokens cannot be revoked individually.
const TokenTTL = 30 * 24 * time.Hour

// Rides as associated data: the gateway's cookies use the same key, and this
// is what stops a token being replayed as a cookie or vice versa.
const purposeCLI = "donkey/cli/v1"

// Seal binds purpose as the AEAD's associated data, so a value sealed for one
// purpose never opens under another. Cookies and CLI tokens both go through it.
func Seal(key []byte, purpose string, v any) (string, error) {
	aead, err := newAEAD(key)
	if err != nil {
		return "", err
	}
	plain, err := json.Marshal(v)
	if err != nil {
		return "", err
	}
	nonce := make([]byte, aead.NonceSize())
	if _, err := rand.Read(nonce); err != nil {
		return "", err
	}
	sealed := aead.Seal(nonce, nonce, plain, []byte(purpose))
	return base64.RawURLEncoding.EncodeToString(sealed), nil
}

// Unseal: tampered, mis-keyed and mis-purposed all fail the same way.
func Unseal(key []byte, purpose, value string, v any) error {
	aead, err := newAEAD(key)
	if err != nil {
		return err
	}
	raw, err := base64.RawURLEncoding.DecodeString(value)
	if err != nil {
		return fmt.Errorf("not a sealed value")
	}
	if len(raw) < aead.NonceSize() {
		return fmt.Errorf("not a sealed value")
	}
	plain, err := aead.Open(nil, raw[:aead.NonceSize()], raw[aead.NonceSize():], []byte(purpose))
	if err != nil {
		return fmt.Errorf("the sealed value didn't verify")
	}
	return json.Unmarshal(plain, v)
}

func SealToken(key []byte, id Identity) (string, error) {
	return Seal(key, purposeCLI, id)
}

// VerifyToken rejects a blank subject: never vouched for by the IdP.
func VerifyToken(key []byte, token string) (*Identity, error) {
	var id Identity
	if err := Unseal(key, purposeCLI, token, &id); err != nil {
		return nil, fmt.Errorf("the token didn't verify")
	}
	if id.Subject == "" {
		return nil, fmt.Errorf("the token didn't verify")
	}
	if time.Now().Unix() >= id.Expires {
		return nil, fmt.Errorf("the login expired — run `donkey login` again")
	}
	return &id, nil
}

func newAEAD(key []byte) (cipher.AEAD, error) {
	if len(key) != 32 {
		return nil, fmt.Errorf("the session key must be 32 bytes")
	}
	block, err := aes.NewCipher(key)
	if err != nil {
		return nil, err
	}
	return cipher.NewGCM(block)
}
