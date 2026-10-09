import { useEffect, useMemo, useState } from 'react'
import { useMutation, useQuery, useQueryClient } from '@tanstack/react-query'
import { toast } from 'sonner'
import { ClipboardPaste, FlaskConical, Save, Wand2 } from 'lucide-react'
import { endpoints, type Connection, type TestReport, type TypeInfo } from '@/lib/api'
import { useAuth } from '@/hooks/use-auth'
import { Button } from '@/components/ui/button'
import { Input, Textarea } from '@/components/ui/input'
import { Label } from '@/components/ui/label'
import { SchemaForm, UNCHANGED, type FormValues } from '@/components/schema-form'
import { TestChecklist } from '@/components/test-checklist'
import { InlineError } from '@/components/states'
import { cn } from '@/lib/utils'

export interface ConnectionFormProps {
  kind: 'source' | 'destination'
  existing?: Connection | null
  onSaved?: (c: Connection) => void
  onCancel?: () => void
  /** Keep the form after save (used by the setup flow). */
  compact?: boolean
  autoTest?: boolean
}

export function ConnectionForm({ kind, existing, onSaved, onCancel, compact, autoTest }: ConnectionFormProps) {
  const { me } = useAuth()
  const qc = useQueryClient()
  const types = useQuery({ queryKey: ['types'], queryFn: endpoints.types, staleTime: Infinity })
  const list: TypeInfo[] = useMemo(() => (kind === 'source' ? types.data?.sources : types.data?.destinations) || [], [types.data, kind])
  const [type, setType] = useState(existing?.type || '')
  const [name, setName] = useState(existing?.name || '')
  const [values, setValues] = useState<FormValues>({ config: existing?.config || {}, secrets: {} })
  const [paste, setPaste] = useState('')
  const [report, setReport] = useState<TestReport | null>(existing?.last_test_report || null)
  const [error, setError] = useState<unknown>(null)
  const [testedAt, setTestedAt] = useState<string | null>(existing?.last_tested_at || null)
  useEffect(() => {
    if (!type && list.length) setType(list[0].type)
  }, [list, type])
  const ti = list.find((t) => t.type === type)
  const hint = me?.local_stack_hints?.[type]

  const body = () => ({ kind, type, name: name || defaultName(ti), config: values.config, secrets: values.secrets })

  const parse = useMutation({
    mutationFn: () => endpoints.parse(type, paste),
    onSuccess: (r) => {
      setValues((v) => ({ config: { ...v.config, ...r.config }, secrets: { ...v.secrets, ...r.secrets } }))
      setPaste('')
      toast.success('Connection string parsed. Credentials went into the masked fields.')
      setError(null)
    },
    onError: setError,
  })
  const test = useMutation({
    mutationFn: () => endpoints.testDraft(body(), existing?.id),
    onSuccess: (r) => {
      setReport(r)
      setTestedAt(new Date().toISOString())
      setError(null)
      if (r.ok) toast.success('All checks passed')
      else toast.error('Some checks failed; see the checklist')
    },
    onError: setError,
  })
  const save = useMutation({
    mutationFn: async () => {
      const b = body()
      const secrets = Object.fromEntries(Object.entries(b.secrets).filter(([, v]) => v !== UNCHANGED || existing))
      const c = existing ? await endpoints.updateConnection(existing.id, { name: b.name, config: b.config, secrets }) : await endpoints.createConnection({ ...b, secrets })
      if (report) {
        // persist the latest test result on the saved row
        try {
          await endpoints.testConnection(c.id)
        } catch {
          /* ignore */
        }
      }
      return c
    },
    onSuccess: (c) => {
      qc.invalidateQueries({ queryKey: ['connections'] })
      toast.success(existing ? 'Connection updated' : 'Connection saved')
      onSaved?.(c)
    },
    onError: setError,
  })

  const applyHint = () => {
    if (!hint) return
    setValues({ config: { ...hint.config }, secrets: { ...hint.secrets } })
    if (!name) setName(kind === 'source' ? 'Local MongoDB' : 'Local Nessie + S3')
    toast.success('Filled in the local stack values')
  }

  if (types.isLoading || !ti) return <div className="text-[13px] text-muted-foreground">Loading…</div>
  const canSave = !!type && (report?.ok || !autoTest)
  return (
    <div className={cn('space-y-5', compact && 'space-y-4')}>
      {!existing && list.length > 1 ? (
        <div className="grid grid-cols-2 gap-2">
          {list.map((t) => (
            <button key={t.type} type="button" onClick={() => setType(t.type)} className={cn('rounded-md border p-3 text-left hover:bg-accent', type === t.type && 'border-primary ring-1 ring-primary')}>
              <div className="text-[13px] font-medium">{t.title}</div>
              <div className="text-xs text-muted-foreground">{t.description}</div>
            </button>
          ))}
        </div>
      ) : (
        <div className="text-xs text-muted-foreground">
          {ti.title} · {ti.description}
        </div>
      )}
      <div className="space-y-1.5">
        <Label htmlFor="conn-name">Name</Label>
        <Input id="conn-name" value={name} placeholder={defaultName(ti)} onChange={(e) => setName(e.target.value)} />
      </div>
      {ti.can_parse ? (
        <div className="space-y-1.5 rounded-md border border-dashed p-3">
          <Label htmlFor="paste">{ti.schema['x-parse']?.title || 'Paste a connection string'}</Label>
          <p className="text-xs text-muted-foreground">{ti.schema['x-parse']?.description || 'The form is filled in from it; credentials go into the masked fields.'}</p>
          <div className="flex gap-2">
            <Textarea id="paste" rows={2} className="font-mono text-xs" placeholder={ti.schema['x-parse']?.placeholder || 'mongodb+srv://user:password@cluster0.example.mongodb.net/'} value={paste} onChange={(e) => setPaste(e.target.value)} spellCheck={false} />
            <Button type="button" variant="secondary" onClick={() => parse.mutate()} disabled={!paste.trim()} loading={parse.isPending}>
              <ClipboardPaste /> Parse
            </Button>
          </div>
        </div>
      ) : null}
      {hint ? (
        <Button type="button" variant="outline" size="sm" onClick={applyHint}>
          <Wand2 /> Use the local stack values
        </Button>
      ) : null}
      <SchemaForm schema={ti.schema} values={values} onChange={setValues} storedSecrets={existing?.secret_fields || []} />
      <InlineError error={error} />
      {report ? (
        <div className="space-y-1">
          <div className="text-xs text-muted-foreground">Connection test{testedAt ? ` · ${new Date(testedAt).toLocaleTimeString()}` : ''}</div>
          <TestChecklist report={report} />
        </div>
      ) : null}
      <div className="flex flex-wrap items-center justify-end gap-2 pt-1">
        {onCancel ? (
          <Button type="button" variant="ghost" onClick={onCancel}>
            Cancel
          </Button>
        ) : null}
        <Button type="button" variant="outline" onClick={() => test.mutate()} loading={test.isPending}>
          <FlaskConical /> Test connection
        </Button>
        <Button type="button" onClick={() => save.mutate()} loading={save.isPending} disabled={!canSave} title={!canSave ? 'Run a passing test first' : undefined}>
          <Save /> {existing ? 'Save changes' : 'Save'}
        </Button>
      </div>
    </div>
  )
}

function defaultName(t?: TypeInfo) {
  return t ? `${t.title}` : 'Connection'
}
