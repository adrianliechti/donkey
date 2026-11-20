// A download's name, from what the thing is called.

/** `name.ext`, without the characters file systems refuse; `fallback` when nothing is left. */
export function downloadName(name: string, fallback: string, ext: string): string {
  const base = name
    .replace(new RegExp(`\\.${ext}$`, 'i'), '')
    .replace(/[\\/:*?"<>|\p{Cc}]+/gu, ' ')
    .replace(/\s+/g, ' ')
    .trim()
  return `${base || fallback}.${ext}`
}
