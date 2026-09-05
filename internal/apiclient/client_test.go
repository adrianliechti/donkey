package apiclient

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestDeployAcceptsLargeRecipeMessages(t *testing.T) {
	// Source patches and generated recipes may exceed Scanner's old 4 MiB
	// ceiling. Losing the stream at that point hid an otherwise valid result.
	line := strings.Repeat("generated source\n", 350000)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		enc := json.NewEncoder(w)
		_ = enc.Encode(map[string]any{"line": line})
		_ = enc.Encode(map[string]any{"result": map[string]any{"ok": true}})
	}))
	defer server.Close()
	client, err := New(server.URL, "test", "")
	if err != nil {
		t.Fatal(err)
	}
	received := ""
	result, err := client.Deploy(context.Background(), strings.NewReader(""), DeployOpts{}, func(s string) { received = s }, func(string) {})
	if err != nil || result == nil || !result.OK || received != line {
		t.Fatalf("large deployment message was lost: result=%+v, err=%v, received=%d bytes", result, err, len(received))
	}
}
