package server

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strconv"
	"time"
	"unicode/utf8"

	"github.com/redis/go-redis/v9"

	"github.com/adrianliechti/donkey/internal/addons"
	"github.com/adrianliechti/donkey/internal/config"
	"github.com/adrianliechti/donkey/pkg/runtime"
)

const redisTimeout = 10 * time.Second
const redisPreviewBytes = 256 << 10

func (s *Server) redisConnect(ctx context.Context, r *http.Request) (*redis.Client, error) {
	space, err := s.space(r)
	if err != nil {
		return nil, runtime.UserError{Err: err}
	}
	name := r.PathValue("service")
	if !config.ValidName(name) {
		return nil, runtime.UserError{Err: fmt.Errorf("invalid service name")}
	}
	binding, err := s.rt.ServiceBinding(ctx, space, addons.SharedOwner, name)
	if err != nil {
		return nil, err
	}
	if binding["REDIS_URL"] == "" {
		return nil, runtime.UserError{Err: fmt.Errorf("%s has no Redis binding", name)}
	}
	options, err := redis.ParseURL(binding["REDIS_URL"])
	if err != nil {
		return nil, runtime.UserError{Err: fmt.Errorf("the service's Redis URL is invalid")}
	}
	// One short-lived client per browser request; credentials never reach the browser.
	options.ContextTimeoutEnabled = true
	options.DialTimeout, options.ReadTimeout, options.WriteTimeout = redisTimeout, redisTimeout, redisTimeout
	options.MaxRetries = -1
	return redis.NewClient(options), nil
}

func redisKeyID(r *http.Request) (string, error) {
	if !r.URL.Query().Has("id") {
		return "", fmt.Errorf("choose a key")
	}
	key, err := base64.RawURLEncoding.DecodeString(r.URL.Query().Get("id"))
	if err != nil || len(key) > 4096 {
		return "", fmt.Errorf("invalid key ID")
	}
	return string(key), nil
}

func (s *Server) handleRedisKeys(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := context.WithTimeout(r.Context(), redisTimeout)
	defer cancel()
	cursor := uint64(0)
	if raw := r.URL.Query().Get("cursor"); raw != "" {
		var err error
		cursor, err = strconv.ParseUint(raw, 10, 64)
		if err != nil {
			writeErr(w, http.StatusBadRequest, fmt.Errorf("invalid scan cursor"))
			return
		}
	}
	client, err := s.redisConnect(ctx, r)
	if err != nil {
		writeErr(w, statusFor(err), err)
		return
	}
	defer client.Close()
	keys, next, err := client.Scan(ctx, cursor, r.URL.Query().Get("pattern"), 100).Result()
	if err != nil {
		writeErr(w, http.StatusBadGateway, err)
		return
	}
	type key struct {
		ID   string `json:"id"`
		Name string `json:"name"`
	}
	items := make([]key, 0, len(keys))
	for _, name := range keys {
		label := name
		if !utf8.ValidString(name) {
			label = "base64:" + base64.StdEncoding.EncodeToString([]byte(name))
		}
		items = append(items, key{ID: base64.RawURLEncoding.EncodeToString([]byte(name)), Name: label})
	}
	writeJSON(w, http.StatusOK, map[string]any{"keys": items, "cursor": strconv.FormatUint(next, 10), "db": client.Options().DB})
}

// Read one bounded preview atomically, including its type and expiry. Collection
// values are trimmed inside Redis so a large value cannot flood the browser.
var redisPreview = redis.NewScript(`
local key, limit = KEYS[1], tonumber(ARGV[1])
local kind = redis.call('TYPE', key).ok
local ttl, size, value, truncated = redis.call('TTL', key), 0, {}, false
local function text(v)
  if #v > limit then truncated = true end
  local out = string.sub(v, 1, limit)
  limit = limit - #out
  return out
end
if kind == 'string' then
  size = redis.call('STRLEN', key)
  value = redis.call('GETRANGE', key, 0, limit - 1)
  truncated = size > limit
elseif kind == 'hash' or kind == 'set' then
  local cmd = kind == 'hash' and 'HSCAN' or 'SSCAN'
  size = redis.call(kind == 'hash' and 'HLEN' or 'SCARD', key)
  local scan = redis.call(cmd, key, '0', 'COUNT', 100)
  truncated = scan[1] ~= '0'
  local step = kind == 'hash' and 2 or 1
  for i = 1, #scan[2], step do
    if #value >= 100 or limit <= 0 then truncated = true; break end
    if step == 2 then table.insert(value, {text(scan[2][i]), text(scan[2][i+1])})
    else table.insert(value, text(scan[2][i])) end
  end
elseif kind == 'list' or kind == 'zset' then
  size = redis.call(kind == 'list' and 'LLEN' or 'ZCARD', key)
  local items
  if kind == 'list' then items = redis.call('LRANGE', key, 0, 99)
  else items = redis.call('ZRANGE', key, 0, 99, 'WITHSCORES') end
  truncated = size > 100
  local step = kind == 'zset' and 2 or 1
  for i = 1, #items, step do
    if limit <= 0 then truncated = true; break end
    if step == 2 then table.insert(value, {text(items[i]), items[i+1]})
    else table.insert(value, text(items[i])) end
  end
elseif kind == 'stream' then
  size = redis.call('XLEN', key)
  truncated = size > 100
  for _, entry in ipairs(redis.call('XRANGE', key, '-', '+', 'COUNT', 100)) do
    if limit <= 0 then truncated = true; break end
    local fields = {}
    for i = 1, #entry[2], 2 do
      if limit <= 0 or #fields >= 100 then truncated = true; break end
      table.insert(fields, {text(entry[2][i]), text(entry[2][i+1])})
    end
    table.insert(value, {entry[1], fields})
  end
end
return {kind, ttl, size, value, truncated and 1 or 0}
`)

func redisDisplay(value any) any {
	switch v := value.(type) {
	case string:
		if !utf8.ValidString(v) {
			return map[string]string{"base64": base64.StdEncoding.EncodeToString([]byte(v))}
		}
	case []any:
		for i := range v {
			v[i] = redisDisplay(v[i])
		}
	}
	return value
}

func (s *Server) handleRedisValue(w http.ResponseWriter, r *http.Request) {
	key, err := redisKeyID(r)
	if err != nil {
		writeErr(w, http.StatusBadRequest, err)
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), redisTimeout)
	defer cancel()
	client, err := s.redisConnect(ctx, r)
	if err != nil {
		writeErr(w, statusFor(err), err)
		return
	}
	defer client.Close()
	result, err := redisPreview.Run(ctx, client, []string{key}, redisPreviewBytes).Slice()
	if err != nil {
		writeErr(w, http.StatusBadGateway, err)
		return
	}
	if result[0] == "none" {
		writeErr(w, http.StatusNotFound, fmt.Errorf("this key expired or was deleted"))
		return
	}
	text, isText := result[3].(string)
	editable := result[0] == "string" && isText && utf8.ValidString(text) && result[4] == int64(0)
	writeJSON(w, http.StatusOK, map[string]any{"type": result[0], "ttl": result[1], "size": result[2], "value": redisDisplay(result[3]), "editable": editable, "truncated": result[4] == int64(1)})
}

var redisEdit = redis.NewScript(`
if redis.call('TYPE', KEYS[1]).ok ~= 'string' then return 0 end
if redis.call('GET', KEYS[1]) ~= ARGV[1] then return 0 end
redis.call('SET', KEYS[1], ARGV[2], 'KEEPTTL')
if ARGV[3] == '0' then redis.call('PERSIST', KEYS[1])
elseif ARGV[3] ~= '' then redis.call('EXPIRE', KEYS[1], ARGV[3]) end
return 1
`)

func (s *Server) handleRedisSave(w http.ResponseWriter, r *http.Request) {
	var input struct {
		Name     string `json:"name"`
		Value    string `json:"value"`
		Previous string `json:"previous"`
		TTL      *int64 `json:"ttl"`
	}
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 4<<20)).Decode(&input); err != nil {
		writeErr(w, http.StatusBadRequest, fmt.Errorf("invalid key data"))
		return
	}
	if len(input.Value) > redisPreviewBytes || len(input.Previous) > redisPreviewBytes || len(input.Name) > 4096 || input.TTL != nil && (*input.TTL < 0 || *input.TTL > 2147483647) {
		writeErr(w, http.StatusBadRequest, fmt.Errorf("values must be at most 256 KB and expiry must be 0–2147483647 seconds"))
		return
	}
	key := input.Name
	if r.Method == http.MethodPut {
		var err error
		key, err = redisKeyID(r)
		if err != nil {
			writeErr(w, http.StatusBadRequest, err)
			return
		}
	}
	ctx, cancel := context.WithTimeout(r.Context(), redisTimeout)
	defer cancel()
	client, err := s.redisConnect(ctx, r)
	if err != nil {
		writeErr(w, statusFor(err), err)
		return
	}
	defer client.Close()
	if r.Method == http.MethodPost {
		ttl := time.Duration(0)
		if input.TTL != nil {
			ttl = time.Duration(*input.TTL) * time.Second
		}
		var created bool
		created, err = client.SetNX(ctx, key, input.Value, ttl).Result()
		if err == nil && !created {
			writeErr(w, http.StatusConflict, fmt.Errorf("that key already exists"))
			return
		}
	} else {
		ttl := ""
		if input.TTL != nil {
			ttl = strconv.FormatInt(*input.TTL, 10)
		}
		var saved int
		saved, err = redisEdit.Run(ctx, client, []string{key}, input.Previous, input.Value, ttl).Int()
		if err == nil && saved == 0 {
			writeErr(w, http.StatusConflict, fmt.Errorf("this key changed or expired; reload it before saving"))
			return
		}
	}
	if err != nil {
		writeErr(w, http.StatusBadGateway, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true})
}

func (s *Server) handleRedisDelete(w http.ResponseWriter, r *http.Request) {
	key, err := redisKeyID(r)
	if err != nil {
		writeErr(w, http.StatusBadRequest, err)
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), redisTimeout)
	defer cancel()
	client, err := s.redisConnect(ctx, r)
	if err != nil {
		writeErr(w, statusFor(err), err)
		return
	}
	defer client.Close()
	if err := client.Unlink(ctx, key).Err(); err != nil && !errors.Is(err, redis.Nil) {
		writeErr(w, http.StatusBadGateway, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true})
}
