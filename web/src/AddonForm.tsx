import { useState, type ReactNode } from 'react'
import { ArrowLeft, ChevronDown, ChevronRight } from 'lucide-react'
import { type AddAddonRequest, type AddonType, type ServiceParameters } from './api'
import { Modal, ModalBody, ModalFooter, ModalHeader, ModalNotice } from './dialog'
import { parameterDefault, parameterFields, type Field } from './parameters'
import { useCatalog } from './queries'
import { serviceUI } from './serviceProtocol'
import { Button, Chip, Eyebrow, ErrorNote, Input, Select, Spinner } from './ui'

// A short ladder of disk sizes is easier to pick right than a free-text
// field is to fill right; the type's own default always leads the list.
const diskSizes = ['1Gi', '2Gi', '5Gi', '10Gi', '20Gi', '50Gi']

/**
 * One control per schema property. The value is a string while it is being
 * edited (the server coerces it against the schema); booleans are the one
 * exception, since a checkbox has nothing to type.
 */
export function ParameterField({
  field,
  value,
  onChange,
  disabled,
}: {
  field: Field
  value: unknown
  onChange: (value: unknown) => void
  disabled: boolean
}) {
  const { name, schema } = field
  const label = schema.title || name
  const fallback = schema.default === undefined || schema.default === null ? '' : String(schema.default)
  const current = value === undefined || value === null ? fallback : String(value)
  let control: ReactNode
  if (schema.enum && schema.enum.length > 0) {
    const options = schema.enum.map((v) => ({ value: String(v), label: String(v) }))
    control = <Select ariaLabel={label} value={current} onChange={onChange} options={options} />
  } else if (schema['x-donkey-widget'] === 'disk-size' || schema.format === 'quantity') {
    const options = [...new Set([fallback, current, ...diskSizes])]
      .filter(Boolean)
      .map((s) => ({ value: s, label: s }))
    control = <Select ariaLabel={label} value={current} onChange={onChange} options={options} />
  } else if (schema.type === 'boolean') {
    const checked = value === undefined || value === null ? Boolean(schema.default) : Boolean(value)
    control = (
      <input
        type="checkbox"
        aria-label={label}
        checked={checked}
        disabled={disabled}
        onChange={(e) => onChange(e.target.checked)}
        className="size-4"
      />
    )
  } else if (schema.type === 'integer' || schema.type === 'number') {
    control = (
      <Input
        type="number"
        aria-label={label}
        value={current}
        min={schema.minimum}
        max={schema.maximum}
        step={schema.type === 'integer' ? 1 : 'any'}
        disabled={disabled}
        onChange={(e) => onChange(e.target.value)}
        className="w-28"
      />
    )
  } else {
    // Strings, and arrays as comma-separated strings.
    control = (
      <Input
        aria-label={label}
        value={current}
        placeholder={schema.type === 'array' ? 'one, two' : fallback}
        disabled={disabled}
        onChange={(e) => onChange(e.target.value)}
        className="w-44"
      />
    )
  }
  return (
    <label className="flex min-w-0 flex-col gap-1.5" title={schema.description}>
      <Eyebrow>{label}</Eyebrow>
      {schema.type === 'boolean' ? <span className="flex h-8 items-center">{control}</span> : control}
      {schema.description && <span className="max-w-56 text-[11px] leading-4 text-muted">{schema.description}</span>}
    </label>
  )
}

/**
 * The add-service popup. Two callers open it: a space's + menu, which already
 * picked the type (initialType) and lands straight on its configuration, and
 * the app page, which opens the small catalog first. Closing is blocked while
 * a request runs: provisioning holds the connection open, and an abandoned
 * dialog would look like nothing happened.
 */
export function AddServiceDialog({
  space,
  open,
  title,
  submitLabel,
  busy,
  error,
  note,
  initialType,
  onSubmit,
  onClose,
}: {
  space: string
  open: boolean
  title: string
  submitLabel: string
  busy: boolean
  error?: string
  note?: string
  /** When set, the dialog opens straight on that service's configuration —
   *  the caller (a space's + menu) already chose the type, so the catalog
   *  step is skipped. */
  initialType?: string
  onSubmit: (req: AddAddonRequest) => void
  onClose: () => void
}) {
  return (
    <Modal open={open} onClose={() => !busy && onClose()} size="md">
      <ModalHeader eyebrow={`Add a service · ${space}`} title={title} onClose={onClose} closeDisabled={busy} />
      <ModalNotice message={error ?? ''} />
      {/* The form owns its footer: its buttons depend on which step it is on. */}
      <AddonForm
        space={space}
        busy={busy}
        note={note}
        submitLabel={submitLabel}
        initialType={initialType}
        onSubmit={onSubmit}
        onCancel={onClose}
      />
    </Modal>
  )
}

/**
 * Configure a service before it is created. The form is the type's own
 * parameter schema rendered: every built-in and every plugin describes what
 * can be chosen the same way, so nothing here knows about disks or versions
 * in particular. The contract strip shows the variable names its apps will
 * receive. (Attaching an external service by address is a CLI-only path —
 * `donkey addon attach`.)
 */
export function AddonForm({
  space,
  busy,
  note,
  submitLabel,
  initialType,
  onSubmit,
  onCancel,
}: {
  space: string
  busy: boolean
  note?: string
  submitLabel: string
  initialType?: string
  onSubmit: (req: AddAddonRequest) => void
  onCancel: () => void
}) {
  const { data: catalog, error: catalogError } = useCatalog(space)
  const types = catalog ?? []
  // An errored fetch counts as loaded — the empty-catalog message says what
  // to do, a spinner would just spin.
  const loaded = catalog !== null || catalogError !== ''
  // '' means the catalog step; a type name means its configuration step.
  const [type, setType] = useState(initialType ?? '')
  const [name, setName] = useState('')
  const [values, setValues] = useState<ServiceParameters>({})
  const [showAdvanced, setShowAdvanced] = useState(false)

  const chosen = types.find((t) => t.type === type)
  const { main, advanced } = parameterFields(chosen)

  if (!loaded) return <ModalBody><Spinner label="Opening the catalog…" /></ModalBody>
  if (types.length === 0) {
    return (
      <>
        <ModalBody>
          <p className="py-6 text-center text-sm text-muted">
            {catalogError || 'No offerings are enabled in this space. Ask a space admin to add one in space settings.'}
          </p>
        </ModalBody>
        <ModalFooter>
          <Button kind="quiet" onClick={onCancel}>Close</Button>
        </ModalFooter>
      </>
    )
  }

  // Step 1 — the catalog, shown only when the caller didn't preselect a type
  // (the app page's "add a service"). A space's + menu skips straight past it.
  // Entries are grouped by category, in the server's order, so a catalog of
  // many providers still reads as one.
  if (!chosen) {
    const categories: { name: string; entries: AddonType[] }[] = []
    for (const t of types) {
      const name = t.category || 'other'
      const group = categories.find((c) => c.name === name)
      if (group) group.entries.push(t)
      else categories.push({ name, entries: [t] })
    }
    return (
      <>
      <ModalBody className="space-y-4">
        {categories.map((group) => (
          <div key={group.name}>
            {categories.length > 1 && <Eyebrow className="mb-1.5">{group.name}</Eyebrow>}
            <div className="divide-y divide-line overflow-hidden rounded-lg border border-line">
              {group.entries.map((t) => {
                const { Icon } = serviceUI(t.icon || t.protocol)
                const available = t.available !== false
                return (
                  <button
                    key={t.type}
                    type="button"
                    disabled={!available}
                    title={available ? undefined : t.error}
                    onClick={() => {
                      setValues({})
                      setType(t.type)
                    }}
                    className="flex w-full items-start gap-3 px-3 py-2.5 text-left transition-colors hover:bg-accent-soft/60 focus-visible:outline-2 focus-visible:-outline-offset-2 focus-visible:outline-accent-strong disabled:cursor-not-allowed disabled:opacity-50 disabled:hover:bg-transparent"
                  >
                    <Icon className="mt-0.5 size-4 shrink-0 text-muted" aria-hidden />
                    <span className="min-w-0 flex-1">
                      <span className="flex items-baseline gap-2">
                        <span className="font-mono text-sm font-bold">{t.title || t.type}</span>
                        <span className="font-mono text-[11px] text-muted">{parameterDefault(t, 'version')}</span>
                        {t.provider && <span className="ml-auto font-mono text-[10px] text-muted">via {t.provider}</span>}
                      </span>
                      {t.description && (
                        <span className="mt-0.5 block text-xs leading-relaxed text-ink-soft">{t.description}</span>
                      )}
                      {!available && <span className="mt-1 block text-xs text-danger">{t.error || 'Not available right now.'}</span>}
                    </span>
                    <ChevronRight className="mt-2 size-4 shrink-0 text-muted" aria-hidden />
                  </button>
                )
              })}
            </div>
          </div>
        ))}
      </ModalBody>
      <ModalFooter>
        <Button kind="quiet" onClick={onCancel}>
          Cancel
        </Button>
      </ModalFooter>
      </>
    )
  }

  // Step 2 — configure the chosen service: a name, and whatever its schema
  // offers. Values the person did not touch are left to the server's
  // defaults, so the request says only what was chosen.
  const { Icon: ChosenIcon } = serviceUI(chosen.icon || chosen.protocol)
  const setValue = (field: Field, value: unknown) => setValues((v) => ({ ...v, [field.name]: value }))
  const runsSomething = Boolean(chosen.parameters?.properties?.version)
  const disk = String(values.storage ?? parameterDefault(chosen, 'storage'))
  const envNames = (chosen.env ?? []).map((v) => v.name)
  const keys = envNames.length > 0 ? envNames : (chosen.keys ?? [])

  return (
    <>
    <ModalBody className="space-y-4">
      {/* The catalog caller can step back; the + menu caller opened on this
          type, and the dialog title already names it. */}
      {!initialType && (
        <div className="flex items-center gap-2 text-sm">
          <Button kind="quiet" size="sm" icon={ArrowLeft} onClick={() => setType('')} disabled={busy}>
            All services
          </Button>
          <ChosenIcon className="size-4 text-muted" aria-hidden />
          <span className="font-mono font-semibold">{chosen.title || chosen.type}</span>
          <span className="font-mono text-xs text-muted">{parameterDefault(chosen, 'version')}</span>
        </div>
      )}

      {chosen.available === false && <ErrorNote message={chosen.error || 'This service is not available right now.'} />}

      <div className="flex flex-wrap items-start gap-3">
        <label className="flex min-w-0 flex-col gap-1.5">
          <Eyebrow>Name · optional</Eyebrow>
          <Input
            value={name}
            onChange={(e) => setName(e.target.value)}
            placeholder={chosen.type}
            className="w-44"
          />
        </label>
        {main.map((field) => (
          <ParameterField key={field.name} field={field} value={values[field.name]} onChange={(v) => setValue(field, v)} disabled={busy} />
        ))}
      </div>

      {advanced.length > 0 && (
        <div className="flex flex-col gap-3">
          <button
            type="button"
            onClick={() => setShowAdvanced((s) => !s)}
            className="flex w-fit items-center gap-1 text-xs text-muted hover:text-ink"
          >
            {showAdvanced ? <ChevronDown className="size-3.5" aria-hidden /> : <ChevronRight className="size-3.5" aria-hidden />}
            Advanced settings
          </button>
          {showAdvanced && (
            <div className="flex flex-wrap items-start gap-3">
              {advanced.map((field) => (
                <ParameterField key={field.name} field={field} value={values[field.name]} onChange={(v) => setValue(field, v)} disabled={busy} />
              ))}
            </div>
          )}
        </div>
      )}

      <div className="space-y-1.5">
        <Eyebrow>Your apps will see</Eyebrow>
        <div className="flex flex-wrap gap-1">
          {keys.map((k) => <Chip key={k}>{k}</Chip>)}
        </div>
      </div>
      {note && <p className="text-xs text-ink-soft">{note}</p>}
    </ModalBody>
    <ModalFooter
      note={
        runsSomething
          ? `Runs in the cluster${disk ? ` on a ${disk} disk` : ''}, shared by every app in the space.`
          : 'Provided by the platform — nothing runs in your space.'
      }
    >
      <Button kind="quiet" onClick={onCancel} disabled={busy}>
        Cancel
      </Button>
      <Button
        onClick={() =>
          onSubmit({
            type: chosen.type,
            name: name.trim() || undefined,
            parameters: Object.keys(values).length > 0 ? values : undefined,
            external: false,
            url: undefined,
          })
        }
        disabled={busy || chosen.available === false}
      >
        {busy ? 'Adding…' : submitLabel}
      </Button>
    </ModalFooter>
    </>
  )
}
