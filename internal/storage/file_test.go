package storage

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strconv"
	"sync"
	"testing"
	"time"

	"github.com/gofrs/flock"
)

func TestFileConcurrentTransactionsAndRollback(t *testing.T) {
	path := filepath.Join(t.TempDir(), "nested", "state.json")
	var wg sync.WaitGroup
	for range 30 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			// Independent store instances still serialize through the OS lock.
			if err := (&File{Path: path}).Update(context.Background(), func(raw []byte) ([]byte, error) {
				n := 0
				if len(raw) > 0 {
					var err error
					n, err = strconv.Atoi(string(raw))
					if err != nil {
						return nil, err
					}
				}
				return []byte(strconv.Itoa(n + 1)), nil
			}); err != nil {
				t.Error(err)
			}
		}()
	}
	wg.Wait()
	if err := (&File{Path: path}).Update(context.Background(), func(raw []byte) ([]byte, error) {
		if string(raw) != "30" {
			t.Fatalf("lost update: %q", raw)
		}
		return []byte("broken"), errors.New("abort")
	}); err == nil {
		t.Fatal("transaction succeeded despite callback error")
	}
	raw, err := os.ReadFile(path)
	if err != nil || string(raw) != "30" {
		t.Fatalf("failed transaction committed: %q %v", raw, err)
	}
	info, err := os.Stat(path)
	if err != nil || info.Mode().Perm() != 0o600 {
		t.Fatal("store file is not private")
	}
}

func TestFileLockHonorsCancellation(t *testing.T) {
	path := filepath.Join(t.TempDir(), "state.json")
	lock := flock.New(path + ".lock")
	if err := lock.Lock(); err != nil {
		t.Fatal(err)
	}
	defer lock.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel()
	called := false
	err := (&File{Path: path}).Update(ctx, func([]byte) ([]byte, error) { called = true; return []byte("new"), nil })
	if err == nil || called {
		t.Fatal("transaction ignored the held lock or context deadline")
	}
}
