package server

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"reflect"
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

type redisPage struct {
	Type      string
	TTL       int64
	Size      int64
	Value     any
	Items     []map[string]any
	Next      string
	Editable  bool
	Truncated bool
}

func redisGet(t *testing.T, s *Server, suffix string) redisPage {
	t.Helper()
	var page redisPage
	if err := json.Unmarshal(redisCall(t, s, "GET", suffix, nil, 200), &page); err != nil {
		t.Fatal(err)
	}
	return page
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
		items               int
		next                string
	}{
		{"text", "string", true, false, 0, ""}, {"empty", "string", true, false, 0, ""},
		{"binary", "string", false, false, 0, ""}, {"large", "string", false, true, 0, ""},
		{"hash", "hash", false, false, 2, ""}, {"set", "set", false, false, 2, ""},
		{"zset", "zset", false, false, 1, ""}, {"stream", "stream", false, false, 1, ""},
		{"list", "list", false, false, 100, "100"},
	} {
		t.Run(tc.key, func(t *testing.T) {
			value := redisGet(t, s, redisValuePath(tc.key))
			if value.Type != tc.kind || value.Editable != tc.editable || value.Truncated != tc.truncated || len(value.Items) != tc.items || value.Next != tc.next {
				t.Fatalf("unexpected preview: %+v", value)
			}
			switch tc.key {
			case "binary":
				if value.Value.(map[string]any)["base64"] != "/wD+" {
					t.Fatal("binary data was corrupted")
				}
			case "large":
				cut := value.Value.(map[string]any)
				if cut["cut"] != true || cut["size"] != float64(redisPreviewBytes+1) || len(cut["text"].(string)) != redisPreviewBytes {
					t.Fatal("preview wasn't bounded")
				}
			case "hash":
				for _, item := range value.Items {
					if item["field"] == "data" && item["value"].(map[string]any)["base64"] != "/wA=" {
						t.Fatal("binary hash value was corrupted")
					}
				}
			case "zset":
				if value.Items[0]["member"] != "one" || value.Items[0]["score"] != "1.5" {
					t.Fatalf("sorted set: %v", value.Items)
				}
			case "list":
				// The rest of the list is the next page.
				rest := redisGet(t, s, redisValuePath(tc.key)+"&at=100")
				if len(rest.Items) != 1 || rest.Items[0]["index"] != float64(100) || rest.Next != "" {
					t.Fatalf("second page: %+v", rest)
				}
			}
		})
	}
	// An opaque ID must round-trip even a key name that JSON cannot represent.
	client.Set(ctx, "\xff", "binary key", 0)
	var page struct {
		Keys   []struct{ ID, Name, Type string }
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
			if key.Name != "base64:/w==" || key.Type != "string" {
				t.Fatalf("binary key listed as %+v", key)
			}
			redisCall(t, s, "GET", "/value?id="+key.ID, nil, 200)
			redisCall(t, s, "DELETE", "/value?id="+key.ID, nil, 200)
		}
	}
	if !found {
		t.Fatal("binary key missing from scan")
	}
}

func TestRedisBrowserBoundsPagesOfLargeElements(t *testing.T) {
	s, _, client := redisTestServer(t)
	ctx := context.Background()
	big := strings.Repeat("é", 50<<10) // 100 KB, cut mid-rune at 64 KB
	for i := range 10 {
		client.HSet(ctx, "hash", string(rune('a'+i)), big)
		client.RPush(ctx, "list", big)
	}
	// A hash page is one scan batch: every field is listed, the ones past
	// the budget cut short, none lost.
	hash := redisGet(t, s, redisValuePath("hash"))
	if len(hash.Items) != 10 || !hash.Truncated || hash.Next != "" {
		t.Fatalf("hash page: %d items, truncated %v, next %q", len(hash.Items), hash.Truncated, hash.Next)
	}
	shown := 0
	for _, item := range hash.Items {
		cut := item["value"].(map[string]any)
		text, _ := cut["text"].(string)
		if cut["size"] != float64(len(big)) || !strings.HasPrefix(big, text) {
			t.Fatalf("cut value: size %v, %d bytes", cut["size"], len(text))
		}
		shown += len(text)
	}
	if shown > redisPageBytes+2*redisItemBytes {
		t.Fatalf("hash page carried %d bytes", shown)
	}
	// A list page ends at the budget and the next one picks up after it.
	first := redisGet(t, s, redisValuePath("list"))
	if len(first.Items) == 0 || len(first.Items) == 10 || first.Next == "" {
		t.Fatalf("list page: %d items, next %q", len(first.Items), first.Next)
	}
	rest := redisGet(t, s, redisValuePath("list")+"&at="+first.Next)
	if len(first.Items)+len(rest.Items) != 10 {
		t.Fatalf("list pages: %d + %d items", len(first.Items), len(rest.Items))
	}
	// Streams page by entry ID.
	for range 150 {
		client.XAdd(ctx, &redis.XAddArgs{Stream: "stream", Values: []any{"n", "1"}})
	}
	page := redisGet(t, s, redisValuePath("stream"))
	if len(page.Items) != redisPageItems || page.Next != page.Items[len(page.Items)-1]["id"] {
		t.Fatalf("stream page: %d entries, next %q", len(page.Items), page.Next)
	}
	page = redisGet(t, s, redisValuePath("stream")+"&at="+page.Next)
	if len(page.Items) != 50 || page.Next != "" {
		t.Fatalf("stream rest: %d entries, next %q", len(page.Items), page.Next)
	}
	redisCall(t, s, "GET", redisValuePath("stream")+"&at=not-a-position", nil, 400)
}

func TestRedisBrowserListsKeysByTypeAndDatabase(t *testing.T) {
	s, db, client := redisTestServer(t)
	ctx := context.Background()
	client.Set(ctx, "user:1", "a", time.Minute)
	client.HSet(ctx, "user:2", "name", "b")
	client.Set(ctx, "other", "c", 0)
	db.DB(3).Set("elsewhere", "d")
	type listing struct {
		Keys []struct {
			Name, Type string
			TTL        int64
		}
		Cursor string
		DB     int
	}
	list := func(suffix string) listing {
		var page listing
		if err := json.Unmarshal(redisCall(t, s, "GET", suffix, nil, 200), &page); err != nil {
			t.Fatal(err)
		}
		return page
	}
	page := list("?pattern=user:*")
	if len(page.Keys) != 2 || page.Cursor != "0" || page.DB != 0 {
		t.Fatalf("pattern: %+v", page)
	}
	for _, key := range page.Keys {
		if key.Name == "user:1" && (key.Type != "string" || key.TTL != 60) || key.Name == "user:2" && (key.Type != "hash" || key.TTL != -1) {
			t.Fatalf("badges: %+v", key)
		}
	}
	if page := list("?type=hash"); len(page.Keys) != 1 || page.Keys[0].Name != "user:2" {
		t.Fatalf("type filter: %+v", page)
	}
	if page := list("?db=3"); len(page.Keys) != 1 || page.Keys[0].Name != "elsewhere" || page.DB != 3 {
		t.Fatalf("database 3: %+v", page)
	}
	redisCall(t, s, "GET", "/value?db=3&id="+redisID("elsewhere"), nil, 200)
	redisCall(t, s, "GET", "?type=module", nil, 400)
	redisCall(t, s, "GET", "?db=-1", nil, 400)
	// The picker is best effort: without CONFIG and INFO it still names the
	// binding's own database.
	var databases struct {
		Default, Count int
		Databases      []redisDatabase
	}
	if err := json.Unmarshal(redisCall(t, s, "GET", "/databases", nil, 200), &databases); err != nil {
		t.Fatal(err)
	}
	if databases.Default != 0 || databases.Databases == nil {
		t.Fatalf("databases: %+v", databases)
	}
}

func TestRedisKeyspaceParsesInfo(t *testing.T) {
	info := "# Keyspace\r\ndb0:keys=12,expires=1,avg_ttl=0\r\ndb10:keys=3,expires=0,avg_ttl=0\r\ndb2:keys=7,expires=0,avg_ttl=0\r\n"
	want := []redisDatabase{{DB: 0, Keys: 12}, {DB: 2, Keys: 7}, {DB: 10, Keys: 3}}
	if got := redisKeyspace(info); !reflect.DeepEqual(got, want) {
		t.Fatalf("keyspace: %+v", got)
	}
}

func TestRedisBrowserCreatesEveryType(t *testing.T) {
	s, db, client := redisTestServer(t)
	ctx := context.Background()
	for _, tc := range []struct {
		kind  string
		items [][]string
	}{
		{"hash", [][]string{{"name", "ada"}, {"lang", "en"}}},
		{"list", [][]string{{"first"}, {"second"}}},
		{"set", [][]string{{"a"}, {"b"}}},
		{"zset", [][]string{{"low", "1"}, {"high", "inf"}}},
		{"stream", [][]string{{"event", "signup"}}},
	} {
		name := "new:" + tc.kind
		redisCall(t, s, "POST", "", map[string]any{"name": name, "type": tc.kind, "items": tc.items, "ttl": 30}, 200)
		if kind, _ := client.Type(ctx, name).Result(); kind != tc.kind {
			t.Fatalf("%s created as %s", tc.kind, kind)
		}
		if ttl := db.TTL(name); ttl != 30*time.Second {
			t.Fatalf("%s expiry: %v", tc.kind, ttl)
		}
		redisCall(t, s, "POST", "", map[string]any{"name": name, "type": tc.kind, "items": tc.items}, 409)
	}
	if got, _ := client.LRange(ctx, "new:list", 0, -1).Result(); !reflect.DeepEqual(got, []string{"first", "second"}) {
		t.Fatalf("list order: %v", got)
	}
	if score, _ := client.ZScore(ctx, "new:zset", "low").Result(); score != 1 {
		t.Fatalf("score: %v", score)
	}
	for _, bad := range []map[string]any{
		{"name": "empty", "type": "list", "items": [][]string{}},
		{"name": "arity", "type": "hash", "items": [][]string{{"field"}}},
		{"name": "score", "type": "zset", "items": [][]string{{"m", "lots"}}},
		{"name": "kind", "type": "module", "items": [][]string{{"x"}}},
		{"name": "", "value": "nameless"},
	} {
		redisCall(t, s, "POST", "", bad, 400)
	}
	if db.Exists("empty") || db.Exists("arity") {
		t.Fatal("a rejected key was created")
	}
}

func TestRedisBrowserChangesElementsWithoutResurrectingKeys(t *testing.T) {
	s, db, client := redisTestServer(t)
	ctx := context.Background()
	change := func(key string, body map[string]any, status int) map[string]any {
		t.Helper()
		var out map[string]any
		_ = json.Unmarshal(redisCall(t, s, "PATCH", redisValuePath(key), body, status), &out)
		return out
	}

	// Hash: add only when new, edit only what was read, delete.
	client.HSet(ctx, "h", "a", "1")
	change("h", map[string]any{"op": "hset", "field": "b", "value": "2", "create": true}, 200)
	change("h", map[string]any{"op": "hset", "field": "b", "value": "3", "create": true}, 409)
	change("h", map[string]any{"op": "hset", "field": "a", "value": "edited", "previous": "1"}, 200)
	change("h", map[string]any{"op": "hset", "field": "a", "value": "stale", "previous": "1"}, 409)
	if v, _ := client.HGet(ctx, "h", "a").Result(); v != "edited" {
		t.Fatalf("hash field: %q", v)
	}
	change("h", map[string]any{"op": "hset", "field": map[string]string{"base64": "/w=="}, "value": "binary field", "create": true}, 200)
	if v, _ := client.HGet(ctx, "h", "\xff").Result(); v != "binary field" {
		t.Fatal("a binary field name was not addressed exactly")
	}
	change("h", map[string]any{"op": "hdel", "field": map[string]string{"base64": "/w=="}}, 200)
	change("h", map[string]any{"op": "hdel", "field": "b"}, 200)
	// Redis drops a hash with its last field; the answer says so.
	if out := change("h", map[string]any{"op": "hdel", "field": "a"}, 200); out["exists"] != false {
		t.Fatalf("last field: %v", out)
	}
	change("h", map[string]any{"op": "hset", "field": "a", "value": "back", "create": true}, 404)
	if db.Exists("h") {
		t.Fatal("an edit recreated a deleted hash")
	}

	// List: push at both ends, set and remove by index only while the item
	// is still the one that was read.
	client.RPush(ctx, "l", "a", "b", "a")
	change("l", map[string]any{"op": "lpush", "value": "head"}, 200)
	change("l", map[string]any{"op": "rpush", "value": "tail"}, 200)
	change("l", map[string]any{"op": "lset", "index": 1, "previous": "a", "value": "A"}, 200)
	change("l", map[string]any{"op": "lset", "index": 1, "previous": "a", "value": "stale"}, 409)
	change("l", map[string]any{"op": "lrem", "index": 3, "previous": "a"}, 200)
	if got, _ := client.LRange(ctx, "l", 0, -1).Result(); !reflect.DeepEqual(got, []string{"head", "A", "b", "tail"}) {
		t.Fatalf("list: %v", got)
	}
	change("l", map[string]any{"op": "lrem", "index": 9, "previous": "a"}, 409)
	change("l", map[string]any{"op": "lset", "previous": "a", "value": "x"}, 400)

	// Set and sorted set.
	client.SAdd(ctx, "s", "x")
	change("s", map[string]any{"op": "sadd", "member": "y"}, 200)
	change("s", map[string]any{"op": "sadd", "member": "y"}, 409)
	change("s", map[string]any{"op": "srem", "member": "x"}, 200)
	if got, _ := client.SMembers(ctx, "s").Result(); !reflect.DeepEqual(got, []string{"y"}) {
		t.Fatalf("set: %v", got)
	}
	client.ZAdd(ctx, "z", redis.Z{Score: 1, Member: "m"})
	change("z", map[string]any{"op": "zadd", "member": "n", "score": "2.5", "create": true}, 200)
	change("z", map[string]any{"op": "zadd", "member": "n", "score": "3", "create": true}, 409)
	change("z", map[string]any{"op": "zadd", "member": "m", "score": "-inf"}, 200)
	change("z", map[string]any{"op": "zadd", "member": "gone", "score": "1"}, 409)
	change("z", map[string]any{"op": "zadd", "member": "m", "score": "many"}, 400)
	change("z", map[string]any{"op": "zrem", "member": "n"}, 200)
	if got, _ := client.ZRangeWithScores(ctx, "z", 0, -1).Result(); len(got) != 1 || got[0].Member != "m" {
		t.Fatalf("sorted set: %v", got)
	}

	// Stream: append and remove entries.
	id, _ := client.XAdd(ctx, &redis.XAddArgs{Stream: "x", Values: []any{"a", "1"}}).Result()
	change("x", map[string]any{"op": "xadd", "fields": [][2]string{{"b", "2"}, {"c", "3"}}}, 200)
	change("x", map[string]any{"op": "xdel", "entry": id}, 200)
	if entries, _ := client.XRange(ctx, "x", "-", "+").Result(); len(entries) != 1 || entries[0].Values["c"] != "3" {
		t.Fatalf("stream: %v", entries)
	}
	change("x", map[string]any{"op": "xdel", "entry": "not-an-id"}, 400)

	// The wrong type, and keys that are gone, are refused.
	change("s", map[string]any{"op": "hset", "field": "f", "value": "v"}, 409)
	change("missing", map[string]any{"op": "rpush", "value": "v"}, 404)
	change("missing", map[string]any{"op": "sadd", "member": "v"}, 404)
	if db.Exists("missing") {
		t.Fatal("an element change created a key")
	}
	change("s", map[string]any{"op": "explode"}, 400)
}

func TestRedisBrowserRenamesAndExpiresKeys(t *testing.T) {
	s, db, client := redisTestServer(t)
	ctx := context.Background()
	client.Set(ctx, "a", "1", 0)
	client.Set(ctx, "taken", "2", 0)
	change := func(key string, body map[string]any, status int) map[string]any {
		t.Helper()
		var out map[string]any
		_ = json.Unmarshal(redisCall(t, s, "PATCH", redisValuePath(key), body, status), &out)
		return out
	}
	change("a", map[string]any{"op": "expire", "ttl": 90}, 200)
	if ttl := db.TTL("a"); ttl != 90*time.Second {
		t.Fatalf("expire: %v", ttl)
	}
	change("a", map[string]any{"op": "expire", "ttl": 0}, 400)
	change("a", map[string]any{"op": "persist"}, 200)
	if ttl := db.TTL("a"); ttl != 0 {
		t.Fatalf("persist: %v", ttl)
	}
	change("a", map[string]any{"op": "rename", "name": "taken"}, 409)
	out := change("a", map[string]any{"op": "rename", "name": "b"}, 200)
	if out["id"] != redisID("b") || db.Exists("a") || !db.Exists("b") {
		t.Fatalf("rename: %v", out)
	}
	if v, _ := db.Get("taken"); v != "2" {
		t.Fatal("rename overwrote another key")
	}
	change("a", map[string]any{"op": "rename", "name": "c"}, 404)
	change("a", map[string]any{"op": "expire", "ttl": 5}, 404)
	change("a", map[string]any{"op": "persist"}, 404)
	change("b", map[string]any{"op": "rename", "name": ""}, 400)
}

func TestRedisBrowserRequiresAdminAndValidRequests(t *testing.T) {
	s, _, _ := redisTestServer(t)
	for _, call := range []struct{ method, suffix string }{
		{"GET", "/value?id=eA"}, {"POST", ""}, {"PUT", "/value?id=eA"}, {"DELETE", "/value?id=eA"},
		{"PATCH", "/value?id=eA"}, {"GET", "/databases"},
	} {
		rec := httptest.NewRecorder()
		s.routes().ServeHTTP(rec, httptest.NewRequest(call.method, "/v1/spaces/default/services/redis/keys"+call.suffix, strings.NewReader(`{}`)))
		if rec.Code != http.StatusUnauthorized {
			t.Fatalf("%s %s without admin: %d", call.method, call.suffix, rec.Code)
		}
	}
	redisCall(t, s, "GET", "?cursor=-1", nil, 400)
	redisCall(t, s, "GET", "/value?id=not!base64", nil, 400)
	redisCall(t, s, "GET", "/value", nil, 400)
	redisCall(t, s, "POST", "", map[string]any{"name": "bad", "ttl": -1}, 400)
	redisCall(t, s, "POST", "", map[string]any{"name": "big", "value": strings.Repeat("x", redisPreviewBytes+1)}, 400)
	redisCall(t, s, "PATCH", "/value?id=eA", map[string]any{"op": "rpush", "value": strings.Repeat("x", redisPreviewBytes+1)}, 400)
}
