// Command mint-token seals a donkey CLI token from an installation's session
// key ($DONKEY_SESSION_KEY, 32 bytes hex — the `session-key` entry of the
// donkey Secret). It is the scripted stand-in for the browser
// round trip `donkey login` runs: e2e and the demo cannot click an IdP, but
// they can read the cluster's key. DEMO/TEST tooling — a human signs in
// through the browser.
package main

import (
	"encoding/hex"
	"fmt"
	"os"
	"time"

	"github.com/adrianliechti/donkey/internal/identity"
)

func main() {
	key, err := hex.DecodeString(os.Getenv("DONKEY_SESSION_KEY"))
	if err != nil || len(key) != 32 {
		fmt.Fprintln(os.Stderr, "DONKEY_SESSION_KEY must be 32 bytes of hex (the session-key entry of donkey)")
		os.Exit(1)
	}
	// MINT_TOKEN_EMAIL names who the token is for — an installation that has
	// seen its first sign-in has its admin, and nobody else manages spaces.
	email := os.Getenv("MINT_TOKEN_EMAIL")
	if email == "" {
		email = "mint-token@localhost"
	}
	tok, err := identity.SealToken(key, identity.Identity{
		Subject: "mint-token",
		Email:   email,
		Expires: time.Now().Add(12 * time.Hour).Unix(),
	})
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	fmt.Print(tok)
}
