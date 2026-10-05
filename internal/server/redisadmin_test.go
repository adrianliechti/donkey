package server

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/alicebob/miniredis/v2"
	"github.com/redis/go-redis/v9"

	"github.com/adrianliechti/donkey/internal/addons"
	"github.com/adrianliechti/donkey/internal/ai"
	"github.com/adrianliechti/donkey/pkg/runtime"
)

type redisTestRuntime struct {
	planRuntime
	url string
}

func (r redisTestRuntime) ServiceBinding(_ context.Context, space, owner, name string) (map[string]string, error) {
	if space == "default" && owner == addons.SharedOwner && name == "redis" {
		return map[string]string{"REDIS_URL": r.url}, nil
	}
	return nil, nil
}

func (r redisTestRuntime) ServiceConnection(ctx context.Context, space, name string) (runtime.ServiceConnection, error) {
	binding, err := r.ServiceBinding(ctx, space, addons.SharedOwner, name)
	return runtime.ServiceConnection{Binding: binding, Type: "redis-provider", Protocol: addons.ProtocolRedis}, err
}

func redisTestServer(t *testing.T) (*Server, *miniredis.Miniredis, *redis.Client) {
	t.Helper()
	db := miniredis.RunT(t)
	db.RequireAuth("test-password")
	s := planServer(redisTestRuntime{url: "redis://default:test-password@" + db.Addr() + "/0"}, &ai.Analysis{})
	client := redis.NewClient(&redis.Options{Addr: db.Addr(), Password: "test-password"})
	t.Cleanup(func() { client.Close() })
	return s, db, client
}

func redisCall(t *testing.T, s *Server, method, suffix string, body any, status int) []byte {
	t.Helper()
	data, err := json.Marshal(body)
	if err != nil {
		t.Fatal(err)
	}
	req := httptest.NewRequest(method, "/v1/spaces/default/services/redis/keys"+suffix, bytes.NewReader(data))
	asAdmin(t, req)
	rec := httptest.NewRecorder()
	s.routes().ServeHTTP(rec, req)
	if rec.Code != status {
		t.Fatalf("%s %s: %d, want %d: %s", method, suffix, rec.Code, status, rec.Body.String())
	}
	return rec.Body.Bytes()
}

func redisValuePath(key string) string {
	return "/value?id=" + base64.RawURLEncoding.EncodeToString([]byte(key))
}

func TestRedisBrowserEditsPreserveExpiryAndRejectStaleValues(t *testing.T) {
	s, db, client := redisTestServer(t)
	key := "session:/a +?"
	path := redisValuePath(key)
	redisCall(t, s, "POST", "", map[string]any{"name": key, "value": "before", "ttl": 60}, 200)
	redisCall(t, s, "POST", "", map[string]any{"name": key, "value": "overwrite"}, 409)
	db.FastForward(10 * time.Second)
	redisCall(t, s, "PUT", path, map[string]any{"previous": "before", "value": "after"}, 200)
	if ttl := db.TTL(key); ttl != 50*time.Second {
		t.Fatalf("editing reset expiry: %v", ttl)
	}
	redisCall(t, s, "PUT", path, map[string]any{"previous": "before", "value": "stale"}, 409)
	if value, _ := db.Get(key); value != "after" {
		t.Fatal("stale edit overwrote the live value")
	}
	redisCall(t, s, "PUT", path, map[string]any{"previous": "after", "value": "persistent", "ttl": 0}, 200)
	if ttl := db.TTL(key); ttl != 0 {
		t.Fatalf("expiry wasn't removed: %v", ttl)
	}
	redisCall(t, s, "PUT", path, map[string]any{"previous": "persistent", "value": "expires", "ttl": 5}, 200)
	db.FastForward(6 * time.Second)
	redisCall(t, s, "GET", path, nil, 404)
	redisCall(t, s, "PUT", path, map[string]any{"previous": "expires", "value": "resurrected"}, 409)
	if db.Exists(key) {
		t.Fatal("expired key was recreated by an edit")
	}
	client.HSet(context.Background(), key, "field", "value")
	redisCall(t, s, "PUT", path, map[string]any{"previous": "", "value": "string"}, 409)
	redisCall(t, s, "DELETE", path, nil, 200)
	if db.Exists(key) {
		t.Fatal("key wasn't deleted")
	}
}

func TestRedisBrowserPreviewsTypesAndBinaryValues(t *testing.T) {
	s, _, client := redisTestServer(t)
	ctx := context.Background()
	client.Set(ctx, "text", "hello", 0)
	client.Set(ctx, "empty", "", 0)
	client.Set(ctx, "binary", "\xff\x00\xfe", 0)
	client.Set(ctx, "large", strings.Repeat("x", redisPreviewBytes+1), 0)
	client.HSet(ctx, "hash", "data", "\xff\x00", "field", "value")
	client.SAdd(ctx, "set", "one", "two")
	client.ZAdd(ctx, "zset", redis.Z{Score: 1.5, Member: "one"})
	client.XAdd(ctx, &redis.XAddArgs{Stream: "stream", Values: map[string]any{"field": "value"}})
	for i := 0; i < 101; i++ {
		client.RPush(ctx, "list", "item")
	}
	for _, tc := range []struct {
		key, kind           string
		editable, truncated bool
	}{
		{"text", "string", true, false}, {"empty", "string", true, false},
		{"binary", "string", false, false}, {"large", "string", false, true},
		{"hash", "hash", false, false}, {"set", "set", false, false},
		{"zset", "zset", false, false}, {"stream", "stream", false, false}, {"list", "list", false, true},
	} {
		t.Run(tc.key, func(t *testing.T) {
			data := redisCall(t, s, "GET", redisValuePath(tc.key), nil, 200)
			var value struct {
				Type                string
				Editable, Truncated bool
				Value               any
			}
			if err := json.Unmarshal(data, &value); err != nil {
				t.Fatal(err)
			}
			if value.Type != tc.kind || value.Editable != tc.editable || value.Truncated != tc.truncated {
				t.Fatalf("unexpected preview: %s", data)
			}
			if tc.key == "binary" && value.Value.(map[string]any)["base64"] != "/wD+" {
				t.Fatal("binary data was corrupted")
			}
			if tc.key == "large" && len(value.Value.(string)) != redisPreviewBytes {
				t.Fatal("preview wasn't bounded")
			}
		})
	}
	// An opaque ID must round-trip even a key name that JSON cannot represent.
	client.Set(ctx, "\xff", "binary key", 0)
	var page struct {
		Keys   []struct{ ID, Name string }
		Cursor string
	}
	data := redisCall(t, s, "GET", "?pattern=*", nil, 200)
	if err := json.Unmarshal(data, &page); err != nil {
		t.Fatal(err)
	}
	found := false
	for _, key := range page.Keys {
		if key.ID == "_w" {
			found = true
			redisCall(t, s, "GET", "/value?id="+key.ID, nil, 200)
			redisCall(t, s, "DELETE", "/value?id="+key.ID, nil, 200)
		}
	}
	if !found {
		t.Fatal("binary key missing from scan")
	}
}

func TestRedisBrowserRequiresAdminAndValidRequests(t *testing.T) {
	s, _, _ := redisTestServer(t)
	for _, method := range []string{"GET", "POST", "PUT", "DELETE"} {
		suffix := "/value?id=eA"
		if method == "POST" {
			suffix = ""
		}
		rec := httptest.NewRecorder()
		s.routes().ServeHTTP(rec, httptest.NewRequest(method, "/v1/spaces/default/services/redis/keys"+suffix, strings.NewReader(`{}`)))
		if rec.Code != http.StatusUnauthorized {
			t.Fatalf("%s without admin: %d", method, rec.Code)
		}
	}
	redisCall(t, s, "GET", "?cursor=-1", nil, 400)
	redisCall(t, s, "GET", "/value?id=not!base64", nil, 400)
	redisCall(t, s, "GET", "/value", nil, 400)
	redisCall(t, s, "POST", "", map[string]any{"name": "bad", "ttl": -1}, 400)
	redisCall(t, s, "POST", "", map[string]any{"name": "big", "value": strings.Repeat("x", redisPreviewBytes+1)}, 400)
}
