// Live browser test of upload → review → deploy → use both apps and their services.
import assert from 'node:assert/strict'
import { mkdirSync, readFileSync, readdirSync, writeFileSync } from 'node:fs'
import { createRequire } from 'node:module'
import path from 'node:path'
import { fileURLToPath } from 'node:url'

const root = path.resolve(path.dirname(fileURLToPath(import.meta.url)), '..')
const require = createRequire(import.meta.url)
const { chromium } = require(process.env.DONKEY_E2E_PLAYWRIGHT || path.join(root, '.build/browser/node_modules/playwright'))
const { zipSync } = createRequire(path.join(root, 'web/package.json'))('fflate')
const domain = process.env.DONKEY_E2E_DOMAIN || 'apps.localhost'
const consoleURL = `https://${domain}`
const space = 'e2e-services'
const reports = path.join(root, '.build/e2e')
mkdirSync(reports, { recursive: true })
const files = {}
function collect(dir, prefix = '') {
  for (const entry of readdirSync(dir, { withFileTypes: true })) {
    if (['node_modules', 'dist', '.git', '__pycache__', '.venv'].includes(entry.name) || entry.name.endsWith('.tsbuildinfo')) continue
    const name = prefix + entry.name
    if (entry.isDirectory()) collect(path.join(dir, entry.name), `${name}/`)
    else if (entry.isFile()) files[name] = readFileSync(path.join(dir, entry.name))
  }
}
for (const sample of ['addons-sample', 'static-site']) {
  collect(path.join(root, 'testdata', sample), sample + '/')
}
const browser = await chromium.launch({ headless: true, args: [`--host-resolver-rules=MAP *.${domain} 127.0.0.1, MAP ${domain} 127.0.0.1`] })
const context = await browser.newContext({ ignoreHTTPSErrors: true, viewport: { width: 1440, height: 1100 } })
const page = await context.newPage()
page.setDefaultTimeout(30000)
try {
  await page.goto(consoleURL, { waitUntil: 'networkidle' })
  if (await page.getByRole('link', { name: 'Sign in', exact: true }).count()) {
    await page.getByRole('link', { name: 'Sign in', exact: true }).click()
    await page.waitForURL(url => url.hostname === domain)
  }
  const removed = await context.request.delete(`${consoleURL}/v1/spaces/${space}`)
  assert.ok(removed.ok() || removed.status() === 404, `test space cleanup: ${removed.status()}`)
  const created = await context.request.post(`${consoleURL}/v1/spaces`, { data: { name: space, allowPublic: true } })
  assert.ok(created.ok(), `test space creation: ${created.status()}`)
  await page.goto(`${consoleURL}/#/deploy/${space}`)
  const reading = page.waitForResponse(r => r.url().includes('/v1/projects/plans?') && r.request().method() === 'POST', { timeout: 360000 })
  await page.locator('input[type=file]').setInputFiles({ name: 'samples.zip', mimeType: 'application/zip', buffer: Buffer.from(zipSync(files)) })
  const response = await reading
  assert.equal(response.status(), 200, await response.text())
  const plan = await response.json()
  writeFileSync(path.join(reports, 'services-browser-plan.json'), JSON.stringify(plan, null, 2), { mode: 0o600 })
  assert.deepEqual(plan.apps.map(a => a.root).sort(), ['addons-sample', 'static-site'])
  const guestbook = plan.apps.find(a => a.root === 'addons-sample')
  assert.deepEqual(guestbook.services.filter(s => s.selected).map(s => s.type), ['postgres'])
  const deployButton = page.getByRole('button', { name: 'Deploy all 2 apps', exact: true })
  assert.equal(await deployButton.isEnabled(), true, 'a ready plan must be deployable')
  const access = page.getByRole('radiogroup', { name: 'Who can reach it' })
  for (const [index, app] of plan.apps.entries()) {
    const exposure = app.root === 'addons-sample' ? /^Public/ : /^Internal/
    await access.nth(index).getByRole('radio', { name: exposure }).click()
  }
  const checked = page.waitForResponse(r => r.url().includes('/v1/projects/plans/') && r.request().method() === 'PUT')
  await page.getByRole('button', { name: 'Check changes', exact: true }).click()
  const review = await checked
  assert.equal(review.status(), 200, await review.text())
  const settled = await review.json()
  writeFileSync(path.join(reports, 'services-browser-plan.json'), JSON.stringify(settled, null, 2), { mode: 0o600 })
  assert.equal(settled.ready, true, 'review has blocking findings')
  for (const app of settled.apps) {
    assert.equal(app.app.exposure, app.root === 'addons-sample' ? 'public' : 'internal')
  }
  await page.screenshot({ path: path.join(reports, 'services-browser-review.png'), fullPage: true })
  console.log('Browser review: two apps with their selected services; independent exposure choices passed')
  const deploying = page.waitForResponse(r => r.url().includes('/v1/projects/deploys?'), { timeout: 2400000 })
  await deployButton.click()
  const deployment = await deploying
  assert.equal(deployment.status(), 200)
  const stream = await deployment.text()
  writeFileSync(path.join(reports, 'services-browser-deploy.ndjson'), stream, { mode: 0o600 })
  const result = stream.split('\n').filter(Boolean).map(line => JSON.parse(line)).find(message => message.result)?.result
  assert.ok(result?.ok, result?.error || 'deployment ended without success')
  assert.equal(result.apps.length, 2)
  assert.ok(result.apps.every(app => app.ok), 'both apps must deploy')
  console.log('Browser deployment: both apps succeeded')
  await page.goto(`https://addons-sample.${space}.${domain}`, { waitUntil: 'networkidle' })
  const message = `Browser connection check ${Date.now()}`
  await page.getByPlaceholder('leave a note').fill(message)
  await page.getByRole('button', { name: 'Sign', exact: true }).click()
  await page.getByText(message, { exact: false }).waitFor()
  await page.reload({ waitUntil: 'networkidle' })
  await page.getByText(message, { exact: false }).waitFor()
  await page.screenshot({ path: path.join(reports, 'services-browser-app.png'), fullPage: true })
  console.log('Browser app: the guestbook retained the PostgreSQL write')
} catch (error) {
  await page.screenshot({ path: path.join(reports, 'services-browser-error.png'), fullPage: true }).catch(() => {})
  console.error((await page.locator('body').innerText()).slice(-1800))
  throw error
} finally {
  if (process.env.DONKEY_E2E_KEEP !== 'true') await context.request.delete(`${consoleURL}/v1/spaces/${space}`).catch(() => {})
  await browser.close()
}
