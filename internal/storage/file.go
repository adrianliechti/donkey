// Package storage provides runtime-independent persistent transactions.
package storage

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"time"

	"github.com/gofrs/flock"
)

// Store runs a read-modify-write transaction. A failed callback commits
// nothing; an error from Update means callers must not publish its result.
type Store interface {
	Update(context.Context, func([]byte) ([]byte, error)) error
}

// File stores a single record using an OS lock and durable atomic replacement.
// All processes must use the same path on a filesystem supporting flock and
// atomic rename. A local volume with one gateway is the default deployment.
type File struct{ Path string }

func (f *File) Update(ctx context.Context, fn func([]byte) ([]byte, error)) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	dirPath := filepath.Dir(f.Path)
	if err := os.MkdirAll(dirPath, 0o700); err != nil {
		return err
	}
	lock := flock.New(f.Path + ".lock")
	defer lock.Close()
	locked, err := lock.TryLockContext(ctx, 20*time.Millisecond)
	if err != nil {
		return err
	}
	if !locked {
		return fmt.Errorf("state lock unavailable")
	}
	defer lock.Unlock()
	path := f.Path
	raw, err := os.ReadFile(path)
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	updated, err := fn(append([]byte(nil), raw...))
	if err != nil || bytes.Equal(raw, updated) {
		return err
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	tmp, err := os.CreateTemp(dirPath, ".state-*")
	if err != nil {
		return err
	}
	defer os.Remove(tmp.Name())
	defer tmp.Close()
	if _, err = tmp.Write(updated); err != nil {
		return err
	}
	if err = tmp.Sync(); err != nil {
		return err
	}
	if err = tmp.Close(); err != nil {
		return err
	}
	if err = os.Rename(tmp.Name(), path); err != nil {
		return err
	}
	dir, err := os.Open(dirPath)
	if err != nil {
		return err
	}
	defer dir.Close()
	return dir.Sync()
}
