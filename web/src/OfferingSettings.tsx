import { useState } from 'react'
import { ChevronDown } from 'lucide-react'
import { serviceUI } from './serviceProtocol'
import { type ParametersSchema, type ServiceParameters, type SpaceOffering } from './api'
import { ParameterField } from './AddonForm'
import { schemaFields } from './parameters'
import { useCatalog } from './queries'
import { ErrorNote, Eyebrow, IconButton, List, SectionHeader, Spinner, Toggle } from './ui'

/**
 * The values an offering is pinned to for this space. A field is either
 * left to its default (one line: name, default, a link to change it) or set
 * (its control, and a way back to the default).
 */
function OfferingValues({
  schema,
  values,
  onChange,
}: {
  schema?: ParametersSchema
  values: ServiceParameters
  onChange: (values: ServiceParameters) => void
}) {
  const fields = schemaFields(schema)
  if (!fields.length) return null
  return (
    <ul className="divide-y divide-line/60">
      {fields.map((field) => {
        const set = Object.hasOwn(values, field.name)
        const required = schema?.required?.includes(field.name)
        const title = field.schema.title || field.name
        return (
          <li key={field.name} className="flex min-h-10 flex-wrap items-center gap-x-3 gap-y-2 py-2">
            {set ? (
              <>
                <ParameterField
                  field={field}
                  value={values[field.name]}
                  disabled={false}
                  onChange={(value) => onChange({ ...values, [field.name]: value })}
                />
                <button
                  type="button"
                  onClick={() => {
                    const next = { ...values }
                    delete next[field.name]
                    onChange(next)
                  }}
                  className="self-end pb-2 text-xs text-muted underline-offset-2 hover:text-ink hover:underline"
                >
                  Use default
                </button>
              </>
            ) : (
              <>
                <span className="min-w-0 flex-1 text-sm">
                  <span className="text-ink">{title}</span>
                  <span className="ml-2 text-xs text-muted">
                    {field.schema.default !== undefined
                      ? `default ${String(field.schema.default)}`
                      : required
                        ? 'required at creation'
                        : 'optional'}
                  </span>
                </span>
                <button
                  type="button"
                  onClick={() =>
                    onChange({
                      ...values,
                      [field.name]: field.schema.default ?? (field.schema.type === 'boolean' ? false : ''),
                    })
                  }
                  className="text-xs text-muted underline-offset-2 hover:text-ink hover:underline"
                >
                  Change
                </button>
              </>
            )}
          </li>
        )
      })}
    </ul>
  )
}

/**
 * Which services this space can create, and with what defaults. Common
 * services are always on; the others switch on per space. A row expands to
 * its configuration when it has any.
 */
export function OfferingSettings({
  offerings = {},
  onChange,
}: {
  offerings?: Record<string, SpaceOffering>
  onChange: (offerings: Record<string, SpaceOffering>) => void
}) {
  const { data: catalog, error } = useCatalog()
  const [editing, setEditing] = useState<string | null>(null)
  function update(id: string, entry?: SpaceOffering) {
    const next = { ...offerings }
    if (entry) next[id] = entry
    else delete next[id]
    onChange(next)
  }
  const ids = [...new Set([...(catalog ?? []).map((type) => type.type), ...Object.keys(offerings)])]
  return (
    <section aria-label="Offerings" className="space-y-3">
      <SectionHeader
        title="Service offerings"
        description="Common services are always available. Switch others on for this space and pin defaults for new services."
      />
      <ErrorNote message={error} />
      {!catalog && !error && <Spinner label="Loading offerings…" />}
      <List>
        {ids.map((id) => {
          const type = catalog?.find((item) => item.type === id)
          const entry = offerings[id]
          const enabled = type?.common || !!entry
          const adminFields = schemaFields(type?.spaceConfiguration)
          const defaultFields = schemaFields(type?.parameters)
          const configurable = adminFields.length > 0 || defaultFields.length > 0
          const { Icon } = serviceUI(type?.protocol)
          const expanded = editing === id && enabled
          return (
            <li key={id}>
              <div className="flex min-h-11 items-center gap-3 py-2 pl-3 pr-1.5">
                <Icon className={`size-4 shrink-0 ${enabled ? 'text-ink-soft' : 'text-muted/60'}`} aria-hidden />
                <div className="min-w-0 flex-1">
                  <span className={`text-sm ${enabled ? 'text-ink' : 'text-muted'}`}>{type?.title || id}</span>
                  {type?.description && (
                    <span className="ml-2 hidden text-xs text-muted sm:inline">{type.description}</span>
                  )}
                  {!type && <span className="ml-2 text-xs text-muted">unavailable · saved settings kept</span>}
                </div>
                {type?.common ? (
                  <span className="text-xs text-muted">included</span>
                ) : (
                  <Toggle
                    size="sm"
                    label={`Enable ${type?.title || id}`}
                    checked={!!entry}
                    onChange={(on) => {
                      update(id, on ? {} : undefined)
                      if (!on && editing === id) setEditing(null)
                    }}
                  />
                )}
                <IconButton
                  icon={ChevronDown}
                  size="sm"
                  label={`Configure ${type?.title || id}`}
                  disabled={!enabled || !configurable}
                  active={expanded}
                  onClick={() => setEditing(expanded ? null : id)}
                  className={`${configurable ? '' : 'invisible'} [&>svg]:transition-transform ${expanded ? '[&>svg]:rotate-180' : ''}`}
                />
              </div>
              {expanded && configurable && (
                <div className="space-y-4 border-t border-line/60 bg-surface/40 px-3 py-3 pl-10">
                  {adminFields.length > 0 && (
                    <div>
                      <Eyebrow>Admin configuration</Eyebrow>
                      <p className="mt-0.5 text-xs text-muted">
                        Required before creating a service. Apps cannot override these values.
                      </p>
                      <OfferingValues
                        schema={type?.spaceConfiguration}
                        values={entry?.config ?? {}}
                        onChange={(config) => update(id, { ...entry, config })}
                      />
                    </div>
                  )}
                  {defaultFields.length > 0 && (
                    <div>
                      <Eyebrow>Defaults for new services</Eyebrow>
                      <p className="mt-0.5 text-xs text-muted">
                        Apps can override these values. Existing services keep their parameters.
                      </p>
                      <OfferingValues
                        schema={type?.parameters}
                        values={entry?.defaults ?? {}}
                        onChange={(defaults) => update(id, { ...entry, defaults })}
                      />
                    </div>
                  )}
                  {type?.common && entry && (
                    <button
                      type="button"
                      onClick={() => {
                        update(id)
                        setEditing(null)
                      }}
                      className="text-xs text-muted underline-offset-2 hover:text-ink hover:underline"
                    >
                      Reset to common defaults
                    </button>
                  )}
                </div>
              )}
            </li>
          )
        })}
      </List>
    </section>
  )
}
