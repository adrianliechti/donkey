// Browser CRUD and service isolation, without an AI build or sample deployment.
import assert from 'node:assert/strict'
import { mkdirSync } from 'node:fs'
import { createRequire } from 'node:module'
import path from 'node:path'
import { fileURLToPath } from 'node:url'

const root = path.resolve(path.dirname(fileURLToPath(import.meta.url)), '..')
const require = createRequire(import.meta.url)
const { chromium } = require(process.env.DONKEY_E2E_PLAYWRIGHT || path.join(root, '.build/browser/node_modules/playwright'))
const domain = process.env.DONKEY_E2E_DOMAIN || 'apps.localhost'
const base = `https://${domain}`
const reports = path.join(root, '.build/e2e')
const spaces = ['e2e-admin-one', 'e2e-admin-two']
mkdirSync(reports, { recursive: true })
const browser = await chromium.launch({ headless: true, args: [`--host-resolver-rules=MAP *.${domain} 127.0.0.1, MAP ${domain} 127.0.0.1`] })
const context = await browser.newContext({ ignoreHTTPSErrors: true, viewport: { width: 1440, height: 1100 } })
const page = await context.newPage()
page.setDefaultTimeout(30000)
const servicePath = (space, service) => `/v1/spaces/${space}/services/${service}`
async function request(method, endpoint, data) {
  const res = await context.request.fetch(base + endpoint, { method, data, timeout: 120000 })
  assert.ok(res.ok(), `${method} ${endpoint}: ${res.status()} ${await res.text()}`)
  return res
}
async function sql(space, statement) {
  return (await request('POST', servicePath(space, 'postgres') + '/databases/app/query', { sql: statement })).json()
}
async function openService(space, service) {
  const card = page.locator('section').filter({ has: page.getByRole('heading', { name: space, exact: true }) })
  await card.getByRole('button', { name: service, exact: true }).click()
  return page.getByRole('dialog')
}
try {
  await page.goto(base, { waitUntil: 'networkidle' })
  if (await page.getByRole('link', { name: 'Sign in', exact: true }).count()) {
    await page.getByRole('link', { name: 'Sign in', exact: true }).click()
    await page.waitForURL(url => url.hostname === domain)
  }
  for (const space of spaces) {
    const removed = await context.request.delete(`${base}/v1/spaces/${space}`)
    assert.ok(removed.ok() || removed.status() === 404)
    await request('POST', '/v1/spaces', { name: space })
    for (const type of ['postgres', 'redis']) await request('POST', `/v1/spaces/${space}/services`, { type })
    await sql(space, 'CREATE TABLE marker (value text PRIMARY KEY)')
    await sql(space, `INSERT INTO marker VALUES ('${space}')`)
    await request('POST', servicePath(space, 'redis') + '/keys', { name: 'marker', value: space })
  }
  await page.reload({ waitUntil: 'networkidle' })
  const keyID = Buffer.from('marker').toString('base64url')
  for (const space of spaces) {
    assert.deepEqual((await sql(space, 'SELECT value FROM marker')).rows, [[space]])
    const value = await (await request('GET', servicePath(space, 'redis') + `/keys/value?id=${keyID}`)).json()
    assert.equal(value.value, space, 'same-named services must stay isolated between spaces')
    const dialog = await openService(space, 'postgres')
    await dialog.getByRole('button', { name: 'Run SQL', exact: true }).click()
    await dialog.getByRole('textbox', { name: 'SQL statement' }).fill('SELECT value FROM marker')
    await dialog.getByRole('button', { name: 'Run', exact: true }).click()
    await dialog.getByText(space, { exact: true }).waitFor()
    await dialog.getByRole('button', { name: 'Close', exact: true }).click()
  }
  console.log('PostgreSQL and Redis: same service names in two spaces stay isolated; SQL browser works')

  const dialog = await openService(spaces[0], 'redis')
  await dialog.getByRole('button', { name: 'New key', exact: true }).click()
  await dialog.getByRole('textbox', { name: 'Key name' }).fill('browser-key')
  await dialog.getByRole('textbox', { name: 'Key value' }).fill('created in Chromium')
  await dialog.getByRole('spinbutton', { name: 'Expiry in seconds' }).fill('600')
  await dialog.getByRole('button', { name: 'Create key', exact: true }).click()
  await dialog.getByRole('textbox', { name: 'Key pattern' }).fill('browser-*')
  await dialog.getByRole('button', { name: 'Find keys', exact: true }).click()
  await dialog.getByRole('button', { name: 'browser-key', exact: true }).click()
  const value = dialog.getByRole('textbox', { name: 'Key value' })
  await value.waitFor()
  assert.equal(await value.inputValue(), 'created in Chromium')
  await value.fill('edited in Chromium')
  const saved = page.waitForResponse(r => r.url().includes('/keys/value?') && r.request().method() === 'PUT')
  await dialog.getByRole('button', { name: 'Save value', exact: true }).click()
  assert.equal((await saved).status(), 200)
  const id = Buffer.from('browser-key').toString('base64url')
  const edited = await (await request('GET', servicePath(spaces[0], 'redis') + `/keys/value?id=${id}`)).json()
  assert.equal(edited.value, 'edited in Chromium')
  assert.ok(edited.ttl > 0 && edited.ttl <= 600, 'editing must preserve expiry')
  await page.screenshot({ path: path.join(reports, 'redis-browser.png'), fullPage: true })
  await dialog.getByRole('button', { name: 'Delete key', exact: true }).click()
  const deleted = page.waitForResponse(r => r.url().includes('/keys/value?') && r.request().method() === 'DELETE')
  await dialog.getByRole('button', { name: 'Delete this key', exact: true }).click()
  assert.ok((await deleted).ok())
  assert.equal((await context.request.get(base + servicePath(spaces[0], 'redis') + `/keys/value?id=${id}`)).status(), 404)
  console.log('Redis browser: search, create, edit, expiry preservation and delete passed')
} catch (error) {
  await page.screenshot({ path: path.join(reports, 'admin-browser-error.png'), fullPage: true }).catch(() => {})
  throw error
} finally {
  for (const space of spaces) await context.request.delete(`${base}/v1/spaces/${space}`).catch(() => {})
  await browser.close()
}
