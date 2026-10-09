import { Link, useNavigate } from 'react-router-dom'
import { useQuery } from '@tanstack/react-query'
import { AlertTriangle, ArrowRight, GitBranch, Plus, Sparkles } from 'lucide-react'
import { endpoints } from '@/lib/api'
import { Page } from '@/components/layout'
import { PageHeader, EmptyState, ErrorState, Stat, CardSkeleton } from '@/components/states'
import { Button } from '@/components/ui/button'
import { StatusBadge } from '@/components/status-badge'
import { formatCount, formatSeconds, relativeTime } from '@/lib/utils'
import { PipelineCard } from '@/pages/pipelines'

export default function Overview() {
  const nav = useNavigate()
  const q = useQuery({ queryKey: ['overview'], queryFn: endpoints.overview, refetchInterval: 15000 })
  if (q.isLoading)
    return (
      <Page>
        <PageHeader title="Overview" />
        <div className="grid gap-3 md:grid-cols-4">
          <CardSkeleton />
          <CardSkeleton />
          <CardSkeleton />
          <CardSkeleton />
        </div>
      </Page>
    )
  if (q.error)
    return (
      <Page>
        <PageHeader title="Overview" />
        <ErrorState error={q.error} onRetry={() => q.refetch()} />
      </Page>
    )
  const d = q.data!
  if (d.first_run) {
    return (
      <Page>
        <PageHeader title="Welcome" description="Nothing is set up yet. The guided setup takes you from an empty app to streaming data in three steps." />
        <EmptyState
          icon={<Sparkles className="h-5 w-5" />}
          title="Connect MongoDB, pick a destination, choose collections."
          description="Each step runs a connection test with plain-English hints, so problems are caught before the first copy starts."
          action={
            <Button onClick={() => nav('/setup')}>
              Start setup <ArrowRight />
            </Button>
          }
        />
      </Page>
    )
  }
  return (
    <Page>
      <PageHeader
        title="Overview"
        description={`${d.pipelines.length} pipeline${d.pipelines.length === 1 ? '' : 's'} · ${d.connections} connection${d.connections === 1 ? '' : 's'}`}
        actions={
          <Button asChild>
            <Link to="/pipelines/new">
              <Plus /> New pipeline
            </Link>
          </Button>
        }
      />
      <div className="grid gap-3 md:grid-cols-4">
        <Stat label="Rows synced" value={formatCount(d.totals.rows_synced)} sub="events written to raw tables" />
        <Stat label="Events, last hour" value={formatCount(d.totals.events_last_hour)} />
        <Stat label="Current lag" value={d.totals.lag_seconds == null ? '–' : formatSeconds(d.totals.lag_seconds)} sub="now minus source time of the last applied event" />
        <Stat label="Needs attention" value={d.totals.attention} sub={d.totals.attention ? 'see below' : 'all clear'} className={d.totals.attention ? 'border-warning/50' : ''} />
      </div>
      {d.attention.length ? (
        <section className="mt-6">
          <h2 className="mb-2 flex items-center gap-1.5 text-xs font-semibold uppercase tracking-wide text-muted-foreground">
            <AlertTriangle className="h-3.5 w-3.5 text-warning" /> Needs attention
          </h2>
          <div className="divide-y rounded-lg border bg-card">
            {d.attention.map((a, i) => (
              <Link key={i} to={a.state === 'conflicts' ? `/tables/${encodeURIComponent(a.table.split('.')[0])}/${encodeURIComponent(a.table.split('.').slice(1).join('.'))}/health` : `/pipelines/${a.pipeline_id}`} className="flex items-center gap-3 px-4 py-2.5 hover:bg-accent/50">
                <StatusBadge state={a.state === 'conflicts' ? 'source_removed' : a.state} className={a.state === 'conflicts' ? 'hidden' : ''} />
                {a.state === 'conflicts' ? <span className="rounded-full bg-warning/18 px-2 py-0.5 text-[11px] font-medium text-warning">Type conflicts</span> : null}
                <span className="text-[13px] font-medium">{a.table}</span>
                <span className="truncate text-xs text-muted-foreground">{a.reason}</span>
                <ArrowRight className="ml-auto h-4 w-4 text-muted-foreground" />
              </Link>
            ))}
          </div>
        </section>
      ) : null}
      <section className="mt-6">
        <h2 className="mb-2 text-xs font-semibold uppercase tracking-wide text-muted-foreground">Pipelines</h2>
        {d.pipelines.length === 0 ? (
          <EmptyState icon={<GitBranch className="h-5 w-5" />} title="No pipelines yet" description="A pipeline copies collections from one source into one destination and keeps them in sync." action={<Button asChild><Link to="/pipelines/new"><Plus /> New pipeline</Link></Button>} />
        ) : (
          <div className="grid gap-3 lg:grid-cols-2">
            {d.pipelines.map((p) => (
              <PipelineCard key={p.id} p={p} />
            ))}
          </div>
        )}
        <div className="mt-2 text-[11px] text-muted-foreground">Updated {relativeTime(new Date(q.dataUpdatedAt).toISOString())}</div>
      </section>
    </Page>
  )
}
