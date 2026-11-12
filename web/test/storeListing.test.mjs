import assert from 'node:assert/strict'
import { test } from 'node:test'
import { lastSegment, listingNames, storeLabel, storeRows } from '../src/storeListing.ts'

const file = (key, size = 1, modified = '2026-11-01T00:00:00Z') => ({ key, size, modified })
const page = (prefixes, objects, displayNames) => ({ prefixes, objects, displayNames, truncated: false, next: '' })
const sort = { by: 'name', desc: false }

test('labels control name order and filtering while duplicate names keep separate keys and selections', () => {
  const pages = [
    page(['dir-2/', 'dir-1/'], [file('file-2'), file('file-1')], {
      'dir-1/': 'Reports',
      'dir-2/': 'Reports',
      'file-1': 'Zebra',
      'file-2': 'Alpha',
    }),
    page(['dir-1/'], [file('file-3'), file('file-2')], { 'file-3': 'Alpha' }),
  ]
  const rows = storeRows(pages, '', sort)
  assert.deepEqual(
    rows.map((r) => r.key),
    ['dir-1/', 'dir-2/', 'file-2', 'file-3', 'file-1'],
  )
  assert.deepEqual(
    rows.map((r) => r.label),
    ['Reports', 'Reports', 'Alpha', 'Alpha', 'Zebra'],
  )
  assert.equal(new Set(rows.map((r) => r.id)).size, 5)
  assert.deepEqual(
    storeRows(pages, ' REPORTS ', sort).map((r) => r.key),
    ['dir-1/', 'dir-2/'],
  )
  assert.deepEqual(
    storeRows(pages, 'alpha', sort).map((r) => r.key),
    ['file-2', 'file-3'],
  )
  assert.deepEqual(storeRows(pages, 'file-1', sort), [])
  assert.deepEqual(
    storeRows(pages, '', { ...sort, desc: true }).map((r) => r.key),
    ['dir-2/', 'dir-1/', 'file-1', 'file-3', 'file-2'],
  )
  // Navigation, deletion, downloads and upload conflicts all retain these keys.
  assert.deepEqual(
    rows.filter((r) => r.folder).map((r) => r.key),
    ['dir-1/', 'dir-2/'],
  )
  assert.deepEqual(
    rows.filter((r) => !r.folder).map((r) => r.object.key),
    ['file-2', 'file-3', 'file-1'],
  )
})

test('missing names use the already decoded basename without decoding literal percent escapes again', () => {
  const pages = [page(['opaque/fallback ü/'], [file('opaque/a b.txt'), file('opaque/100%20.txt')])]
  assert.deepEqual(
    storeRows(pages, '', sort).map((r) => r.label),
    ['fallback ü', '100%20.txt', 'a b.txt'],
  )
  assert.equal(storeRows(pages, 'a b', sort)[0].key, 'opaque/a b.txt')
  assert.equal(lastSegment('opaque/100%20.txt'), '100%20.txt')
  assert.equal(storeLabel('empty.txt', new Map([['empty.txt', '']])), 'empty.txt')
})

test('breadcrumb labels and HTML-like names remain literal strings keyed by the original paths', () => {
  const label = '<img src=x onerror=alert(1)> & **notes** / 100%20'
  const pages = [
    page([], [file('opaque')], { '': 'Shared files', 'dir-1/': 'Reports', opaque: label, __proto__: 'ignored' }),
  ]
  const names = listingNames(pages)
  assert.equal(storeLabel('dir-1/', names), 'Reports')
  assert.equal(storeLabel('', names), 'Shared files')
  assert.equal(storeRows(pages, 'onerror', sort)[0].label, label)
  assert.equal(storeRows(pages, '', sort)[0].key, 'opaque')
  // Keys that match Object prototype properties still have ordinary labels.
  assert.equal(storeLabel('constructor', names), 'constructor')
  assert.equal(storeLabel('__proto__', listingNames([page([], [], JSON.parse('{"__proto__":"A label"}'))])), 'A label')
})

test('size and modified sorting keep their meaning and use display names to break ties', () => {
  const pages = [
    page(['z/', 'a/'], [file('a', 4, '2026-11-02'), file('b', 4, '2026-11-01'), file('c', 2, '2026-11-01')], {
      'z/': 'Alpha',
      'a/': 'Zebra',
      a: 'Zebra',
      b: 'Alpha',
      c: 'Middle',
    }),
  ]
  assert.deepEqual(
    storeRows(pages, '', { by: 'size', desc: false }).map((r) => r.key),
    ['z/', 'a/', 'c', 'b', 'a'],
  )
  assert.deepEqual(
    storeRows(pages, '', { by: 'modified', desc: false }).map((r) => r.key),
    ['z/', 'a/', 'b', 'c', 'a'],
  )
})
