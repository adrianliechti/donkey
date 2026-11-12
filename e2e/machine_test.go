//go:build e2e

package e2e

import (
	"encoding/hex"
	"io"
	"net/http"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/adrianliechti/donkey/internal/identity"
	"github.com/adrianliechti/donkey/internal/server"
)

// TestMachine is the life of a personal machine on a real installation:
// created through the API, reached through the gateway by its owner and by
// nobody else, stopped with its disk kept, and deleted. It is skipped where
// the installation has no machines (machines.image).
func TestMachine(t *testing.T) {
	s := newSuite(t)
	var machine server.MachineResponse
	s.raw(t, "GET", "/v1/machine", nil, &machine)
	if !machine.Enabled {
		t.Skip("this installation has no machines (machines.image is not set)")
	}
	if !machine.Allowed {
		t.Fatalf("%s may not have a machine — the suite's user has to be a platform admin or a developer of a space", s.user)
	}
	// Leftovers of an interrupted run, and the machine this run makes.
	s.rawTry("DELETE", "/v1/machine")
	t.Cleanup(func() { s.rawTry("DELETE", "/v1/machine") })

	// until follows the machine the way the console does.
	until := func(t *testing.T, want string, timeout time.Duration) server.MachineResponse {
		t.Helper()
		deadline := time.Now().Add(timeout)
		var got server.MachineResponse
		for {
			s.raw(t, "GET", "/v1/machine", nil, &got)
			if got.State == want {
				return got
			}
			if got.State == "failed" || time.Now().After(deadline) {
				t.Fatalf("the machine is %q after %s, want %q: %s", got.State, timeout, want, got.Message)
			}
			time.Sleep(2 * time.Second)
		}
	}

	// edge asks the machine's address through the gateway as one identity
	// ("" for nobody), the way a script holding a donkey token would.
	edge := func(t *testing.T, url, token string) *http.Response {
		t.Helper()
		req, err := http.NewRequest("GET", url+"/api/workspace", nil)
		if err != nil {
			t.Fatal(err)
		}
		req.Header.Set("Accept", "application/json")
		if token != "" {
			req.Header.Set("Authorization", "Bearer "+token)
		}
		resp, err := s.edge.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { resp.Body.Close() })
		return resp
	}

	t.Run("create", func(t *testing.T) {
		s.raw(t, "POST", "/v1/machine", nil, &machine)
		if machine.State == "absent" || machine.State == "stopped" {
			t.Fatalf("a started machine is %q", machine.State)
		}
		// The first start pulls the image, which is most of the wait.
		machine = until(t, "running", 10*time.Minute)
		if !strings.HasPrefix(machine.URL, "https://m") || !strings.HasSuffix(machine.URL, ".platform."+s.domain) {
			t.Fatalf("the machine answers on %q", machine.URL)
		}
		if machine.SignInExpires == "" {
			t.Error("the machine does not say when its sign-in ends")
		}
	})

	t.Run("only its owner gets in", func(t *testing.T) {
		if s.edge == nil {
			t.Skip("no gateway to go through (DONKEY_E2E_GATEWAY)")
		}
		// The route appears with the machine; the gateway may need a moment
		// to read it.
		var owner *http.Response
		for deadline := time.Now().Add(time.Minute); ; time.Sleep(2 * time.Second) {
			owner = edge(t, machine.URL, s.token)
			if owner.StatusCode == http.StatusOK || time.Now().After(deadline) {
				break
			}
		}
		body, _ := io.ReadAll(io.LimitReader(owner.Body, 1<<20))
		if owner.StatusCode != http.StatusOK || !strings.Contains(string(body), `"path":"/data/workspace"`) {
			t.Fatalf("the owner got %d: %s", owner.StatusCode, body)
		}
		// The console and the machine itself may show it in a frame.
		if got := owner.Header.Get("Content-Security-Policy"); !strings.Contains(got, "frame-ancestors 'self' https://"+s.domain) {
			t.Errorf("frame policy = %q", got)
		}

		// Somebody else with a perfectly good sign-in — the same email, even
		// a platform admin's — is not the owner.
		key, _ := hex.DecodeString(os.Getenv("DONKEY_E2E_SESSION_KEY"))
		other, err := identity.SealToken(key, identity.Identity{
			Subject: "e2e-somebody-else", Email: s.user, Expires: time.Now().Add(time.Hour).Unix(),
		})
		if err != nil {
			t.Fatal(err)
		}
		if resp := edge(t, machine.URL, other); resp.StatusCode != http.StatusForbidden {
			t.Errorf("another signed-in person got %d, want 403", resp.StatusCode)
		}
		if resp := edge(t, machine.URL, ""); resp.StatusCode != http.StatusUnauthorized {
			t.Errorf("nobody got %d, want 401", resp.StatusCode)
		}
	})

	t.Run("stop keeps the disk", func(t *testing.T) {
		s.raw(t, "POST", "/v1/machine/stop", nil, &machine)
		machine = until(t, "stopped", 2*time.Minute)
		if machine.URL != "" {
			t.Errorf("a stopped machine still has an address: %q", machine.URL)
		}
		s.raw(t, "POST", "/v1/machine", nil, &machine)
		until(t, "running", 5*time.Minute)
	})

	t.Run("delete", func(t *testing.T) {
		s.raw(t, "DELETE", "/v1/machine", nil, &machine)
		until(t, "absent", 2*time.Minute)
	})
}
