import type { AddonType, ParameterSchema, ParametersSchema, ServiceParameters } from './api'

/** A parameter field as the form renders it: its schema plus its name. */
export interface Field {
  name: string
  schema: ParameterSchema
}

/** The fields of a type's schema, in presentation order: explicit order first,
 *  then by name; advanced ones separated out. */
export function parameterFields(type: AddonType | undefined): { main: Field[]; advanced: Field[] } {
  const fields = schemaFields(type?.parameters)
  return {
    main: fields.filter((f) => !f.schema['x-donkey-advanced']),
    advanced: fields.filter((f) => f.schema['x-donkey-advanced']),
  }
}

export function schemaFields(schema: ParametersSchema | undefined): Field[] {
  return Object.entries(schema?.properties ?? {})
    .map(([name, schema]) => ({ name, schema }))
    .sort((a, b) => {
      const oa = a.schema['x-donkey-order'] ?? 1000
      const ob = b.schema['x-donkey-order'] ?? 1000
      return oa !== ob ? oa - ob : a.name.localeCompare(b.name)
    })
}

/** The default of one parameter, rendered for display. */
export function parameterDefault(type: AddonType | undefined, name: string): string {
  const value = type?.parameters?.properties?.[name]?.default
  return value === undefined || value === null ? '' : String(value)
}

/** "storage=1Gi, version=18.6" — the same summary the CLI prints. */
export function summarizeParameters(parameters?: ServiceParameters): string {
  if (!parameters) return ''
  return Object.keys(parameters)
    .sort()
    .map((k) => `${k}=${Array.isArray(parameters[k]) ? (parameters[k] as unknown[]).join(',') : String(parameters[k])}`)
    .join(', ')
}
