package packages

import (
	"encoding/json"
	"io"
	"net/url"
	"strings"
	"testing"
	"time"
)

func npmRequest(t *testing.T) proxyRequest {
	t.Helper()
	u, err := url.Parse("https://registry.example/npm")
	if err != nil {
		t.Fatal(err)
	}
	return proxyRequest{route: proxyRoute{prefix: "/npm/", upstream: u, allowed: map[string]bool{originKey(u): true}}, target: u, public: "http://donkey-packages/npm"}
}

func TestNPMMetadataStreamsBeforeUpstreamCompletes(t *testing.T) {
	upstream, source := io.Pipe()
	body := streamNPMMetadata(upstream, npmRequest(t))
	defer body.Close()
	defer source.Close()
	written := make(chan error, 1)
	go func() {
		// More than the output buffer, but deliberately no closing bracket:
		// waiting for the whole response here would deadlock.
		_, err := io.WriteString(source, `[`+strings.Repeat(`{"dist":{"tarball":"https://registry.example/npm/pkg/-/pkg.tgz"}},`, 1000))
		written <- err
	}()
	read := make(chan error, 1)
	go func() {
		data := make([]byte, 5000)
		_, err := io.ReadFull(body, data)
		if err == nil && !strings.Contains(string(data), "http://donkey-packages/npm/pkg/-/pkg.tgz") {
			err = io.ErrUnexpectedEOF
		}
		read <- err
	}()
	select {
	case err := <-read:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("metadata was buffered until the upstream response finished")
	}
	_ = body.Close()
	select {
	case <-written:
	case <-time.After(5 * time.Second):
		t.Fatal("closing the response did not cancel its upstream read")
	}
}

func TestNPMMetadataPreservesValuesAndRewritesTarballs(t *testing.T) {
	input := `{"versions":{"1.0":{"dist":{"tarball":"https://registry.example/npm/pkg/-/pkg.tgz","integrity":"sha512-a/b+c="},"external":{"tarball":"https://cdn.example/pkg.tgz"},"values":[9007199254740993,1.50e2,true,null,"escaped \"text\""]}},"https://registry.example/npm/key":"unchanged key"}`
	body := streamNPMMetadata(io.NopCloser(strings.NewReader(input)), npmRequest(t))
	defer body.Close()
	data, err := io.ReadAll(body)
	if err != nil {
		t.Fatal(err)
	}
	if !json.Valid(data) {
		t.Fatalf("invalid metadata: %s", data)
	}
	for _, want := range []string{"http://donkey-packages/npm/pkg/-/pkg.tgz", "http://donkey-packages/npm/-/resource/", "9007199254740993", "1.50e2", "sha512-a/b+c=", `"https://registry.example/npm/key"`} {
		if !strings.Contains(string(data), want) {
			t.Errorf("metadata lost %q: %s", want, data)
		}
	}
}

func TestNPMMetadataRejectsTruncatedAndTrailingJSON(t *testing.T) {
	for _, input := range []string{`{"versions":`, `{} {}`, `[1,2`, `{"key":"unterminated`} {
		body := streamNPMMetadata(io.NopCloser(strings.NewReader(input)), npmRequest(t))
		_, err := io.ReadAll(body)
		body.Close()
		if err == nil {
			t.Errorf("accepted incomplete or invalid metadata %q", input)
		}
	}
}
