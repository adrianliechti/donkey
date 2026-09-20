import { useState, type ReactNode } from 'react'
import { Dialog, DialogBackdrop, DialogPanel, DialogTitle } from '@headlessui/react'
import { ArrowLeft, ChevronDown, ChevronRight } from 'lucide-react'
import { type AddAddonRequest, type ServiceParameters } from './api'
import { parameterDefault, parameterFields, type Field } from './parameters'
import { useCatalog } from './queries'
import { serviceUI } from './serviceProtocol'
import { Button, ErrorNote, Select, Spinner } from './ui'

const inputStyle =
  'rounded-md border border-line bg-surface px-2 py-1.5 font-mono text-sm focus:outline-2 focus:outline-accent-strong'

// A short ladder of disk sizes is easier to pick right than a free-text
// field is to fill right; the type's own default always leads the list.
const diskSizes = ['1Gi', '2Gi', '5Gi', '10Gi', '20Gi', '50Gi']

function Eyebrow({ children }: { children: ReactNode }) {
  return (
    <span className="font-mono text-[10px] font-semibold uppercase tracking-[0.14em] text-muted">
      {children}
    </span>
  )
}

/**
 * One control per schema property. The value is a string while it is being
 * edited (the server coerces it against the schema); booleans are the one
 * exception, since a checkbox has nothing to type.
 */
function ParameterField({
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
        className="size-4 accent-accent"
      />
    )
  } else if (schema.type === 'integer' || schema.type === 'number') {
    control = (
      <input
        type="number"
        aria-label={label}
        value={current}
        min={schema.minimum}
        max={schema.maximum}
        step={schema.type === 'integer' ? 1 : 'any'}
        disabled={disabled}
        onChange={(e) => onChange(e.target.value)}
        className={`${inputStyle} w-28`}
      />
    )
  } else {
    // Strings, and arrays as comma-separated strings.
    control = (
      <input
        aria-label={label}
        value={current}
        placeholder={schema.type === 'array' ? 'one, two' : fallback}
        disabled={disabled}
        onChange={(e) => onChange(e.target.value)}
        className={`${inputStyle} w-44`}
      />
    )
  }
  return (
    <label className="flex flex-col gap-1.5" title={schema.description}>
      <Eyebrow>{label}</Eyebrow>
      {control}
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
    <Dialog open={open} onClose={() => !busy && onClose()} className="relative z-50">
      <DialogBackdrop
        transition
        className="fixed inset-0 bg-black/60 transition-opacity duration-(--duration-base) ease-emphasized data-closed:opacity-0 data-leave:duration-(--duration-fast) data-leave:ease-exit"
      />
      <div className="fixed inset-0 flex items-start justify-center p-4 pt-[14vh]">
        <DialogPanel
          transition
          className="w-full max-w-lg rounded-lg border border-line bg-card p-4 shadow-2xl transition duration-(--duration-base) ease-emphasized data-closed:scale-95 data-closed:opacity-0 data-leave:duration-(--duration-fast) data-leave:ease-exit"
        >
          <DialogTitle className="font-mono text-[11px] font-semibold uppercase tracking-[0.14em] text-muted">
            {title}
          </DialogTitle>
          <ErrorNote message={error ?? ''} />
          {note && <p className="mt-2 text-xs text-ink-soft">{note}</p>}
          <div className="mt-3">
            <AddonForm
              busy={busy}
              submitLabel={submitLabel}
              initialType={initialType}
              onSubmit={onSubmit}
              onCancel={onClose}
            />
          </div>
        </DialogPanel>
      </div>
    </Dialog>
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
  busy,
  submitLabel,
  initialType,
  onSubmit,
  onCancel,
}: {
  busy: boolean
  submitLabel: string
  initialType?: string
  onSubmit: (req: AddAddonRequest) => void
  onCancel: () => void
}) {
  const { data: catalog, error: catalogError } = useCatalog()
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

  if (!loaded) return <Spinner label="Opening the catalog…" />
  if (types.length === 0) {
    return (
      <p className="py-6 text-sm text-muted">
        The catalog didn't load — reopen this dialog, or check the server if it keeps happening.
      </p>
    )
  }

  // Step 1 — the catalog, shown only when the caller didn't preselect a type
  // (the app page's "add a service"). A space's + menu skips straight past it.
  if (!chosen) {
    return (
      <div className="flex flex-col gap-3">
        <div className="grid gap-2 sm:grid-cols-2">
          {types.map((t) => {
            const { Icon } = serviceUI(t.protocol)
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
                className="rounded-lg border border-line bg-surface p-3 text-left transition-colors hover:border-accent/70 hover:bg-accent-soft focus-visible:outline-2 focus-visible:outline-accent-strong disabled:cursor-not-allowed disabled:opacity-50 disabled:hover:border-line disabled:hover:bg-surface"
              >
                <div className="flex items-center gap-2">
                  <Icon className="size-4 text-muted" aria-hidden />
                  <span className="font-mono text-sm font-bold">{t.title || t.type}</span>
                  <span className="font-mono text-[11px] text-muted">{parameterDefault(t, 'version')}</span>
                </div>
                {t.description && (
                  <p className="mt-1.5 text-xs leading-relaxed text-ink-soft">{t.description}</p>
                )}
                {!available && <p className="mt-1.5 text-xs text-danger">{t.error || 'Not available right now.'}</p>}
              </button>
            )
          })}
        </div>
        <div className="flex justify-end">
          <Button kind="quiet" onClick={onCancel}>
            Cancel
          </Button>
        </div>
      </div>
    )
  }

  // Step 2 — configure the chosen service: a name, and whatever its schema
  // offers. Values the person did not touch are left to the server's
  // defaults, so the request says only what was chosen.
  const { Icon: ChosenIcon } = serviceUI(chosen.protocol)
  const setValue = (field: Field, value: unknown) => setValues((v) => ({ ...v, [field.name]: value }))
  const runsSomething = Boolean(chosen.parameters?.properties?.version)
  const disk = String(values.storage ?? parameterDefault(chosen, 'storage'))
  const envNames = (chosen.env ?? []).map((v) => v.name)
  const keys = envNames.length > 0 ? envNames : (chosen.keys ?? [])

  return (
    <div className="flex flex-col gap-4">
      {/* The catalog caller can step back; the + menu caller has nowhere to
          go back to, so it just names the type it opened on. */}
      <div className="flex items-center gap-2">
        {!initialType && (
          <button
            type="button"
            onClick={() => setType('')}
            disabled={busy}
            className="flex items-center gap-1 rounded-md px-1.5 py-1 text-xs text-muted hover:bg-line/50 hover:text-ink focus-visible:outline-2 focus-visible:outline-accent-strong"
          >
            <ArrowLeft className="size-3.5" aria-hidden /> All services
          </button>
        )}
        <span className="flex items-center gap-2 rounded-md border border-accent/40 bg-accent-soft px-2 py-1">
          <ChosenIcon className="size-3.5 text-accent" aria-hidden />
          <span className="font-mono text-sm font-bold">{chosen.title || chosen.type}</span>
          <span className="font-mono text-[11px] text-muted">{parameterDefault(chosen, 'version')}</span>
        </span>
      </div>

      {chosen.available === false && <ErrorNote message={chosen.error || 'This service is not available right now.'} />}

      <div className="flex flex-wrap items-start gap-3">
        <label className="flex flex-col gap-1.5">
          <Eyebrow>Name · optional</Eyebrow>
          <input
            value={name}
            onChange={(e) => setName(e.target.value)}
            placeholder={chosen.type}
            className={`${inputStyle} w-44`}
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

      <div className="rounded-md border border-line/60 bg-raised/60 px-3 py-2.5">
        <Eyebrow>Your apps will see</Eyebrow>
        <p className="mt-1 font-mono text-[11px] leading-5 text-ink-soft">{keys.join(' · ')}</p>
      </div>

      <div className="mt-1 flex flex-col gap-3 border-t border-line/60 pt-3">
        <p className="text-xs text-muted">
          {runsSomething
            ? `Runs in the cluster${disk ? ` on a ${disk} disk` : ''}, shared by every app in the space.`
            : 'Provided by the platform — nothing runs in your space.'}
        </p>
        <div className="flex justify-end gap-2">
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
        </div>
      </div>
    </div>
  )
}
