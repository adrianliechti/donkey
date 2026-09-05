// Render an already deployed sample before the Go suite cleans up its space.
import assert from 'node:assert/strict'
import { mkdirSync } from 'node:fs'
import { createRequire } from 'node:module'
import path from 'node:path'
import { fileURLToPath } from 'node:url'

const root = path.resolve(path.dirname(fileURLToPath(import.meta.url)), '..')
const require = createRequire(import.meta.url)
const { chromium } = require(process.env.DONKEY_E2E_PLAYWRIGHT || path.join(root, '.build/browser/node_modules/playwright'))
const [sample, base] = process.argv.slice(2)
assert.ok(sample && base, 'provide the sample name and its deployed URL')
const domain = process.env.DONKEY_E2E_DOMAIN || 'apps.test'
const gateway = process.env.DONKEY_E2E_BROWSER_GATEWAY || '127.0.0.1:18443'
const reports = process.env.DONKEY_E2E_REPORTS || path.join(root, '.build/e2e')
mkdirSync(reports, { recursive: true })
const browser = await chromium.launch({ headless: true, args: [`--host-resolver-rules=MAP *.${domain}:443 ${gateway}, MAP ${domain}:443 ${gateway}`] })
const context = await browser.newContext({ ignoreHTTPSErrors: true, viewport: { width: 1280, height: 900 } })
const page = await context.newPage()
page.setDefaultTimeout(45000)
let streamlitFrames = 0
const messages = []
page.on('console', message => messages.push(message.text()))
page.on('websocket', socket => {
  if (socket.url().includes('/_stcore/stream')) socket.on('framereceived', () => streamlitFrames++)
})
const screenshot = suffix => path.join(reports, `${sample.replaceAll(/[^a-zA-Z0-9-]/g, '-')}-${suffix}.png`)
try {
  if (sample === 'local-habits') {
    // The fixture deliberately has no backend; verify its rendered error
    // without making a request to some unrelated service on the test laptop.
    await page.route('http://localhost:3001/**', route => route.abort('connectionrefused'))
  }
  const response = await page.goto(base, { waitUntil: 'domcontentloaded' })
  assert.equal(response.status(), 200, `page status: ${response.status()}`)
  if (sample.includes('streamlit')) {
    await page.getByRole('heading', { name: 'Quarterly report', exact: true }).waitFor()
    await page.getByText(sample === 'streamlit-report' ? 'Best quarter' : 'Revenue', { exact: true }).first().waitFor()
    assert.ok(streamlitFrames > 0, 'the report did not receive its application through the WebSocket')
    assert.equal(await page.locator('[data-testid="stException"]').count(), 0, 'Streamlit raised an application exception')
    if (sample === 'streamlit-report') {
      await page.getByText('The numbers', { exact: true }).click()
      await page.locator('[data-testid="stDataFrame"]').waitFor()
    }
  } else {
    const heading = { 'static-site': 'Field notes', 'My Cool App': 'My Cool App', 'nextjs-app': 'It works', 'local-habits': 'Todos', 'kitchen-sink-sample/web': 'kitchen sink web' }[sample]
    await page.getByRole('heading', { name: heading, exact: true }).waitFor()
    if (sample === 'My Cool App') {
      await page.waitForFunction(() => [...document.images].every(image => image.complete && image.naturalWidth > 0))
      assert.ok(messages.includes('My Cool App loaded'), 'the script with its original asset layout did not execute')
    }
    if (sample === 'local-habits') await page.getByText('Could not reach', { exact: false }).waitFor()
    if (sample === 'static-site') {
      await page.getByRole('link', { name: 'About this sample', exact: false }).click()
      assert.ok(page.url().includes('about'), 'the second page was not navigable')
    }
  }
  await page.screenshot({ path: screenshot('browser'), fullPage: true })
  console.log(`${sample}: rendered application verified${streamlitFrames ? ` (${streamlitFrames} Streamlit WebSocket frames)` : ''}`)
} catch (error) {
  await page.screenshot({ path: screenshot('browser-error'), fullPage: true }).catch(() => {})
  console.error((await page.locator('body').innerText()).slice(-2000))
  throw error
} finally {
  await browser.close()
}
