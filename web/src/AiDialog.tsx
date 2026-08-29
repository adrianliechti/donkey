import { useCallback, useEffect, useMemo, useRef, useState } from 'react'
import { Dialog, DialogBackdrop, DialogPanel, DialogTitle } from '@headlessui/react'
import { chat } from '@tanstack/ai'
import { openaiCompatibleText } from '@tanstack/ai-openai/compatible'
import { useChat } from '@tanstack/ai-react'
import { Markdown } from '@tanstack/markdown/react'
import { highlightToHtml } from '@tanstack/highlight'
import githubDarkTheme from '@tanstack/highlight/themes/github-dark'
import githubLightTheme from '@tanstack/highlight/themes/github-light'
import { Send, Sparkles, Square } from 'lucide-react'
import { api } from './api'
import { Button, ErrorNote, Select, Spinner, errText, useColorScheme } from './ui'

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
 * The AI explorer: a chat against a space's openai service, spoken through
 * the same proxy and the same per-space token the apps get — what you try
 * here is exactly what an app will see. Everything renders as markdown with
 * highlighted code, streamed as it arrives.
 */
export function AiDialog({
  space,
  service,
  onClose,
}: {
  space: string
  service: string
  onClose: () => void
}) {
  const [token, setToken] = useState('')
  const [base, setBase] = useState('')
  const [external, setExternal] = useState('')
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
  const settings = useRef({ base: '', token: '', model: '' })
  settings.current = { base, token, model }

  useEffect(() => {
    api
      .serviceBinding(space, service)
      .then(async (res) => {
        const bound = res.binding.OPENAI_BASE_URL ?? ''
        // Only the platform's own endpoint is reachable from the browser
        // (and from this origin's CSP). A service pointing elsewhere is for
        // apps.
        //
        // The key is the tell, not the address. donkey mints a per-space
        // token for its own endpoint alone, and that endpoint accepts no
        // other credential — so a binding carrying one is donkey's, whatever
        // the address says (a Service on Kubernetes, a container name on
        // Docker: both unresolvable here, and both have moved before). A
        // service attached with --url carries an address and nothing else;
        // that one is the apps', and they bring their own key.
        const key = res.binding.OPENAI_API_KEY ?? ''
        if (!key) {
          setExternal(bound)
          return
        }
        // donkey's endpoint answers on this origin under /ai/v1, which the
        // server forwards to it — on this origin /v1 is already the admin
        // API.
        const sameOrigin = `${location.origin}/ai/v1`
        setBase(sameOrigin)
        setToken(key)
        const preferred = res.binding.OPENAI_MODEL
        try {
          const answer = await fetch(`${sameOrigin}/models`, {
            headers: { Authorization: `Bearer ${key}` },
          })
          const listed = ((await answer.json()).data ?? []).map((m: { id: string }) => m.id)
          // The binding's model leads: it is the one the platform vouches
          // for, whether or not the upstream's own listing mentions it.
          const list = [...new Set([...(preferred ? [preferred] : []), ...listed])]
          setModels(list)
          setModel(preferred ?? list[0] ?? '')
        } catch {
          // No model list is not fatal — the binding's own model still works.
          setModel(preferred ?? '')
        }
      })
      .catch((err) => setLoadError(errText(err, 'The service did not answer.')))
  }, [space, service])

  const fetcher = useCallback(
    ({ messages }: { messages: Parameters<typeof chat>[0]['messages'] }) => {
      // Read at send time, not render time — the binding arrives after the
      // first render, and a snapshot from back then would chat with an
      // empty key.
      const { base, token, model } = settings.current
      return chat({
        adapter: openaiCompatibleText(model, {
          baseURL: base,
          apiKey: token,
          // The "danger" is putting a credential in a browser — this one is
          // the console operator's own per-space token, already visible to
          // them via the binding API.
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

  const ready = Boolean(token && model)

  return (
    <Dialog open onClose={onClose} className="relative z-50">
      <style>{themeCss}</style>
      <DialogBackdrop className="fixed inset-0 bg-black/60" />
      <div className="fixed inset-0 flex items-center justify-center p-4">
        <DialogPanel className="flex h-[85vh] w-full max-w-3xl flex-col rounded-lg border border-line bg-card shadow-2xl">
          <div className="flex flex-wrap items-center justify-between gap-3 border-b border-line px-4 py-3">
            <div className="flex items-center gap-3">
              <DialogTitle className="flex items-center gap-2 font-mono text-[11px] font-semibold uppercase tracking-[0.14em] text-muted">
                <Sparkles className="size-3.5" aria-hidden />
                {service} · {space}
              </DialogTitle>
              {models.length > 0 && (
                <Select
                  size="sm"
                  ariaLabel="Model"
                  value={model}
                  onChange={setModel}
                  options={models.map((m) => ({ value: m, label: m }))}
                />
              )}
              {models.length === 0 && model && (
                <span className="font-mono text-xs text-muted">{model}</span>
              )}
            </div>
            <Button kind="quiet" onClick={onClose}>
              Close
            </Button>
          </div>

          <div className="px-4 pt-2 empty:hidden">
            <ErrorNote message={loadError || (error ? errText(error, 'The model did not answer.') : '')} />
          </div>

          {external ? (
            <div className="flex flex-1 items-center justify-center p-8">
              <p className="max-w-md text-center text-sm text-muted">
                This service points at <span className="font-mono">{external}</span> — somewhere the
                browser can't go. Apps use it through their binding; the explorer only speaks to the
                platform's own proxy.
              </p>
            </div>
          ) : !ready && !loadError ? (
            <div className="flex flex-1 items-center justify-center">
              <Spinner label="Opening the line…" />
            </div>
          ) : (
            <>
              <div ref={scroller} className="min-h-0 flex-1 overflow-y-auto px-4 py-3">
                {messages.length === 0 && (
                  <div className="flex h-full items-center justify-center">
                    <p className="max-w-sm text-center text-sm text-muted">
                      This talks to the same proxy, with the same token, that connected apps get —
                      what works here works there. Ask something.
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
              <div className="border-t border-line p-3">
                <div className="flex items-end gap-2">
                  <textarea
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
                    className="max-h-40 min-h-0 flex-1 resize-none rounded-md border border-line bg-surface p-2 font-mono text-sm field-sizing-content focus:outline-2 focus:outline-accent-strong"
                  />
                  {isLoading ? (
                    <Button kind="quiet" onClick={() => stop()}>
                      <span className="flex items-center gap-1.5">
                        <Square className="size-3.5" aria-hidden /> Stop
                      </span>
                    </Button>
                  ) : (
                    <Button onClick={send} disabled={!input.trim() || !ready}>
                      <span className="flex items-center gap-1.5">
                        <Send className="size-3.5" aria-hidden /> Send
                      </span>
                    </Button>
                  )}
                </div>
                <p className="mt-1.5 text-xs text-muted">
                  Enter sends, Shift-Enter breaks the line. The conversation lives only in this tab.
                </p>
              </div>
            </>
          )}
        </DialogPanel>
      </div>
    </Dialog>
  )
}
