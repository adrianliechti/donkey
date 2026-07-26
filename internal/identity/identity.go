// Package identity is the sealed credential donkey's browser login mints for
// the CLI: what the IdP said about a person, encrypted-and-authenticated
// with the installation's session key. The gateway mints and verifies these
// tokens; the server verifies them too for direct (port-forwarded) access —
// one key, one format, no state. Rotating the session key revokes every
// token at once.
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

// Identity is what a completed login learned about the person.
type Identity struct {
	Subject string   `json:"sub"`
	Email   string   `json:"email,omitempty"`
	Name    string   `json:"name,omitempty"`
	Groups  []string `json:"groups,omitempty"`
	Roles   []string `json:"roles,omitempty"`
	// Expires is Unix seconds; a token never outlives it.
	Expires int64 `json:"exp"`
}

// TokenTTL is how long a CLI login lasts. Long on purpose — a laptop should
// not re-open the browser daily — and bounded because sealed tokens cannot
// be revoked individually.
const TokenTTL = 30 * 24 * time.Hour

// purposeCLI rides as the AEAD's associated data: the gateway's cookies are
// sealed with the same key, and this binding is what keeps a token from
// being replayed as a cookie or the other way round.
const purposeCLI = "donkey/cli/v1"

// Seal encrypts-and-authenticates v (as JSON) under key, binding purpose as
// the AEAD's associated data — a value sealed for one purpose never opens
// under another. This is the one sealed-value implementation donkey has;
// the gateway's cookies and the CLI tokens all go through it.
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

// Unseal opens a value sealed for exactly this purpose; tampered, mis-keyed
// and mis-purposed all fail the same way.
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

// SealToken turns an identity into a CLI token.
func SealToken(key []byte, id Identity) (string, error) {
	return Seal(key, purposeCLI, id)
}

// VerifyToken opens a CLI token. Every token donkey mints carries the ID
// token's subject — a blank one was never vouched for by the IdP.
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
