import { KeyRound, ShieldAlert } from 'lucide-react'
import type { OfferingMetadata } from './api'
import { isPolicyDenial, policyLines } from './policyVerdict'
import { Callout, Chip, ErrorNote } from './ui'

// An offering's policy facts — its tags and how its backend authorizes — as
// one line of chips. It sits at the end of the name line wherever an offering
// is listed, so a tagged service is no taller than an untagged one. Unknown
// authorization is no news and says nothing. On a tile (a hover `group`) the
// chips turn card-coloured under the hover tint, like its Mark, and the
// authorization chip shows only when the tile (an `@container`) has room for
// it beside the name — the tags are what a glance needs.
export function OfferingBadges({
  offering,
  tile = false,
  className = '',
}: {
  offering?: OfferingMetadata
  tile?: boolean
  className?: string
}) {
  const tags = offering?.tags ?? []
  const auth = offering?.authorization !== 'unknown' ? offering?.authorization : undefined
  if (tags.length === 0 && !auth) return null
  const chip = tile ? 'transition-colors duration-(--duration-fast) group-hover:bg-card' : ''
  return (
    <span className={`flex shrink-0 items-center gap-1 ${className}`}>
      {tags.map((tag) => (
        <Chip key={tag} title={tag} className={chip}>
          <span className="max-w-24 truncate">{tag}</span>
        </Chip>
      ))}
      {auth && (
        <span className={tile ? 'hidden @min-[17rem]:inline-flex' : 'inline-flex'}>
          <Chip title={`Backend authorization: ${auth}`} className={chip}>
            <KeyRound className="size-2.5" aria-hidden />
            {auth}
            <span className="sr-only"> authorization</span>
          </Chip>
        </span>
      )}
    </span>
  )
}

// A blocked choice in a form: what the space's service policy says against
// it, one denial per line under one heading.
export function PolicyNote({ message }: { message: string }) {
  if (!message) return null
  if (!isPolicyDenial(message)) return <ErrorNote message={message} />
  return (
    <Callout tone="danger" icon={ShieldAlert} title="The service policy does not allow this" role="alert">
      <ul className="mt-1 list-disc space-y-1 pl-4 marker:text-danger/60">
        {policyLines(message).map((line) => (
          <li key={line}>{line}</li>
        ))}
      </ul>
    </Callout>
  )
}
