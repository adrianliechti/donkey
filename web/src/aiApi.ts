import { queryOptions, useQuery } from '@tanstack/react-query'
import { ApiError, request, servicePath } from './api'
import { box, dropOldest, isRecord, listOf, newId, num, str } from './httpStore'
import { useServiceBinding } from './queries'
import { errText } from './format'

// The chat and model list of an OpenAI-compatible service. Calls go to the
// service's console routes (…/ai/models, …/ai/chat/completions), where the
// server swaps the session for the service's own key; answers stream back
// as server-sent events, passed through as they arrive.

// ─── Models ────────────────────────────────────────────────────────────

/** A model as the service lists it; providers add fields of their own. */
export type Model = { id: string; owned_by?: string; created?: number } & Record<string, unknown>

export const modelsQuery = (space: string, service: string) =>
  queryOptions({
    queryKey: ['ai', space, service, 'models'],
    queryFn: async ({ signal }) => {
      const answer = await request<{ data?: unknown; models?: unknown }>(servicePath(space, service, '/ai/models'), { signal })
      const list = Array.isArray(answer.data) ? answer.data : Array.isArray(answer.models) ? answer.models : []
      return list
        .filter((m): m is Model => isRecord(m) && typeof m.id === 'string' && m.id !== '')
        .sort((a, b) => a.id.localeCompare(b.id))
    },
    staleTime: 5 * 60_000,
    // A service without a model listing is common and not worth asking twice.
    retry: false,
  })

export type ModelChoices = { ids: string[]; bound: string; preferred: string; loading: boolean }

/** The models a conversation can use: the binding's own first. */
export function useModelChoices(space: string, service: string): ModelChoices {
  const { data: binding } = useServiceBinding(space, service)
  const listed = useQuery(modelsQuery(space, service))
  // The binding's model leads: it is the one the platform vouches for,
  // whether or not the upstream's own listing mentions it.
  const bound = binding?.binding.OPENAI_MODEL ?? ''
  const ids = [...new Set([...(bound ? [bound] : []), ...(listed.data ?? []).map((m) => m.id)])]
  return { ids, bound, preferred: bound || ids[0] || '', loading: listed.isPending }
}

// ─── Conversations ─────────────────────────────────────────────────────

export type Usage = { prompt?: number; completion?: number; reasoning?: number }

export type Message = {
  id: string
  role: 'user' | 'assistant'
  content: string
  /** What a reasoning model thought before answering, when it says. */
  reasoning?: string
  model?: string
  usage?: Usage
  /** Why the answer ended, when not simply finished ("length", …). */
  finish?: string
  ms?: number
  error?: string
  stopped?: boolean
}

export type Conversation = {
  id: string
  title: string
  /** Set by hand: the first message no longer names it. */
  named?: boolean
  model: string
  system: string
  /** Empty for the model's default — sent only when set. */
  temperature: string
  maxTokens: string
  messages: Message[]
  updatedAt: number
}

export const freshConversation = (model = ''): Conversation => ({
  id: newId(),
  title: 'New chat',
  model,
  system: '',
  temperature: '',
  maxTokens: '',
  messages: [],
  updatedAt: Date.now(),
})

function parseMessage(m: Record<string, unknown>): Message | undefined {
  if (m.role !== 'user' && m.role !== 'assistant') return undefined
  const usage = isRecord(m.usage)
    ? { prompt: num(m.usage.prompt), completion: num(m.usage.completion), reasoning: num(m.usage.reasoning) }
    : undefined
  return {
    id: str(m.id) || newId(),
    role: m.role,
    content: str(m.content),
    reasoning: typeof m.reasoning === 'string' ? m.reasoning : undefined,
    model: typeof m.model === 'string' ? m.model : undefined,
    usage,
    finish: typeof m.finish === 'string' ? m.finish : undefined,
    ms: num(m.ms),
    error: typeof m.error === 'string' ? m.error : undefined,
    stopped: m.stopped === true || undefined,
  }
}

const storeKey = (space: string, service: string, what: string) => `donkey:ai:${what}:${space}/${service}`

// Conversations, most recently used first. Fifty of them, within 1.5 MB —
// past that the oldest go.
export const maxConversations = 50

export const chatsBox = (space: string, service: string) =>
  box<Conversation[]>(
    storeKey(space, service, 'chats'),
    (raw) =>
      listOf(raw, (c) => ({
        id: str(c.id) || newId(),
        title: str(c.title, 'Chat'),
        named: c.named === true || undefined,
        model: str(c.model),
        system: str(c.system),
        temperature: str(c.temperature),
        maxTokens: str(c.maxTokens),
        messages: listOf(c.messages, parseMessage),
        updatedAt: num(c.updatedAt) ?? 0,
      })),
    { maxBytes: 1536 * 1024, shrink: dropOldest },
  )

// The conversation not started yet: it joins the list with its first message.
export const draftBox = (space: string, service: string) =>
  box<Conversation>(storeKey(space, service, 'draft'), () => freshConversation(), { memory: true })

/** What is being typed, by conversation — it waits while another is open. */
export const composerBox = (space: string, service: string) =>
  box<Record<string, string>>(storeKey(space, service, 'composer'), () => ({}), { memory: true })

/** Whether the settings column is open, for this viewer. */
export const settingsOpenBox = box<boolean>('donkey:ai:settings-open', (raw) => raw === true)

/** An answer being written, by conversation. */
export type Live = { content: string; reasoning: string; model: string; startedAt: number }

export const liveBox = (space: string, service: string) =>
  box<Record<string, Live>>(storeKey(space, service, 'live'), () => ({}), { memory: true })

const running = new Map<string, AbortController>()

/** A title from the first thing asked. */
export function titleFrom(text: string): string {
  const line = text.trim().split('\n')[0].replace(/\s+/g, ' ')
  return line.length > 60 ? `${line.slice(0, 57)}…` : line || 'New chat'
}

/** Changes one conversation, wherever it is — the list or the draft. */
export function updateConversation(space: string, service: string, id: string, patch: (c: Conversation) => Conversation) {
  const draft = draftBox(space, service)
  if (draft.get().id === id) {
    draft.set(patch)
    return
  }
  chatsBox(space, service).set((list) => list.map((c) => (c.id === id ? patch(c) : c)))
}

export function deleteConversation(space: string, service: string, id: string) {
  running.get(id)?.abort()
  chatsBox(space, service).set((list) => list.filter((c) => c.id !== id))
  composerBox(space, service).set((all) => {
    const next = { ...all }
    delete next[id]
    return next
  })
}

/**
 * Asks, and streams the answer in. A conversation still in the draft joins
 * the list first; one used again moves to its top. The answer is written to
 * the conversation when it ends however it ends — finished, stopped or
 * failed — so leaving the tab mid-answer loses nothing.
 */
export function ask(space: string, service: string, id: string, text: string) {
  const user: Message = { id: newId(), role: 'user', content: text }
  const draft = draftBox(space, service)
  const chats = chatsBox(space, service)
  if (draft.get().id === id) {
    const started = draft.get()
    chats.set((list) => [{ ...started, title: titleFrom(text), messages: [user], updatedAt: Date.now() }, ...list].slice(0, maxConversations))
    draft.set(freshConversation(started.model))
  } else {
    chats.set((list) => {
      const at = list.find((c) => c.id === id)
      if (!at) return list
      const title = at.named || at.messages.length > 0 ? at.title : titleFrom(text)
      return [{ ...at, title, messages: [...at.messages, user], updatedAt: Date.now() }, ...list.filter((c) => c !== at)]
    })
  }
  void answer(space, service, id)
}

/** Asks for the last answer again, in place of the one there. */
export function regenerate(space: string, service: string, id: string) {
  chatsBox(space, service).set((list) =>
    list.map((c) => {
      if (c.id !== id || c.messages.at(-1)?.role !== 'assistant') return c
      return { ...c, messages: c.messages.slice(0, -1) }
    }),
  )
  void answer(space, service, id)
}

export function stop(id: string) {
  running.get(id)?.abort()
}

async function answer(space: string, service: string, id: string) {
  const chat = chatsBox(space, service)
    .get()
    .find((c) => c.id === id)
  if (!chat || running.has(id) || chat.messages.at(-1)?.role !== 'user') return
  const live = liveBox(space, service)
  const controller = new AbortController()
  running.set(id, controller)
  const startedAt = Date.now()
  const out: Live = { content: '', reasoning: '', model: chat.model, startedAt }
  live.set((all) => ({ ...all, [id]: { ...out } }))

  // Tokens arrive faster than they are worth rendering: the live answer is
  // published at most every 50 ms.
  let timer = 0
  const publish = () => {
    timer = 0
    live.set((all) => (all[id] ? { ...all, [id]: { ...out } } : all))
  }

  const message: Message = { id: newId(), role: 'assistant', content: '', model: chat.model }
  try {
    const temperature = parseFloat(chat.temperature)
    const maxTokens = parseInt(chat.maxTokens, 10)
    const result = await streamChat(
      space,
      service,
      {
        model: chat.model,
        messages: [
          ...(chat.system.trim() ? [{ role: 'system' as const, content: chat.system }] : []),
          // A failed answer with nothing in it is not part of the conversation.
          ...chat.messages.filter((m) => m.content || !m.error).map((m) => ({ role: m.role, content: m.content })),
        ],
        temperature: Number.isFinite(temperature) ? temperature : undefined,
        maxTokens: Number.isFinite(maxTokens) && maxTokens > 0 ? maxTokens : undefined,
      },
      controller.signal,
      (delta) => {
        out.content += delta.content ?? ''
        out.reasoning += delta.reasoning ?? ''
        if (delta.model) out.model = delta.model
        if (!timer) timer = window.setTimeout(publish, 50)
      },
    )
    Object.assign(message, {
      content: result.content || out.content,
      reasoning: (result.reasoning || out.reasoning) || undefined,
      model: result.model || out.model,
      usage: result.usage,
      finish: result.finish && result.finish !== 'stop' ? result.finish : undefined,
    })
  } catch (err) {
    Object.assign(message, { content: out.content, reasoning: out.reasoning || undefined, model: out.model })
    if (controller.signal.aborted) message.stopped = true
    else message.error = errText(err, 'The model did not answer.')
  } finally {
    window.clearTimeout(timer)
    running.delete(id)
    message.ms = Date.now() - startedAt
    chatsBox(space, service).set((list) =>
      list.map((c) => (c.id === id ? { ...c, messages: [...c.messages, message], updatedAt: Date.now() } : c)),
    )
    live.set((all) => {
      const next = { ...all }
      delete next[id]
      return next
    })
  }
}

// ─── The chat completions stream ───────────────────────────────────────

type ChatInput = {
  model: string
  messages: { role: 'system' | 'user' | 'assistant'; content: string }[]
  temperature?: number
  maxTokens?: number
}

type Delta = { content?: string; reasoning?: string; model?: string }

type ChatResult = { content: string; reasoning: string; model?: string; usage?: Usage; finish?: string }

// Compatible servers differ in what they accept. Usage in a stream needs
// stream_options, which some refuse; newer OpenAI models want
// max_completion_tokens where the rest want max_tokens. What a service
// refused once is remembered for the session.
type Quirks = { noStreamOptions?: boolean; maxTokensKey?: 'max_tokens' | 'max_completion_tokens' }
const quirks = new Map<string, Quirks>()

async function streamChat(
  space: string,
  service: string,
  input: ChatInput,
  signal: AbortSignal,
  onDelta: (delta: Delta) => void,
): Promise<ChatResult> {
  const known = quirks.get(`${space}/${service}`) ?? {}
  quirks.set(`${space}/${service}`, known)
  for (let attempt = 0; ; attempt++) {
    const body: Record<string, unknown> = { model: input.model, messages: input.messages, stream: true }
    if (!known.noStreamOptions) body.stream_options = { include_usage: true }
    if (input.temperature !== undefined) body.temperature = input.temperature
    if (input.maxTokens !== undefined) body[known.maxTokensKey ?? 'max_tokens'] = input.maxTokens
    const res = await fetch(servicePath(space, service, '/ai/chat/completions'), {
      method: 'POST',
      headers: { 'Content-Type': 'application/json', Accept: 'text/event-stream, application/json' },
      body: JSON.stringify(body),
      signal,
    })
    if (res.status === 401) {
      // The console's own session ran out (a provider's refusal arrives as
      // 502). The shared client knows how to sign in again; a ping through
      // it lets it.
      void request('/v1/ping').catch(() => {})
      throw new ApiError(401, 'The session expired — signing in again.')
    }
    if (!res.ok) {
      const message = await errorOf(res)
      if (attempt < 2 && (res.status === 400 || res.status === 422)) {
        if (!known.noStreamOptions && /stream_options/i.test(message)) {
          known.noStreamOptions = true
          continue
        }
        if (input.maxTokens !== undefined && /max_completion_tokens/i.test(message) && known.maxTokensKey !== 'max_completion_tokens') {
          known.maxTokensKey = 'max_completion_tokens'
          continue
        }
      }
      throw new ApiError(res.status, message)
    }
    return (res.headers.get('Content-Type') ?? '').includes('text/event-stream')
      ? readStream(res, onDelta)
      : readCompletion(await res.json())
  }
}

async function errorOf(res: Response): Promise<string> {
  const fallback = `The service answered HTTP ${res.status}`
  try {
    const body = await res.json()
    return failure(body) || fallback
  } catch {
    return fallback
  }
}

// OpenAI puts an error's message under error.message; donkey and some
// compatible servers put a sentence under error.
function failure(body: unknown): string {
  if (!isRecord(body)) return ''
  const error = body.error
  if (typeof error === 'string') return error
  if (isRecord(error) && typeof error.message === 'string') return error.message
  return ''
}

function usageOf(raw: unknown): Usage | undefined {
  if (!isRecord(raw)) return undefined
  const details = isRecord(raw.completion_tokens_details) ? raw.completion_tokens_details : {}
  return { prompt: num(raw.prompt_tokens), completion: num(raw.completion_tokens), reasoning: num(details.reasoning_tokens) || undefined }
}

// Reasoning arrives as reasoning_content (DeepSeek, vLLM) or reasoning
// (OpenRouter, Ollama), when a server sends it at all.
const reasoningOf = (m: Record<string, unknown>) => str(m.reasoning_content) || str(m.reasoning)

/** Reads a whole (unstreamed) chat completion; exported for its tests. */
export function readCompletion(body: unknown): ChatResult {
  const fail = failure(body)
  if (fail) throw new Error(fail)
  const record = isRecord(body) ? body : {}
  const choice = Array.isArray(record.choices) && isRecord(record.choices[0]) ? record.choices[0] : {}
  const message = isRecord(choice.message) ? choice.message : {}
  return {
    content: str(message.content),
    reasoning: reasoningOf(message),
    model: str(record.model) || undefined,
    usage: usageOf(record.usage),
    finish: str(choice.finish_reason) || undefined,
  }
}

/** Reads a chat completions event stream; exported for its tests. */
export async function readStream(res: Response, onDelta: (delta: Delta) => void): Promise<ChatResult> {
  const result: ChatResult = { content: '', reasoning: '' }
  if (!res.body) return result
  const reader = res.body.getReader()
  const decoder = new TextDecoder()
  let buffer = ''
  const handle = (event: string): boolean => {
    const data = event
      .split(/\r?\n/)
      .filter((line) => line.startsWith('data:'))
      .map((line) => line.slice(5).trimStart())
      .join('\n')
    if (!data) return false
    if (data === '[DONE]') return true
    let chunk: unknown
    try {
      chunk = JSON.parse(data)
    } catch {
      return false
    }
    const fail = failure(chunk)
    if (fail) throw new Error(fail)
    if (!isRecord(chunk)) return false
    if (typeof chunk.model === 'string' && chunk.model) result.model = chunk.model
    const usage = usageOf(chunk.usage)
    if (usage) result.usage = usage
    const choice = Array.isArray(chunk.choices) && isRecord(chunk.choices[0]) ? chunk.choices[0] : undefined
    if (choice) {
      const delta = isRecord(choice.delta) ? choice.delta : {}
      const content = str(delta.content)
      const reasoning = reasoningOf(delta)
      result.content += content
      result.reasoning += reasoning
      if (typeof choice.finish_reason === 'string' && choice.finish_reason) result.finish = choice.finish_reason
      if (content || reasoning || result.model) onDelta({ content, reasoning, model: result.model })
    }
    return false
  }
  for (;;) {
    const { done, value } = await reader.read()
    buffer += decoder.decode(value, { stream: !done })
    let at: RegExpExecArray | null
    while ((at = /\r?\n\r?\n/.exec(buffer))) {
      const event = buffer.slice(0, at.index)
      buffer = buffer.slice(at.index + at[0].length)
      if (handle(event)) {
        void reader.cancel().catch(() => {})
        return result
      }
    }
    if (done) {
      if (buffer.trim()) handle(buffer)
      return result
    }
  }
}
