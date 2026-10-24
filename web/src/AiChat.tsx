import { useEffect, useLayoutEffect, useRef, useState } from 'react'
import { Bot, MessagesSquare, Plus, RotateCcw, Send, SlidersHorizontal, Square, Trash2 } from 'lucide-react'
import {
  ask,
  chatsBox,
  composerBox,
  deleteConversation,
  draftBox,
  freshConversation,
  liveBox,
  regenerate,
  settingsOpenBox,
  stop,
  updateConversation,
  useModelChoices,
  type Conversation,
  type Live,
  type Message,
  type ModelChoices,
} from './aiApi'
import { Markdown, MarkdownStyle } from './AiMarkdown'
import { useBox } from './httpStore'
import { ConfirmIconButton, Keys, Pane, PaneHeader, SideHeader, SideItem } from './HttpWorkbench'
import { useNow } from './hooks'
import { useServiceSearch } from './serviceSearch'
import { Button, Callout, Chip, CopyButton, Disclosure, Field, IconButton, Input, Select, TextArea } from './ui'
import { ago } from './format'

// The Chat tab: conversations on the left, the open one beside them, its
// settings (model, system prompt, temperature, token limit) in a column that
// opens on the right. The open conversation is the console's place
// (?chat=…); conversations themselves live in this browser.

export function ChatView({ space, service }: { space: string; service: string }) {
  const [search, move] = useServiceSearch()
  const chats = useBox(chatsBox(space, service))
  const draft = useBox(draftBox(space, service))
  const live = useBox(liveBox(space, service))
  const settingsOpen = useBox(settingsOpenBox)
  const models = useModelChoices(space, service)

  const open = search.chat ? chats.find((c) => c.id === search.chat) : undefined
  const missing = Boolean(search.chat && !open)
  const chat = open ?? draft
  const answering = live[chat.id]

  // A new conversation starts on the service's own model, once known.
  useEffect(() => {
    if (!open && !draft.model && models.preferred) draftBox(space, service).set((d) => ({ ...d, model: models.preferred }))
  }, [open, draft.model, models.preferred, space, service])

  // A conversation started from the draft becomes the place it now is.
  const asked = useRef('')
  useEffect(() => {
    if (asked.current && !open && chats.some((c) => c.id === asked.current)) {
      move({ ...search, chat: asked.current }, 'replace')
      asked.current = ''
    }
  }, [chats, open, search, move])

  const send = (text: string) => {
    if (!open) asked.current = chat.id
    ask(space, service, chat.id, text)
  }

  const newChat = () => {
    if (!open && draft.messages.length === 0) return
    draftBox(space, service).set(freshConversation(chat.model || models.preferred))
    move({ ...search, chat: '' })
  }

  return (
    <div className="flex min-h-0 flex-1 gap-4">
      <MarkdownStyle />
      <Pane label="Conversations" className="hidden w-64 shrink-0 md:flex">
        <div className="shrink-0 p-2">
          <Button kind="quiet" size="sm" icon={Plus} className="w-full" onClick={newChat}>
            New chat
          </Button>
        </div>
        <SideHeader
          title="Conversations"
          count={chats.length}
          actions={
            chats.length > 0 && (
              <ConfirmIconButton
                icon={Trash2}
                label="Delete every conversation"
                onConfirm={() => {
                  for (const c of chats) deleteConversation(space, service, c.id)
                  move({ ...search, chat: '' }, 'replace')
                }}
              />
            )
          }
        />
        <ul className="min-h-0 flex-1 overflow-y-auto pb-1">
          {chats.length === 0 && (
            <li className="px-3 pb-2 text-xs leading-relaxed text-muted">
              Conversations are kept here, in this browser, as you have them.
            </li>
          )}
          {chats.map((c) => (
            <SideItem
              key={c.id}
              selected={c.id === chat.id}
              onOpen={() => c.id !== search.chat && move({ ...search, chat: c.id })}
              title={c.title}
              meta={
                <>
                  {live[c.id] ? <span className="text-accent-strong">answering…</span> : `${ago(new Date(c.updatedAt).toISOString())} ago`}
                  {' · '}
                  {c.messages.length} {c.messages.length === 1 ? 'message' : 'messages'}
                </>
              }
              onRename={{
                name: c.title,
                set: (title) => updateConversation(space, service, c.id, (x) => ({ ...x, title, named: true })),
              }}
              onDelete={() => {
                deleteConversation(space, service, c.id)
                if (c.id === search.chat) move({ ...search, chat: '' }, 'replace')
              }}
              deleteLabel={`Delete ${c.title}`}
            />
          ))}
        </ul>
      </Pane>

      <Pane label="Chat" className="flex-1">
        <PaneHeader
          actions={
            <>
              <IconButton icon={Plus} label="New chat" onClick={newChat} className="md:hidden" />
              <IconButton
                icon={SlidersHorizontal}
                label={settingsOpen ? 'Hide the settings' : 'Settings: system prompt, temperature, tokens'}
                active={settingsOpen}
                onClick={() => settingsOpenBox.set(!settingsOpen)}
              />
            </>
          }
        >
          <div className="flex min-w-0 items-center gap-2">
            <ModelPicker
              value={chat.model}
              choices={models}
              onChange={(model) => updateConversation(space, service, chat.id, (c) => ({ ...c, model }))}
            />
            {chat.system.trim() && <Chip title={chat.system}>system prompt</Chip>}
            {chat.temperature && <Chip>temperature {chat.temperature}</Chip>}
            {chat.maxTokens && <Chip>max {chat.maxTokens} tokens</Chip>}
          </div>
        </PaneHeader>
        <div className="flex min-h-0 flex-1">
          <div className="flex min-w-0 flex-1 flex-col">
            {missing && (
              <Callout tone="warning" className="m-3 mb-0" action={<Button kind="quiet" size="sm" onClick={() => move({ ...search, chat: '' }, 'replace')}>Dismiss</Button>}>
                That conversation is not in this browser — conversations stay where they were had.
              </Callout>
            )}
            <Messages
              key={chat.id}
              service={service}
              chat={chat}
              live={answering}
              onRegenerate={() => regenerate(space, service, chat.id)}
              onStop={() => stop(chat.id)}
            />
            <Composer
              key={`composer:${chat.id}`}
              space={space}
              service={service}
              chat={chat}
              busy={Boolean(answering)}
              onSend={send}
              onStop={() => stop(chat.id)}
            />
          </div>
          {settingsOpen && (
            <Settings
              chat={chat}
              onChange={(patch) => updateConversation(space, service, chat.id, (c) => ({ ...c, ...patch }))}
            />
          )}
        </div>
      </Pane>
    </div>
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
      <Input
        size="sm"
        aria-label="Model"
        placeholder="model id"
        value={value}
        onChange={(e) => onChange(e.target.value)}
        className="w-56"
        spellCheck={false}
      />
    )
  }
  const ids = value && !choices.ids.includes(value) ? [value, ...choices.ids] : choices.ids
  return (
    <Select
      size="sm"
      ariaLabel="Model"
      value={value}
      onChange={onChange}
      options={ids.map((id) => ({ value: id, label: id === choices.bound ? `${id} · default` : id }))}
      className="max-w-72 min-w-40"
    />
  )
}

// ─── The conversation ──────────────────────────────────────────────────

function Messages({
  service,
  chat,
  live,
  onRegenerate,
  onStop,
}: {
  service: string
  chat: Conversation
  live?: Live
  onRegenerate: () => void
  onStop: () => void
}) {
  const scroller = useRef<HTMLDivElement>(null)
  // Follow the answer as it grows — unless the reader scrolled up to read.
  const pinned = useRef(true)
  useLayoutEffect(() => {
    const el = scroller.current
    if (el && pinned.current) el.scrollTop = el.scrollHeight
  }, [chat.messages.length, live?.content, live?.reasoning])

  const last = chat.messages.at(-1)
  return (
    <div
      ref={scroller}
      onScroll={() => {
        const el = scroller.current
        if (el) pinned.current = el.scrollHeight - el.scrollTop - el.clientHeight < 48
      }}
      className="min-h-0 flex-1 overflow-y-auto"
    >
      {chat.messages.length === 0 && !live ? (
        <div className="flex h-full flex-col items-center justify-center gap-2 p-8 text-center">
          <MessagesSquare className="size-6 text-muted/70" aria-hidden />
          <p className="text-sm font-medium text-ink">Chat with {service}</p>
          <p className="max-w-sm text-xs leading-relaxed text-muted">
            {chat.model ? (
              <>
                Answers come from <span className="font-mono text-ink-soft">{chat.model}</span>, over the same connection the
                space's apps use.
              </>
            ) : (
              'Pick a model above to start.'
            )}
          </p>
        </div>
      ) : (
        <div className="mx-auto flex max-w-3xl flex-col gap-6 px-5 py-5">
          {chat.messages.map((m) =>
            m.role === 'user' ? (
              <UserMessage key={m.id} message={m} />
            ) : (
              <Answer
                key={m.id}
                message={m}
                onRegenerate={m === last && !live ? onRegenerate : undefined}
              />
            ),
          )}
          {live && <LiveAnswer live={live} onStop={onStop} />}
        </div>
      )}
    </div>
  )
}

function UserMessage({ message }: { message: Message }) {
  return (
    <div className="group flex items-start justify-end gap-1">
      <div className="opacity-0 transition-opacity group-focus-within:opacity-100 group-hover:opacity-100">
        <CopyButton text={message.content} label="Copy the message" />
      </div>
      <div className="max-w-[85%] rounded-lg bg-raised px-3 py-2 text-sm leading-relaxed break-words whitespace-pre-wrap text-ink">
        {message.content}
      </div>
    </div>
  )
}

function Speaker({ model }: { model?: string }) {
  return (
    <div className="mb-1.5 flex items-center gap-1.5 text-xs text-muted">
      <Bot className="size-3.5" aria-hidden />
      <span className="font-mono">{model || 'assistant'}</span>
    </div>
  )
}

function Thinking({ text, open = false }: { text: string; open?: boolean }) {
  return (
    <div className="mb-2">
      <Disclosure summary="Thinking" defaultOpen={open}>
        <p className="border-l-2 border-line pl-3 text-xs leading-relaxed whitespace-pre-wrap text-muted">{text}</p>
      </Disclosure>
    </div>
  )
}

function Answer({ message, onRegenerate }: { message: Message; onRegenerate?: () => void }) {
  const usage = message.usage
  const facts = [
    message.ms !== undefined && `${(message.ms / 1000).toFixed(1)} s`,
    usage?.prompt !== undefined && `${usage.prompt} in`,
    usage?.completion !== undefined && `${usage.completion} out`,
    usage?.reasoning && `${usage.reasoning} thinking`,
  ].filter(Boolean)
  return (
    <div className="group">
      <Speaker model={message.model} />
      {message.reasoning && <Thinking text={message.reasoning} />}
      {message.content && <Markdown text={message.content} className="text-sm leading-relaxed text-ink-soft" />}
      {message.error && (
        <Callout tone="danger" className="mt-2">
          {message.error}
        </Callout>
      )}
      {message.finish === 'length' && (
        <p className="mt-2 text-xs text-warning">Cut off at the token limit.</p>
      )}
      {message.finish && message.finish !== 'length' && (
        <p className="mt-2 text-xs text-muted">Ended: {message.finish.replaceAll('_', ' ')}.</p>
      )}
      {message.stopped && <p className="mt-2 text-xs text-muted">Stopped.</p>}
      <div className="mt-1 flex min-h-8 items-center gap-2">
        {facts.length > 0 && (
          <span className="font-mono text-[11px] text-muted" title="Time, and tokens as the service counted them">
            {facts.join(' · ')}
          </span>
        )}
        <div className="flex items-center opacity-0 transition-opacity group-focus-within:opacity-100 group-hover:opacity-100">
          {message.content && <CopyButton text={message.content} label="Copy the answer" />}
          {onRegenerate && <IconButton icon={RotateCcw} label="Answer again" onClick={onRegenerate} />}
        </div>
      </div>
    </div>
  )
}

function LiveAnswer({ live, onStop }: { live: Live; onStop: () => void }) {
  const now = useNow(500)
  return (
    <div aria-live="polite" aria-busy>
      <Speaker model={live.model} />
      {live.reasoning && <Thinking text={live.reasoning} open={!live.content} />}
      {live.content ? (
        <Markdown text={live.content} className="text-sm leading-relaxed text-ink-soft" />
      ) : (
        !live.reasoning && <p className="text-sm text-muted">…</p>
      )}
      <div className="mt-1.5 flex min-h-7 items-center gap-2 text-[11px] text-muted">
        <span className="inline-block size-1.5 animate-pulse rounded-full bg-accent" aria-hidden />
        <span className="font-mono">
          {live.content ? 'writing' : live.reasoning ? 'thinking' : 'waiting'} · {Math.max(0, (now - live.startedAt) / 1000).toFixed(0)} s
        </span>
        <Button kind="quiet" size="sm" icon={Square} onClick={onStop}>
          Stop
        </Button>
      </div>
    </div>
  )
}

// ─── Asking ────────────────────────────────────────────────────────────

function Composer({
  space,
  service,
  chat,
  busy,
  onSend,
  onStop,
}: {
  space: string
  service: string
  chat: Conversation
  busy: boolean
  onSend: (text: string) => void
  onStop: () => void
}) {
  const composer = composerBox(space, service)
  const text = useBox(composer)[chat.id] ?? ''
  const setText = (value: string) => composer.set((all) => ({ ...all, [chat.id]: value }))
  const ready = Boolean(text.trim() && chat.model.trim() && !busy)
  const submit = () => {
    if (!ready) return
    onSend(text.trim())
    setText('')
  }
  return (
    <div className="shrink-0 border-t border-line px-5 py-3">
      <div className="mx-auto flex max-w-3xl items-end gap-2">
        <TextArea
          // Keyed by conversation: opening one puts the cursor here.
          autoFocus
          value={text}
          onChange={(e) => setText(e.target.value)}
          onKeyDown={(e) => {
            // Enter sends, as in any chat; ⌘/Ctrl-Enter too, as everywhere
            // in the consoles. Shift-Enter breaks the line.
            if (e.key === 'Enter' && !e.shiftKey && !e.nativeEvent.isComposing) {
              e.preventDefault()
              submit()
            }
          }}
          placeholder={chat.model ? `Ask ${chat.model}…` : 'Pick a model first'}
          aria-label="Message"
          rows={1}
          className="max-h-48 min-h-9 flex-1 resize-none field-sizing-content"
        />
        {busy ? (
          <Button kind="quiet" icon={Square} onClick={onStop}>
            Stop
          </Button>
        ) : (
          <Button icon={Send} onClick={submit} disabled={!ready}>
            Send
          </Button>
        )}
      </div>
      <p className="mx-auto mt-1.5 max-w-3xl text-xs text-muted">
        <Keys chord="send" /> or Enter sends, Shift-Enter breaks the line. Conversations are kept in this browser only.
      </p>
    </div>
  )
}

function Settings({ chat, onChange }: { chat: Conversation; onChange: (patch: Partial<Conversation>) => void }) {
  const [temperature, setTemperature] = useState(chat.temperature)
  const [maxTokens, setMaxTokens] = useState(chat.maxTokens)
  // The fields follow the open conversation.
  const [shown, setShown] = useState(chat.id)
  if (shown !== chat.id) {
    setShown(chat.id)
    setTemperature(chat.temperature)
    setMaxTokens(chat.maxTokens)
  }
  const badTemperature = temperature !== '' && !(Number(temperature) >= 0 && Number(temperature) <= 2)
  const badTokens = maxTokens !== '' && !(Number.isInteger(Number(maxTokens)) && Number(maxTokens) > 0)
  return (
    <aside aria-label="Conversation settings" className="flex w-72 shrink-0 flex-col gap-4 overflow-y-auto border-l border-line p-4">
      <Field label="System prompt" hint="Sent first, before the conversation.">
        <TextArea
          size="sm"
          value={chat.system}
          onChange={(e) => onChange({ system: e.target.value })}
          rows={8}
          placeholder="You are a helpful assistant."
          className="resize-y font-sans"
        />
      </Field>
      <Field label="Temperature" hint={badTemperature ? 'Between 0 and 2.' : 'Empty uses the model’s default.'}>
        <Input
          size="sm"
          inputMode="decimal"
          value={temperature}
          placeholder="default"
          aria-invalid={badTemperature || undefined}
          onChange={(e) => {
            setTemperature(e.target.value)
            const v = e.target.value.trim()
            if (v === '' || (Number(v) >= 0 && Number(v) <= 2)) onChange({ temperature: v })
          }}
          className={badTemperature ? 'border-danger' : ''}
        />
      </Field>
      <Field label="Max tokens" hint={badTokens ? 'A whole number above 0.' : 'The longest answer; empty uses the model’s default.'}>
        <Input
          size="sm"
          inputMode="numeric"
          value={maxTokens}
          placeholder="default"
          aria-invalid={badTokens || undefined}
          onChange={(e) => {
            setMaxTokens(e.target.value)
            const v = e.target.value.trim()
            if (v === '' || (Number.isInteger(Number(v)) && Number(v) > 0)) onChange({ maxTokens: v })
          }}
          className={badTokens ? 'border-danger' : ''}
        />
      </Field>
      <p className="text-xs leading-relaxed text-muted">These apply to this conversation, from its next answer.</p>
    </aside>
  )
}
