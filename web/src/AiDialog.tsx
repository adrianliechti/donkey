import { useCallback, useEffect, useMemo, useRef, useState } from 'react'
import { chat } from '@tanstack/ai'
import { openaiCompatibleText } from '@tanstack/ai-openai/compatible'
import { useChat } from '@tanstack/ai-react'
import { Markdown } from '@tanstack/markdown/react'
import { highlightToHtml } from '@tanstack/highlight'
import githubDarkTheme from '@tanstack/highlight/themes/github-dark'
import githubLightTheme from '@tanstack/highlight/themes/github-light'
import { Send, Square, Trash2 } from 'lucide-react'
import { api } from './api'
import { Modal, ModalBody, ModalHeader, ModalNotice } from './dialog'
import { Button, IconButton, Input, Select, Spinner, TextArea, errText, useColorScheme } from './ui'

// The highlighter emits `th-<token>` classes; the theme is a token→color
// map. One generated stylesheet marries them — scoped under the dialog's own
// class so the console's styles stay untouched — and follows the OS light/dark
// preference, since these colors are hex, not CSS tokens.
type highlightTheme = { background: string; foreground: string; tokens: Record<string, string> }
function themeCssFor(theme: highlightTheme): string {
  return [
    `.ai-md .th-code{display:block;overflow-x:auto;border-radius:6px;padding:10px 12px;background:${theme.background};color:${theme.foreground};font-size:11px;line-height:1.6}`,
    ...Object.entries(theme.tokens).map(([name, color]) => `.ai-md .th-${name}{color:${color}}`),
    `.ai-md pre{margin:6px 0}`,
    `.ai-md p{margin:4px 0}`,
    `.ai-md ul,.ai-md ol{margin:4px 0;padding-left:1.2rem;list-style:revert}`,
    `.ai-md h1,.ai-md h2,.ai-md h3{font-weight:600;margin:8px 0 4px}`,
    `.ai-md a{text-decoration:underline}`,
    `.ai-md code{font-family:inherit}`,
    `.ai-md table{border-collapse:collapse;margin:6px 0}`,
    `.ai-md td,.ai-md th{border:1px solid currentColor;padding:2px 8px;opacity:0.9}`,
  ].join('\n')
}

const codeHighlighter = (code: string, lang?: string) => highlightToHtml(code, { lang })

/**
 * Chat with any OpenAI-compatible service through its authenticated console
 * route. The server supplies the binding's endpoint and upstream credential.
 */
export function AiDialog({
  space,
  service,
  onClose,
  onDelete,
}: {
  space: string
  service: string
  onClose: () => void
  onDelete: () => void
}) {
  const [base, setBase] = useState('')
  const [loading, setLoading] = useState(true)
  const [models, setModels] = useState<string[]>([])
  const [model, setModel] = useState('')
  const [loadError, setLoadError] = useState('')
  const [input, setInput] = useState('')
  const scroller = useRef<HTMLDivElement>(null)
  const scheme = useColorScheme()
  const themeCss = useMemo(
    () => themeCssFor(scheme === 'dark' ? githubDarkTheme : githubLightTheme),
    [scheme],
  )

  // The fetcher stays stable across renders; refs carry the current choice
  // so switching models applies to the next message without resetting the
  // conversation.
  const settings = useRef({ base: '', model: '' })
  settings.current = { base, model }

  useEffect(() => {
    const controller = new AbortController()
    setLoading(true)
    setLoadError('')
    api
      .serviceBinding(space, service)
      .then(async (res) => {
        if (controller.signal.aborted) return
        const sameOrigin = `${location.origin}${api.serviceAIBase(space, service)}`
        setBase(sameOrigin)
        const preferred = res.binding.OPENAI_MODEL
        try {
          const answer = await fetch(`${sameOrigin}/models`, {
            signal: controller.signal,
          })
          if (!answer.ok) throw new Error('The service could not list its models.')
          const listed = ((await answer.json()).data ?? []).map((m: { id: string }) => m.id)
          // The binding's model leads: it is the one the platform vouches
          // for, whether or not the upstream's own listing mentions it.
          const list = [...new Set([...(preferred ? [preferred] : []), ...listed])]
          setModels(list)
          setModel(preferred ?? list[0] ?? '')
        } catch {
          if (controller.signal.aborted) return
          // No model list is not fatal — the binding's own model still works.
          setModels(preferred ? [preferred] : [])
          setModel(preferred ?? '')
        }
      })
      .catch((err) => { if (!controller.signal.aborted) setLoadError(errText(err, 'The service did not answer.')) })
      .finally(() => { if (!controller.signal.aborted) setLoading(false) })
    return () => controller.abort()
  }, [space, service])

  const fetcher = useCallback(
    ({ messages }: { messages: Parameters<typeof chat>[0]['messages'] }) => {
      const { base, model } = settings.current
      return chat({
        adapter: openaiCompatibleText(model, {
          baseURL: base,
          // The SDK requires a key. Authentication uses the gateway session;
          // the server replaces this placeholder with the service's key.
          apiKey: 'console',
          dangerouslyAllowBrowser: true,
        }),
        messages,
      })
    },
    [],
  )

  const { messages, sendMessage, isLoading, error, stop } = useChat({ fetcher })

  useEffect(() => {
    scroller.current?.scrollTo({ top: scroller.current.scrollHeight })
  }, [messages])

  function send() {
    const text = input.trim()
    if (!text || isLoading || !model) return
    setInput('')
    sendMessage(text)
  }

  return (
    <Modal onClose={onClose} size="lg" height="fill">
      <style>{themeCss}</style>
      <ModalHeader eyebrow={`Chat · ${space}`} title={<span className="font-mono">{service}</span>}
        actions={<IconButton icon={Trash2} label="Delete service" kind="danger" onClick={onDelete} />}
        onClose={onClose} closeLabel="Close chat">
        {models.length > 0 && (
          <Select size="sm" ariaLabel="Model" value={model} onChange={setModel}
            options={models.map((m) => ({ value: m, label: m }))} className="max-w-56" />
        )}
        {!loading && models.length === 0 && (
          <Input size="sm" aria-label="Model" placeholder="Model name" value={model}
            onChange={(event) => setModel(event.target.value)} className="w-44" />
        )}
      </ModalHeader>
      <ModalNotice message={loadError || (error ? errText(error, 'The model did not answer.') : '')} />
      <ModalBody padded={false}>
        {loading ? (
          <div className="flex flex-1 items-center justify-center">
            <Spinner label="Opening the line…" />
          </div>
        ) : (
          <>
            <div ref={scroller} className="min-h-0 flex-1 overflow-y-auto px-5 py-4">
              {messages.length === 0 && (
                <div className="flex h-full items-center justify-center">
                  <p className="max-w-sm text-center text-sm text-muted">
                    Chat with {service} using the same connection as its apps.
                  </p>
                </div>
              )}
              <div className="space-y-3">
                {messages.map((m) => (
                  <div key={m.id} className={m.role === 'user' ? 'flex justify-end' : ''}>
                    {m.role === 'user' ? (
                      <div className="max-w-[85%] rounded-lg bg-accent-soft px-3 py-2 text-sm text-ink">
                        {m.parts.map((p, i) => (p.type === 'text' ? <span key={i}>{p.content}</span> : null))}
                      </div>
                    ) : (
                      <div className="ai-md max-w-[95%] text-sm leading-relaxed text-ink-soft">
                        {m.parts.map((p, i) =>
                          p.type === 'thinking' ? (
                            <p key={i} className="italic text-muted">
                              {p.content}
                            </p>
                          ) : p.type === 'text' ? (
                            <Markdown key={i} highlighter={codeHighlighter}>
                              {p.content}
                            </Markdown>
                          ) : null,
                        )}
                      </div>
                    )}
                  </div>
                ))}
              </div>
            </div>
            <div className="shrink-0 border-t border-line px-5 py-3">
              <div className="flex items-end gap-2">
                <TextArea
                  value={input}
                  onChange={(e) => setInput(e.target.value)}
                  onKeyDown={(e) => {
                    if (e.key === 'Enter' && !e.shiftKey) {
                      e.preventDefault()
                      send()
                    }
                  }}
                  placeholder={`Ask ${model || 'the model'}…`}
                  aria-label="Prompt"
                  rows={1}
                  className="max-h-40 min-h-9 flex-1 resize-none field-sizing-content"
                />
                {isLoading ? (
                  <Button kind="quiet" icon={Square} onClick={() => stop()}>Stop</Button>
                ) : (
                  <Button icon={Send} onClick={send} disabled={!input.trim() || !base || !model.trim() || Boolean(loadError)}>Send</Button>
                )}
              </div>
              <p className="mt-1.5 text-xs text-muted">
                Enter sends, Shift-Enter breaks the line. The conversation lives only in this tab.
              </p>
            </div>
          </>
        )}
      </ModalBody>
    </Modal>
  )
}
