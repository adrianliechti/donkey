package server

import (
	"context"
	"errors"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/adrianliechti/donkey/internal/config"
	"github.com/adrianliechti/donkey/pkg/runtime"
)

type platformTestRuntime struct {
	*spaceTestRuntime
	mu              sync.Mutex
	saved           *config.PlatformConfig
	err             error
	initializations int
}

func (r *platformTestRuntime) PlatformConfig(context.Context) (config.PlatformConfig, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.err != nil {
		return config.PlatformConfig{}, r.err
	}
	if r.saved == nil {
		return config.PlatformConfig{}, runtime.ErrPlatformNotFound
	}
	return *r.saved, nil
}
func (r *platformTestRuntime) InitPlatformConfig(_ context.Context, settings config.PlatformConfig) (config.PlatformConfig, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.saved == nil {
		r.saved = &settings
		r.initializations++
	}
	return *r.saved, nil
}
func (r *platformTestRuntime) SetPlatformConfig(_ context.Context, settings config.PlatformConfig) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.saved = &settings
	return nil
}

func TestFirstVerifiedUserBecomesPlatformAdminAndCanDelegate(t *testing.T) {
	s, spaces := spaceTestServer()
	r := &platformTestRuntime{spaceTestRuntime: spaces}
	s.rt = r
	for _, token := range []string{"", "garbage"} {
		req := httptest.NewRequest("GET", "/v1/spaces", nil)
		req.Header.Set("X-Access-Token", token)
		req.Header.Set("X-Auth-Request-User", "first@example.com")
		rec := httptest.NewRecorder()
		s.routes().ServeHTTP(rec, req)
		if rec.Code != 401 || r.initializations != 0 {
			t.Fatal("an unverified request initialized admins")
		}
	}
	if rec := callAs(t, s, "first@example.com", "GET", "/v1/platform/settings", nil); rec.Code != 200 || !strings.Contains(rec.Body.String(), "first@example.com") {
		t.Fatalf("first sign-in: %d %s", rec.Code, rec.Body.String())
	}
	if rec := callAs(t, s, "second@example.com", "GET", "/v1/platform/settings", nil); rec.Code != 403 {
		t.Fatal("later sign-in became admin")
	}
	add := []byte(`{"admins":{"users":["first@example.com","second@example.com"]}}`)
	if rec := callAs(t, s, "first@example.com", "PUT", "/v1/platform/settings", add); rec.Code != 200 {
		t.Fatalf("delegate: %d %s", rec.Code, rec.Body.String())
	}
	if rec := callAs(t, s, "second@example.com", "GET", "/v1/platform/settings", nil); rec.Code != 200 {
		t.Fatal("new admin could not manage settings")
	}
	if rec := callAs(t, s, "second@example.com", "PUT", "/v1/platform/settings", []byte(`{"admins":{}}`)); rec.Code != 400 {
		t.Fatal("removed all admins")
	}
	if rec := callAs(t, s, "second@example.com", "PUT", "/v1/platform/settings", []byte(`{"admins":{"users":["second@example.com"]}}`)); rec.Code != 200 {
		t.Fatal("could not remove another admin")
	}
	if rec := callAs(t, s, "first@example.com", "GET", "/v1/platform/settings", nil); rec.Code != 403 {
		t.Fatal("revoked admin retained access")
	}
	if r.initializations != 1 {
		t.Fatal("bootstrap ran again")
	}
	r.err = errors.New("storage unavailable")
	if rec := callAs(t, s, "second@example.com", "GET", "/v1/spaces", nil); rec.Code != 502 {
		t.Fatal("storage error granted access")
	}
}

func TestPlatformAdminSeedOnlyAppliesBeforeInitialization(t *testing.T) {
	s, spaces := spaceTestServer()
	r := &platformTestRuntime{spaceTestRuntime: spaces}
	s.rt = r
	s.cfg.PlatformAdmins = &config.AccessConfig{Users: []string{"seed@example.com"}}
	if rec := callAs(t, s, "visitor@example.com", "GET", "/v1/platform/settings", nil); rec.Code != 403 {
		t.Fatal("seed ignored on first sign-in")
	}
	if rec := callAs(t, s, "seed@example.com", "GET", "/v1/platform/settings", nil); rec.Code != 200 {
		t.Fatal("seed admin missing")
	}
	s.cfg.PlatformAdmins = &config.AccessConfig{Users: []string{"visitor@example.com"}}
	if rec := callAs(t, s, "visitor@example.com", "GET", "/v1/platform/settings", nil); rec.Code != 403 {
		t.Fatal("seed overwrote saved admins")
	}
}
