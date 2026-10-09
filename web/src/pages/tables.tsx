import { useMemo, useState } from 'react'
import { Link } from 'react-router-dom'
import { useQuery } from '@tanstack/react-query'
import { Database, HeartPulse, RefreshCw, Search, Table2 } from 'lucide-react'
import { endpoints, type TableView } from '@/lib/api'
import { Page } from '@/components/layout'
import { PageHeader, EmptyState, ErrorState, ListSkeleton } from '@/components/states'
import { Button } from '@/components/ui/button'
import { Input } from '@/components/ui/input'
import { Badge } from '@/components/ui/badge'
import { StatusBadge } from '@/components/status-badge'
import { Tip } from '@/components/ui/tooltip'
import { exactTime, formatBytes, formatCount, relativeTime } from '@/lib/utils'

export default function Tables() {
  const q = useQuery({ queryKey: ['tables'], queryFn: () => endpoints.tables(), refetchInterval: 30000 })
  const [filter, setFilter] = useState('')
  const [showRaw, setShowRaw] = useState(false)
  const groups = useMemo(() => {
    const m = new Map<string, TableView[]>()
    for (const t of q.data || []) {
      if (!showRaw && t.kind === 'raw') continue
      if (filter && !`${t.namespace}.${t.name}`.toLowerCase().includes(filter.toLowerCase())) continue
      const k = `${t.destination_name}|${t.namespace}`
      if (!m.has(k)) m.set(k, [])
      m.get(k)!.push(t)
    }
    return Array.from(m.entries())
  }, [q.data, filter, showRaw])
  return (
    <Page>
      <PageHeader
        title="Tables"
        description="Every namespace and table in your destinations, with row counts, size and health."
        actions={
          <>
            <Button variant={showRaw ? 'secondary' : 'outline'} size="sm" onClick={() => setShowRaw((s) => !s)}>
              {showRaw ? 'Hide raw tables' : 'Show raw tables'}
            </Button>
            <Button variant="outline" size="sm" onClick={() => q.refetch()} loading={q.isFetching}>
              <RefreshCw /> Refresh
            </Button>
          </>
        }
      />
      <div className="relative mb-4 max-w-sm">
        <Search className="absolute left-2.5 top-1/2 h-3.5 w-3.5 -translate-y-1/2 text-muted-foreground" />
        <Input className="pl-8" placeholder="Filter tables" value={filter} onChange={(e) => setFilter(e.target.value)} />
      </div>
      {q.isLoading ? <ListSkeleton /> : q.error ? <ErrorState error={q.error} onRetry={() => q.refetch()} /> : null}
      {q.data && groups.length === 0 ? <EmptyState icon={<Table2 className="h-5 w-5" />} title="No tables yet" description="Tables appear once a pipeline starts copying." /> : null}
      <div className="space-y-5">
        {groups.map(([k, list]) => {
          const [dest, ns] = k.split('|')
          return (
            <section key={k}>
              <h2 className="mb-2 flex items-center gap-2 text-xs font-semibold uppercase tracking-wide text-muted-foreground">
                <Database className="h-3.5 w-3.5" /> {ns} <span className="font-normal normal-case">· {dest}</span>
              </h2>
              <div className="overflow-hidden rounded-lg border bg-card">
                <table className="w-full text-[13px]">
                  <thead className="bg-muted/50 text-xs text-muted-foreground">
                    <tr>
                      <th className="px-3 py-2 text-left font-medium">Table</th>
                      <th className="px-3 py-2 text-right font-medium">Rows</th>
                      <th className="px-3 py-2 text-right font-medium">Size</th>
                      <th className="px-3 py-2 text-left font-medium">Last updated</th>
                      <th className="px-3 py-2 text-left font-medium">State</th>
                      <th className="px-3 py-2 text-right font-medium"></th>
                    </tr>
                  </thead>
                  <tbody className="divide-y">
                    {list.map((t) => {
                      const href = `/tables/${encodeURIComponent(t.namespace)}/${encodeURIComponent(t.name)}?destination=${t.destination_id}`
                      return (
                        <tr key={t.name} className="hover:bg-accent/40">
                          <td className="px-3 py-2">
                            <Link to={href} className="font-medium hover:underline">
                              {t.name}
                            </Link>
                            <Badge variant={t.kind === 'raw' ? 'muted' : t.kind === 'clean' ? 'default' : 'outline'} className="ml-2">
                              {t.kind === 'clean' ? 'clean' : t.kind}
                            </Badge>
                          </td>
                          <td className="px-3 py-2 text-right tabular-nums">{formatCount(t.row_count)}</td>
                          <td className="px-3 py-2 text-right tabular-nums">{formatBytes(t.size_bytes)}</td>
                          <td className="px-3 py-2">
                            <Tip label={exactTime(t.last_updated)}>
                              <span>{relativeTime(t.last_updated)}</span>
                            </Tip>
                          </td>
                          <td className="px-3 py-2">{t.state ? <StatusBadge state={t.state} /> : <span className="text-xs text-muted-foreground">not managed</span>}</td>
                          <td className="px-3 py-2 text-right">
                            <Button asChild variant="ghost" size="sm">
                              <Link to={`${href.replace('?', '/health?')}`}>
                                <HeartPulse /> Health
                              </Link>
                            </Button>
                          </td>
                        </tr>
                      )
                    })}
                  </tbody>
                </table>
              </div>
            </section>
          )
        })}
      </div>
    </Page>
  )
}
