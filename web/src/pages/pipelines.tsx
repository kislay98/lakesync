import { Link } from 'react-router-dom'
import { useQuery } from '@tanstack/react-query'
import { ArrowRight, GitBranch, Plus } from 'lucide-react'
import { endpoints, type Pipeline } from '@/lib/api'
import { Page } from '@/components/layout'
import { PageHeader, EmptyState, ErrorState, ListSkeleton } from '@/components/states'
import { Button } from '@/components/ui/button'
import { StatusBadge } from '@/components/status-badge'
import { Sparkline } from '@/components/sparkline'
import { Tip } from '@/components/ui/tooltip'
import { exactTime, formatCount, formatSeconds, percent, relativeTime } from '@/lib/utils'
import { Progress } from '@/components/ui/misc'

export default function Pipelines() {
  const q = useQuery({ queryKey: ['pipelines'], queryFn: endpoints.pipelines, refetchInterval: 15000 })
  return (
    <Page>
      <PageHeader
        title="Pipelines"
        description="Each pipeline keeps a set of collections in sync with Iceberg tables."
        actions={
          <Button asChild>
            <Link to="/pipelines/new">
              <Plus /> New pipeline
            </Link>
          </Button>
        }
      />
      {q.isLoading ? <ListSkeleton /> : q.error ? <ErrorState error={q.error} onRetry={() => q.refetch()} /> : null}
      {q.data?.length === 0 ? (
        <EmptyState icon={<GitBranch className="h-5 w-5" />} title="No pipelines yet" description="Pick a source and a destination, tick the collections, and start." action={<Button asChild><Link to="/pipelines/new"><Plus /> New pipeline</Link></Button>} />
      ) : null}
      {q.data?.length ? (
        <div className="grid gap-3 lg:grid-cols-2">
          {q.data.map((p) => (
            <PipelineCard key={p.id} p={p} />
          ))}
        </div>
      ) : null}
    </Page>
  )
}

export function PipelineCard({ p }: { p: Pipeline }) {
  const series = useQuery({ queryKey: ['series', p.id, 60], queryFn: () => endpoints.series(p.id, 60), refetchInterval: 60000 })
  const copying = p.collections.filter((c) => c.enabled && (c.state === 'copying' || c.state === 'resyncing'))
  const done = copying.reduce((a, c) => a + c.snapshot_done, 0)
  const total = copying.reduce((a, c) => a + Math.max(c.snapshot_total_est, c.snapshot_done), 0)
  const last = p.collections.map((c) => c.last_commit_at).filter(Boolean).sort().pop() || null
  return (
    <Link to={`/pipelines/${p.id}`} className="group rounded-lg border bg-card p-4 transition-colors hover:border-primary/50">
      <div className="flex items-start justify-between gap-3">
        <div className="min-w-0">
          <div className="flex items-center gap-2">
            <span className="truncate text-sm font-semibold">{p.name}</span>
            <StatusBadge state={p.state} />
          </div>
          <div className="mt-0.5 truncate text-xs text-muted-foreground">
            {p.source.name} → {p.destination.name} · namespace <span className="font-mono">{p.namespace}</span> · every {p.sync_interval_s}s
          </div>
        </div>
        <ArrowRight className="h-4 w-4 shrink-0 text-muted-foreground opacity-0 transition-opacity group-hover:opacity-100" />
      </div>
      {copying.length ? (
        <div className="mt-3">
          <div className="mb-1 flex justify-between text-[11px] text-muted-foreground">
            <span>
              Copying {copying.length} collection{copying.length > 1 ? 's' : ''}
            </span>
            <span className="tabular-nums">
              {formatCount(done)} / {formatCount(total)} · {percent(done, total)}%
            </span>
          </div>
          <Progress value={percent(done, total)} />
        </div>
      ) : null}
      <div className="mt-3 grid grid-cols-4 gap-2 text-xs">
        <div>
          <div className="text-muted-foreground">Collections</div>
          <div className="font-medium tabular-nums">{p.stats.collections}</div>
        </div>
        <div>
          <div className="text-muted-foreground">Rows</div>
          <div className="font-medium tabular-nums">{formatCount(p.stats.rows_synced)}</div>
        </div>
        <div>
          <div className="text-muted-foreground">Lag</div>
          <div className="font-medium tabular-nums">{p.stats.lag_seconds == null ? '–' : formatSeconds(p.stats.lag_seconds)}</div>
        </div>
        <div>
          <div className="text-muted-foreground">Last commit</div>
          <Tip label={exactTime(last)}>
            <div className="font-medium">{relativeTime(last)}</div>
          </Tip>
        </div>
      </div>
      <div className="mt-3 flex items-end justify-between">
        <div className="text-[11px] text-muted-foreground">
          {formatCount(p.stats.events_last_hour)} events in the last hour
        </div>
        <div className="text-primary">
          <Sparkline values={(series.data || []).map((m) => m.events)} width={140} height={26} />
        </div>
      </div>
    </Link>
  )
}
