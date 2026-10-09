import { useEffect, useMemo, useRef, useState } from 'react'
import { Link, useNavigate, useParams } from 'react-router-dom'
import { useMutation, useQuery, useQueryClient } from '@tanstack/react-query'
import { toast } from 'sonner'
import { AlertTriangle, ChevronRight, Pause, Play, RefreshCw, RotateCcw, Search, Square, Table2, Trash2 } from 'lucide-react'
import { endpoints, type LogEntry, type Pipeline, type PipelineCollection, type Run } from '@/lib/api'
import { Page } from '@/components/layout'
import { PageHeader, EmptyState, ErrorState, ListSkeleton, Stat } from '@/components/states'
import { Button } from '@/components/ui/button'
import { Badge } from '@/components/ui/badge'
import { Input } from '@/components/ui/input'
import { Checkbox } from '@/components/ui/checkbox'
import { Tabs, TabsContent, TabsList, TabsTrigger } from '@/components/ui/tabs'
import { Progress } from '@/components/ui/misc'
import { Select, SelectContent, SelectItem, SelectTrigger, SelectValue } from '@/components/ui/select'
import { AlertDialog, AlertDialogAction, AlertDialogCancel, AlertDialogContent, AlertDialogDescription, AlertDialogFooter, AlertDialogHeader, AlertDialogTitle } from '@/components/ui/alert-dialog'
import { DropdownMenu, DropdownMenuContent, DropdownMenuItem, DropdownMenuTrigger } from '@/components/ui/dropdown-menu'
import { StatusBadge } from '@/components/status-badge'
import { Sparkline } from '@/components/sparkline'
import { Tip } from '@/components/ui/tooltip'
import { cn, eta, exactTime, formatBytes, formatCount, formatDuration, formatSeconds, percent, relativeTime } from '@/lib/utils'
import { useLive } from '@/lib/live'

export default function PipelineDetail() {
  const { id = '' } = useParams()
  const nav = useNavigate()
  const qc = useQueryClient()
  const q = useQuery({ queryKey: ['pipeline', id], queryFn: () => endpoints.pipeline(id), refetchInterval: 10000 })
  const series = useQuery({ queryKey: ['series', id, 60], queryFn: () => endpoints.series(id, 60), refetchInterval: 30000 })
  const [confirm, setConfirm] = useState<null | { kind: 'resync' | 'delete'; collection?: string }>(null)
  const [dropTables, setDropTables] = useState(false)
  const act = useMutation({
    mutationFn: (a: 'start' | 'pause' | 'stop' | 'resync' | 'retry') => endpoints.pipelineAction(id, a),
    onSuccess: (_, a) => {
      toast.success({ start: 'Pipeline started', pause: 'Pipeline paused', stop: 'Pipeline stopped', resync: 'Resync requested', retry: 'Retrying' }[a])
      qc.invalidateQueries({ queryKey: ['pipeline', id] })
    },
    onError: (e) => toast.error(e.message),
  })
  const cact = useMutation({
    mutationFn: ({ name, a }: { name: string; a: 'resync' | 'retry' | 'enable' | 'disable' }) => endpoints.collectionAction(id, name, a),
    onSuccess: (_, v) => {
      toast.success(`${v.name}: ${v.a} requested`)
      qc.invalidateQueries({ queryKey: ['pipeline', id] })
    },
    onError: (e) => toast.error(e.message),
  })
  const del = useMutation({
    mutationFn: () => endpoints.deletePipeline(id, dropTables),
    onSuccess: (r) => {
      toast.success(r.tables_dropped ? 'Pipeline deleted and tables dropped' : r.drop_errors?.length ? `Pipeline deleted; some tables could not be dropped: ${r.drop_errors.join('; ')}` : 'Pipeline deleted')
      qc.invalidateQueries({ queryKey: ['pipelines'] })
      nav('/pipelines')
    },
    onError: (e) => toast.error(e.message),
  })
  if (q.isLoading)
    return (
      <Page>
        <ListSkeleton />
      </Page>
    )
  if (q.error || !q.data)
    return (
      <Page>
        <ErrorState error={q.error} onRetry={() => q.refetch()} />
      </Page>
    )
  const p = q.data
  const running = p.desired_state === 'running'
  const anyFailed = p.collections.some((c) => c.state === 'failed')
  return (
    <Page>
      <PageHeader
        crumbs={
          <span className="flex items-center gap-1">
            <Link to="/pipelines" className="hover:underline">
              Pipelines
            </Link>
            <ChevronRight className="h-3 w-3" />
          </span>
        }
        title={
          <span className="flex items-center gap-2">
            {p.name} <StatusBadge state={p.state} />
          </span>
        }
        description={
          <>
            {p.source.name} → {p.destination.name} · namespace <span className="font-mono">{p.namespace}</span> · every {p.sync_interval_s}s
          </>
        }
        actions={
          <>
            {anyFailed ? (
              <Button variant="outline" onClick={() => act.mutate('retry')} loading={act.isPending && act.variables === 'retry'}>
                <RotateCcw /> Retry
              </Button>
            ) : null}
            {running ? (
              <Button variant="outline" onClick={() => act.mutate('pause')} loading={act.isPending && act.variables === 'pause'}>
                <Pause /> Pause
              </Button>
            ) : (
              <Button onClick={() => act.mutate('start')} loading={act.isPending && act.variables === 'start'}>
                <Play /> {p.desired_state === 'paused' ? 'Resume' : 'Start'}
              </Button>
            )}
            <DropdownMenu>
              <DropdownMenuTrigger asChild>
                <Button variant="outline" size="icon" aria-label="More actions">
                  …
                </Button>
              </DropdownMenuTrigger>
              <DropdownMenuContent align="end">
                <DropdownMenuItem onSelect={() => setConfirm({ kind: 'resync' })}>
                  <RefreshCw /> Resync everything
                </DropdownMenuItem>
                <DropdownMenuItem onSelect={() => act.mutate('stop')} disabled={p.desired_state === 'stopped'}>
                  <Square /> Stop
                </DropdownMenuItem>
                <DropdownMenuItem destructive onSelect={() => setConfirm({ kind: 'delete' })}>
                  <Trash2 /> Delete pipeline
                </DropdownMenuItem>
              </DropdownMenuContent>
            </DropdownMenu>
          </>
        }
      />
      <div className="grid gap-3 md:grid-cols-4">
        <Stat label="Rows synced" value={formatCount(p.stats.rows_synced)} />
        <Stat label="Events, last hour" value={formatCount(p.stats.events_last_hour)} sub={<span className="text-primary"><Sparkline values={(series.data || []).map((m) => m.events)} width={160} height={24} /></span>} />
        <Stat label="Lag" value={p.stats.lag_seconds == null ? '–' : formatSeconds(p.stats.lag_seconds)} sub="delay between a change in MongoDB and its commit" />
        <Stat label="Last commit" value={relativeTime(p.collections.map((c) => c.last_commit_at).filter(Boolean).sort().pop() || null)} />
      </div>
      {p.oplog && !p.oplog.error ? (
        <div className={cn('mt-3 flex flex-wrap items-center gap-2 rounded-md border px-3 py-2 text-xs', p.oplog.warning ? 'border-warning/60 bg-warning/10' : 'text-muted-foreground')}>
          {p.oplog.warning ? <AlertTriangle className="h-3.5 w-3.5 text-warning" /> : null}
          <span>
            MongoDB keeps about <span className="font-medium text-foreground">{formatSeconds(p.oplog.window_seconds)}</span> of change history{p.oplog.size_bytes ? ` (${formatBytes(p.oplog.size_bytes)} oplog)` : ''}; a pause or outage longer than that forces a full resync.
          </span>
          {p.oplog.paused_seconds ? <span>Bookmark idle for {formatSeconds(p.oplog.paused_seconds)}.</span> : null}
          {p.oplog.warning ? <span className="font-medium text-warning">{p.oplog.warning}</span> : null}
        </div>
      ) : null}
      <section className="mt-6">
        <h2 className="mb-2 text-xs font-semibold uppercase tracking-wide text-muted-foreground">Collections</h2>
        <div className="divide-y rounded-lg border bg-card">
          {p.collections.map((c) => (
            <CollectionRow key={c.collection} p={p} c={c} onAction={(a) => (a === 'resync' ? setConfirm({ kind: 'resync', collection: c.collection }) : cact.mutate({ name: c.collection, a }))} />
          ))}
        </div>
      </section>
      <Tabs defaultValue="logs" className="mt-6">
        <TabsList>
          <TabsTrigger value="logs">Logs</TabsTrigger>
          <TabsTrigger value="runs">Run history</TabsTrigger>
        </TabsList>
        <TabsContent value="logs">
          <Logs id={id} collections={p.collections.map((c) => c.collection)} />
        </TabsContent>
        <TabsContent value="runs">
          <Runs id={id} />
        </TabsContent>
      </Tabs>
      <AlertDialog open={!!confirm} onOpenChange={(o) => !o && setConfirm(null)}>
        <AlertDialogContent>
          <AlertDialogHeader>
            <AlertDialogTitle>{confirm?.kind === 'delete' ? `Delete pipeline ${p.name}?` : confirm?.collection ? `Resync ${confirm.collection} from scratch?` : `Resync every collection of ${p.name}?`}</AlertDialogTitle>
            <AlertDialogDescription>
              {confirm?.kind === 'delete'
                ? 'The pipeline, its run history and logs are deleted. Choose below whether the Iceberg tables are dropped as well.'
                : 'A new generation is started: a fresh bookmark is taken, every document is copied again, and the clean table is rebuilt from the new copy (documents deleted at the source disappear). Streaming resumes afterwards.'}
            </AlertDialogDescription>
          </AlertDialogHeader>
          {confirm?.kind === 'delete' ? (
            <label className="flex items-center gap-2 text-[13px]">
              <Checkbox checked={dropTables} onCheckedChange={(c) => setDropTables(!!c)} /> Also drop the {p.collections.length * 2} Iceberg tables in <span className="font-mono">{p.namespace}</span>
            </label>
          ) : null}
          <AlertDialogFooter>
            <AlertDialogCancel>Cancel</AlertDialogCancel>
            <AlertDialogAction
              destructive={confirm?.kind === 'delete'}
              onClick={() => {
                if (confirm?.kind === 'delete') del.mutate()
                else if (confirm?.collection) cact.mutate({ name: confirm.collection, a: 'resync' })
                else act.mutate('resync')
                setConfirm(null)
              }}
            >
              {confirm?.kind === 'delete' ? (dropTables ? 'Delete pipeline and tables' : 'Delete pipeline') : 'Resync'}
            </AlertDialogAction>
          </AlertDialogFooter>
        </AlertDialogContent>
      </AlertDialog>
    </Page>
  )
}

function CollectionRow({ p, c, onAction }: { p: Pipeline; c: PipelineCollection; onAction: (a: 'resync' | 'retry' | 'enable' | 'disable') => void }) {
  const copying = c.state === 'copying' || c.state === 'resyncing'
  const total = Math.max(c.snapshot_total_est, c.snapshot_done)
  const lag = c.state === 'streaming' && c.last_source_ts ? (Date.now() - new Date(c.last_source_ts).getTime()) / 1000 : null
  const remaining = eta(c.snapshot_done, total, c.updated_at && copying ? null : null)
  const href = `/tables/${encodeURIComponent(p.namespace)}/${encodeURIComponent(c.clean_table)}?destination=${p.destination_id}`
  return (
    <div className={cn('px-4 py-3', !c.enabled && 'opacity-60')}>
      <div className="flex flex-wrap items-center gap-3">
        <div className="min-w-0 flex-1">
          <div className="flex items-center gap-2">
            <span className="font-mono text-[13px] font-medium">{c.collection}</span>
            <StatusBadge state={c.state} />
            {!c.enabled ? <Badge variant="muted">disabled</Badge> : null}
            {c.generation > 1 ? (
              <Tip label={c.resync_reason ? `Resynced from scratch: ${c.resync_reason}` : 'Resynced from scratch'}>
                <Badge variant="info">generation {c.generation}</Badge>
              </Tip>
            ) : null}
          </div>
          {c.state_message ? <div className="mt-0.5 text-xs text-muted-foreground">{c.state_message}</div> : null}
          {c.last_error && (c.state === 'failed' || c.failures > 0) ? (
            <div className="mt-1 flex items-start gap-1.5 rounded-sm bg-destructive/8 px-2 py-1 text-xs text-destructive">
              <AlertTriangle className="mt-0.5 h-3 w-3 shrink-0" /> <span className="break-all">{c.last_error}</span>
            </div>
          ) : null}
        </div>
        <div className="grid grid-cols-4 gap-x-4 text-xs sm:w-[420px]">
          <div>
            <div className="text-muted-foreground">Rows</div>
            <div className="tabular-nums">{formatCount(c.last_seq)}</div>
          </div>
          <div>
            <div className="text-muted-foreground">Merged</div>
            <div className="tabular-nums">{formatCount(c.merged_seq)}</div>
          </div>
          <div>
            <div className="text-muted-foreground">Lag</div>
            <div className="tabular-nums">{lag == null ? '–' : formatSeconds(lag)}</div>
          </div>
          <div>
            <div className="text-muted-foreground">Last commit</div>
            <Tip label={exactTime(c.last_commit_at)}>
              <div>{relativeTime(c.last_commit_at)}</div>
            </Tip>
          </div>
        </div>
        <div className="flex items-center gap-1">
          <Button asChild variant="ghost" size="sm">
            <Link to={href}>
              <Table2 /> View data
            </Link>
          </Button>
          <DropdownMenu>
            <DropdownMenuTrigger asChild>
              <Button variant="ghost" size="icon" aria-label="Collection actions">
                …
              </Button>
            </DropdownMenuTrigger>
            <DropdownMenuContent align="end">
              {c.state === 'failed' ? (
                <DropdownMenuItem onSelect={() => onAction('retry')}>
                  <RotateCcw /> Retry
                </DropdownMenuItem>
              ) : null}
              <DropdownMenuItem onSelect={() => onAction('resync')}>
                <RefreshCw /> Resync from scratch
              </DropdownMenuItem>
              <DropdownMenuItem onSelect={() => onAction(c.enabled ? 'disable' : 'enable')}>{c.enabled ? 'Disable' : 'Enable'}</DropdownMenuItem>
              <DropdownMenuItem asChild>
                <Link to={`${href.replace('?', '/health?')}`}>Table health</Link>
              </DropdownMenuItem>
            </DropdownMenuContent>
          </DropdownMenu>
        </div>
      </div>
      {copying ? (
        <div className="mt-2">
          <div className="mb-1 flex justify-between text-[11px] text-muted-foreground">
            <span>{c.state === 'resyncing' ? 'Copying again' : 'Copying existing documents'}</span>
            <span className="tabular-nums">
              {formatCount(c.snapshot_done)} / ~{formatCount(total)} · {percent(c.snapshot_done, total)}%{remaining ? ` · ~${formatDuration(remaining)} left` : ''}
            </span>
          </div>
          <Progress value={percent(c.snapshot_done, total)} indeterminate={c.snapshot_done === 0} />
        </div>
      ) : null}
    </div>
  )
}

const levelColor: Record<string, string> = { debug: 'text-muted-foreground', info: 'text-info', warn: 'text-warning', error: 'text-destructive' }

function Logs({ id, collections }: { id: string; collections: string[] }) {
  const [level, setLevel] = useState('info')
  const [collection, setCollection] = useState('')
  const [search, setSearch] = useState('')
  const [live, setLive] = useState<LogEntry[]>([])
  const q = useQuery({ queryKey: ['logs', id, level, collection, search], queryFn: () => endpoints.logs(id, { level, collection, q: search, limit: 200 }) })
  useEffect(() => setLive([]), [q.data])
  useLive(
    (e) => {
      if (e.type === 'log' && e.pipeline_id === id && e.data) {
        const l = e.data as LogEntry
        if (collection && l.collection !== collection) return
        if (level === 'warn' && l.level === 'info') return
        if (level === 'error' && l.level !== 'error') return
        if (search && !l.message.toLowerCase().includes(search.toLowerCase())) return
        setLive((prev) => [l, ...prev].slice(0, 200))
      }
    },
    [id, collection, level, search],
  )
  const all = useMemo(() => {
    const seen = new Set<number>()
    const out: LogEntry[] = []
    for (const l of [...live, ...(q.data || [])]) {
      if (seen.has(l.id)) continue
      seen.add(l.id)
      out.push(l)
    }
    return out
  }, [live, q.data])
  const listRef = useRef<HTMLDivElement>(null)
  return (
    <div className="rounded-lg border bg-card">
      <div className="flex flex-wrap items-center gap-2 border-b p-2">
        <Select value={level} onValueChange={setLevel}>
          <SelectTrigger className="w-28">
            <SelectValue />
          </SelectTrigger>
          <SelectContent>
            <SelectItem value="debug">All levels</SelectItem>
            <SelectItem value="info">Info and up</SelectItem>
            <SelectItem value="warn">Warnings</SelectItem>
            <SelectItem value="error">Errors</SelectItem>
          </SelectContent>
        </Select>
        <Select value={collection || '__all'} onValueChange={(v) => setCollection(v === '__all' ? '' : v)}>
          <SelectTrigger className="w-48">
            <SelectValue />
          </SelectTrigger>
          <SelectContent>
            <SelectItem value="__all">All collections</SelectItem>
            {collections.map((c) => (
              <SelectItem key={c} value={c}>
                {c}
              </SelectItem>
            ))}
          </SelectContent>
        </Select>
        <div className="relative min-w-[200px] flex-1">
          <Search className="absolute left-2.5 top-1/2 h-3.5 w-3.5 -translate-y-1/2 text-muted-foreground" />
          <Input className="pl-8" placeholder="Search messages" value={search} onChange={(e) => setSearch(e.target.value)} />
        </div>
        <span className="flex items-center gap-1.5 text-[11px] text-muted-foreground">
          <span className="h-1.5 w-1.5 animate-pulse rounded-full bg-success" /> live
        </span>
      </div>
      <div ref={listRef} className="max-h-[420px] overflow-auto font-mono text-xs">
        {q.isLoading ? <div className="p-4 text-muted-foreground">Loading…</div> : null}
        {all.length === 0 && !q.isLoading ? <div className="p-6 text-center text-muted-foreground">No log lines match.</div> : null}
        {all.map((l) => (
          <div key={l.id} className="flex gap-3 border-b px-3 py-1.5 last:border-0 hover:bg-accent/40">
            <Tip label={exactTime(l.at)}>
              <span className="shrink-0 tabular-nums text-muted-foreground">{new Date(l.at).toLocaleTimeString()}</span>
            </Tip>
            <span className={cn('w-10 shrink-0 uppercase', levelColor[l.level])}>{l.level}</span>
            <span className="w-36 shrink-0 truncate text-muted-foreground">{l.collection.split('.').pop()}</span>
            <span className="min-w-0 flex-1 break-words">
              {l.message}
              {l.fields && Object.keys(l.fields).length ? (
                <span className="ml-2 text-muted-foreground">
                  {Object.entries(l.fields)
                    .map(([k, v]) => `${k}=${typeof v === 'object' ? JSON.stringify(v) : String(v)}`)
                    .join(' ')}
                </span>
              ) : null}
            </span>
          </div>
        ))}
      </div>
    </div>
  )
}

const runStatus: Record<string, 'success' | 'destructive' | 'info' | 'muted' | 'warning'> = { ok: 'success', failed: 'destructive', running: 'info', stopped: 'muted', skipped: 'muted', interrupted: 'warning' }

function Runs({ id }: { id: string }) {
  const q = useQuery({ queryKey: ['runs', id], queryFn: () => endpoints.runs(id, 200), refetchInterval: 15000 })
  if (q.isLoading) return <ListSkeleton />
  if (!q.data?.length) return <EmptyState title="No runs yet" description="Copies, merges and maintenance jobs show up here." className="py-8" />
  return (
    <div className="overflow-hidden rounded-lg border bg-card">
      <table className="w-full text-[13px]">
        <thead className="bg-muted/50 text-xs text-muted-foreground">
          <tr>
            <th className="px-3 py-2 text-left font-medium">Kind</th>
            <th className="px-3 py-2 text-left font-medium">Collection</th>
            <th className="px-3 py-2 text-left font-medium">Status</th>
            <th className="px-3 py-2 text-right font-medium">Rows</th>
            <th className="px-3 py-2 text-right font-medium">Bytes</th>
            <th className="px-3 py-2 text-left font-medium">Started</th>
            <th className="px-3 py-2 text-right font-medium">Duration</th>
          </tr>
        </thead>
        <tbody className="divide-y">
          {q.data.map((r: Run) => (
            <tr key={r.id} className="hover:bg-accent/40">
              <td className="px-3 py-1.5 capitalize">{r.kind}</td>
              <td className="px-3 py-1.5 font-mono text-xs">{r.collection}</td>
              <td className="px-3 py-1.5">
                <Badge variant={runStatus[r.status] || 'secondary'}>{r.status}</Badge>
                {r.error ? <span className="ml-2 text-xs text-destructive">{r.error.slice(0, 120)}</span> : null}
                {r.details && typeof r.details === 'object' && 'result' in r.details && (r.details as { result?: { reason?: string } }).result?.reason ? <span className="ml-2 text-xs text-muted-foreground">{(r.details as { result?: { reason?: string } }).result?.reason}</span> : null}
              </td>
              <td className="px-3 py-1.5 text-right tabular-nums">{formatCount(r.rows)}</td>
              <td className="px-3 py-1.5 text-right tabular-nums">{r.bytes ? formatBytes(r.bytes) : '–'}</td>
              <td className="px-3 py-1.5">
                <Tip label={exactTime(r.started_at)}>
                  <span>{relativeTime(r.started_at)}</span>
                </Tip>
              </td>
              <td className="px-3 py-1.5 text-right tabular-nums">{r.ended_at ? formatDuration(new Date(r.ended_at).getTime() - new Date(r.started_at).getTime()) : 'running'}</td>
            </tr>
          ))}
        </tbody>
      </table>
    </div>
  )
}
