import assert from 'node:assert/strict'
import { createHash } from 'node:crypto'
import { test } from 'node:test'
import { api } from '../src/api.ts'

test('browser file uploads preserve binary content through multipart encoding', async (t) => {
  for (const size of [0, 170 * 1024, 800 * 1024, 1100 * 1024, 1400 * 1024, 2 * 1024 * 1024]) {
    await t.test(`${size} bytes of binary PDF data`, async (t) => {
      const original = Buffer.concat([
        Buffer.from('%PDF-1.6\n%\x00\xff\x80\r\n', 'latin1'),
        Buffer.alloc(size, 0xff),
        Buffer.from('\n%%EOF\n'),
      ])
      t.mock.method(globalThis, 'fetch', async (path, init) => {
        const request = new Request(new URL(path, 'https://donkey.example.com'), init)
        assert.equal(request.method, 'POST')
        assert.match(request.headers.get('Content-Type'), /^multipart\/form-data; boundary=/)
        const form = await request.formData()
        assert.deepEqual([...form.keys()], ['file'])
        const file = form.get('file')
        assert.equal(file.name, '736333792.pdf')
        assert.equal(file.type, 'application/pdf')
        const uploaded = Buffer.from(await file.arrayBuffer())
        assert.deepEqual(uploaded, original)
        assert.equal(
          createHash('sha256').update(uploaded).digest('hex'),
          createHash('sha256').update(original).digest('hex'),
        )
        return Response.json({ ok: true, path: 'app-data/736333792.pdf', size: file.size })
      })

      const result = await api.uploadFile(
        'team',
        'api',
        'app-data/736333792.pdf',
        new File([original], '736333792.pdf', { type: 'application/pdf' }),
      )
      assert.equal(result.size, original.length)
    })
  }
})

test('file paths survive query encoding without trimming or decoding twice', async (t) => {
  for (const path of ['', 'app-data/', 'app-data/renamed.pdf', 'app-data/é #?&+%25.pdf ']) {
    await t.test(JSON.stringify(path), async (t) => {
      t.mock.method(globalThis, 'fetch', async (input, init) => {
        const request = new Request(new URL(input, 'https://donkey.example.com'), init)
        const url = new URL(request.url)
        assert.equal(url.pathname, '/v1/spaces/team/apps/api/files')
        assert.equal(url.searchParams.get('path'), path)
        assert.deepEqual([...url.searchParams.keys()], ['path'])
        const form = await request.formData()
        assert.equal(form.get('file').name, 'original.pdf')
        return Response.json({ ok: true })
      })
      await api.uploadFile('team', 'api', path, new File(['%PDF-1.6'], 'original.pdf', { type: 'application/pdf' }))
    })
  }
})

test('project archive upload paths preserve their explicit content type and bytes', async (t) => {
  const original = Buffer.from('PK\x03\x04\x00\xff', 'latin1')
  const paths = []
  t.mock.method(globalThis, 'fetch', async (path, init) => {
    const request = new Request(new URL(path, 'https://donkey.example.com'), init)
    assert.equal(request.headers.get('Content-Type'), 'application/zip')
    assert.deepEqual(Buffer.from(await request.arrayBuffer()), original)
    const url = new URL(request.url)
    assert.equal(url.searchParams.get('space'), 'team')
    assert.equal(url.searchParams.get('name'), 'api')
    paths.push(url.pathname)
    if (url.pathname.endsWith('/deploys')) {
      return new Response(JSON.stringify({ result: { ok: true } }) + '\n', {
        headers: { 'Content-Type': 'application/x-ndjson' },
      })
    }
    return Response.json({ ok: true })
  })
  const blob = new Blob([original])
  await api.plan(blob, { space: 'team', name: 'api' })
  await api.projectPlan(blob, { space: 'team', name: 'api' })
  const ignore = () => {}
  await api.deployUpload(blob, { space: 'team', name: 'api' }, ignore, ignore, ignore)
  await api.deployUpload(blob, { space: 'team', name: 'api', project: true }, ignore, ignore, ignore)
  assert.deepEqual(paths, ['/v1/plans', '/v1/projects/plans', '/v1/deploys', '/v1/projects/deploys'])
})

test('JSON requests still send JSON and reads have no content type', async (t) => {
  t.mock.method(globalThis, 'fetch', async (path, init) => {
    const request = new Request(new URL(path, 'https://donkey.example.com'), init)
    if (request.method === 'PUT') {
      assert.equal(request.headers.get('Content-Type'), 'application/json')
      assert.deepEqual(await request.json(), { env: { EXAMPLE: 'value' }, restart: false })
    } else {
      assert.equal(request.headers.get('Content-Type'), null)
    }
    return Response.json({ ok: true })
  })

  await api.setEnv('team', 'api', { EXAMPLE: 'value' }, false)
  await api.ping()
})
