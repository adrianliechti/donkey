import { useState } from 'react'
import { Plus, UserRound, Users, X } from 'lucide-react'
import type { AccessConfig } from './api'
import { IconButton, Input, List, ListRow, Select } from './ui'

type Kind = 'users' | 'groups'
type Role = 'admin' | 'developer' | 'viewer'
type Member = { kind: Kind; value: string; role: Role }
const kinds = ['users', 'groups'] as const
const labels = { users: 'User', groups: 'Group' }
const placeholders = { users: 'name@example.com', groups: 'engineering' }
// Without developers the list has one role, so no role column.
const icons = { users: UserRound, groups: Users }
const roleOptions: { value: Role; label: string }[] = [
  { value: 'viewer', label: 'Viewer' },
  { value: 'developer', label: 'Developer' },
  { value: 'admin', label: 'Space admin' },
]

/**
 * Who has access, as one list: each member with its kind and role, and an
 * add row at the bottom in the same shape — pick the kind, type the name,
 * pick the role, add. Roles are quiet dropdowns, so the list reads as names
 * first; removing is the row's hover action.
 */
export function MembershipEditor({
  admins,
  developers,
  viewers,
  onChange,
  emptyText = 'No members yet. Platform admins always have access.',
}: {
  admins: AccessConfig
  developers?: AccessConfig
  viewers?: AccessConfig
  onChange: (admins: AccessConfig, developers: AccessConfig, viewers: AccessConfig) => void
  /** What an empty list means where the editor is used. */
  emptyText?: string
}) {
  const [kind, setKind] = useState<Kind>('users')
  const [value, setValue] = useState('')
  const [role, setRole] = useState<Role>(developers ? 'viewer' : 'admin')
  const members: Member[] = kinds.flatMap((kind) => [
    ...(admins[kind] ?? []).map((value) => ({ kind, value, role: 'admin' as const })),
    ...(developers?.[kind] ?? []).map((value) => ({ kind, value, role: 'developer' as const })),
    ...(viewers?.[kind] ?? []).map((value) => ({ kind, value, role: 'viewer' as const })),
  ])

  function update(next: Member[]) {
    const admins: AccessConfig = {}
    const developers: AccessConfig = {}
    const viewers: AccessConfig = {}
    for (const member of next) {
      const target = member.role === 'admin' ? admins : member.role === 'developer' ? developers : viewers
      target[member.kind] = [...(target[member.kind] ?? []), member.value]
    }
    onChange(admins, developers, viewers)
  }

  function add() {
    const entry = value.trim()
    if (!entry) return
    update([...members.filter((m) => m.kind !== kind || m.value !== entry), { kind, value: entry, role }])
    setValue('')
  }

  const KindIcon = icons[kind]

  return (
    <div className="space-y-2">
      <List>
        {members.length === 0 && (
          <ListRow>
            <span className="text-sm text-muted">{emptyText}</span>
          </ListRow>
        )}
        {members.map((member, index) => {
          const Icon = icons[member.kind]
          return (
            <ListRow
              key={`${member.kind}:${member.value}:${member.role}`}
              actions={
                <IconButton
                  icon={X}
                  size="sm"
                  kind="danger"
                  label={`Remove ${member.value}`}
                  onClick={() => update(members.filter((_, i) => i !== index))}
                />
              }
            >
              {/* The icon says what kind of member this is; a tooltip spells it out. */}
              <Icon className="size-4 shrink-0 text-muted" aria-label={labels[member.kind]} />
              <span className="min-w-0 flex-1 truncate text-sm" title={`${member.value} · ${labels[member.kind]}`}>
                {member.value}
              </span>
              {developers && (
                <Select<Role>
                  variant="quiet"
                  ariaLabel={`Role for ${member.value}`}
                  value={member.role}
                  onChange={(role) => update(members.map((m, i) => (i === index ? { ...m, role } : m)))}
                  options={roleOptions}
                  className="w-32"
                />
              )}
            </ListRow>
          )
        })}
        {/* The add row: the same shape as a member row, before it exists. */}
        <ListRow
          className="bg-surface/40"
          actions={<IconButton icon={Plus} size="sm" label="Add member" onClick={add} disabled={!value.trim()} />}
        >
          <KindIcon className="size-4 shrink-0 text-muted" aria-hidden />
          <Select<Kind>
            variant="quiet"
            ariaLabel="Member type"
            value={kind}
            onChange={setKind}
            options={kinds.map((kind) => ({ value: kind, label: labels[kind] }))}
            className="w-20 shrink-0"
          />
          <Input
            bare
            aria-label="Member identity"
            value={value}
            onChange={(e) => setValue(e.target.value)}
            placeholder={placeholders[kind]}
            autoCapitalize="none"
            spellCheck={false}
            className="flex-1 font-sans"
            onKeyDown={(e) => {
              if (e.key === 'Enter') {
                e.preventDefault()
                add()
              }
            }}
          />
          {developers && (
            <Select<Role>
              variant="quiet"
              ariaLabel="New member role"
              value={role}
              onChange={setRole}
              options={roleOptions}
              className="w-32"
            />
          )}
        </ListRow>
      </List>
      {developers && (
        <p className="text-xs leading-relaxed text-muted">
          Space admins manage members and settings. Developers manage apps and services. Viewers open apps and see
          health and metrics; logs, build history and settings stay private.
        </p>
      )}
    </div>
  )
}
