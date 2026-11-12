package server

// The console's key browser. Like the bucket browser it speaks to the
// space's Redis straight from donkey-server with the binding's own URL, so
// the browser never sees a credential. Every read is bounded: keys are found
// with SCAN (never KEYS), values are read a page at a time inside Redis, and
// long elements arrive cut rather than whole.

import (
	"context"
	"crypto/rand"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/redis/go-redis/v9"

	"github.com/adrianliechti/donkey/internal/addons"
	"github.com/adrianliechti/donkey/pkg/runtime"
)

const (
	redisTimeout = 10 * time.Second
	// A string is shown, and edited, up to this size; so is any one value a
	// change writes.
	redisPreviewBytes = 256 << 10
	// Collections are read a page at a time: about this many elements, each
	// whole up to redisItemBytes, the page as a whole about redisPageBytes.
	redisPageItems = 100
	redisItemBytes = 64 << 10
	redisPageBytes = 512 << 10
	// Redis allows key names far longer than anyone types.
	redisNameBytes = 4096
	// One page of keys looks at up to rounds×count slots and stops early once
	// it has found redisScanKeys — a sparse pattern still comes back with
	// something instead of an empty page and a cursor.
	redisScanCount  = 200
	redisScanRounds = 25
	redisScanKeys   = 100
	// What a new key may bring along: its first elements.
	redisCreateItems = 1000
	redisMaxTTL      = 2147483647
)

// The types the console knows how to show. Anything else (a module's type)
// is listed and can be renamed, expired and deleted, but not opened.
var redisTypes = []string{"string", "hash", "list", "set", "zset", "stream"}

// A page position: a SCAN cursor or list offset (digits), or the last stream
// entry ID read.
var redisPosition = regexp.MustCompile(`^([0-9]{1,20}(-[0-9]{1,20})?)?$`)

var redisStreamID = regexp.MustCompile(`^[0-9]{1,20}-[0-9]{1,20}$`)

// redisConnect opens a client for the service the request names. `?db=`
// picks another logical database than the binding's; the binding's own is
// returned beside the client, since apps only ever see that one.
func (s *Server) redisConnect(ctx context.Context, r *http.Request) (*redis.Client, int, error) {
	service, err := s.serviceConnection(ctx, r, addons.ProtocolRedis)
	if err != nil {
		return nil, 0, err
	}
	options, err := redis.ParseURL(service.Binding["REDIS_URL"])
	if err != nil {
		return nil, 0, runtime.UserError{Err: fmt.Errorf("the service's Redis URL is invalid")}
	}
	bound := options.DB
	if raw := r.URL.Query().Get("db"); raw != "" {
		db, err := strconv.Atoi(raw)
		if err != nil || db < 0 || db > 1<<20 {
			return nil, 0, runtime.UserError{Err: fmt.Errorf("invalid database index")}
		}
		options.DB = db
	}
	// One short-lived client per browser request; credentials never reach the browser.
	options.ContextTimeoutEnabled = true
	options.DialTimeout = redisTimeout
	options.ReadTimeout, options.WriteTimeout = -1, -1
	options.MaxRetries = -1
	client := redis.NewClient(options)
	context.AfterFunc(ctx, func() { _ = client.Close() })
	return client, bound, nil
}

// errRedisGone is a key that was there when the console read it and is not
// any more.
var errRedisGone = errors.New("this key expired or was deleted")

// redisStatus says whose a failure is: the request's, Redis's answer about
// this key, or the backend's.
func redisStatus(err error) int {
	var ue runtime.UserError
	if errors.As(err, &ue) {
		return http.StatusBadRequest
	}
	if errors.Is(err, redis.Nil) || errors.Is(err, errRedisGone) {
		return http.StatusNotFound
	}
	msg := err.Error()
	switch {
	case strings.HasPrefix(msg, "WRONGTYPE"):
		return http.StatusConflict
	case strings.Contains(msg, "DB index is out of range"), strings.Contains(msg, "invalid DB index"):
		return http.StatusBadRequest
	}
	return http.StatusBadGateway
}

func redisKeyID(r *http.Request) (string, error) {
	if !r.URL.Query().Has("id") {
		return "", fmt.Errorf("choose a key")
	}
	key, err := base64.RawURLEncoding.DecodeString(r.URL.Query().Get("id"))
	if err != nil || len(key) > redisNameBytes {
		return "", fmt.Errorf("invalid key ID")
	}
	return string(key), nil
}

func redisID(name string) string { return base64.RawURLEncoding.EncodeToString([]byte(name)) }

// redisLabel is a key's name as text; one that is not UTF-8 is shown as its
// base64, and found by its ID.
func redisLabel(name string) string {
	if utf8.ValidString(name) {
		return name
	}
	return "base64:" + base64.StdEncoding.EncodeToString([]byte(name))
}

// redisSeconds turns a TTL reply into seconds, keeping Redis's -1 (no
// expiry) and -2 (no key).
func redisSeconds(d time.Duration) int64 {
	if d < 0 {
		return int64(d)
	}
	return int64(d / time.Second)
}

func (s *Server) handleRedisKeys(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	cursor := uint64(0)
	if raw := q.Get("cursor"); raw != "" {
		var err error
		cursor, err = strconv.ParseUint(raw, 10, 64)
		if err != nil {
			writeErr(w, http.StatusBadRequest, fmt.Errorf("invalid scan cursor"))
			return
		}
	}
	pattern, kind := q.Get("pattern"), q.Get("type")
	if len(pattern) > redisNameBytes {
		writeErr(w, http.StatusBadRequest, fmt.Errorf("the pattern is too long"))
		return
	}
	if kind != "" && !slices.Contains(redisTypes, kind) {
		writeErr(w, http.StatusBadRequest, fmt.Errorf("unknown key type %q", kind))
		return
	}
	ctx, cancel := context.WithCancel(r.Context())
	defer cancel()
	client, _, err := s.redisConnect(ctx, r)
	if err != nil {
		writeErr(w, redisStatus(err), err)
		return
	}
	defer client.Close()
	// SCAN may hand a key out twice; a page lists it once.
	seen := map[string]bool{}
	var names []string
	for range redisScanRounds {
		var batch []string
		if kind != "" {
			batch, cursor, err = client.ScanType(ctx, cursor, pattern, redisScanCount, kind).Result()
		} else {
			batch, cursor, err = client.Scan(ctx, cursor, pattern, redisScanCount).Result()
		}
		if err != nil {
			writeErr(w, redisStatus(err), err)
			return
		}
		for _, name := range batch {
			if !seen[name] {
				seen[name] = true
				names = append(names, name)
			}
		}
		if cursor == 0 || len(names) >= redisScanKeys {
			break
		}
	}
	// Each key's type and expiry, for the list's badges, in one round trip.
	types := make([]*redis.StatusCmd, len(names))
	ttls := make([]*redis.DurationCmd, len(names))
	if len(names) > 0 {
		pipe := client.Pipeline()
		for i, name := range names {
			types[i] = pipe.Type(ctx, name)
			ttls[i] = pipe.TTL(ctx, name)
		}
		if _, err := pipe.Exec(ctx); err != nil {
			writeErr(w, redisStatus(err), err)
			return
		}
	}
	type key struct {
		ID   string `json:"id"`
		Name string `json:"name"`
		Type string `json:"type"`
		TTL  int64  `json:"ttl"`
	}
	items := make([]key, 0, len(names))
	for i, name := range names {
		// Gone between the scan and now.
		if t := types[i].Val(); t != "" && t != "none" {
			items = append(items, key{ID: redisID(name), Name: redisLabel(name), Type: t, TTL: redisSeconds(ttls[i].Val())})
		}
	}
	writeJSON(w, http.StatusOK, map[string]any{"keys": items, "cursor": strconv.FormatUint(cursor, 10), "db": client.Options().DB})
}

// handleRedisDatabases says which logical databases there are and how many
// keys each holds, for the picker. Managed Redis often refuses CONFIG, and a
// cluster has only database 0, so both answers are best effort.
func (s *Server) handleRedisDatabases(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := context.WithCancel(r.Context())
	defer cancel()
	client, bound, err := s.redisConnect(ctx, r)
	if err != nil {
		writeErr(w, redisStatus(err), err)
		return
	}
	defer client.Close()
	if err := client.Ping(ctx).Err(); err != nil {
		writeErr(w, redisStatus(err), err)
		return
	}
	count := 0
	if config, err := client.ConfigGet(ctx, "databases").Result(); err == nil {
		count, _ = strconv.Atoi(config["databases"])
	}
	databases := []redisDatabase{}
	if info, err := client.Info(ctx, "keyspace").Result(); err == nil {
		databases = redisKeyspace(info)
	}
	writeJSON(w, http.StatusOK, map[string]any{"default": bound, "count": count, "databases": databases})
}

type redisDatabase struct {
	DB   int   `json:"db"`
	Keys int64 `json:"keys"`
}

// redisKeyspace reads INFO keyspace: one "db3:keys=12,expires=0,avg_ttl=0"
// line per database that holds anything.
func redisKeyspace(info string) []redisDatabase {
	out := []redisDatabase{}
	for line := range strings.Lines(info) {
		name, fields, ok := strings.Cut(strings.TrimSpace(line), ":")
		if !ok || !strings.HasPrefix(name, "db") {
			continue
		}
		db, err := strconv.Atoi(name[2:])
		if err != nil {
			continue
		}
		entry := redisDatabase{DB: db}
		for field := range strings.SplitSeq(fields, ",") {
			if k, v, _ := strings.Cut(field, "="); k == "keys" {
				entry.Keys, _ = strconv.ParseInt(v, 10, 64)
			}
		}
		out = append(out, entry)
	}
	slices.SortFunc(out, func(a, b redisDatabase) int { return a.DB - b.DB })
	return out
}

// Read one page of a value atomically, with its type, expiry and size.
// ARGV: position, items per page, page bytes, element bytes, string bytes.
// Hash and set pages are one HSCAN/SSCAN batch — a batch cannot be resumed
// halfway, so once the page budget is spent its remaining elements arrive
// cut short instead of being dropped. Lists, sorted sets and streams are
// addressed by offset or ID, so their pages simply end early. An element
// that was cut comes back as {prefix, full length}.
var redisRead = redis.NewScript(`
local key, at = KEYS[1], ARGV[1]
local count, budget, itemcap, strcap = tonumber(ARGV[2]), tonumber(ARGV[3]), tonumber(ARGV[4]), tonumber(ARGV[5])
local kind = redis.call('TYPE', key).ok
local ttl, size, value, nextpos, cut = redis.call('TTL', key), 0, {}, '', 0
local function take(v)
  local cap = itemcap
  if budget <= 0 then cap = 256 end
  budget = budget - math.min(#v, cap)
  if #v <= cap then return v end
  cut = 1
  return {string.sub(v, 1, cap), #v}
end
local function fits(v)
  return #value == 0 or math.min(#v, itemcap) <= budget
end
if kind == 'string' then
  size = redis.call('STRLEN', key)
  local v = redis.call('GETRANGE', key, 0, strcap - 1)
  if size > strcap then cut = 1; value = {v, size} else value = v end
elseif kind == 'hash' or kind == 'set' then
  size = redis.call(kind == 'hash' and 'HLEN' or 'SCARD', key)
  local scan = redis.call(kind == 'hash' and 'HSCAN' or 'SSCAN', key, at == '' and '0' or at, 'COUNT', count)
  if scan[1] ~= '0' then nextpos = scan[1] end
  local step = kind == 'hash' and 2 or 1
  for i = 1, #scan[2], step do
    if step == 2 then table.insert(value, {take(scan[2][i]), take(scan[2][i+1])})
    else table.insert(value, take(scan[2][i])) end
  end
elseif kind == 'list' or kind == 'zset' then
  local start = tonumber(at) or 0
  local items, step
  if kind == 'list' then
    size = redis.call('LLEN', key)
    items, step = redis.call('LRANGE', key, start, start + count - 1), 1
  else
    size = redis.call('ZCARD', key)
    items, step = redis.call('ZRANGE', key, start, start + count - 1, 'WITHSCORES'), 2
  end
  local read = 0
  for i = 1, #items, step do
    if not fits(items[i]) then break end
    if step == 2 then table.insert(value, {take(items[i]), items[i+1]})
    else table.insert(value, take(items[i])) end
    read = read + 1
  end
  if start + read < size then nextpos = tostring(start + read) end
elseif kind == 'stream' then
  size = redis.call('XLEN', key)
  local from = at == '' and '-' or ('(' .. at)
  local entries = redis.call('XRANGE', key, from, '+', 'COUNT', count)
  local last = ''
  for _, entry in ipairs(entries) do
    if #value > 0 and budget <= 0 then nextpos = last; break end
    local fields = {}
    for i = 1, #entry[2], 2 do
      if #fields >= count then cut = 1; break end
      table.insert(fields, {take(entry[2][i]), take(entry[2][i+1])})
    end
    table.insert(value, {entry[1], fields})
    last = entry[1]
  end
  if nextpos == '' and #entries == count then nextpos = last end
end
return {kind, ttl, size, value, nextpos, cut}
`)

// redisText is a whole element as the console shows it: the text itself
// when it is UTF-8, {base64} when it is not.
func redisText(v string) any {
	if utf8.ValidString(v) {
		return v
	}
	return map[string]string{"base64": base64.StdEncoding.EncodeToString([]byte(v))}
}

// redisElement is one string of a value from the read script: whole, or
// {prefix, length} when it was cut — then shown with its full size, as text
// if the prefix is UTF-8 once a rune split at the cut is dropped.
func redisElement(v any) any {
	parts, ok := v.([]any)
	if !ok || len(parts) != 2 {
		s, _ := v.(string)
		return redisText(s)
	}
	prefix, _ := parts[0].(string)
	size, _ := parts[1].(int64)
	out := map[string]any{"cut": true, "size": size}
	for drop := 0; drop < 4 && drop <= len(prefix); drop++ {
		if utf8.ValidString(prefix[:len(prefix)-drop]) {
			out["text"] = prefix[:len(prefix)-drop]
			return out
		}
	}
	out["base64"] = base64.StdEncoding.EncodeToString([]byte(prefix))
	return out
}

// redisPairs reads [[a, b], …] as {left: a, right: b} objects.
func redisPairs(v any, left, right string, rightRaw bool) []map[string]any {
	rows, _ := v.([]any)
	out := make([]map[string]any, 0, len(rows))
	for _, row := range rows {
		pair, _ := row.([]any)
		if len(pair) != 2 {
			continue
		}
		item := map[string]any{left: redisElement(pair[0])}
		if rightRaw {
			item[right] = pair[1]
		} else {
			item[right] = redisElement(pair[1])
		}
		out = append(out, item)
	}
	return out
}

func (s *Server) handleRedisValue(w http.ResponseWriter, r *http.Request) {
	key, err := redisKeyID(r)
	if err != nil {
		writeErr(w, http.StatusBadRequest, err)
		return
	}
	at := r.URL.Query().Get("at")
	if !redisPosition.MatchString(at) {
		writeErr(w, http.StatusBadRequest, fmt.Errorf("invalid page position"))
		return
	}
	ctx, cancel := context.WithCancel(r.Context())
	defer cancel()
	client, _, err := s.redisConnect(ctx, r)
	if err != nil {
		writeErr(w, redisStatus(err), err)
		return
	}
	defer client.Close()
	result, err := redisRead.Run(ctx, client, []string{key}, at, redisPageItems, redisPageBytes, redisItemBytes, redisPreviewBytes).Slice()
	if err != nil {
		writeErr(w, redisStatus(err), err)
		return
	}
	kind, _ := result[0].(string)
	if kind == "none" {
		writeErr(w, http.StatusNotFound, errRedisGone)
		return
	}
	cut := result[5] == int64(1)
	out := map[string]any{"type": kind, "ttl": result[1], "size": result[2], "next": result[4], "truncated": cut, "editable": false}
	switch kind {
	case "string":
		out["value"] = redisElement(result[3])
		text, isText := result[3].(string)
		out["editable"] = isText && utf8.ValidString(text)
	case "hash":
		out["items"] = redisPairs(result[3], "field", "value", false)
	case "zset":
		out["items"] = redisPairs(result[3], "member", "score", true)
	case "list", "set":
		rows, _ := result[3].([]any)
		start, _ := strconv.Atoi(at)
		items := make([]map[string]any, 0, len(rows))
		for i, row := range rows {
			if kind == "list" {
				items = append(items, map[string]any{"index": start + i, "value": redisElement(row)})
			} else {
				items = append(items, map[string]any{"member": redisElement(row)})
			}
		}
		out["items"] = items
	case "stream":
		rows, _ := result[3].([]any)
		items := make([]map[string]any, 0, len(rows))
		for _, row := range rows {
			entry, _ := row.([]any)
			if len(entry) != 2 {
				continue
			}
			items = append(items, map[string]any{"id": entry[0], "fields": redisPairs(entry[1], "field", "value", false)})
		}
		out["items"] = items
	}
	writeJSON(w, http.StatusOK, out)
}

var redisEdit = redis.NewScript(`
if redis.call('TYPE', KEYS[1]).ok ~= 'string' then return 0 end
if redis.call('GET', KEYS[1]) ~= ARGV[1] then return 0 end
redis.call('SET', KEYS[1], ARGV[2], 'KEEPTTL')
if ARGV[3] == '0' then redis.call('PERSIST', KEYS[1])
elseif ARGV[3] ~= '' then redis.call('EXPIRE', KEYS[1], ARGV[3]) end
return 1
`)

// A new key of any type, with its first elements, only if the name is free.
// ARGV: type, expiry (0 for none), then the elements as the type's own
// command takes them.
var redisCreate = redis.NewScript(`
local key, kind, ttl = KEYS[1], ARGV[1], tonumber(ARGV[2])
if redis.call('EXISTS', key) == 1 then return 0 end
local args = {}
for i = 3, #ARGV do args[#args + 1] = ARGV[i] end
if kind == 'string' then redis.call('SET', key, args[1])
elseif kind == 'hash' then redis.call('HSET', key, unpack(args))
elseif kind == 'list' then redis.call('RPUSH', key, unpack(args))
elseif kind == 'set' then redis.call('SADD', key, unpack(args))
elseif kind == 'zset' then redis.call('ZADD', key, unpack(args))
elseif kind == 'stream' then redis.call('XADD', key, '*', unpack(args)) end
if ttl > 0 then redis.call('EXPIRE', key, ttl) end
return 1
`)

// handleRedisSave creates a key (POST) or saves a string's new text (PUT).
// A save only lands on the text it was edited from: a value that changed or
// expired in the meantime is never overwritten or brought back.
func (s *Server) handleRedisSave(w http.ResponseWriter, r *http.Request) {
	if r.Method == http.MethodPost {
		s.handleRedisCreate(w, r)
		return
	}
	var input struct {
		Value    string `json:"value"`
		Previous string `json:"previous"`
		TTL      *int64 `json:"ttl"`
	}
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 2*redisPreviewBytes+4096)).Decode(&input); err != nil {
		writeErr(w, http.StatusBadRequest, fmt.Errorf("invalid key data"))
		return
	}
	if len(input.Value) > redisPreviewBytes || len(input.Previous) > redisPreviewBytes || input.TTL != nil && (*input.TTL < 0 || *input.TTL > redisMaxTTL) {
		writeErr(w, http.StatusBadRequest, fmt.Errorf("values must be at most 256 KB and expiry must be 0–2147483647 seconds"))
		return
	}
	key, err := redisKeyID(r)
	if err != nil {
		writeErr(w, http.StatusBadRequest, err)
		return
	}
	ctx, cancel := context.WithCancel(r.Context())
	defer cancel()
	client, _, err := s.redisConnect(ctx, r)
	if err != nil {
		writeErr(w, redisStatus(err), err)
		return
	}
	defer client.Close()
	ttl := ""
	if input.TTL != nil {
		ttl = strconv.FormatInt(*input.TTL, 10)
	}
	saved, err := redisEdit.Run(ctx, client, []string{key}, input.Previous, input.Value, ttl).Int()
	if err != nil {
		writeErr(w, redisStatus(err), err)
		return
	}
	if saved == 0 {
		writeErr(w, http.StatusConflict, fmt.Errorf("this key changed or expired; reload it before saving"))
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true})
}

func (s *Server) handleRedisCreate(w http.ResponseWriter, r *http.Request) {
	var input struct {
		Name  string     `json:"name"`
		Type  string     `json:"type"`
		Value string     `json:"value"`
		Items [][]string `json:"items"`
		TTL   *int64     `json:"ttl"`
	}
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 2*redisPreviewBytes)).Decode(&input); err != nil {
		writeErr(w, http.StatusBadRequest, fmt.Errorf("invalid key data"))
		return
	}
	if input.Type == "" {
		input.Type = "string"
	}
	ttl := int64(0)
	if input.TTL != nil {
		ttl = *input.TTL
	}
	args, err := redisCreateArgs(input.Name, input.Type, input.Value, input.Items, ttl)
	if err != nil {
		writeErr(w, http.StatusBadRequest, err)
		return
	}
	ctx, cancel := context.WithCancel(r.Context())
	defer cancel()
	client, _, err := s.redisConnect(ctx, r)
	if err != nil {
		writeErr(w, redisStatus(err), err)
		return
	}
	defer client.Close()
	created, err := redisCreate.Run(ctx, client, []string{input.Name}, args...).Int()
	if err != nil {
		writeErr(w, redisStatus(err), err)
		return
	}
	if created == 0 {
		writeErr(w, http.StatusConflict, fmt.Errorf("that key already exists"))
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "id": redisID(input.Name), "name": input.Name})
}

// redisCreateArgs checks a new key and lays its elements out for the
// create script. Redis keeps no empty collections, so every type but a
// string starts with at least one element.
func redisCreateArgs(name, kind, value string, items [][]string, ttl int64) ([]any, error) {
	if name == "" || len(name) > redisNameBytes {
		return nil, fmt.Errorf("name the key — at most 4096 bytes")
	}
	if ttl < 0 || ttl > redisMaxTTL {
		return nil, fmt.Errorf("expiry must be 0–2147483647 seconds")
	}
	args := []any{kind, ttl}
	if kind == "string" {
		if len(value) > redisPreviewBytes {
			return nil, fmt.Errorf("values must be at most 256 KB")
		}
		return append(args, value), nil
	}
	width := map[string]int{"hash": 2, "list": 1, "set": 1, "zset": 2, "stream": 2}[kind]
	if width == 0 {
		return nil, fmt.Errorf("unknown key type %q", kind)
	}
	if len(items) == 0 || len(items) > redisCreateItems {
		return nil, fmt.Errorf("a new %s starts with 1 to %d elements", kind, redisCreateItems)
	}
	total := 0
	for _, item := range items {
		if len(item) != width {
			return nil, fmt.Errorf("every element of a %s has %d parts", kind, width)
		}
		for _, part := range item {
			total += len(part)
		}
		if kind == "zset" {
			// ZADD takes the score first.
			score, err := redisScore(item[1])
			if err != nil {
				return nil, err
			}
			args = append(args, score, item[0])
			continue
		}
		for _, part := range item {
			args = append(args, part)
		}
	}
	if total > redisPreviewBytes {
		return nil, fmt.Errorf("a new key's elements must add up to at most 256 KB")
	}
	return args, nil
}

func redisScore(raw string) (string, error) {
	raw = strings.TrimSpace(raw)
	switch strings.ToLower(raw) {
	case "inf", "+inf", "-inf":
		return strings.ToLower(raw), nil
	}
	f, err := strconv.ParseFloat(raw, 64)
	if err != nil || f != f {
		return "", fmt.Errorf("%q is not a score — use a number, inf or -inf", raw)
	}
	return raw, nil
}

// redisBytes is an element as the console sends it back to address or
// write: text, or {"base64": …} for bytes that are not.
type redisBytes []byte

func (b *redisBytes) UnmarshalJSON(data []byte) error {
	var text string
	if err := json.Unmarshal(data, &text); err == nil {
		*b = redisBytes(text)
		return nil
	}
	var wrapped struct {
		Base64 *string `json:"base64"`
	}
	if err := json.Unmarshal(data, &wrapped); err != nil || wrapped.Base64 == nil {
		return fmt.Errorf(`expected text or {"base64": …}`)
	}
	raw, err := base64.StdEncoding.DecodeString(*wrapped.Base64)
	if err != nil {
		return fmt.Errorf("invalid base64")
	}
	*b = raw
	return nil
}

// One change to an element of a collection, made only while the key is
// still the type it was read as — a key that expired is never brought back
// by an edit. An edit that names the element's previous value (a list item,
// a hash field) lands only on that value. ARGV[1] is the operation; the
// rest are its arguments, in the order handleRedisChange lays them out.
var redisChange = redis.NewScript(`
local key, op = KEYS[1], ARGV[1]
local want = ({hset='hash', hdel='hash', lpush='list', rpush='list', lset='list', lrem='list',
  sadd='set', srem='set', zadd='zset', zrem='zset', xadd='stream', xdel='stream'})[op]
local kind = redis.call('TYPE', key).ok
if kind == 'none' then return {'missing'} end
if kind ~= want then return {'type', kind} end
if op == 'hset' then
  if ARGV[4] == 'create' then
    if redis.call('HSETNX', key, ARGV[2], ARGV[3]) == 0 then return {'exists'} end
  else
    if ARGV[4] == 'replace' and redis.call('HGET', key, ARGV[2]) ~= ARGV[5] then return {'changed'} end
    redis.call('HSET', key, ARGV[2], ARGV[3])
  end
elseif op == 'hdel' then redis.call('HDEL', key, ARGV[2])
elseif op == 'lpush' then redis.call('LPUSH', key, ARGV[2])
elseif op == 'rpush' then redis.call('RPUSH', key, ARGV[2])
elseif op == 'lset' or op == 'lrem' then
  local index = tonumber(ARGV[2])
  if redis.call('LINDEX', key, index) ~= ARGV[3] then return {'changed'} end
  redis.call('LSET', key, index, ARGV[4])
  -- Lists remove by value, not position: the item becomes a one-off
  -- marker, and the marker is what goes.
  if op == 'lrem' then redis.call('LREM', key, 1, ARGV[4]) end
elseif op == 'sadd' then
  if redis.call('SADD', key, ARGV[2]) == 0 then return {'exists'} end
elseif op == 'srem' then redis.call('SREM', key, ARGV[2])
elseif op == 'zadd' then
  if ARGV[4] == 'create' then
    if redis.call('ZADD', key, 'NX', ARGV[3], ARGV[2]) == 0 then return {'exists'} end
  else
    if not redis.call('ZSCORE', key, ARGV[2]) then return {'changed'} end
    redis.call('ZADD', key, 'XX', ARGV[3], ARGV[2])
  end
elseif op == 'zrem' then redis.call('ZREM', key, ARGV[2])
elseif op == 'xadd' then
  local args = {}
  for i = 2, #ARGV do args[#args + 1] = ARGV[i] end
  redis.call('XADD', key, '*', unpack(args))
elseif op == 'xdel' then redis.call('XDEL', key, ARGV[2])
end
return {'ok', redis.call('EXISTS', key)}
`)

// handleRedisChange changes one key in place: its expiry, its name, or one
// element of a collection. The answer says whether the key still exists —
// Redis drops a collection with its last element.
func (s *Server) handleRedisChange(w http.ResponseWriter, r *http.Request) {
	key, err := redisKeyID(r)
	if err != nil {
		writeErr(w, http.StatusBadRequest, err)
		return
	}
	var in struct {
		Op       string      `json:"op"`
		Field    *redisBytes `json:"field"`
		Member   *redisBytes `json:"member"`
		Value    *redisBytes `json:"value"`
		Previous *redisBytes `json:"previous"`
		Index    *int64      `json:"index"`
		Score    string      `json:"score"`
		Entry    string      `json:"entry"`
		Fields   [][2]string `json:"fields"`
		Create   bool        `json:"create"`
		TTL      int64       `json:"ttl"`
		Name     string      `json:"name"`
	}
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 4*redisPreviewBytes)).Decode(&in); err != nil {
		writeErr(w, http.StatusBadRequest, fmt.Errorf("invalid change: %w", err))
		return
	}
	for _, part := range []*redisBytes{in.Field, in.Member, in.Value, in.Previous} {
		if part != nil && len(*part) > redisPreviewBytes {
			writeErr(w, http.StatusBadRequest, fmt.Errorf("values must be at most 256 KB"))
			return
		}
	}
	args, err := redisChangeArgs(in.Op, in.Field, in.Member, in.Value, in.Previous, in.Index, in.Score, in.Entry, in.Fields, in.Create)
	switch {
	case err != nil:
	case in.Op == "expire" && (in.TTL < 1 || in.TTL > redisMaxTTL):
		err = fmt.Errorf("expiry must be 1–2147483647 seconds")
	case in.Op == "rename" && (in.Name == "" || len(in.Name) > redisNameBytes):
		err = fmt.Errorf("name the key — at most 4096 bytes")
	}
	if err != nil {
		writeErr(w, http.StatusBadRequest, err)
		return
	}
	ctx, cancel := context.WithCancel(r.Context())
	defer cancel()
	client, _, err := s.redisConnect(ctx, r)
	if err != nil {
		writeErr(w, redisStatus(err), err)
		return
	}
	defer client.Close()
	switch in.Op {
	case "expire":
		ok, err := client.Expire(ctx, key, time.Duration(in.TTL)*time.Second).Result()
		if err == nil && !ok {
			err = errRedisGone
		}
		redisChanged(w, err, map[string]any{"ok": true, "exists": true})
	case "persist":
		n, err := client.Exists(ctx, key).Result()
		if err == nil && n == 0 {
			err = errRedisGone
		}
		if err == nil {
			err = client.Persist(ctx, key).Err()
		}
		redisChanged(w, err, map[string]any{"ok": true, "exists": true})
	case "rename":
		renamed := map[string]any{"ok": true, "exists": true, "id": redisID(in.Name), "name": in.Name}
		if in.Name == key {
			writeJSON(w, http.StatusOK, renamed)
			return
		}
		ok, err := client.RenameNX(ctx, key, in.Name).Result()
		if err != nil && strings.Contains(err.Error(), "no such key") {
			err = errRedisGone
		}
		if err == nil && !ok {
			writeErr(w, http.StatusConflict, fmt.Errorf("a key named %q already exists", in.Name))
			return
		}
		redisChanged(w, err, renamed)
	default:
		result, err := redisChange.Run(ctx, client, []string{key}, args...).Slice()
		if err != nil {
			writeErr(w, redisStatus(err), err)
			return
		}
		switch result[0] {
		case "ok":
			writeJSON(w, http.StatusOK, map[string]any{"ok": true, "exists": result[1] == int64(1)})
		case "missing":
			writeErr(w, http.StatusNotFound, errRedisGone)
		case "type":
			writeErr(w, http.StatusConflict, fmt.Errorf("this key holds a %v now; reload it", result[1]))
		case "exists":
			what := map[string]string{"hset": "that field is already in this hash", "sadd": "that member is already in this set", "zadd": "that member is already in this sorted set"}[in.Op]
			writeErr(w, http.StatusConflict, errors.New(what))
		default:
			writeErr(w, http.StatusConflict, fmt.Errorf("this item changed since it was read; reload it"))
		}
	}
}

// redisChanged answers a plain-command change: its error in the console's
// terms, or the given answer.
func redisChanged(w http.ResponseWriter, err error, answer map[string]any) {
	if err != nil {
		writeErr(w, redisStatus(err), err)
		return
	}
	writeJSON(w, http.StatusOK, answer)
}

// redisChangeArgs lays out the change script's arguments for one element
// operation, or nothing for the key-level ones (expire, persist, rename).
func redisChangeArgs(op string, field, member, value, previous *redisBytes, index *int64, score, entry string, fields [][2]string, create bool) ([]any, error) {
	need := func(parts ...*redisBytes) error {
		for _, p := range parts {
			if p == nil {
				return fmt.Errorf("%s needs more to go on", op)
			}
		}
		return nil
	}
	str := func(b *redisBytes) string { return string(*b) }
	switch op {
	case "expire", "persist", "rename":
		return nil, nil
	case "hset":
		if err := need(field, value); err != nil {
			return nil, err
		}
		mode, before := "set", ""
		if create {
			mode = "create"
		} else if previous != nil {
			mode, before = "replace", str(previous)
		}
		return []any{op, str(field), str(value), mode, before}, nil
	case "hdel":
		if err := need(field); err != nil {
			return nil, err
		}
		return []any{op, str(field)}, nil
	case "lpush", "rpush":
		if err := need(value); err != nil {
			return nil, err
		}
		return []any{op, str(value)}, nil
	case "lset", "lrem":
		if index == nil {
			return nil, fmt.Errorf("%s needs the item's index", op)
		}
		if err := need(previous); err != nil {
			return nil, err
		}
		if op == "lset" {
			if err := need(value); err != nil {
				return nil, err
			}
			return []any{op, *index, str(previous), str(value)}, nil
		}
		marker := make([]byte, 16)
		_, _ = rand.Read(marker)
		return []any{op, *index, str(previous), "donkey:removed:" + hex.EncodeToString(marker)}, nil
	case "sadd", "srem", "zrem":
		if err := need(member); err != nil {
			return nil, err
		}
		return []any{op, str(member)}, nil
	case "zadd":
		if err := need(member); err != nil {
			return nil, err
		}
		parsed, err := redisScore(score)
		if err != nil {
			return nil, err
		}
		mode := "change"
		if create {
			mode = "create"
		}
		return []any{op, str(member), parsed, mode}, nil
	case "xadd":
		if len(fields) == 0 || len(fields) > redisPageItems {
			return nil, fmt.Errorf("a stream entry has 1 to %d fields", redisPageItems)
		}
		args := []any{op}
		total := 0
		for _, f := range fields {
			total += len(f[0]) + len(f[1])
			args = append(args, f[0], f[1])
		}
		if total > redisPreviewBytes {
			return nil, fmt.Errorf("an entry's fields must add up to at most 256 KB")
		}
		return args, nil
	case "xdel":
		if !redisStreamID.MatchString(entry) {
			return nil, fmt.Errorf("invalid stream entry ID")
		}
		return []any{op, entry}, nil
	}
	return nil, fmt.Errorf("unknown change %q", op)
}

func (s *Server) handleRedisDelete(w http.ResponseWriter, r *http.Request) {
	key, err := redisKeyID(r)
	if err != nil {
		writeErr(w, http.StatusBadRequest, err)
		return
	}
	ctx, cancel := context.WithCancel(r.Context())
	defer cancel()
	client, _, err := s.redisConnect(ctx, r)
	if err != nil {
		writeErr(w, redisStatus(err), err)
		return
	}
	defer client.Close()
	if err := client.Unlink(ctx, key).Err(); err != nil && !errors.Is(err, redis.Nil) {
		writeErr(w, redisStatus(err), err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true})
}
