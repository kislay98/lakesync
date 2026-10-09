import { Link, useParams, useSearchParams } from 'react-router-dom'
import { useMutation, useQuery, useQueryClient } from '@tanstack/react-query'
import { toast } from 'sonner'
import { ChevronRight, Table2, Wrench } from 'lucide-react'
import { endpoints } from '@/lib/api'
import { Page } from '@/components/layout'
import { PageHeader, ErrorState, ListSkeleton, Stat } from '@/components/states'
import { Button } from '@/components/ui/button'
import { Badge } from '@/components/ui/badge'
import { HealthBadge } from '@/components/status-badge'
import { Card, CardContent, CardDescription, CardHeader, CardTitle } from '@/components/ui/card'
import { Tip } from '@/components/ui/tooltip'
import { cn, exactTime, formatBytes, formatCount, relativeTime } from '@/lib/utils'

export default function TableHealthPage() {
  const { ns = '', table = '' } = useParams()
  const [sp] = useSearchParams()
  const dest = sp.get('destination') || undefined
  const qc = useQueryClient()
  const q = useQuery({ queryKey: ['health', ns, table, dest], queryFn: () => endpoints.tableHealth(ns, table, dest) })
  const snaps = useQuery({ queryKey: ['snapshots', ns, table, dest], queryFn: () => endpoints.tableSnapshots(ns, table, dest) })
  const maint = useMutation({
    mutationFn: () => endpoints.maintenance(ns, table, dest),
    onSuccess: (r) => {
      toast.success(r.queued ? r.message || 'Maintenance queued' : `Maintenance finished${r.errors?.length ? ` with ${r.errors.length} error(s)` : ''}`)
      qc.invalidateQueries({ queryKey: ['health', ns, table, dest] })
      qc.invalidateQueries({ queryKey: ['tables'] })
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
  const h = q.data.health
  const t = q.data.thresholds
  const href = `/tables/${encodeURIComponent(ns)}/${encodeURIComponent(table)}${dest ? `?destination=${dest}` : ''}`
  return (
    <Page>
      <PageHeader
        crumbs={
          <span className="flex items-center gap-1">
            <Link to="/tables" className="hover:underline">
              Tables
            </Link>
            <ChevronRight className="h-3 w-3" />
            <Link to={href} className="font-mono hover:underline">
              {ns}.{table}
            </Link>
          </span>
        }
        title={
          <span className="flex items-center gap-2">
            Table health <HealthBadge status={q.data.status} />
          </span>
        }
        description={q.data.reason}
        actions={
          <>
            <Button asChild variant="outline">
              <Link to={href}>
                <Table2 /> View data
              </Link>
            </Button>
            <Button onClick={() => maint.mutate()} loading={maint.isPending} disabled={!q.data.pipeline}>
              <Wrench /> Run maintenance now
            </Button>
          </>
        }
      />
      <div className="grid gap-3 md:grid-cols-4">
        <Stat label="Data files" value={formatCount(h.data_files)} sub={`${h.small_files} under ${t.SmallFileBytes ? formatBytes(t.SmallFileBytes) : '16 MB'}`} className={h.small_files > (t.SmallFilesTrigger || 50) ? 'border-warning/50' : ''} />
        <Stat label="Average file size" value={formatBytes(h.avg_file_bytes)} sub={`target ${formatBytes(t.TargetFileBytes || 128 << 20)}`} />
        <Stat label="Delete files" value={formatCount(h.delete_files)} sub="positional deletes from merges" className={h.delete_files > (t.DeleteFilesTrigger || 20) ? 'border-warning/50' : ''} />
        <Stat label="Total size" value={formatBytes(h.total_bytes)} sub={`${formatCount(h.row_count)} rows in data files`} />
        <Stat label="Snapshots" value={formatCount(h.snapshots)} sub={h.gc_enabled ? 'expiry managed by the app' : 'expiry managed by the catalog (gc.enabled=false)'} />
        <Stat label="Last commit" value={relativeTime(h.last_commit)} sub={exactTime(h.last_commit)} />
        <Stat label="Last compaction" value={h.last_compaction && !h.last_compaction.startsWith('0001') ? relativeTime(h.last_compaction) : 'never'} />
        <Stat label="Partitions" value={h.partitions} sub={`Iceberg format v${h.format_version}`} />
      </div>
      {q.data.pipeline ? (
        <div className="mt-3 text-xs text-muted-foreground">
          Managed by pipeline{' '}
          <Link to={`/pipelines/${q.data.pipeline.id}`} className="font-medium text-foreground hover:underline">
            {q.data.pipeline.name}
          </Link>{' '}
          · collection <span className="font-mono">{q.data.pipeline.collection}</span> · state {q.data.pipeline.state}
        </div>
      ) : null}
      <div className="mt-6 grid gap-4 lg:grid-cols-2">
        <Card>
          <CardHeader>
            <CardTitle>Schema and type conflicts</CardTitle>
            <CardDescription>{q.data.conflicts ? `${formatCount(q.data.conflicts)} values did not fit their column and were stored as null (the true value is kept in _cdc_doc).` : 'No type conflicts recorded.'}</CardDescription>
          </CardHeader>
          <CardContent>
            <table className="w-full text-xs">
              <thead className="text-muted-foreground">
                <tr>
                  <th className="py-1 text-left font-medium">Source field</th>
                  <th className="py-1 text-left font-medium">Column</th>
                  <th className="py-1 text-left font-medium">Type</th>
                  <th className="py-1 text-right font-medium">Conflicts</th>
                </tr>
              </thead>
              <tbody className="divide-y">
                {(q.data.mappings || []).map((m) => (
                  <tr key={m.source_field} className={cn(m.conflict_count > 0 && 'bg-warning/8')}>
                    <td className="py-1 font-mono">{m.source_field}</td>
                    <td className="py-1 font-mono">
                      {m.column_name}
                      {m.reason ? (
                        <Tip label={m.reason}>
                          <Badge variant="warning" className="ml-1">
                            renamed
                          </Badge>
                        </Tip>
                      ) : null}
                    </td>
                    <td className="py-1 text-muted-foreground">{m.type}</td>
                    <td className={cn('py-1 text-right tabular-nums', m.conflict_count > 0 && 'font-medium text-warning')}>{m.conflict_count}</td>
                  </tr>
                ))}
                {!q.data.mappings?.length ? (
                  <tr>
                    <td colSpan={4} className="py-3 text-center text-muted-foreground">
                      No field mappings (not a pipeline table).
                    </td>
                  </tr>
                ) : null}
              </tbody>
            </table>
          </CardContent>
        </Card>
        <Card>
          <CardHeader>
            <CardTitle>Maintenance</CardTitle>
            <CardDescription>Compaction rewrites small files into {formatBytes(t.TargetFileBytes || 128 << 20)} files and applies delete files. Every job verifies row counts and a content checksum.</CardDescription>
          </CardHeader>
          <CardContent>
            {q.data.maintenance_runs?.length ? (
              <ul className="divide-y text-xs">
                {q.data.maintenance_runs.map((r) => {
                  const det = r.details as { kind?: string; table?: string; result?: { reason?: string; skipped?: boolean; files_before?: number; files_after?: number } } | undefined
                  return (
                    <li key={r.id} className="flex items-start gap-2 py-1.5">
                      <Badge variant={r.status === 'ok' ? 'success' : r.status === 'skipped' ? 'muted' : 'destructive'}>{r.status}</Badge>
                      <div className="min-w-0 flex-1">
                        <div>
                          <span className="font-medium">{det?.kind}</span> on <span className="font-mono">{det?.table}</span>
                        </div>
                        <div className="text-muted-foreground">{r.error || det?.result?.reason || (det?.result && det.result.files_before != null ? `${det.result.files_before} → ${det.result.files_after} files` : '')}</div>
                      </div>
                      <span className="shrink-0 text-muted-foreground" title={exactTime(r.started_at)}>
                        {relativeTime(r.started_at)}
                      </span>
                    </li>
                  )
                })}
              </ul>
            ) : (
              <div className="text-xs text-muted-foreground">No maintenance has run yet. Jobs run automatically when thresholds are exceeded, or now with the button above.</div>
            )}
          </CardContent>
        </Card>
        <Card className="lg:col-span-2">
          <CardHeader>
            <CardTitle>Snapshots</CardTitle>
            <CardDescription>{!h.gc_enabled ? 'This catalog serves only the current snapshot; history lives in the catalog.' : 'Every commit creates a snapshot.'}</CardDescription>
          </CardHeader>
          <CardContent>
            {snaps.data?.length ? (
              <table className="w-full text-xs">
                <thead className="text-muted-foreground">
                  <tr>
                    <th className="py-1 text-left font-medium">Snapshot</th>
                    <th className="py-1 text-left font-medium">Operation</th>
                    <th className="py-1 text-left font-medium">When</th>
                    <th className="py-1 text-left font-medium">Checkpoint</th>
                    <th className="py-1 text-right font-medium">Records</th>
                  </tr>
                </thead>
                <tbody className="divide-y">
                  {snaps.data.map((s) => (
                    <tr key={s.id}>
                      <td className="py-1 font-mono">
                        {s.id}
                        {s.is_current ? <Badge className="ml-1">current</Badge> : null}
                      </td>
                      <td className="py-1">{s.operation}</td>
                      <td className="py-1">{exactTime(s.timestamp)}</td>
                      <td className="py-1 font-mono text-muted-foreground">
                        {s.summary['lakesync.phase'] ? `${s.summary['lakesync.phase']} · seq ${s.summary['lakesync.seq']} · gen ${s.summary['lakesync.generation']}` : s.summary['lakesync.maintenance'] ? `maintenance: ${s.summary['lakesync.maintenance']}` : '–'}
                      </td>
                      <td className="py-1 text-right tabular-nums">{s.summary['total-records'] ? formatCount(Number(s.summary['total-records'])) : '–'}</td>
                    </tr>
                  ))}
                </tbody>
              </table>
            ) : (
              <div className="text-xs text-muted-foreground">No snapshots.</div>
            )}
          </CardContent>
        </Card>
      </div>
    </Page>
  )
}
