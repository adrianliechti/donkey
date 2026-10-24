// The SQL consoles' plain helpers — no React, no requests — so the parts
// that are easy to get subtly wrong (where a statement ends, how a CSV field
// is quoted) are tested on their own (test/sqlText.test.mjs).

export type SqlDialect = 'postgres' | 'mysql' | 'trino'

export type SqlCell = string | null

/** A statement's place in the editor's text: [start, end), no semicolon. */
export type StatementRange = { start: number; end: number }

const identChar = /[A-Za-z0-9_$]/

/**
 * Where the statements of a script are. Semicolons inside strings, quoted
 * names, comments and postgres' dollar quotes do not end anything. Ranges
 * are trimmed of the whitespace around them; stretches that are only
 * comments are not statements.
 */
export function statementRanges(text: string, dialect: SqlDialect): StatementRange[] {
  const out: StatementRange[] = []
  const n = text.length
  let first = -1 // the statement's first character that is not whitespace
  let last = -1 // just past its last one
  let code = false // whether it holds anything but comments
  const close = () => {
    if (code) out.push({ start: first, end: last })
    first = last = -1
    code = false
  }
  const mark = (from: number, to: number, isCode: boolean) => {
    if (first < 0) first = from
    last = to
    if (isCode) code = true
  }

  let i = 0
  while (i < n) {
    const c = text[i]
    const next = text[i + 1]
    if (c === ';') {
      close()
      i++
    } else if (/\s/.test(c)) {
      i++
    } else if ((c === '-' && next === '-') || (c === '#' && dialect === 'mysql')) {
      const end = text.indexOf('\n', i)
      const stop = end < 0 ? n : end
      mark(i, stop, false)
      i = stop
    } else if (c === '/' && next === '*') {
      // Postgres nests block comments; MySQL and Trino end at the first */.
      let depth = 1
      let j = i + 2
      while (j < n && depth > 0) {
        if (text[j] === '*' && text[j + 1] === '/') {
          depth--
          j += 2
        } else if (dialect === 'postgres' && text[j] === '/' && text[j + 1] === '*') {
          depth++
          j += 2
        } else j++
      }
      mark(i, j, false)
      i = j
    } else if (c === "'" || c === '"' || (c === '`' && dialect === 'mysql')) {
      // MySQL reads backslash escapes in both string quotes; postgres only
      // in E'…' strings. Doubling the quote escapes it everywhere.
      const prefix = text[i - 1]
      const escapes =
        c !== '`' &&
        (dialect === 'mysql' || (c === "'" && (prefix === 'E' || prefix === 'e') && !identChar.test(text[i - 2] ?? '')))
      let j = i + 1
      while (j < n) {
        if (escapes && text[j] === '\\') j += 2
        else if (text[j] === c && text[j + 1] === c) j += 2
        else if (text[j] === c) break
        else j++
      }
      j = Math.min(j + 1, n)
      mark(i, j, true)
      i = j
    } else if (c === '$' && dialect === 'postgres' && !identChar.test(text[i - 1] ?? '')) {
      const tag = /^\$(?:[A-Za-z_][A-Za-z0-9_]*)?\$/.exec(text.slice(i, i + 64))?.[0]
      if (tag) {
        const end = text.indexOf(tag, i + tag.length)
        const stop = end < 0 ? n : end + tag.length
        mark(i, stop, true)
        i = stop
      } else {
        mark(i, i + 1, true)
        i++
      }
    } else {
      mark(i, i + 1, true)
      i++
    }
  }
  close()
  return out
}

/** The statements in a stretch of text, in order, without their semicolons. */
export function statements(text: string, dialect: SqlDialect): string[] {
  return statementRanges(text, dialect).map((r) => text.slice(r.start, r.end))
}

/**
 * The statement the cursor is in — or, between statements, the one it just
 * left, which is where a cursor sits after typing a semicolon.
 */
export function statementAt(text: string, cursor: number, dialect: SqlDialect): StatementRange | null {
  const ranges = statementRanges(text, dialect)
  let found: StatementRange | null = null
  for (const r of ranges) {
    if (r.start <= cursor) found = r
    else break
  }
  return found ?? ranges[0] ?? null
}

/**
 * CSV as spreadsheets read it (RFC 4180): fields with a comma, quote, line
 * break or edge whitespace are quoted, quotes doubled. Null is an empty
 * field — CSV has no other way to say it.
 */
export function toCsv(columns: string[], rows: SqlCell[][]): string {
  const field = (v: SqlCell) => {
    if (v === null) return ''
    return /[",\r\n]|^\s|\s$/.test(v) ? `"${v.replaceAll('"', '""')}"` : v
  }
  return [columns, ...rows].map((row) => row.map(field).join(',')).join('\r\n') + '\r\n'
}

/** The value as JSON, when it is a JSON object or array; undefined otherwise. */
export function jsonOf(value: SqlCell): unknown {
  if (value === null) return undefined
  const v = value.trim()
  if (!(v.startsWith('{') && v.endsWith('}')) && !(v.startsWith('[') && v.endsWith(']'))) return undefined
  try {
    return JSON.parse(v)
  } catch {
    return undefined
  }
}

/** A value too long or too structured to edit in a one-line cell. */
export function isLongValue(value: SqlCell): boolean {
  return value !== null && (value.length > 120 || value.includes('\n') || jsonOf(value) !== undefined)
}

const numericType =
  /^((tiny|small|medium|big)?int(eger)?\d*\b|(small|big)?serial\d*\b|numeric|decimal|dec\b|real\b|double|float|money\b|number\b)/i

/** Numbers line up on the right, as in every spreadsheet. */
export function isNumericType(type?: string): boolean {
  return !!type && numericType.test(type.trim())
}

/**
 * A first width for a column, from its name, its type and what it holds:
 * the grid opens readable, and every column can still be dragged wider.
 */
export function columnWidth(name: string, type: string | undefined, values: SqlCell[]): number {
  let chars = Math.max(name.length + 3, (type ?? '').length)
  for (const v of values.slice(0, 60)) {
    if (v !== null) chars = Math.max(chars, Math.min(v.length, 48))
  }
  return Math.round(Math.min(Math.max(chars * 7.4 + 24, 64), 380))
}

/** Statements that only read leave every cached table as it was. */
export function onlyReads(sql: string): boolean {
  const stripped = sql.replace(/^(\s|--[^\n]*\n?|\/\*[\s\S]*?\*\/)+/, '')
  return /^(select|show|explain|describe|desc|values|table)\b/i.test(stripped) && !/\binto\b/i.test(stripped)
}

export function plural(n: number, one: string, many = `${one}s`): string {
  return `${n.toLocaleString()} ${n === 1 ? one : many}`
}
