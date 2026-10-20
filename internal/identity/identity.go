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

type Identity struct {
	Subject string   `json:"sub"`
	Email   string   `json:"email,omitempty"`
	Name    string   `json:"name,omitempty"`
	Groups  []string `json:"groups,omitempty"`
	Roles   []string `json:"roles,omitempty"`
	// Expires (Unix seconds); a token never outlives it.
	Expires int64 `json:"exp"`
	// RefreshToken is the IdP's, kept when the installation names a token
	// scope so donkey can mint access tokens for it (see Upstream). Sealed
	// like the rest: it never reaches an app, or the CLI's disk, in the clear.
	RefreshToken string `json:"rt,omitempty"`
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
