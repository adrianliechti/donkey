package docker

import (
	"context"
	"os"
	"path/filepath"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/adrianliechti/donkey/internal/config"
)

func TestGatewaySnapshotLoadsFileRoutesAndPolicy(t *testing.T) {
	dir := t.TempDir()
	if err := WriteSystemRoute(dir, "web", Route{Space: "team", App: "web", Host: "web.example", Backend: "http://web:80"}); err != nil {
		t.Fatal(err)
	}
	state := state{dir: dir}
	if err := state.writeRecord(config.SpaceConfig{Viewers: config.AccessConfig{Users: []string{"viewer@example.com"}}}, "spaces", "team", "config"); err != nil {
		t.Fatal(err)
	}
	snapshot, err := (&GatewayAdapter{StateDir: dir}).GatewaySnapshot(context.Background())
	if err != nil || len(snapshot.Routes) != 1 || len(snapshot.Spaces["team"].Viewers.Users) != 1 {
		t.Fatalf("bad snapshot: %+v %v", snapshot, err)
	}
}

func TestGatewayCAPreservesLegacyAndInitializesOnce(t *testing.T) {
	for _, legacy := range []bool{false, true} {
		dir := t.TempDir()
		if legacy {
			if err := os.MkdirAll(filepath.Join(dir, "ca"), 0700); err != nil {
				t.Fatal(err)
			}
			for name, data := range map[string]string{"ca.crt": "cert", "ca.key": "key"} {
				if err := os.WriteFile(filepath.Join(dir, "ca", name), []byte(data), 0600); err != nil {
					t.Fatal(err)
				}
			}
		}
		var calls atomic.Int64
		var wg sync.WaitGroup
		for range 10 {
			wg.Add(1)
			go func() {
				defer wg.Done()
				cert, key, err := (&GatewayAdapter{StateDir: dir}).GatewayCA(context.Background(), func() ([]byte, []byte, error) { calls.Add(1); return []byte("cert"), []byte("key"), nil })
				if err != nil || string(cert) != "cert" || string(key) != "key" {
					t.Errorf("CA changed: %q %q %v", cert, key, err)
				}
			}()
		}
		wg.Wait()
		want := int64(1)
		if legacy {
			want = 0
		}
		if calls.Load() != want {
			t.Fatalf("CA created %d times, want %d", calls.Load(), want)
		}
	}
}
