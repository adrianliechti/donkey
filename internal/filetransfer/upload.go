// Package filetransfer verifies streamed uploads without buffering their contents.
package filetransfer

import (
	"bytes"
	"cmp"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"hash"
	"io"
	"strings"
	"sync"
)

// uploadReader records completion separately from the command's exit status:
// client-go logs stdin copy failures but can still report a successful exec.
// Its stdin copy runs on a goroutine that may outlive the exec, so the mutex
// orders the fields against it.
type uploadReader struct {
	io.Reader
	mu   sync.Mutex
	hash hash.Hash
	err  error
}

func (r *uploadReader) Read(p []byte) (int, error) {
	n, err := r.Reader.Read(p)
	r.mu.Lock()
	defer r.mu.Unlock()
	r.hash.Write(p[:n])
	if err != nil {
		r.err = err
	}
	return n, err
}

// Write stores the stream in a temporary file and verifies its contents on the
// disk. The caller only moves it over the destination after this succeeds.
func Write(r io.Reader, temp string, exec func([]string, io.Reader, io.Writer) error) error {
	upload := &uploadReader{Reader: r, hash: sha256.New()}
	// BusyBox dd replaces fd 1 when opening of=, closing the exec stdout
	// pipe before it reads stdin. Containerd closes exec stdin when either
	// output pipe closes, so a direct dd can exit zero with a truncated file.
	// The shell holds stdout open while cat writes, then checksums the disk
	// file in the same exec. Pass the path as an argument, never as shell
	// source; checksum stdin so filenames cannot change the output format.
	var out bytes.Buffer
	if err := exec([]string{"sh", "-c", `cat > "$1" && sha256sum < "$1"`, "upload", temp}, upload, &out); err != nil {
		return err
	}
	upload.mu.Lock()
	defer upload.mu.Unlock()
	if upload.err != io.EOF {
		return fmt.Errorf("the upload stopped before it was complete: %w", cmp.Or(upload.err, io.ErrUnexpectedEOF))
	}
	if !strings.HasPrefix(out.String(), hex.EncodeToString(upload.hash.Sum(nil))+" ") {
		return fmt.Errorf("the uploaded file's checksum doesn't match — the original file was kept; try uploading again")
	}
	return nil
}
