import { useId, useState } from 'react'
import { Check, ChevronDown, Settings2 } from 'lucide-react'
import { serviceUI } from './serviceProtocol'
import { type ParametersSchema, type ServiceParameters, type SpaceOffering } from './api'
import { ParameterField } from './AddonForm'
import { schemaFields } from './parameters'
import { useCatalog } from './queries'
import { ErrorNote, Spinner } from './ui'

function OfferingValues({ schema, values, onChange }: {
  schema?: ParametersSchema
  values: ServiceParameters
  onChange: (values: ServiceParameters) => void
}) {
  const fields = schemaFields(schema)
  if (!fields.length) return null
  return <div className="space-y-3">
    {fields.map((field) => {
      const set = Object.hasOwn(values, field.name)
      const required = schema?.required?.includes(field.name)
      return <div key={field.name} className="flex items-start gap-3">
        <input type="checkbox" aria-label={`Set ${field.schema.title || field.name}`} checked={set}
          className="mt-1 size-4 shrink-0 accent-accent"
          onChange={(e) => {
            const next = { ...values }
            if (e.target.checked) next[field.name] = field.schema.default ?? (field.schema.type === 'boolean' ? false : '')
            else delete next[field.name]
            onChange(next)
          }} />
        {set ? <ParameterField field={field} value={values[field.name]} disabled={false}
          onChange={(value) => onChange({ ...values, [field.name]: value })} />
          : <div className="text-xs text-muted">
            <span className="font-medium">{field.schema.title || field.name}</span>
            <span className="ml-2">{field.schema.default !== undefined ? `Default: ${String(field.schema.default)}` : required ? 'Required at creation' : 'Optional'}</span>
          </div>}
      </div>
    })}
  </div>
}

export function OfferingSettings({ offerings = {}, onChange }: {
  offerings?: Record<string, SpaceOffering>
  onChange: (offerings: Record<string, SpaceOffering>) => void
}) {
  const { data: catalog, error } = useCatalog()
  const [editing, setEditing] = useState<string | null>(null)
  const panelId = useId()
  function update(id: string, entry?: SpaceOffering) {
    const next = { ...offerings }
    if (entry) next[id] = entry
    else delete next[id]
    onChange(next)
  }
  const ids = [...new Set([...(catalog ?? []).map((type) => type.type), ...Object.keys(offerings)])]
  return <section aria-label="Offerings" className="space-y-3">
    <div>
      <h3 className="text-sm font-medium">Service offerings</h3>
      <p className="mt-1 text-xs text-muted">Common services are always available. Enable others here and configure space defaults.</p>
    </div>
    <ErrorNote message={error} />
    {!catalog && !error && <Spinner label="Loading offerings…" />}
    <div className="divide-y divide-line rounded-lg border border-line">
      {ids.map((id) => {
        const type = catalog?.find((item) => item.type === id)
        const entry = offerings[id]
        const enabled = type?.common || !!entry
        const configurable = !!schemaFields(type?.spaceConfiguration).length || !!schemaFields(type?.parameters).length
        const { Icon } = serviceUI(type?.protocol)
        const expanded = editing === id && enabled
        return <div key={id}>
          <div className="flex flex-wrap items-center gap-x-3 gap-y-2 px-3 py-2.5">
            <Icon className="size-4 shrink-0 text-muted" aria-hidden />
            <div className="min-w-0 flex-1 basis-28">
              <p className="text-xs font-medium">{type?.title || id}</p>
              {type?.description && <p className="mt-0.5 truncate text-[11px] text-muted" title={type.description}>{type.description}</p>}
              {!type && <p className="mt-0.5 text-[11px] text-muted">Unavailable · saved settings kept</p>}
            </div>
            {type?.common ? <span title="Available in every space" className="inline-flex items-center gap-1 text-[11px] text-muted">
              <Check className="size-3 text-success" aria-hidden /> Common
            </span> : <label className="flex items-center gap-1.5 text-[11px] text-muted">
              <input type="checkbox" checked={!!entry} aria-label={`Enable ${type?.title || id}`}
                onChange={(e) => { update(id, e.target.checked ? {} : undefined); if (!e.target.checked && editing === id) setEditing(null) }} className="size-3.5 accent-accent" />
              Enabled
            </label>}
            {configurable && <button type="button" disabled={!enabled} aria-label={`Configure ${type?.title || id}`}
              aria-expanded={expanded} aria-controls={`${panelId}-${id}`} onClick={() => setEditing(expanded ? null : id)}
              className="flex items-center gap-1 rounded px-1.5 py-1 text-[11px] text-muted hover:bg-raised hover:text-ink disabled:opacity-30">
              <Settings2 className="size-3" aria-hidden /> Configure
              <ChevronDown className={`size-3 transition-transform ${expanded ? 'rotate-180' : ''}`} aria-hidden />
            </button>}
          </div>
          {expanded && configurable && <div id={`${panelId}-${id}`} className="space-y-4 border-t border-line bg-surface/50 p-3">
            {!!schemaFields(type?.spaceConfiguration).length && <div className="space-y-2">
              <h4 className="text-xs font-medium">Admin configuration</h4>
              <p className="text-[11px] text-muted">Required before creating a service. Apps cannot override these values.</p>
              <OfferingValues schema={type?.spaceConfiguration} values={entry?.config ?? {}}
                onChange={(config) => update(id, { ...entry, config })} />
            </div>}
            {!!schemaFields(type?.parameters).length && <div className="space-y-2">
              <h4 className="text-xs font-medium">Defaults for new services</h4>
              <p className="text-[11px] text-muted">Apps can override these values. Existing services keep their parameters.</p>
              <OfferingValues schema={type?.parameters} values={entry?.defaults ?? {}}
                onChange={(defaults) => update(id, { ...entry, defaults })} />
            </div>}
            {type?.common && entry && <button type="button" onClick={() => { update(id); setEditing(null) }}
              className="text-[11px] text-muted hover:text-ink">Reset to common defaults</button>}
          </div>}
        </div>
      })}
    </div>
  </section>
}
