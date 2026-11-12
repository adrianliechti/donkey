/**
 * Renders a unified diff.
 *
 * The console shows this next to a failed deploy, where the reader is often
 * the person who wrote the app but not someone who reads patches for a
 * living. So it stays literal — real line numbers, added and removed lines
 * coloured, nothing folded away — because the only thing that makes a
 * proposed change to your own source trustworthy is being able to see all of
 * it at once.
 */
type DiffLine = { text: string; hunk: boolean; added: boolean; removed: boolean; left: number | ''; right: number | '' }

// Line numbers come from the hunk headers: @@ -12,7 +12,8 @@ — counted once,
// ahead of rendering.
function numbered(diff: string): DiffLine[] {
  let oldNo = 0
  let newNo = 0
  const out: DiffLine[] = []
  for (const text of diff.replace(/\n$/, '').split('\n')) {
    if (text.startsWith('---') || text.startsWith('+++')) continue
    const hunk = /^@@ -(\d+)(?:,\d+)? \+(\d+)(?:,\d+)? @@/.exec(text)
    if (hunk) {
      oldNo = Number(hunk[1])
      newNo = Number(hunk[2])
      out.push({ text, hunk: true, added: false, removed: false, left: '', right: '' })
      continue
    }
    const added = text.startsWith('+')
    const removed = text.startsWith('-')
    out.push({
      text,
      hunk: false,
      added,
      removed,
      left: removed || (!added && !removed) ? oldNo++ : '',
      right: added || (!added && !removed) ? newNo++ : '',
    })
  }
  return out
}

export function DiffView({ diff }: { diff: string }) {
  const lines = numbered(diff)

  return (
    <div className="mt-2 overflow-x-auto rounded-md bg-well">
      <table className="w-full border-collapse font-mono text-xs">
        <tbody>
          {lines.map((line, i) =>
            line.hunk ? (
              <tr key={i}>
                <td colSpan={3} className="px-2 py-1 text-muted select-none">
                  {line.text}
                </td>
              </tr>
            ) : (
              <tr
                key={i}
                className={
                  line.added
                    ? 'bg-success/10 text-success'
                    : line.removed
                      ? 'bg-danger/10 text-danger'
                      : 'text-ink-soft'
                }
              >
                <td className="w-10 select-none px-2 text-right text-muted">{line.left}</td>
                <td className="w-10 select-none px-2 text-right text-muted">{line.right}</td>
                <td className="whitespace-pre px-2">{line.text}</td>
              </tr>
            ),
          )}
        </tbody>
      </table>
    </div>
  )
}
