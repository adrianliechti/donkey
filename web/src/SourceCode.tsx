import { useMemo } from 'react'
import { highlightToHtml, type HighlightLanguage } from '@tanstack/highlight'
import githubDarkTheme from '@tanstack/highlight/themes/github-dark'
import githubLightTheme from '@tanstack/highlight/themes/github-light'
import { useColorScheme } from './ui'

// One file of an app's source, highlighted, with line numbers in the same
// gutter the diff view uses. The highlighter knows a fixed set of languages;
// anything else (Go, Java, …) reads as plain text, which beats guessing.

const byExtension: Record<string, HighlightLanguage> = {
  js: 'js', mjs: 'js', cjs: 'js', jsx: 'jsx',
  ts: 'ts', mts: 'ts', cts: 'ts', tsx: 'tsx',
  py: 'python', json: 'json', yaml: 'yaml', yml: 'yaml', toml: 'toml',
  css: 'css', scss: 'css', html: 'html', htm: 'html', vue: 'vue', svelte: 'svelte', ejs: 'ejs',
  md: 'markdown', markdown: 'markdown', mmd: 'mermaid',
  sh: 'shell', bash: 'shell', zsh: 'shell', sql: 'sql',
  diff: 'diff', patch: 'diff', dockerfile: 'dockerfile', env: 'env', conf: 'nginx',
}

function languageOf(path: string): HighlightLanguage {
  const name = path.split('/').pop()?.toLowerCase() ?? ''
  if (name === 'dockerfile' || name.startsWith('dockerfile.') || name.endsWith('.dockerfile')) return 'dockerfile'
  if (name === '.env' || name.startsWith('.env.')) return 'env'
  if (name === '.htaccess') return 'apache'
  return byExtension[name.split('.').pop() ?? ''] ?? 'plaintext'
}

// The highlighter emits `th-<token>` classes; the theme is a token→color
// map. Scoped under the viewer's class, following the OS light/dark choice
// like the AI chat does. The background stays the console's well colour, so
// the theme's catch-all `token` color is left out: every token carries that
// class too, and at equal specificity it would paint over the real colors.
type Theme = { tokens: Record<string, string> }
function themeCss(theme: Theme): string {
  return Object.entries(theme.tokens)
    .filter(([name]) => name !== 'token')
    .map(([name, color]) => `.source-code .th-${name}{color:${color}}`)
    .join('\n')
}

// With line numbers on, every line comes back as its own self-contained
// span (a multi-line comment is split per line), so the HTML can be cut at
// the newlines between them.
function highlightedLines(path: string, text: string): string[] {
  const html = highlightToHtml(text, { lang: languageOf(path), lineNumbers: true })
  const start = html.indexOf('<code>') + '<code>'.length
  const end = html.lastIndexOf('</code>')
  const lines = html.slice(start, end).split('\n')
  if (lines.length > 1 && lines[lines.length - 1] === '') lines.pop()
  return lines
}

export function SourceCode({ path, text, className = 'rounded-md' }: { path: string; text: string; className?: string }) {
  const scheme = useColorScheme()
  const css = useMemo(() => themeCss(scheme === 'dark' ? githubDarkTheme : githubLightTheme), [scheme])
  const lines = useMemo(() => highlightedLines(path, text), [path, text])

  return (
    <div className={`source-code overflow-x-auto bg-well py-1 ${className}`}>
      <style>{css}</style>
      <table className="w-full border-collapse font-mono text-xs leading-relaxed">
        <tbody>
          {lines.map((html, i) => (
            <tr key={i}>
              <td className="w-10 select-none px-2 text-right align-top text-muted">{i + 1}</td>
              {/* The highlighter escapes the file's text; what is set here
                  is its own markup around it. */}
              <td className="whitespace-pre px-2 text-emphasis" dangerouslySetInnerHTML={{ __html: html }} />
            </tr>
          ))}
        </tbody>
      </table>
    </div>
  )
}
