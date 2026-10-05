import { Blocks } from 'lucide-react'
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
  function update(id: string, entry?: SpaceOffering) {
    const next = { ...offerings }
    if (entry) next[id] = entry
    else delete next[id]
    onChange(next)
  }
  const ids = [...new Set([...(catalog ?? []).map((type) => type.type), ...Object.keys(offerings)])]
  return <section aria-label="Offerings" className="space-y-3">
    <h3 className="flex items-center gap-2 text-sm font-medium"><Blocks className="size-4 text-muted" aria-hidden /> Offerings</h3>
    <p className="text-xs text-muted">Common offerings are available in every space. Add others here, set defaults for new services, and configure settings that only admins can change.</p>
    <ErrorNote message={error} />
    {!catalog && !error && <Spinner label="Loading offerings…" />}
    <div className="space-y-2">
      {ids.map((id) => {
        const type = catalog?.find((item) => item.type === id)
        const entry = offerings[id]
        return <div key={id} className="rounded-lg border border-line p-3">
          <label className="flex items-center gap-2 text-sm">
            <input type="checkbox" checked={!!entry} aria-label={type?.common ? `Customize ${type.title}` : `Enable ${type?.title || id}`}
              onChange={(e) => update(id, e.target.checked ? {} : undefined)} className="size-4 accent-accent" />
            <span className="flex-1 font-medium">{type?.title || id}</span>
            <span className="text-xs text-muted">{type?.common ? 'Common · customize' : 'Enable in this space'}</span>
          </label>
          {entry && <div className="mt-3 space-y-3 border-t border-line pt-3">
            {!type && <p className="text-xs text-muted">This offering is unavailable. Saved settings will be kept.</p>}
            {schemaFields(type?.spaceConfiguration).length > 0 && <details open>
              <summary className="mb-3 cursor-pointer text-xs font-medium">Admin configuration</summary>
              <p className="mb-3 text-xs text-muted">Required values must be set before creating a service. Apps cannot override these settings.</p>
              <OfferingValues schema={type?.spaceConfiguration} values={entry.config ?? {}}
                onChange={(config) => update(id, { ...entry, config })} />
            </details>}
            {schemaFields(type?.parameters).length > 0 && <details>
              <summary className="cursor-pointer text-xs font-medium">Parameter defaults</summary>
              <p className="my-3 text-xs text-muted">Apps can override these values. Existing services keep their saved parameters.</p>
              <OfferingValues schema={type?.parameters} values={entry.defaults ?? {}}
                onChange={(defaults) => update(id, { ...entry, defaults })} />
            </details>}
            {type && !schemaFields(type.parameters).length && !schemaFields(type.spaceConfiguration).length && <p className="text-xs text-muted">No settings needed.</p>}
          </div>}
        </div>
      })}
    </div>
  </section>
}
