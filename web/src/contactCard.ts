// The contacts console's words for a card, kept pure: what a contact is
// called, what its avatar says, what a search finds, and how its labels and
// days read.
import type { ContactSummary, Typed } from './contactsApi'
import { downloadName } from './fileName.ts'

/** What the card is called; the server already fell back to org or email. */
export const contactName = (c: Pick<ContactSummary, 'name'>) => c.name.trim() || 'No name'

/** Up to two letters for an avatar: "Jane Doe" → "JD", "acme" → "A". */
export function initials(name: string): string {
  const words = name
    .trim()
    .split(/[\s,.@_-]+/)
    .filter((w) => /\p{L}|\p{N}/u.test(w))
  if (words.length === 0) return ''
  const first = (w: string) => [...w.replace(/^[^\p{L}\p{N}]+/u, '')][0] ?? ''
  const letters = words.length === 1 ? first(words[0]) : first(words[0]) + first(words[words.length - 1])
  return letters.toLocaleUpperCase()
}

/**
 * Whether a contact answers a search: by name, organisation or email, and
 * by phone number whichever way its digits are spaced.
 */
export function contactMatches(c: ContactSummary, query: string): boolean {
  const needle = query.trim().toLocaleLowerCase()
  if (!needle) return true
  if ([c.name, c.org, c.email].some((v) => v?.toLocaleLowerCase().includes(needle))) return true
  const digits = needle.replace(/[^\d]/g, '')
  return digits.length >= 3 && /^[\d\s()+./-]+$/.test(needle) && (c.phone ?? '').replace(/[^\d]/g, '').includes(digits)
}

// vCard's words for what a value is for, as people say them. The server
// has already left out the ones that say nothing: INTERNET, VOICE, PREF.
const typeWords: Record<string, string> = { cell: 'mobile', iphone: 'mobile' }

/** A value's label: "work", "home fax", "mobile". */
export const typeLabel = (v: Typed): string =>
  [...new Set((v.types ?? []).map((t) => t.toLowerCase().replace(/^x-/, '')).map((t) => typeWords[t] ?? t))].join(' ')

/**
 * A birthday or anniversary to read: "1 May 1980", or "1 May" for a card
 * that keeps no year. Anything else is shown as written.
 */
export function cardDay(raw: string, locale?: string): string {
  let m = /^(\d{4})-(\d{2})-(\d{2})$/.exec(raw)
  if (m) {
    const d = new Date(Number(m[1]), Number(m[2]) - 1, Number(m[3]))
    if (d.getMonth() === Number(m[2]) - 1) return new Intl.DateTimeFormat(locale, { dateStyle: 'long' }).format(d)
  }
  m = /^--(\d{2})-?(\d{2})$/.exec(raw)
  if (m) {
    // A leap year, so 29 February has a day to be.
    const d = new Date(2000, Number(m[1]) - 1, Number(m[2]))
    if (d.getMonth() === Number(m[1]) - 1)
      return new Intl.DateTimeFormat(locale, { day: 'numeric', month: 'long' }).format(d)
  }
  return raw
}

/** A file name for a card's download: its name, else its key's last segment. */
export const vcardFileName = (name: string, key: string) =>
  downloadName(name.trim() || key.replace(/\/$/, '').split('/').pop() || '', 'contact', 'vcf')
