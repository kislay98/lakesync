import { useEffect, useMemo, useState } from 'react'
import { Link, useNavigate, useSearchParams } from 'react-router-dom'
import { useMutation, useQuery } from '@tanstack/react-query'
import { toast } from 'sonner'
import { ArrowLeft, ArrowRight, Check, Database, Play, Search } from 'lucide-react'
import { endpoints, type Entity } from '@/lib/api'
import { Page } from '@/components/layout'
import { PageHeader, EmptyState, ErrorState, InlineError, ListSkeleton } from '@/components/states'
import { Button } from '@/components/ui/button'
import { Input } from '@/components/ui/input'
import { Label } from '@/components/ui/label'
import { Checkbox } from '@/components/ui/checkbox'
import { Select, SelectContent, SelectItem, SelectTrigger, SelectValue } from '@/components/ui/select'
import { cn, formatBytes, formatCount } from '@/lib/utils'

const steps = ['Connections', 'Collections', 'Settings', 'Review']

export default function NewPipeline({ embedded, onCreated }: { embedded?: boolean; onCreated?: (id: string) => void } = {}) {
  const nav = useNavigate()
  const [sp] = useSearchParams()
  const conns = useQuery({ queryKey: ['connections'], queryFn: () => endpoints.connections() })
  const settings = useQuery({ queryKey: ['settings'], queryFn: endpoints.settings })
  const sources = conns.data?.filter((c) => c.kind === 'source') || []
  const dests = conns.data?.filter((c) => c.kind === 'destination') || []
  const [step, setStep] = useState(0)
  const [sourceId, setSourceId] = useState(sp.get('source') || '')
  const [destId, setDestId] = useState(sp.get('destination') || '')
  const [selected, setSelected] = useState<Record<string, Entity>>({})
  const [filter, setFilter] = useState('')
  const [name, setName] = useState('')
  const [namespace, setNamespace] = useState('')
  const [interval, setInterval_] = useState(0)
  const [start, setStart] = useState(true)
  useEffect(() => {
    if (!sourceId && sources.length === 1) setSourceId(sources[0].id)
    if (!destId && dests.length === 1) setDestId(dests[0].id)
  }, [sources, dests, sourceId, destId])
  useEffect(() => {
    if (settings.data && !interval) setInterval_(settings.data.general.default_sync_interval_s)
  }, [settings.data, interval])
  const entities = useQuery({ queryKey: ['entities', sourceId], queryFn: () => endpoints.entities(sourceId), enabled: !!sourceId && step >= 1 })
  const byDb = useMemo(() => {
    const m = new Map<string, Entity[]>()
    for (const e of entities.data || []) {
      if (filter && !`${e.namespace}.${e.name}`.toLowerCase().includes(filter.toLowerCase())) continue
      if (!m.has(e.namespace)) m.set(e.namespace, [])
      m.get(e.namespace)!.push(e)
    }
    return Array.from(m.entries())
  }, [entities.data, filter])
  const picked = Object.values(selected)
  const minInterval = settings.data?.limits.min_sync_interval_s || 10
  const create = useMutation({
    mutationFn: () =>
      endpoints.createPipeline({
        name: name || defaultName(),
        source_id: sourceId,
        destination_id: destId,
        namespace: namespace || undefined,
        sync_interval_s: interval,
        start,
        collections: picked.map((e) => ({ collection: `${e.namespace}.${e.name}`, enabled: true, estimated_count: e.estimated_count })),
      }),
    onSuccess: (p) => {
      toast.success(start ? 'Pipeline created and started' : 'Pipeline created')
      if (onCreated) onCreated(p.id)
      else nav(`/pipelines/${p.id}`)
    },
  })
  const defaultName = () => {
    const dbs = Array.from(new Set(picked.map((e) => e.namespace)))
    return dbs.length === 1 ? dbs[0] : 'pipeline'
  }
  const toggle = (e: Entity) => {
    const k = `${e.namespace}.${e.name}`
    setSelected((s) => {
      const n = { ...s }
      if (n[k]) delete n[k]
      else n[k] = e
      return n
    })
  }
  const toggleDb = (list: Entity[]) => {
    const all = list.every((e) => selected[`${e.namespace}.${e.name}`])
    setSelected((s) => {
      const n = { ...s }
      for (const e of list) {
        const k = `${e.namespace}.${e.name}`
        if (all) delete n[k]
        else n[k] = e
      }
      return n
    })
  }
  const canNext = [!!sourceId && !!destId, picked.length > 0, interval >= minInterval, true][step]
  const body = (
    <>
      <ol className="mb-5 flex items-center gap-2 text-xs">
        {steps.map((s, i) => (
          <li key={s} className="flex items-center gap-2">
            <span className={cn('flex h-5 w-5 items-center justify-center rounded-full border text-[10px] font-semibold', i < step ? 'border-primary bg-primary text-primary-foreground' : i === step ? 'border-primary text-primary' : 'text-muted-foreground')}>{i < step ? <Check className="h-3 w-3" /> : i + 1}</span>
            <span className={cn(i === step ? 'font-medium' : 'text-muted-foreground')}>{s}</span>
            {i < steps.length - 1 ? <span className="mx-1 h-px w-6 bg-border" /> : null}
          </li>
        ))}
      </ol>
      {step === 0 ? (
        conns.isLoading ? (
          <ListSkeleton rows={2} />
        ) : (
          <div className="grid gap-4 md:grid-cols-2">
            <div className="space-y-1.5">
              <Label>Source</Label>
              {sources.length ? (
                <Select value={sourceId} onValueChange={setSourceId}>
                  <SelectTrigger>
                    <SelectValue placeholder="Pick a source" />
                  </SelectTrigger>
                  <SelectContent>
                    {sources.map((c) => (
                      <SelectItem key={c.id} value={c.id}>
                        {c.name} · {c.type}
                      </SelectItem>
                    ))}
                  </SelectContent>
                </Select>
              ) : (
                <EmptyState title="No source connection" action={<Button asChild size="sm" variant="outline"><Link to="/connections">Add one</Link></Button>} className="py-6" />
              )}
            </div>
            <div className="space-y-1.5">
              <Label>Destination</Label>
              {dests.length ? (
                <Select value={destId} onValueChange={setDestId}>
                  <SelectTrigger>
                    <SelectValue placeholder="Pick a destination" />
                  </SelectTrigger>
                  <SelectContent>
                    {dests.map((c) => (
                      <SelectItem key={c.id} value={c.id}>
                        {c.name} · {c.type}
                      </SelectItem>
                    ))}
                  </SelectContent>
                </Select>
              ) : (
                <EmptyState title="No destination connection" action={<Button asChild size="sm" variant="outline"><Link to="/connections">Add one</Link></Button>} className="py-6" />
              )}
            </div>
          </div>
        )
      ) : null}
      {step === 1 ? (
        <div className="space-y-3">
          <div className="flex items-center gap-2">
            <div className="relative flex-1">
              <Search className="absolute left-2.5 top-1/2 h-3.5 w-3.5 -translate-y-1/2 text-muted-foreground" />
              <Input className="pl-8" placeholder="Filter databases and collections" value={filter} onChange={(e) => setFilter(e.target.value)} />
            </div>
            <span className="text-xs text-muted-foreground">{picked.length} selected</span>
          </div>
          {entities.isLoading ? <ListSkeleton /> : entities.error ? <ErrorState error={entities.error} onRetry={() => entities.refetch()} title="Could not list collections" /> : null}
          {entities.data && byDb.length === 0 ? <EmptyState icon={<Database className="h-5 w-5" />} title="No collections found" description="The user may lack permission to list databases, or the filter matches nothing." /> : null}
          <div className="space-y-3">
            {byDb.map(([db, list]) => {
              const all = list.every((e) => selected[`${e.namespace}.${e.name}`])
              const some = !all && list.some((e) => selected[`${e.namespace}.${e.name}`])
              return (
                <div key={db} className="rounded-lg border bg-card">
                  <label className="flex cursor-pointer items-center gap-2 border-b px-3 py-2">
                    <Checkbox checked={all ? true : some ? 'indeterminate' : false} onCheckedChange={() => toggleDb(list)} />
                    <Database className="h-4 w-4 text-muted-foreground" />
                    <span className="text-[13px] font-medium">{db}</span>
                    <span className="text-xs text-muted-foreground">{list.length} collections</span>
                  </label>
                  <div className="divide-y">
                    {list.map((e) => {
                      const k = `${e.namespace}.${e.name}`
                      return (
                        <label key={k} className="flex cursor-pointer items-center gap-2 px-3 py-1.5 hover:bg-accent/50">
                          <Checkbox checked={!!selected[k]} onCheckedChange={() => toggle(e)} />
                          <span className="text-[13px]">{e.name}</span>
                          <span className="ml-auto text-xs tabular-nums text-muted-foreground">
                            ~{formatCount(e.estimated_count)} docs · {formatBytes(e.estimated_bytes)}
                          </span>
                        </label>
                      )
                    })}
                  </div>
                </div>
              )
            })}
          </div>
        </div>
      ) : null}
      {step === 2 ? (
        <div className="grid gap-4 md:grid-cols-2">
          <div className="space-y-1.5">
            <Label htmlFor="pname">Pipeline name</Label>
            <Input id="pname" value={name} placeholder={defaultName()} onChange={(e) => setName(e.target.value)} />
          </div>
          <div className="space-y-1.5">
            <Label htmlFor="ns">Iceberg namespace</Label>
            <Input id="ns" value={namespace} placeholder={defaultName()} onChange={(e) => setNamespace(e.target.value)} />
            <p className="text-xs text-muted-foreground">Default: the MongoDB database name. Each collection becomes two tables: <span className="font-mono">name</span> and <span className="font-mono">name__raw</span>.</p>
          </div>
          <div className="space-y-1.5">
            <Label htmlFor="interval">Sync interval (seconds)</Label>
            <Input id="interval" type="number" min={minInterval} value={interval || ''} onChange={(e) => setInterval_(Number(e.target.value))} />
            <p className="text-xs text-muted-foreground">Changes are committed and merged this often. Minimum {minInterval}s.</p>
          </div>
          <label className="flex items-start gap-2 rounded-md border p-3">
            <Checkbox checked={start} onCheckedChange={(c) => setStart(!!c)} className="mt-0.5" />
            <span>
              <span className="text-[13px] font-medium">Start immediately</span>
              <span className="block text-xs text-muted-foreground">Takes a bookmark, copies existing documents, then streams changes.</span>
            </span>
          </label>
        </div>
      ) : null}
      {step === 3 ? (
        <div className="space-y-3 rounded-lg border bg-card p-4 text-[13px]">
          <Row k="Name" v={name || defaultName()} />
          <Row k="Source" v={sources.find((c) => c.id === sourceId)?.name} />
          <Row k="Destination" v={dests.find((c) => c.id === destId)?.name} />
          <Row k="Namespace" v={<span className="font-mono">{namespace || defaultName()}</span>} />
          <Row k="Sync interval" v={`${interval}s`} />
          <Row k="Collections" v={<span>{picked.map((e) => `${e.namespace}.${e.name}`).join(', ')}</span>} />
          <Row k="Estimated documents" v={formatCount(picked.reduce((a, e) => a + e.estimated_count, 0))} />
          <Row k="On create" v={start ? 'start syncing' : 'stay stopped'} />
          <InlineError error={create.error} />
        </div>
      ) : null}
      <div className="mt-6 flex items-center justify-between">
        <Button variant="ghost" onClick={() => (step === 0 ? (embedded ? null : nav(-1)) : setStep(step - 1))} disabled={step === 0 && embedded}>
          <ArrowLeft /> Back
        </Button>
        {step < steps.length - 1 ? (
          <Button onClick={() => setStep(step + 1)} disabled={!canNext}>
            Next <ArrowRight />
          </Button>
        ) : (
          <Button onClick={() => create.mutate()} loading={create.isPending}>
            <Play /> {start ? 'Create and start' : 'Create'}
          </Button>
        )}
      </div>
    </>
  )
  if (embedded) return <div>{body}</div>
  return (
    <Page className="max-w-4xl">
      <PageHeader title="New pipeline" description="Pick where to read from and write to, choose collections, review, start." />
      {body}
    </Page>
  )
}

function Row({ k, v }: { k: string; v: React.ReactNode }) {
  return (
    <div className="flex gap-4">
      <div className="w-40 shrink-0 text-muted-foreground">{k}</div>
      <div className="min-w-0 break-words">{v}</div>
    </div>
  )
}
