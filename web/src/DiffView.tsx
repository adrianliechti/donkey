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
export function DiffView({ diff }: { diff: string }) {
  const lines = diff.replace(/\n$/, '').split('\n')

  // Line numbers come from the hunk headers: @@ -12,7 +12,8 @@
  let oldNo = 0
  let newNo = 0

  return (
    <div className="mt-2 overflow-x-auto rounded-md border border-line bg-canvas">
      <table className="w-full border-collapse font-mono text-xs">
        <tbody>
          {lines.map((line, i) => {
            if (line.startsWith('---') || line.startsWith('+++')) return null

            const hunk = /^@@ -(\d+)(?:,\d+)? \+(\d+)(?:,\d+)? @@/.exec(line)
            if (hunk) {
              oldNo = Number(hunk[1])
              newNo = Number(hunk[2])
              return (
                <tr key={i}>
                  <td colSpan={3} className="px-2 py-1 text-stone select-none">
                    {line}
                  </td>
                </tr>
              )
            }

            const added = line.startsWith('+')
            const removed = line.startsWith('-')
            const left = removed || (!added && !removed) ? oldNo++ : ''
            const right = added || (!added && !removed) ? newNo++ : ''

            return (
              <tr
                key={i}
                className={
                  added ? 'bg-moss/10 text-moss' : removed ? 'bg-brick/10 text-brick' : 'text-ink-soft'
                }
              >
                <td className="w-10 select-none px-2 text-right text-stone">{left}</td>
                <td className="w-10 select-none px-2 text-right text-stone">{right}</td>
                <td className="whitespace-pre px-2">{line}</td>
              </tr>
            )
          })}
        </tbody>
      </table>
    </div>
  )
}
