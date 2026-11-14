import assert from 'node:assert/strict'
import { test } from 'node:test'
import {
  appIndex,
  bySpace,
  contactAddresses,
  cores,
  filterFindings,
  filterTechnologies,
  fullness,
  percent,
  severityCounts,
  share,
  sumResource,
  unassigned,
  worstState,
} from '../src/reports.ts'

const affected = (space, app, state = 'fixable', packages = []) => ({
  space,
  app,
  severity: 'critical',
  state,
  packages,
})
const finding = (id, severity, apps, extra = {}) => ({
  kind: 'vulnerability',
  id,
  severity,
  fixable: true,
  apps,
  ...extra,
})

// The same fleet the server's own filter test reads (reports_test.go), so
// the two filters are held to the same answers.
const findings = [
  finding(
    'CVE-2026-0001',
    'critical',
    [
      affected('shop', 'api', 'judged', [{ name: 'openssl', installed: '3.0.11', fixed: '3.0.13' }]),
      affected('shop', 'web', 'blocking', [
        { name: 'libssl3', installed: '3.0.11', fixed: '3.0.13' },
        { name: 'openssl', installed: '3.0.11', fixed: '3.0.13' },
      ]),
      affected('tools', 'wiki', 'fixable', [{ name: 'openssl', installed: '3.1.4', fixed: '3.1.5' }]),
    ],
    { title: 'Title of CVE-2026-0001' },
  ),
  finding('CVE-2026-0002', 'critical', [affected('shop', 'web', 'waived', [{ name: 'lodash', installed: '4.17.20' }])]),
  {
    kind: 'secret',
    id: 'aws-access-key-id',
    severity: 'critical',
    fixable: false,
    title: 'AWS Access Key ID',
    apps: [{ space: 'shop', app: 'web', severity: 'critical', state: 'blocking', locations: ['app/.env:3'] }],
  },
  finding('CVE-2026-0004', 'medium', [affected('tools', 'wiki', 'fixable', [{ name: 'busybox', installed: '1.36' }])]),
  finding('CVE-2026-0003', 'low', [affected('shop', 'web', 'unfixed', [{ name: 'zlib1g', installed: '1.2.13' }])], {
    fixable: false,
  }),
]

const ids = (list) => list.map((f) => f.id).join(' ')
const filter = (overrides) => filterFindings(findings, { severity: 'all', query: '', fixable: false, ...overrides })

test('findings filter by severity, by fix, and by what a search names', () => {
  assert.equal(ids(filter({})), ids(findings))
  assert.equal(ids(filter({ severity: 'critical' })), 'CVE-2026-0001 CVE-2026-0002 aws-access-key-id')
  assert.equal(ids(filter({ severity: 'low' })), 'CVE-2026-0003')
  assert.equal(ids(filter({ fixable: true })), 'CVE-2026-0001 CVE-2026-0002 CVE-2026-0004')
  assert.equal(ids(filter({ query: 'nothing-like-this' })), '')
})

test('a search that names the finding keeps it whole', () => {
  for (const query of ['cve-2026-0001', '  LIBSSL ', 'title of cve-2026-0001']) {
    const [only, ...rest] = filter({ query })
    assert.equal(rest.length, 0, query)
    assert.equal(only.id, 'CVE-2026-0001')
    assert.equal(only.apps.length, 3, query)
  }
  assert.equal(ids(filter({ query: 'access key' })), 'aws-access-key-id')
})

test('a search that names a space or an app keeps only what is in it', () => {
  const tools = filter({ query: 'tools' })
  assert.equal(ids(tools), 'CVE-2026-0001 CVE-2026-0004')
  assert.deepEqual(
    tools[0].apps.map((app) => `${app.space}/${app.app}`),
    ['tools/wiki'],
  )
  assert.equal(filter({ query: 'shop/api' })[0].apps.length, 1)
  assert.equal(ids(filter({ query: 'wiki', severity: 'medium' })), 'CVE-2026-0004')
  // The report itself is never narrowed: the next search starts from all of it.
  assert.equal(findings[0].apps.length, 3)
})

test('severities are counted for the filter beside each', () => {
  assert.deepEqual(severityCounts(findings), { all: 5, critical: 3, high: 0, medium: 1, low: 1, unknown: 0 })
  assert.deepEqual(severityCounts(filter({ query: 'tools' })), {
    all: 2,
    critical: 1,
    high: 0,
    medium: 1,
    low: 0,
    unknown: 0,
  })
})

test('a finding in several apps is as urgent as its most urgent app', () => {
  assert.equal(worstState(findings[0].apps.map((app) => app.state)), 'blocking')
  assert.equal(worstState(['unfixed', 'waived', 'fixable']), 'waived')
  assert.equal(worstState(['fixable', 'judged']), 'judged')
  assert.equal(worstState([]), undefined)
})

test('technologies filter by name, or by the spaces and apps that use them', () => {
  const categories = [
    {
      id: 'language',
      technologies: [
        {
          name: 'Python',
          key: 'python',
          apps: [
            { space: 'shop', app: 'api' },
            { space: 'tools', app: 'wiki' },
          ],
        },
        {
          name: 'Node.js',
          key: 'node',
          apps: [
            { space: 'shop', app: 'web' },
            { space: 'tools', app: 'wiki' },
          ],
        },
      ],
    },
    { id: 'os', technologies: [{ name: 'debian 12', apps: [{ space: 'shop', app: 'web' }] }] },
  ]
  assert.equal(filterTechnologies(categories, '  '), categories)
  const python = filterTechnologies(categories, 'PYTH')
  assert.deepEqual(
    python.map((c) => c.technologies.map((t) => `${t.name}:${t.apps.length}`)),
    [['Python:2'], []],
  )
  const wiki = filterTechnologies(categories, 'wiki')
  assert.deepEqual(
    wiki.map((c) => c.technologies.map((t) => `${t.name}:${t.apps.length}`)),
    [['Python:1', 'Node.js:1'], []],
  )
  assert.equal(categories[0].technologies[0].apps.length, 2)
})

test('rows are gathered under their spaces, both in name order', () => {
  const grouped = bySpace([
    { space: 'tools', app: 'wiki' },
    { space: 'shop', app: 'web' },
    { space: 'shop', app: 'api' },
  ])
  assert.deepEqual(
    grouped.map((g) => [g.space, g.rows.map((r) => r.app)]),
    [
      ['shop', ['api', 'web']],
      ['tools', ['wiki']],
    ],
  )
})

test('the addresses to write to are a space’s admins, then its developers, then the platform’s', () => {
  const shop = {
    admins: { users: ['ada@shop.example', 'sub:abc123'], groups: ['shop-admins'] },
    developers: { users: ['dev@shop.example'] },
  }
  const devsOnly = { admins: { groups: ['only-a-group'] }, developers: { users: ['dev@tools.example'] } }
  const nobody = { admins: {}, developers: {} }
  const platform = { users: ['root@example.com'] }

  assert.deepEqual(contactAddresses([shop], platform), ['ada@shop.example'])
  // A group and a subject are not addresses; the developers stand in.
  assert.deepEqual(contactAddresses([devsOnly], platform), ['dev@tools.example'])
  assert.deepEqual(contactAddresses([nobody, undefined], platform), ['root@example.com'])
  assert.deepEqual(contactAddresses([shop, devsOnly, nobody, shop], platform), [
    'ada@shop.example',
    'dev@tools.example',
    'root@example.com',
  ])
  assert.deepEqual(contactAddresses([nobody]), [])

  assert.equal(unassigned(shop), false)
  assert.equal(unassigned(devsOnly), false)
  assert.equal(unassigned(nobody), true)
  assert.equal(unassigned(undefined), true)
})

test('apps are found by space and name', () => {
  const index = appIndex([{ space: 'shop', app: 'web', scanned: true }])
  assert.equal(index.get('shop/web').scanned, true)
  assert.equal(index.get('shop/api'), undefined)
})

test('a meter shows a share of what there is, and says when it is full', () => {
  assert.equal(share(1, 4), 0.25)
  assert.equal(share(5, 4), 1)
  assert.equal(share(undefined, 4), undefined)
  assert.equal(share(1, 0), undefined)
  assert.equal(fullness(undefined), 'ok')
  assert.equal(fullness(0.5), 'ok')
  assert.equal(fullness(0.75), 'high')
  assert.equal(fullness(0.9), 'full')
  assert.equal(percent(0.314), '31%')
  assert.equal(cores(0.04), '<0.1')
  assert.equal(cores(0), '0')
  assert.equal(cores(1.25), '1.3')
  assert.equal(cores(12.4), '12')
})

test('totals add up, and say nothing a node did not report', () => {
  assert.deepEqual(
    sumResource([
      { capacity: 4, used: 1, reserved: 2 },
      { capacity: 2, used: 0.5, reserved: 1 },
    ]),
    { capacity: 6, used: 1.5, reserved: 3 },
  )
  // One node without a reading: the total in use is unknown, not smaller.
  assert.deepEqual(sumResource([{ capacity: 4, used: 1 }, { capacity: 2 }]), {
    capacity: 6,
    used: undefined,
    reserved: undefined,
  })
  assert.deepEqual(sumResource([]), { capacity: 0, used: undefined, reserved: undefined })
})
