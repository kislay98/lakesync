import { useMemo, useState } from 'react'
import { Eye, EyeOff } from 'lucide-react'
import { Input } from '@/components/ui/input'
import { Label } from '@/components/ui/label'
import { Switch } from '@/components/ui/switch'
import { Select, SelectContent, SelectItem, SelectTrigger, SelectValue } from '@/components/ui/select'
import { Button } from '@/components/ui/button'
import type { JsonSchema, JsonSchemaProperty } from '@/lib/api'
import { cn } from '@/lib/utils'

export const UNCHANGED = '__unchanged__'

export interface FormValues {
  config: Record<string, unknown>
  secrets: Record<string, string>
}

/** Renders a connection form from a type's JSON schema. Secrets are masked. */
export function SchemaForm({
  schema,
  values,
  onChange,
  storedSecrets = [],
  disabled,
}: {
  schema: JsonSchema
  values: FormValues
  onChange: (v: FormValues) => void
  storedSecrets?: string[]
  disabled?: boolean
}) {
  const groups = useMemo(() => {
    const entries = Object.entries(schema.properties || {}).sort((a, b) => (a[1]['x-order'] ?? 999) - (b[1]['x-order'] ?? 999))
    const byGroup = new Map<string, [string, JsonSchemaProperty][]>()
    for (const e of entries) {
      const g = e[1]['x-group'] || 'General'
      if (!byGroup.has(g)) byGroup.set(g, [])
      byGroup.get(g)!.push(e)
    }
    return Array.from(byGroup.entries())
  }, [schema])
  const required = new Set(schema.required || [])

  const visible = (p: JsonSchemaProperty) => {
    const dep = p['x-depends-on']
    if (!dep) return true
    return Object.entries(dep).every(([k, v]) => {
      const cur = values.config[k] ?? schema.properties[k]?.default
      return String(cur ?? '') === v || (v.includes('|') && v.split('|').includes(String(cur ?? '')))
    })
  }

  return (
    <div className="space-y-5">
      {groups.map(([group, fields]) => {
        const shown = fields.filter(([, p]) => visible(p))
        if (!shown.length) return null
        return (
          <fieldset key={group} className="space-y-3">
            {groups.length > 1 ? <legend className="mb-2 text-xs font-semibold uppercase tracking-wide text-muted-foreground">{group}</legend> : null}
            {shown.map(([name, p]) => (
              <Field key={name} name={name} prop={p} required={required.has(name)} values={values} onChange={onChange} stored={storedSecrets.includes(name)} disabled={disabled} />
            ))}
          </fieldset>
        )
      })}
    </div>
  )
}

function Field({ name, prop, required, values, onChange, stored, disabled }: { name: string; prop: JsonSchemaProperty; required: boolean; values: FormValues; onChange: (v: FormValues) => void; stored: boolean; disabled?: boolean }) {
  const [reveal, setReveal] = useState(false)
  const secret = !!prop['x-secret']
  const id = `f-${name}`
  const label = (
    <div className="flex items-baseline justify-between">
      <Label htmlFor={id}>
        {prop.title || name}
        {required ? <span className="ml-0.5 text-destructive">*</span> : null}
      </Label>
      {secret && stored && (values.secrets[name] ?? UNCHANGED) === UNCHANGED ? <span className="text-[11px] text-muted-foreground">stored, leave blank to keep</span> : null}
    </div>
  )
  const desc = prop.description ? <p className="text-xs text-muted-foreground">{prop.description}</p> : null

  if (secret) {
    const v = values.secrets[name] ?? (stored ? UNCHANGED : '')
    const shownValue = v === UNCHANGED ? '' : v
    return (
      <div className="space-y-1.5">
        {label}
        <div className="relative">
          <Input
            id={id}
            type={reveal ? 'text' : 'password'}
            autoComplete="new-password"
            placeholder={stored && v === UNCHANGED ? '••••••••' : prop['x-placeholder'] || ''}
            value={shownValue}
            disabled={disabled}
            onChange={(e) => onChange({ ...values, secrets: { ...values.secrets, [name]: e.target.value === '' && stored ? UNCHANGED : e.target.value } })}
            className="pr-9"
          />
          <button type="button" className="absolute right-2 top-1/2 -translate-y-1/2 text-muted-foreground hover:text-foreground" onClick={() => setReveal((r) => !r)} tabIndex={-1} aria-label={reveal ? 'Hide' : 'Show'}>
            {reveal ? <EyeOff className="h-4 w-4" /> : <Eye className="h-4 w-4" />}
          </button>
        </div>
        {desc}
      </div>
    )
  }
  if (prop.type === 'boolean') {
    const v = (values.config[name] as boolean | undefined) ?? (prop.default as boolean | undefined) ?? false
    return (
      <div className="flex items-start justify-between gap-4 rounded-md border px-3 py-2.5">
        <div className="space-y-0.5">
          <Label htmlFor={id}>{prop.title || name}</Label>
          {desc}
        </div>
        <Switch id={id} checked={!!v} disabled={disabled} onCheckedChange={(c) => onChange({ ...values, config: { ...values.config, [name]: c } })} />
      </div>
    )
  }
  if (prop.enum) {
    const v = String(values.config[name] ?? prop.default ?? prop.enum[0])
    return (
      <div className="space-y-1.5">
        {label}
        <Select value={v} disabled={disabled} onValueChange={(c) => onChange({ ...values, config: { ...values.config, [name]: c } })}>
          <SelectTrigger id={id}>
            <SelectValue />
          </SelectTrigger>
          <SelectContent>
            {prop.enum.map((o) => (
              <SelectItem key={o} value={o}>
                {o}
              </SelectItem>
            ))}
          </SelectContent>
        </Select>
        {desc}
      </div>
    )
  }
  const v = values.config[name]
  return (
    <div className="space-y-1.5">
      {label}
      <Input
        id={id}
        type={prop.type === 'integer' || prop.type === 'number' ? 'number' : 'text'}
        placeholder={prop['x-placeholder'] || (prop.default != null ? String(prop.default) : '')}
        value={v == null ? '' : String(v)}
        disabled={disabled}
        onChange={(e) => {
          const raw = e.target.value
          const val: unknown = prop.type === 'integer' || prop.type === 'number' ? (raw === '' ? undefined : Number(raw)) : raw
          const next = { ...values.config }
          if (val === undefined || val === '') delete next[name]
          else next[name] = val
          onChange({ ...values, config: next })
        }}
        spellCheck={false}
        autoComplete="off"
      />
      {desc}
    </div>
  )
}

export function FormActions({ children, className }: { children: React.ReactNode; className?: string }) {
  return <div className={cn('flex items-center justify-end gap-2 pt-2', className)}>{children}</div>
}

export { Button as FormButton }
