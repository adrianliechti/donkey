import assert from 'node:assert/strict'
import { test } from 'node:test'
import { filterSpaces, isDeploying, recentDeploys, spaceSecurity, spaceState, summarizeSpaces } from '../src/fleet.ts'

const app = (name, space, healthy = true, ready = '1/1') => ({ name, space, healthy, ready, image: '', url: '' })
const deploy = (name, space, stage, startedAt = '2026-10-25T10:00:00Z') => ({
  app: name,
  space,
  stage,
  startedAt,
  via: 'cli',
  ...(stage === 'failed' || stage === 'done' ? { finishedAt: '2026-10-25T10:02:00Z' } : {}),
})

test('security warnings aggregate without changing running app health', () => {
  const apps = [
    { ...app('clean', 'team'), security: { state: 'clean', summary: { secrets: 0, critical: 0 } } },
    { ...app('warning', 'team'), security: { state: 'warning', summary: { secrets: 1, critical: 0, unknown: 2 } } },
  ]
  assert.deepEqual(spaceSecurity(apps), { state: 'warning', summary: { secrets: 1, critical: 0, unknown: 2 } })
  assert.equal(summarizeSpaces(['team'], apps, [])[0].running, 2)
  assert.equal(apps[0].security.summary.unknown, undefined)
  assert.equal(spaceSecurity([app('legacy', 'team')]), undefined)
})

test('health counts separate running, down, and unknown, including the default space', () => {
  const [space, empty] = summarizeSpaces(
    ['default', 'empty'],
    [
      app('healthy', ''),
      app('down', 'default', false, '0/1'),
      app('unavailable', 'default', true, 'status unavailable'),
      app('hidden', 'not-visible'),
    ],
    [],
  )
  assert.equal(space.running, 1)
  assert.equal(space.down, 1)
  assert.equal(space.unknown, 1)
  assert.deepEqual(
    space.issues.map((issue) => issue.name),
    ['down', 'unavailable'],
  )
  assert.equal(empty.apps.length, 0)
  assert.equal(empty.issues.length, 0)
})

test('failed first deploys and failed updates need attention without counting an app twice', () => {
  const [space] = summarizeSpaces(
    ['team'],
    [app('down', 'team', false, '0/1'), app('old-release', 'team')],
    [
      deploy('down', 'team', 'failed'),
      deploy('first-deploy', 'team', 'failed'),
      deploy('old-release', 'team', 'failed'),
    ],
  )
  assert.equal(space.apps.length, 2)
  assert.equal(space.running, 1)
  assert.equal(space.issues.length, 3)
  assert.ok(space.issues.find((issue) => issue.name === 'down').app)
  assert.ok(space.issues.find((issue) => issue.name === 'down').deploy)
  assert.deepEqual(filterSpaces([space], 'FIRST-DEPLOY', 'attention', new Set()), [space])
})

test('active deploys precede completions, with the latest completion first', () => {
  const older = deploy('older', 'team', 'done')
  const newer = { ...deploy('newer', 'team', 'failed'), finishedAt: '2026-10-25T10:03:00Z' }
  const building = deploy('building', 'team', 'building', '2026-10-25T09:59:00Z')
  const queued = deploy('queued', 'team', 'queued', '2026-10-25T10:04:00Z')
  const source = [older, newer, building, queued]
  assert.deepEqual(recentDeploys(source), [queued, building, newer, older])
  assert.deepEqual(source, [older, newer, building, queued])
  assert.equal(isDeploying({ ...newer, finishedAt: undefined }), false)
  assert.equal(isDeploying({ ...older, finishedAt: undefined }), false)
})

test('search combines names, apps, first deploys, and filters, then puts pins first', () => {
  const spaces = summarizeSpaces(
    ['zeta', 'alpha', 'beta'],
    [app('My-API', 'beta')],
    [deploy('first-app', 'zeta', 'building')],
  )
  const pinned = new Set(['beta', 'not-visible'])
  const names = (query, filter) => filterSpaces(spaces, query, filter, pinned).map((space) => space.name)
  assert.deepEqual(names('', 'all'), ['beta', 'alpha', 'zeta'])
  assert.deepEqual(names('  MY-api  ', 'all'), ['beta'])
  assert.deepEqual(names('FIRST-APP', 'all'), ['zeta'])
  assert.deepEqual(names('', 'pinned'), ['beta'])
  assert.deepEqual(names('alpha', 'pinned'), [])
  assert.deepEqual(names('', 'attention'), [])
  assert.deepEqual(
    spaces.map((space) => space.name),
    ['zeta', 'alpha', 'beta'],
  )
})

test('a space is described by its worst news first, and only news speaks', () => {
  const describe = (apps, deploys) => spaceState(summarizeSpaces(['s'], apps, deploys)[0])
  assert.deepEqual(describe([], []), { tone: 'muted', label: 'No apps', busy: false, news: false })
  assert.deepEqual(describe([app('a', 's')], []), { tone: 'success', label: 'Healthy', busy: false, news: false })
  assert.deepEqual(describe([app('a', 's')], [deploy('b', 's', 'building')]), {
    tone: 'accent',
    label: 'Deploying',
    busy: true,
    news: true,
  })
  assert.deepEqual(describe([app('a', 's', true, 'status unavailable')], [deploy('b', 's', 'building')]), {
    tone: 'warning',
    label: '1 unknown',
    busy: false,
    news: true,
  })
  assert.deepEqual(describe([app('a', 's')], [deploy('a', 's', 'failed')]), {
    tone: 'danger',
    label: 'Deploy failed',
    busy: false,
    news: true,
  })
  assert.deepEqual(describe([app('a', 's', false, '0/1'), app('b', 's', false, '0/1')], [deploy('c', 's', 'failed')]), {
    tone: 'danger',
    label: '2 not fully up',
    busy: false,
    news: true,
  })
})
