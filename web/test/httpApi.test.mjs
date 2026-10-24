import assert from 'node:assert/strict'
import { registerHooks } from 'node:module'
import { test } from 'node:test'

// The console's modules import each other without extensions, as the
// bundler allows; under node the hook adds the .ts.
registerHooks({
  resolve(specifier, context, next) {
    if (/^\.\.?\//.test(specifier) && !/\.[cm]?[jt]sx?$/.test(specifier)) {
      try {
        return next(`${specifier}.ts`, context)
      } catch {
        /* not a .ts module — resolve as written */
      }
    }
    return next(specifier, context)
  },
})

const http = await import('../src/httpApi.ts')
const row = (key, value, on = true) => ({ id: key, key, value, on })
const draft = (patch) => ({ ...http.emptyDraft(), ...patch })

test('the URL bar and the parameters table say the same thing', () => {
  const parsed = http.parseURL('/items?limit=10&q=hello world&flag&a=b=c', '', [])
  assert.equal(parsed.path, '/items')
  assert.deepEqual(
    parsed.params.map(({ key, value, on }) => [key, value, on]),
    [['limit', '10', true], ['q', 'hello world', true], ['flag', '', true], ['a', 'b=c', true]],
  )
  assert.equal(http.composeURL(parsed.path, parsed.params), '/items?limit=10&q=hello%20world&flag&a=b=c')
  // Composed again, it parses into the same table.
  const again = http.parseURL(http.composeURL(parsed.path, parsed.params), '', parsed.params)
  assert.deepEqual(again.params.map(({ key, value }) => [key, value]), parsed.params.map(({ key, value }) => [key, value]))
})

test('a query keeps its shape whatever the values hold', () => {
  const url = http.composeURL('/s', [row('q', 'a&b#c%d'), row('k=y', 'v'), row('plus', '1+1'), row('é', 'ü')])
  assert.equal(url, '/s?q=a%26b%23c%25d&k%3Dy=v&plus=1+1&%C3%A9=%C3%BC')
  const back = http.parseURL(url, '', [])
  assert.deepEqual(back.params.map(({ key, value }) => [key, value]), [['q', 'a&b#c%d'], ['k=y', 'v'], ['plus', '1+1'], ['é', 'ü']])
  // A malformed escape is kept as typed rather than dropped.
  assert.deepEqual(http.parseURL('/s?q=100%', '', []).params.map((p) => p.value), ['100%'])
})

test('unticked parameters stay in the table and out of the URL', () => {
  const params = [row('a', '1'), row('off', 'x', false)]
  assert.equal(http.composeURL('/p', params), '/p?a=1')
  const typed = http.parseURL('/p?a=2&b=3', '', params)
  assert.deepEqual(typed.params.map(({ key, value, on }) => [key, value, on]), [['a', '2', true], ['b', '3', true], ['off', 'x', false]])
  // Rows keep their ids by position, so the table's inputs are not remounted.
  assert.equal(typed.params[0].id, 'a')
})

test('a URL pasted whole loses the service base', () => {
  const base = 'http://proxy.internal:8080/services/erp/'
  assert.deepEqual(http.parseURL(`${base}items/7?x=1`, base, []).path, '/items/7')
  assert.equal(http.fullURL(base, '/items?x=1'), `${base}items?x=1`)
  assert.equal(http.fullURL(base.slice(0, -1), 'items'), `${base}items`)
})

test('the body brings its Content-Type unless a header sets one', () => {
  const json = draft({ bodyKind: 'json', body: '{"a":1}' })
  assert.deepEqual(http.headersOf(json), { 'Content-Type': 'application/json' })
  const own = draft({ bodyKind: 'json', body: '{}', headers: [row('content-type', 'application/merge-patch+json')] })
  assert.deepEqual(http.headersOf(own), { 'content-type': 'application/merge-patch+json' })
  const off = draft({ headers: [row('X-Off', '1', false), row('Accept', 'a'), row('accept', 'b'), row(' ', 'blank name')] })
  assert.deepEqual(http.headersOf(off), { accept: 'b' })
  const form = draft({ bodyKind: 'form', form: [row('name', 'Ada Lovelace'), row('tag', 'a&b'), row('skip', 'x', false)] })
  assert.equal(http.bodyOf(form), 'name=Ada+Lovelace&tag=a%26b')
  assert.equal(http.headersOf(form)['Content-Type'], 'application/x-www-form-urlencoded')
  assert.equal(http.bodyOf(draft({ bodyKind: 'none', body: 'ignored' })), '')
})

test('copy as curl quotes for the shell and says the method only when needed', () => {
  const base = 'http://proxy/services/erp/'
  assert.equal(http.curlFor(draft({ path: '/items' }), base), `curl 'http://proxy/services/erp/items'`)
  const post = draft({ method: 'POST', path: '/it', bodyKind: 'json', body: `{"name":"O'Brien"}` })
  assert.equal(
    http.curlFor(post, base),
    `curl -X POST 'http://proxy/services/erp/it' \\\n  -H 'Content-Type: application/json' \\\n  --data-raw '{"name":"O'\\''Brien"}'`,
  )
  assert.match(http.curlFor(draft({ method: 'HEAD', path: '/' }), base), /^curl --head /)
})

test('bodies are shown in the language they are written in', () => {
  assert.equal(http.bodyLanguage('application/json; charset=utf-8', '{}'), 'json')
  assert.equal(http.bodyLanguage('application/problem+json', '{}'), 'json')
  assert.equal(http.bodyLanguage('text/plain', ' [1, 2]'), 'json')
  assert.equal(http.bodyLanguage('text/plain', '[not json'), '')
  assert.equal(http.bodyLanguage('application/xml', '<a/>'), 'html')
  assert.notEqual(http.jsonProblem('{"a":'), '')
  assert.equal(http.jsonProblem('  '), '')
  assert.deepEqual([...http.decodeBase64(btoa('\x89PNG'))], [0x89, 0x50, 0x4e, 0x47])
})

test('a stored request that no longer has the expected shape is mended or dropped', () => {
  const saved = http.savedBox('t', 'parse')
  assert.deepEqual(saved.get(), [])
  const history = http.historyBox('t', 'parse')
  http.remember('t', 'parse', { draft: draft({ bodyKind: 'text', body: 'x'.repeat(20_000) }), status: 200, durationMs: 3 })
  const [kept] = history.get()
  assert.equal(kept.status, 200)
  assert.equal(kept.draft.body, '')
  assert.equal(kept.bodyDropped, true)
  assert.equal(http.sameDraft(draft({ params: [row('a', '1')] }), draft({ params: [{ ...row('a', '1'), id: 'other' }] })), true)
})
