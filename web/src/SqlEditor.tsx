import { useRef, type KeyboardEvent, type Ref } from 'react'

/**
 * A plain SQL editor: a monospace textarea with line numbers. Tab indents
 * (Esc, then Tab, leaves the editor — the keyboard is never trapped), Enter
 * keeps the line's indentation, and ⌘↵ asks to run. Edits go through the
 * browser's own insert command, so undo keeps working.
 */
export function SqlEditor({
  value,
  onChange,
  onRun,
  placeholder,
  label = 'SQL',
  textareaRef,
}: {
  value: string
  onChange: (value: string) => void
  /** ⌘↵ runs the selection or everything; ⇧⌘↵ the statement at the cursor. */
  onRun: (which: 'selection' | 'statement', editor: HTMLTextAreaElement) => void
  placeholder?: string
  label?: string
  textareaRef?: Ref<HTMLTextAreaElement>
}) {
  const gutter = useRef<HTMLDivElement>(null)
  const released = useRef(false)
  const lines = value.split('\n').length

  const onKeyDown = (e: KeyboardEvent<HTMLTextAreaElement>) => {
    const el = e.currentTarget
    if ((e.metaKey || e.ctrlKey) && e.key === 'Enter') {
      e.preventDefault()
      onRun(e.shiftKey ? 'statement' : 'selection', el)
      return
    }
    if (e.key === 'Escape') {
      released.current = true
      return
    }
    if (e.key === 'Tab' && !released.current && !e.metaKey && !e.ctrlKey && !e.altKey) {
      e.preventDefault()
      indent(el, e.shiftKey ? -1 : 1)
      return
    }
    released.current = false
    if (e.key === 'Enter' && !e.shiftKey && !e.altKey) {
      const start = el.value.lastIndexOf('\n', el.selectionStart - 1) + 1
      const leading = /^[ \t]*/.exec(el.value.slice(start, el.selectionStart))?.[0] ?? ''
      if (leading) {
        e.preventDefault()
        insert(el, `\n${leading}`)
      }
    }
  }

  return (
    <div className="flex min-h-0 flex-1 overflow-hidden bg-surface">
      <div
        ref={gutter}
        aria-hidden
        className="w-10 shrink-0 overflow-hidden border-r border-line/60 py-2.5 pr-2 text-right font-mono text-xs leading-5 text-muted/70 select-none"
      >
        {Array.from({ length: lines }, (_, i) => (
          <div key={i}>{i + 1}</div>
        ))}
      </div>
      <textarea
        ref={textareaRef}
        value={value}
        onChange={(e) => onChange(e.target.value)}
        onKeyDown={onKeyDown}
        onBlur={() => {
          released.current = false
        }}
        onScroll={(e) => {
          if (gutter.current) gutter.current.scrollTop = e.currentTarget.scrollTop
        }}
        placeholder={placeholder}
        aria-label={label}
        spellCheck={false}
        autoCapitalize="off"
        autoCorrect="off"
        autoComplete="off"
        wrap="off"
        // No ligatures: -> and >= must read as typed, not as arrows.
        className="min-w-0 flex-1 resize-none overflow-auto bg-transparent px-3 py-2.5 font-mono text-xs leading-5 whitespace-pre text-ink [font-variant-ligatures:none] outline-none placeholder:text-muted/60"
      />
    </div>
  )
}

const unit = '  '

// insert types text at the selection as the browser would, so the change
// lands on the undo stack; setRangeText is the fallback where the command
// is gone.
function insert(el: HTMLTextAreaElement, text: string) {
  el.focus()
  if (!document.execCommand('insertText', false, text)) {
    el.setRangeText(text, el.selectionStart, el.selectionEnd, 'end')
    el.dispatchEvent(new Event('input', { bubbles: true }))
  }
}

// indent: a caret gets two spaces (or loses them, outdenting); a selection
// across lines moves every line it touches.
function indent(el: HTMLTextAreaElement, dir: 1 | -1) {
  const { value, selectionStart: from, selectionEnd: to } = el
  if (dir === 1 && !value.slice(from, to).includes('\n')) {
    insert(el, unit)
    return
  }
  const start = value.lastIndexOf('\n', from - 1) + 1
  const endBreak = value.indexOf('\n', to > from && value[to - 1] === '\n' ? to - 1 : to)
  const end = endBreak < 0 ? value.length : endBreak
  const block = value.slice(start, end).split('\n')
  const moved = block.map((line) => (dir === 1 ? unit + line : line.replace(/^ {1,2}|^\t/, '')))
  const firstShift = moved[0].length - block[0].length
  const total = moved.join('\n').length - block.join('\n').length
  el.setSelectionRange(start, end)
  insert(el, moved.join('\n'))
  el.setSelectionRange(Math.max(start, from + firstShift), Math.max(start, to + total))
}
