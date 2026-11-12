package analytics

import (
	"bytes"
	"compress/gzip"
	"io"
	"mime"
	"net/http"
	"strings"
)

// scanLimit bounds how much of a document is held back looking for <head>.
// The tag sits in the first bytes of any page; a document without one in
// this much is passed on untouched rather than delayed.
const scanLimit = 64 << 10

// Injectable says the gateway may add the script to this response: a
// successful HTML document, in an encoding it can read, that has not asked
// intermediaries to leave it alone (Cache-Control: no-transform), and whose
// Content-Security-Policy lets a same-origin script load and send.
func Injectable(res *http.Response) bool {
	if res.StatusCode != http.StatusOK {
		return false
	}
	mediaType, _, _ := mime.ParseMediaType(res.Header.Get("Content-Type"))
	if mediaType != "text/html" {
		return false
	}
	if encoding := res.Header.Get("Content-Encoding"); encoding != "" && !strings.EqualFold(encoding, "identity") {
		return false
	}
	for _, value := range res.Header.Values("Cache-Control") {
		if strings.Contains(strings.ToLower(value), "no-transform") {
			return false
		}
	}
	return PolicyAllows(res.Header)
}

// PolicyAllows reads every enforced Content-Security-Policy: each must allow
// a same-origin script (script-src-elem, else script-src, else default-src)
// and a same-origin beacon (connect-src, else default-src). 'strict-dynamic'
// makes browsers ignore 'self', and a nonce belongs to the app's own scripts —
// the gateway does not borrow it. Report-Only policies block nothing.
func PolicyAllows(header http.Header) bool {
	for _, value := range header.Values("Content-Security-Policy") {
		// Several policies may share one header, comma-separated.
		for _, policy := range strings.Split(value, ",") {
			directives := map[string][]string{}
			for _, directive := range strings.Split(policy, ";") {
				fields := strings.Fields(strings.ToLower(directive))
				if len(fields) == 0 {
					continue
				}
				if _, seen := directives[fields[0]]; !seen { // the first one counts
					directives[fields[0]] = fields[1:]
				}
			}
			if !sourcesAllowSelf(directive(directives, "script-src-elem", "script-src", "default-src"), true) ||
				!sourcesAllowSelf(directive(directives, "connect-src", "default-src"), false) {
				return false
			}
		}
	}
	return true
}

func directive(directives map[string][]string, names ...string) []string {
	for _, name := range names {
		if sources, ok := directives[name]; ok {
			return sources
		}
	}
	return nil // not restricted
}

func sourcesAllowSelf(sources []string, script bool) bool {
	if sources == nil {
		return true
	}
	allowed := false
	for _, source := range sources {
		switch source {
		case "'strict-dynamic'":
			if script {
				return false
			}
		case "'self'", "*", "https:":
			allowed = true
		}
	}
	return allowed
}

// Inject returns the body with ScriptTag added: right after the opening
// <head> tag, else before <body>, else after <html>. A document that already
// loads the script, or has none of those tags in its first scanLimit bytes,
// passes through unchanged. Bytes stream on as soon as the insertion point is
// known, so a server-rendered page that flushes early still does.
func Inject(body io.ReadCloser) io.ReadCloser {
	return &injector{src: body}
}

type injector struct {
	src     io.ReadCloser
	pending []byte
	scanned bool
	err     error
}

func (b *injector) Read(p []byte) (int, error) {
	if !b.scanned {
		b.scanned = true
		b.pending, b.err = b.scan()
	}
	if len(b.pending) > 0 {
		n := copy(p, b.pending)
		b.pending = b.pending[n:]
		return n, nil
	}
	if b.err != nil {
		return 0, b.err
	}
	return b.src.Read(p)
}

func (b *injector) Close() error { return b.src.Close() }

// scan reads until it knows where the tag goes, and returns what it read with
// the tag in place. A read error is kept for after those bytes.
func (b *injector) scan() ([]byte, error) {
	buf := make([]byte, 0, 4096)
	chunk := make([]byte, 4096)
	for {
		n, err := b.src.Read(chunk)
		buf = append(buf, chunk[:n]...)
		final := err != nil || len(buf) >= scanLimit
		if at, decided := insertionPoint(buf, final); decided {
			if at >= 0 {
				out := make([]byte, 0, len(buf)+len(ScriptTag))
				out = append(append(append(out, buf[:at]...), ScriptTag...), buf[at:]...)
				buf = out
			}
			if err == io.EOF {
				// Hand EOF back only once the buffered bytes are read.
				return buf, io.EOF
			}
			return buf, err
		}
	}
}

// insertionPoint finds where the tag goes in the document's first bytes.
// decided is false while more bytes could still change the answer; -1 means
// leave the document alone. The answer waits for the end of the head, where a
// script tag the app added itself would be — server-rendered pages send their
// head in one piece, so this holds nothing back that was going to stream.
func insertionPoint(doc []byte, final bool) (int, bool) {
	lower := bytes.ToLower(doc)
	_, body := tagStart(lower, "<body")
	if !final && !body && !bytes.Contains(lower, []byte("</head")) {
		return 0, false
	}
	if bytes.Contains(lower, []byte(ScriptPath)) {
		return -1, true
	}
	if at, complete, found := afterTag(lower, "<head"); found && complete {
		return at, true
	}
	if at, found := tagStart(lower, "<body"); found {
		return at, true
	}
	if at, complete, found := afterTag(lower, "<html"); found && complete {
		return at, true
	}
	return -1, true
}

// tagStart finds an opening tag by name — "<head", but not "<header".
func tagStart(doc []byte, name string) (int, bool) {
	for from := 0; ; {
		i := bytes.Index(doc[from:], []byte(name))
		if i < 0 {
			return 0, false
		}
		i += from
		end := i + len(name)
		if end >= len(doc) {
			return i, true // cut off right after the name; the closing check decides
		}
		switch doc[end] {
		case '>', ' ', '\t', '\n', '\r', '\f', '/':
			return i, true
		}
		from = end
	}
}

// afterTag is the index just past the opening tag's '>'. complete is false
// when the tag has started but its '>' has not arrived yet.
func afterTag(doc []byte, name string) (at int, complete, found bool) {
	start, found := tagStart(doc, name)
	if !found {
		return 0, false, false
	}
	end := bytes.IndexByte(doc[start:], '>')
	if end < 0 {
		return 0, false, true
	}
	return start + end + 1, true, true
}

// Gzip compresses a body as it streams, flushing after every read so a
// response the app sends in pieces still arrives in pieces.
func Gzip(body io.ReadCloser) io.ReadCloser {
	reader, writer := io.Pipe()
	go func() {
		defer body.Close()
		gz := gzip.NewWriter(writer)
		buf := make([]byte, 32<<10)
		for {
			n, err := body.Read(buf)
			if n > 0 {
				if _, werr := gz.Write(buf[:n]); werr != nil {
					writer.CloseWithError(werr)
					return
				}
				if ferr := gz.Flush(); ferr != nil {
					writer.CloseWithError(ferr)
					return
				}
			}
			if err == io.EOF {
				writer.CloseWithError(gz.Close())
				return
			}
			if err != nil {
				writer.CloseWithError(err)
				return
			}
		}
	}()
	return &pipeBody{PipeReader: reader, src: body}
}

// pipeBody closes the upstream body as well, so a client that goes away
// stops the copy instead of leaving it blocked on the pipe.
type pipeBody struct {
	*io.PipeReader
	src io.Closer
}

func (p *pipeBody) Close() error {
	_ = p.src.Close()
	return p.PipeReader.Close()
}

// AcceptsGzip reads the client's Accept-Encoding.
func AcceptsGzip(r *http.Request) bool {
	for _, value := range r.Header.Values("Accept-Encoding") {
		for _, part := range strings.Split(value, ",") {
			coding, params, _ := strings.Cut(strings.TrimSpace(part), ";")
			if !strings.EqualFold(strings.TrimSpace(coding), "gzip") {
				continue
			}
			q := strings.ReplaceAll(strings.TrimSpace(params), " ", "")
			return q != "q=0" && q != "q=0.0" && q != "q=0.00" && q != "q=0.000"
		}
	}
	return false
}
