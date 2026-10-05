import { useState } from 'react'
import { KeyRound, Plus, UserRound, Users, X } from 'lucide-react'
import type { AccessConfig } from './api'
import { Select } from './ui'

type Kind = 'users' | 'groups' | 'roles'
type Role = 'admin' | 'developer'
type Member = { kind: Kind; value: string; role: Role }
const kinds = ['users', 'groups', 'roles'] as const
const labels = { users: 'User', groups: 'Group', roles: 'IdP role' }
const icons = { users: UserRound, groups: Users, roles: KeyRound }
const field = 'min-w-0 rounded border border-line bg-surface px-2.5 py-1.5 text-xs focus:outline-2 focus:outline-accent-strong'

export function MembershipEditor({ admins, developers, onChange }: {
  admins: AccessConfig
  developers?: AccessConfig
  onChange: (admins: AccessConfig, developers: AccessConfig) => void
}) {
  const [kind, setKind] = useState<Kind>('users')
  const [value, setValue] = useState('')
  const [role, setRole] = useState<Role>(developers ? 'developer' : 'admin')
  const members: Member[] = kinds.flatMap((kind) => [
    ...(admins[kind] ?? []).map((value) => ({ kind, value, role: 'admin' as const })),
    ...(developers?.[kind] ?? []).map((value) => ({ kind, value, role: 'developer' as const })),
  ])

  function update(next: Member[]) {
    const admins: AccessConfig = {}
    const developers: AccessConfig = {}
    for (const member of next) {
      const target = member.role === 'admin' ? admins : developers
      target[member.kind] = [...(target[member.kind] ?? []), member.value]
    }
    onChange(admins, developers)
  }

  function add() {
    const entry = value.trim()
    if (!entry) return
    update([...members.filter((m) => m.kind !== kind || m.value !== entry), { kind, value: entry, role }])
    setValue('')
  }

  return (
    <div className="@container space-y-3">
      {members.length === 0 && <p className="rounded-md border border-dashed border-line px-3 py-3 text-xs text-muted">No members added. Platform admins have access.</p>}
      {members.length > 0 && <ul className="divide-y divide-line rounded-lg border border-line">
        {members.map((member, index) => {
          const Icon = icons[member.kind]
          return (
            <li key={`${member.kind}:${member.value}:${member.role}`} className="flex items-center gap-2.5 px-3 py-2.5">
              <Icon className="size-4 shrink-0 text-muted" aria-hidden />
              <div className="min-w-0 flex-1">
                <p className="truncate text-sm" title={member.value}>{member.value}</p>
                <p className="text-[11px] text-muted">{labels[member.kind]}</p>
              </div>
              {developers && <Select<Role> ariaLabel={`Role for ${member.value}`} value={member.role} size="sm"
                onChange={(role) => update(members.map((m, i) => i === index ? { ...m, role } : m))}
                options={[{ value: 'developer', label: 'Developer' }, { value: 'admin', label: 'Space admin' }]} />}
              <button type="button" onClick={() => update(members.filter((_, i) => i !== index))}
                aria-label={`Remove ${member.value}`} className="rounded p-1 text-muted hover:bg-raised hover:text-danger">
                <X className="size-3.5" aria-hidden />
              </button>
            </li>
          )
        })}
      </ul>}
      <div className={`grid grid-cols-[auto_minmax(0,1fr)] items-end gap-2 ${developers ? '@min-[34rem]:grid-cols-[auto_minmax(0,1fr)_auto_auto]' : '@min-[26rem]:grid-cols-[auto_minmax(0,1fr)_auto]'}`}>
        <label className="space-y-1 text-xs text-muted">Type
          <Select<Kind> ariaLabel="Member type" value={kind} onChange={setKind} size="sm"
            options={kinds.map((kind) => ({ value: kind, label: labels[kind] }))} />
        </label>
        <label className="min-w-0 space-y-1 text-xs text-muted">{labels[kind]}
          <input aria-label="Member identity" value={value} onChange={(e) => setValue(e.target.value)}
            placeholder={kind === 'users' ? 'name@example.com' : kind === 'groups' ? 'engineering' : 'Role from your identity provider'}
            autoCapitalize="none" spellCheck={false} className={`${field} w-full`}
            onKeyDown={(e) => { if (e.key === 'Enter') { e.preventDefault(); add() } }} />
        </label>
          {developers && <label className="space-y-1 text-xs text-muted">
            Role
            <Select<Role> ariaLabel="New member role" value={role} onChange={setRole} size="sm"
              options={[{ value: 'developer', label: 'Developer' }, { value: 'admin', label: 'Space admin' }]} />
          </label>}
          <button type="button" onClick={add} disabled={!value.trim()}
            className="flex min-h-8 items-center justify-end gap-1 rounded px-2 py-1 text-xs font-medium text-accent hover:bg-accent-soft disabled:opacity-40">
            <Plus className="size-3.5" aria-hidden /> Add member
          </button>
      </div>
      {developers && <p className="text-xs leading-relaxed text-muted">Space admins manage members and settings. Developers manage apps and services.</p>}
    </div>
  )
}
