package filetransfer

import (
	"crypto/sha256"
	"errors"
	"fmt"
	"io"
	"strings"
	"testing"
	"testing/iotest"
)

type failedReader struct{ err error }

func (r failedReader) Read([]byte) (int, error) { return 0, r.err }

func TestWriteRequiresCompleteStreamAndMatchingDiskContents(t *testing.T) {
	readErr := errors.New("upload connection lost")
	checksum := func(s string) string { return fmt.Sprintf("%x  -\n", sha256.Sum256([]byte(s))) }
	for _, tc := range []struct {
		name    string
		source  io.Reader
		consume int64
		output  string
		wantErr bool
	}{
		{"complete", strings.NewReader("contents"), -1, checksum("contents"), false},
		{"final bytes with EOF", iotest.DataErrReader(strings.NewReader("contents")), -1, checksum("contents"), false},
		{"empty", strings.NewReader(""), -1, checksum(""), false},
		{"read error swallowed by exec", io.MultiReader(strings.NewReader("part"), failedReader{readErr}), -1, checksum("part"), true},
		{"exec stops reading early", strings.NewReader("contents"), 3, checksum("con"), true},
		{"disk truncated after full read", strings.NewReader("contents"), -1, checksum("con"), true},
		{"same size corruption", strings.NewReader("contents"), -1, checksum("CONTENTS"), true},
		{"missing checksum", strings.NewReader("contents"), -1, "", true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			err := Write(tc.source, "/data/.upload", func(_ []string, stdin io.Reader, stdout io.Writer) error {
				if tc.consume >= 0 {
					stdin = io.LimitReader(stdin, tc.consume)
				}
				_, _ = io.Copy(io.Discard, stdin)
				io.WriteString(stdout, tc.output)
				return nil // Deliberately model client-go swallowing the read error.
			})
			if (err != nil) != tc.wantErr {
				t.Fatalf("Write error = %v, want error %v", err, tc.wantErr)
			}
			if tc.name == "read error swallowed by exec" && !errors.Is(err, readErr) {
				t.Fatalf("lost source read error: %v", err)
			}
		})
	}
}
