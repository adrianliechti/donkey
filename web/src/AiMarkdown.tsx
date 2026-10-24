import { useMemo, type ComponentPropsWithoutRef } from 'react'
import { Markdown as Render } from '@tanstack/markdown/react'
import { highlightToHtml } from '@tanstack/highlight'
import githubDarkTheme from '@tanstack/highlight/themes/github-dark'
import githubLightTheme from '@tanstack/highlight/themes/github-light'
import { useColorScheme } from './hooks'

// Markdown as models write it — and as Exa returns a page's text. The
// renderer escapes raw HTML (allowHtml stays off) and sanitizes link
// targets, so a model or a web page cannot put markup into the console.
//
// The highlighter emits `th-<token>` classes; the theme is a token→color
// map. One generated stylesheet marries them, scoped under `.ai-md` so the
// console's styles stay untouched, and follows the OS light/dark preference,
// since these colours are hex, not CSS tokens. Everything else — code
// surfaces, rules, links — is the console's own tokens. The theme's catch-all
// `token` colour is left out: every token carries that class too, and at
// equal specificity it would paint over the real colours.
type HighlightTheme = { tokens: Record<string, string> }

function themeCss(theme: HighlightTheme): string {
  return [
    `.ai-md{overflow-wrap:anywhere}`,
    `.ai-md>:first-child{margin-top:0}`,
    `.ai-md>:last-child{margin-bottom:0}`,
    `.ai-md p{margin:6px 0}`,
    `.ai-md ul,.ai-md ol{margin:6px 0;padding-left:1.25rem;list-style:revert}`,
    `.ai-md li{margin:2px 0}`,
    `.ai-md h1,.ai-md h2,.ai-md h3,.ai-md h4,.ai-md h5,.ai-md h6{font-weight:600;color:var(--color-ink);margin:14px 0 4px}`,
    `.ai-md h1{font-size:1.15rem}.ai-md h2{font-size:1.05rem}.ai-md h3,.ai-md h4{font-size:0.95rem}`,
    `.ai-md a{color:var(--color-accent-strong);text-decoration:underline;text-underline-offset:2px}`,
    `.ai-md strong{font-weight:600;color:var(--color-ink)}`,
    `.ai-md blockquote{margin:8px 0;padding-left:10px;border-left:2px solid var(--color-line-strong);color:var(--color-ink-soft)}`,
    `.ai-md hr{margin:14px 0;border-color:var(--color-line)}`,
    `.ai-md code{font-family:var(--font-mono);font-size:0.85em}`,
    `.ai-md :not(pre)>code{padding:1px 4px;border-radius:4px;background:var(--color-raised);color:var(--color-emphasis)}`,
    `.ai-md pre{margin:8px 0}`,
    `.ai-md pre pre{margin:0}`,
    `.ai-md .th-code{display:block;overflow-x:auto;border-radius:6px;padding:10px 12px;background:var(--color-well);color:var(--color-emphasis);font-size:11px;line-height:1.6}`,
    `.ai-md table{display:block;max-width:100%;overflow-x:auto;border-collapse:collapse;margin:8px 0;font-size:0.9em}`,
    `.ai-md td,.ai-md th{border:1px solid var(--color-line-strong);padding:3px 8px;text-align:left}`,
    `.ai-md th{background:var(--color-raised);font-weight:600;color:var(--color-ink)}`,
    ...Object.entries(theme.tokens)
      .filter(([name]) => name !== 'token')
      .map(([name, color]) => `.ai-md .th-${name}{color:${color}}`),
  ].join('\n')
}

const highlighter = (code: string, lang?: string) => highlightToHtml(code, { lang })

// Links open beside the console, never in its place. Images are links too:
// the console's content policy loads none from elsewhere, and a broken
// picture says less than its address.
function Link(props: ComponentPropsWithoutRef<'a'>) {
  return <a {...props} target="_blank" rel="noopener noreferrer" />
}

function Picture({ src, alt }: ComponentPropsWithoutRef<'img'>) {
  if (!src) return null
  return (
    <a href={String(src)} target="_blank" rel="noopener noreferrer">
      {alt || 'image'}
    </a>
  )
}

const components = { a: Link, img: Picture }

/** The stylesheet the rendered markdown needs, once per view. */
export function MarkdownStyle() {
  const scheme = useColorScheme()
  const css = useMemo(() => themeCss(scheme === 'dark' ? githubDarkTheme : githubLightTheme), [scheme])
  return <style>{css}</style>
}

export function Markdown({ text, className = '' }: { text: string; className?: string }) {
  // Parsing is the cost; a long conversation re-renders on every token of
  // the newest answer, and the older ones need not be parsed again.
  const rendered = useMemo(
    () => (
      <Render highlighter={highlighter} components={components}>
        {text}
      </Render>
    ),
    [text],
  )
  return <div className={`ai-md ${className}`}>{rendered}</div>
}
