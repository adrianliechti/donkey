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

const ai = await import('../src/aiApi.ts')

// A response whose body arrives in the given pieces, as a network would cut it.
function stream(pieces) {
  const encoder = new TextEncoder()
  return new Response(
    new ReadableStream({
      start(controller) {
        for (const piece of pieces) controller.enqueue(encoder.encode(piece))
        controller.close()
      },
    }),
    { headers: { 'Content-Type': 'text/event-stream' } },
  )
}

const event = (chunk) => `data: ${JSON.stringify(chunk)}\n\n`

test('a streamed answer is read however the network cuts it', async () => {
  const text = [
    event({ model: 'm-1', choices: [{ delta: { reasoning_content: 'Hmm. ' } }] }),
    event({ choices: [{ delta: { content: 'Hel' } }] }),
    event({ choices: [{ delta: { content: 'lo, wörld' } }] }).replaceAll('\n', '\r\n'),
    event({ choices: [{ delta: {}, finish_reason: 'length' }] }),
    event({ choices: [], usage: { prompt_tokens: 7, completion_tokens: 3, completion_tokens_details: { reasoning_tokens: 2 } } }),
    'data: [DONE]\n\n',
    event({ choices: [{ delta: { content: 'after done' } }] }),
  ].join('')
  for (const size of [1, 3, 17, text.length]) {
    // Cut on bytes, so a multi-byte character can straddle two pieces.
    const bytes = new TextEncoder().encode(text)
    const raw = []
    for (let i = 0; i < bytes.length; i += size) raw.push(bytes.slice(i, i + size))
    const res = new Response(new ReadableStream({ start(c) { for (const b of raw) c.enqueue(b); c.close() } }), {
      headers: { 'Content-Type': 'text/event-stream' },
    })
    const deltas = []
    const result = await ai.readStream(res, (d) => deltas.push(d))
    assert.equal(result.content, 'Hello, wörld', `cut every ${size} bytes`)
    assert.equal(result.reasoning, 'Hmm. ')
    assert.equal(result.model, 'm-1')
    assert.equal(result.finish, 'length')
    assert.deepEqual(result.usage, { prompt: 7, completion: 3, reasoning: 2 })
    assert.equal(deltas.map((d) => d.content).join(''), 'Hello, wörld')
  }
})

test('reasoning under either name, and comments and blank events, are understood', async () => {
  const result = await ai.readStream(
    stream([': keep-alive\n\n', 'event: message\n', event({ choices: [{ delta: { reasoning: 'think', content: null } }] }), '\n\n']),
    () => {},
  )
  assert.equal(result.reasoning, 'think')
  assert.equal(result.content, '')
})

test('an error inside the stream ends it with the provider’s message', async () => {
  await assert.rejects(
    ai.readStream(stream([event({ choices: [{ delta: { content: 'partial' } }] }), event({ error: { message: 'overloaded' } })]), () => {}),
    /overloaded/,
  )
})

test('an unstreamed completion reads the same', () => {
  assert.deepEqual(
    ai.readCompletion({
      model: 'm',
      choices: [{ message: { content: 'hi', reasoning_content: 'r' }, finish_reason: 'stop' }],
      usage: { prompt_tokens: 1, completion_tokens: 2 },
    }),
    { content: 'hi', reasoning: 'r', model: 'm', usage: { prompt: 1, completion: 2, reasoning: undefined }, finish: 'stop' },
  )
  assert.throws(() => ai.readCompletion({ error: 'no such model' }), /no such model/)
})

test('a conversation is titled by its first question', () => {
  assert.equal(ai.titleFrom('  How do I\nlist items?'), 'How do I')
  assert.equal(ai.titleFrom('x'.repeat(80)).length, 58)
  assert.equal(ai.titleFrom('   '), 'New chat')
})
