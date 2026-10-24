import { useCallback, useLayoutEffect, useRef, useState } from 'react'
import { chat } from '@tanstack/ai'
import { openaiCompatibleText } from '@tanstack/ai-openai/compatible'
import { useChat } from '@tanstack/ai-react'
import { ArrowUp, Bot, Eraser, MessagesSquare, RotateCcw, Square } from 'lucide-react'
import { servicePath } from './api'
import { useModelChoices, type ModelChoices } from './aiApi'
import { Markdown, MarkdownStyle } from './AiMarkdown'
import { Pane, Picker } from './Workbench'
import { Button, Callout, CopyButton, Disclosure, IconButton } from './ui'
import { errText } from './format'

type ChatMessages = Parameters<typeof chat>[0]['messages']

/**
 * A quick line to an OpenAI-compatible service: pick a model, ask, read the
 * answer as it streams. A tester, not a chat app — the conversation lasts as
 * long as the page. TanStack AI speaks the protocol; the console route
 * (…/ai) swaps the session for the service's key on the server.
 */
export function ChatView({ space, service }: { space: string; service: string }) {
  const models = useModelChoices(space, service)
  const [picked, setPicked] = useState('')
  const model = picked || models.preferred
  const [input, setInput] = useState('')

  // The fetcher stays stable across renders; the ref carries the current
  // model, so switching applies to the next question without dropping what
  // was said.
  const current = useRef(model)
  useLayoutEffect(() => {
    current.current = model
  }, [model])
  const fetcher = useCallback(
    ({ messages }: { messages: ChatMessages }) =>
      chat({
        adapter: openaiCompatibleText(current.current, {
          baseURL: `${location.origin}${servicePath(space, service, '/ai')}`,
          // The SDK insists on a key. The gateway session authenticates; the
          // server replaces this placeholder with the service's own.
          apiKey: 'console',
          dangerouslyAllowBrowser: true,
        }),
        messages,
      }),
    [space, service],
  )
  const { messages, sendMessage, reload, stop, clear, isLoading, error } = useChat({ fetcher })

  const send = () => {
    const text = input.trim()
    if (!text || isLoading || !model) return
    setInput('')
    void sendMessage(text)
  }

  return (
    <Pane label={`Chat with ${service}`} className="flex-1">
      <MarkdownStyle />
      <Messages service={service} model={model} messages={messages} busy={isLoading} onAgain={() => void reload()} />

      {/* The composer spans the pane: the message on top, and along its
          bottom edge the model it goes to, a fresh start, and send. */}
      <div className="shrink-0 px-4 pb-4">
        {error && (
          <Callout tone="danger" className="mb-3">
            {errText(error, 'The model did not answer.')}
          </Callout>
        )}
        <div className="rounded-xl border border-line bg-surface transition-colors focus-within:border-line-strong">
          <textarea
            value={input}
            onChange={(e) => setInput(e.target.value)}
            onKeyDown={(e) => {
              if (e.key === 'Enter' && !e.shiftKey) {
                e.preventDefault()
                send()
              }
            }}
            placeholder={model ? `Ask ${model}…` : 'Pick a model first'}
            aria-label="Message"
            rows={2}
            className="block max-h-56 min-h-14 w-full resize-none bg-transparent px-3.5 pt-3 pb-1 text-sm leading-relaxed text-ink placeholder:text-muted focus:outline-none field-sizing-content"
          />
          <div className="flex items-center gap-1 px-2 pb-2">
            <ModelPicker value={model} choices={models} onChange={setPicked} />
            <IconButton
              icon={Eraser}
              size="sm"
              label="Start over"
              disabled={messages.length === 0 || isLoading}
              onClick={() => clear()}
            />
            <span className="ml-auto hidden pr-2 text-[11px] text-muted sm:inline">
              Enter sends · Shift-Enter for a new line
            </span>
            {isLoading ? (
              <Button kind="quiet" size="sm" icon={Square} onClick={() => stop()}>
                Stop
              </Button>
            ) : (
              <Button size="sm" icon={ArrowUp} onClick={send} disabled={!input.trim() || !model}>
                Send
              </Button>
            )}
          </div>
        </div>
      </div>
    </Pane>
  )
}

function ModelPicker({
  value,
  choices,
  onChange,
}: {
  value: string
  choices: ModelChoices
  onChange: (model: string) => void
}) {
  // Without a listing the model is typed — the service still answers.
  if (choices.ids.length === 0 && !choices.loading) {
    return (
      <input
        aria-label="Model"
        placeholder="model id"
        value={value}
        onChange={(e) => onChange(e.target.value)}
        spellCheck={false}
        className="h-7 w-48 rounded-md bg-transparent px-1.5 font-mono text-sm text-ink placeholder:font-sans placeholder:text-muted hover:bg-line/60 focus:bg-line/60 focus:outline-none"
      />
    )
  }
  const ids = value && !choices.ids.includes(value) ? [value, ...choices.ids] : choices.ids
  return (
    <Picker
      up
      label="Model"
      value={value}
      onChange={onChange}
      options={ids.map((id) => ({ value: id, label: id, detail: id === choices.bound ? 'default' : undefined }))}
    />
  )
}

type UIMessages = ReturnType<typeof useChat>['messages']

function Messages({
  service,
  model,
  messages,
  busy,
  onAgain,
}: {
  service: string
  model: string
  messages: UIMessages
  busy: boolean
  onAgain: () => void
}) {
  const scroller = useRef<HTMLDivElement>(null)
  // Follow the answer as it grows — unless the reader scrolled up to read.
  const pinned = useRef(true)
  useLayoutEffect(() => {
    const el = scroller.current
    if (el && pinned.current) el.scrollTop = el.scrollHeight
  }, [messages])

  if (messages.length === 0) {
    return (
      <div className="flex min-h-0 flex-1 flex-col items-center justify-center gap-2 p-8 text-center">
        <MessagesSquare className="size-6 text-muted/70" aria-hidden />
        <p className="text-sm font-medium text-ink">Try {service}</p>
        <p className="max-w-sm text-xs leading-relaxed text-muted">
          {model ? (
            <>
              Answers come from <span className="font-mono text-ink-soft">{model}</span>, over the same connection the
              space's apps use. The conversation lasts as long as this page.
            </>
          ) : (
            'Pick a model above to start.'
          )}
        </p>
      </div>
    )
  }

  const last = messages.at(-1)
  return (
    <div
      ref={scroller}
      onScroll={() => {
        const el = scroller.current
        if (el) pinned.current = el.scrollHeight - el.scrollTop - el.clientHeight < 48
      }}
      className="min-h-0 flex-1 overflow-y-auto"
    >
      <div className="flex flex-col gap-6 px-5 py-5" aria-live="polite" aria-busy={busy}>
        {messages.map((m) => {
          const text = m.parts.flatMap((p) => (p.type === 'text' ? [p.content] : [])).join('')
          const thinking = m.parts.flatMap((p) => (p.type === 'thinking' ? [p.content] : [])).join('')
          if (m.role === 'user') {
            return (
              <div key={m.id} className="flex justify-end">
                <div className="max-w-[min(85%,48rem)] rounded-lg bg-raised px-3 py-2 text-sm leading-relaxed break-words whitespace-pre-wrap text-ink">
                  {text}
                </div>
              </div>
            )
          }
          const answering = busy && m === last
          return (
            <div key={m.id} className="group max-w-3xl">
              <div className="mb-1.5 flex items-center gap-1.5 text-xs text-muted">
                <Bot className="size-3.5" aria-hidden />
                <span className="font-mono">{model || 'assistant'}</span>
              </div>
              {thinking && (
                <div className="mb-2">
                  <Disclosure summary="Thinking" defaultOpen={answering && !text}>
                    <p className="border-l-2 border-line pl-3 text-xs leading-relaxed whitespace-pre-wrap text-muted">
                      {thinking}
                    </p>
                  </Disclosure>
                </div>
              )}
              {text ? (
                <Markdown text={text} className="text-sm leading-relaxed text-ink-soft" />
              ) : (
                answering && !thinking && <p className="text-sm text-muted">…</p>
              )}
              {!answering && text && (
                <div className="mt-1 flex min-h-8 items-center opacity-0 transition-opacity group-focus-within:opacity-100 group-hover:opacity-100">
                  <CopyButton text={text} label="Copy the answer" />
                  {m === last && <IconButton icon={RotateCcw} label="Answer again" onClick={onAgain} />}
                </div>
              )}
            </div>
          )
        })}
        {busy && last?.role === 'user' && <p className="text-sm text-muted">…</p>}
      </div>
    </div>
  )
}
