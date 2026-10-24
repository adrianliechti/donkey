import assert from 'node:assert/strict'
import { registerHooks } from 'node:module'
import { test } from 'node:test'

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

const exa = await import('../src/exaApi.ts')

test('a search in the URL becomes the request Exa takes', () => {
  const options = exa.optionsFrom({ q: 'rust', type: 'neural', n: '250', cat: 'news', inc: 'https://github.com/x, docs.rs', exc: '', from: '2025-01-01', to: '2025-12-31', open: 'ignored' })
  assert.deepEqual(exa.searchBody(options), {
    query: 'rust',
    numResults: 100,
    contents: { highlights: { numSentences: 2, highlightsPerUrl: 3 } },
    type: 'neural',
    category: 'news',
    includeDomains: ['github.com', 'docs.rs'],
    startPublishedDate: '2025-01-01T00:00:00.000Z',
    endPublishedDate: '2025-12-31T23:59:59.999Z',
  })
  // Unset options are not sent: Exa's own defaults apply.
  assert.deepEqual(exa.searchBody(exa.optionsFrom({ q: 'x' })), {
    query: 'x',
    numResults: 10,
    contents: { highlights: { numSentences: 2, highlightsPerUrl: 3 } },
  })
  assert.equal(exa.describe(options), 'Neural · News · 250 results · only github.com, docs.rs · 2025-01-01 – 2025-12-31')
  assert.equal(exa.hostOf('https://www.example.com/a'), 'example.com')
  assert.equal(exa.safeLink('javascript:alert(1)'), false)
})
