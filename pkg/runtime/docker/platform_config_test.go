package docker

import (
	"context"
	"errors"
	"fmt"
	"os"
	"reflect"
	"sync"
	"testing"

	"github.com/adrianliechti/donkey/internal/config"
	rt "github.com/adrianliechti/donkey/pkg/runtime"
)

func TestPlatformConfigInitializationAndPersistence(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	r := &Runtime{state: state{dir: dir}}
	if _, err := r.PlatformConfig(ctx); !errors.Is(err, rt.ErrPlatformNotFound) {
		t.Fatalf("missing: %v", err)
	}
	var wg sync.WaitGroup
	observed := make([]config.PlatformConfig, 16)
	for i := range 16 {
		wg.Go(func() {
			seed := config.PlatformConfig{Admins: config.AccessConfig{Users: []string{fmt.Sprintf("user%d@example.com", i)}}}
			settings, err := r.InitPlatformConfig(ctx, seed)
			if err != nil {
				t.Error(err)
			}
			observed[i] = settings
		})
	}
	wg.Wait()
	first, err := r.PlatformConfig(ctx)
	for _, settings := range observed {
		if !reflect.DeepEqual(settings, first) {
			t.Fatalf("concurrent sign-ins received different admins: %+v and %+v", settings, first)
		}
	}
	if err != nil || len(first.Admins.Users) != 1 {
		t.Fatalf("first admin: %+v, %v", first, err)
	}
	updated := config.PlatformConfig{Admins: config.AccessConfig{Users: append(first.Admins.Users, "added@example.com")}}
	if err := r.SetPlatformConfig(ctx, updated); err != nil {
		t.Fatal(err)
	}
	restarted := &Runtime{state: state{dir: dir}}
	got, err := restarted.InitPlatformConfig(ctx, first)
	if err != nil || !reflect.DeepEqual(got, updated) {
		t.Fatalf("restart reset admins: %+v, %v", got, err)
	}
	if err := os.WriteFile(r.state.path("platform", "config.json"), []byte("{}"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := restarted.InitPlatformConfig(ctx, first); err == nil {
		t.Fatal("empty persisted settings triggered a new bootstrap")
	}
}
