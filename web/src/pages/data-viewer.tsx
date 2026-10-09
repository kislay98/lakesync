import { useCallback, useEffect, useMemo, useState } from 'react'
import { Link, useParams, useSearchParams } from 'react-router-dom'
import { useInfiniteQuery, useQuery } from '@tanstack/react-query'
import { ChevronRight, Columns3, Filter, HeartPulse, History, Layers, Search, X } from 'lucide-react'
import { endpoints, type ColumnMeta, type Filter as ApiFilter, type HistoryEvent } from '@/lib/api'
import { Page } from '@/components/layout'
import { PageHeader, ErrorState, EmptyState } from '@/components/states'
import { Button } from '@/components/ui/button'
import { Input } from '@/components/ui/input'
import { Badge } from '@/components/ui/badge'
import { Sheet, SheetContent, SheetDescription, SheetHeader, SheetTitle } from '@/components/ui/sheet'
import { DropdownMenu, DropdownMenuCheckboxItem, DropdownMenuContent, DropdownMenuLabel, DropdownMenuSeparator, DropdownMenuTrigger } from '@/components/ui/dropdown-menu'
import { Select, SelectContent, SelectItem, SelectTrigger, SelectValue } from '@/components/ui/select'
import { Tabs, TabsContent, TabsList, TabsTrigger } from '@/components/ui/tabs'
import { DataGrid, type GridSort } from '@/components/data-grid'
import { JsonView } from '@/components/json-view'
import { StatusBadge } from '@/components/status-badge'
import { Skeleton } from '@/components/ui/misc'
import { cn, exactTime, formatCount, formatDuration, isJsonString, relativeTime } from '@/lib/utils'

const PAGE = 200
const ops: { v: ApiFilter['op']; label: string; args: number }[] = [
  { v: 'eq', label: 'equals', args: 1 },
  { v: 'neq', label: 'not equals', args: 1 },
  { v: 'contains', label: 'contains', args: 1 },
  { v: 'starts', label: 'starts with', args: 1 },
  { v: 'gt', label: '>', args: 1 },
  { v: 'gte', label: '≥', args: 1 },
  { v: 'lt', label: '<', args: 1 },
  { v: 'lte', label: '≤', args: 1 },
  { v: 'between', label: 'between', args: 2 },
  { v: 'isnull', label: 'is null', args: 0 },
  { v: 'notnull', label: 'is not null', args: 0 },
]

export default function DataViewer() {
  const { ns = '', table = '' } = useParams()
  const [sp, setSp] = useSearchParams()
  const dest = sp.get('destination') || undefined
  const schema = useQuery({ queryKey: ['schema', ns, table, dest], queryFn: () => endpoints.tableSchema(ns, table, dest) })
  const isRaw = table.endsWith('__raw')
  const cleanTable = schema.data?.pipeline?.clean_table || (isRaw ? table.replace(/__raw$/, '') : table)
  const rawTable = schema.data?.pipeline?.raw_table || (isRaw ? table : `${table}__raw`)
  const [sort, setSort] = useState<GridSort | null>(null)
  const [filters, setFilters] = useState<ApiFilter[]>([])
  const [search, setSearch] = useState('')
  const [debounced, setDebounced] = useState('')
  const [hidden, setHidden] = useState<Set<string>>(new Set())
  const [selected, setSelected] = useState<{ row: unknown[]; index: number } | null>(null)
  const [cell, setCell] = useState<{ column: ColumnMeta; value: unknown } | null>(null)
  const [draft, setDraft] = useState<{ column: string; op: ApiFilter['op']; value: string; value2: string } | null>(null)
  useEffect(() => {
    const t = setTimeout(() => setDebounced(search), 300)
    return () => clearTimeout(t)
  }, [search])
  const [hiddenInit, setHiddenInit] = useState('')
  useEffect(() => {
    setSort(null)
    setFilters([])
    setSearch('')
    setSelected(null)
    setHidden(new Set())
    setHiddenInit('')
  }, [ns, table])
  const sortParam = sort ? `${sort.desc ? '-' : ''}${sort.column}` : undefined
  const rows = useInfiniteQuery({
    queryKey: ['rows', ns, table, dest, sortParam, filters, debounced],
    queryFn: ({ pageParam }) => endpoints.tableRows(ns, table, { limit: PAGE, cursor: pageParam || undefined, sort: sortParam, filters, q: debounced, destination: dest, total: !pageParam }),
    initialPageParam: '',
    getNextPageParam: (last) => (last.has_more ? last.next_cursor : undefined),
  })
  const columns = rows.data?.pages[0]?.columns || []
  // default column visibility: clean tables hide the bookkeeping columns, raw tables show the event ones
  useEffect(() => {
    if (!columns.length || hiddenInit === table) return
    const hide = isRaw ? ['_cdc_key', '_cdc_generation', '_cdc_ingested_at', '_cdc_doc'] : ['_cdc_key', '_cdc_seq', '_cdc_generation', '_cdc_ingested_at', '_cdc_source_ts']
    setHidden(new Set(hide.filter((h) => columns.some((c) => c.name === h))))
    setHiddenInit(table)
  }, [columns, isRaw, table, hiddenInit])
  const allRows = useMemo(() => rows.data?.pages.flatMap((p) => p.rows) || [], [rows.data])
  const total = rows.data?.pages[0]?.total
  const visibleColumns = useMemo(() => columns.filter((c) => !hidden.has(c.name)), [columns, hidden])
  const keyIdx = columns.findIndex((c) => c.name === '_cdc_key')
  const idIdx = columns.findIndex((c) => c.name === '_id')
  const onEnd = useCallback(() => {
    if (rows.hasNextPage && !rows.isFetchingNextPage) rows.fetchNextPage()
  }, [rows])
  const addFilter = () => {
    if (!draft) return
    const op = ops.find((o) => o.v === draft.op)!
    const f: ApiFilter = { column: draft.column, op: draft.op }
    if (op.args >= 1) f.value = draft.value
    if (op.args === 2) f.value2 = draft.value2
    setFilters((fs) => [...fs, f])
    setDraft(null)
  }
  const switchTable = (t: string) => {
    setSp((prev) => {
      const n = new URLSearchParams(prev)
      return n
    })
    window.history.replaceState(null, '', `/tables/${encodeURIComponent(ns)}/${encodeURIComponent(t)}${dest ? `?destination=${dest}` : ''}`)
    window.dispatchEvent(new PopStateEvent('popstate'))
  }
  return (
    <Page full className="max-w-none">
      <PageHeader
        crumbs={
          <span className="flex items-center gap-1">
            <Link to="/tables" className="hover:underline">
              Tables
            </Link>
            <ChevronRight className="h-3 w-3" /> <span className="font-mono">{ns}</span>
          </span>
        }
        title={
          <span className="flex items-center gap-2 font-mono">
            {table}
            {schema.data?.pipeline ? <StatusBadge state={schema.data.pipeline.state} /> : null}
            {isRaw ? <Badge variant="muted">raw events</Badge> : null}
          </span>
        }
        description={
          total != null ? (
            <span>
              {formatCount(total, { exact: true })} row{total === 1 ? '' : 's'}
              {filters.length || debounced ? ' match' : ''} · {columns.length} columns{rows.data?.pages[0] ? ` · ${formatDuration(rows.data.pages[0].duration_ms)}` : ''}
            </span>
          ) : (
            <span>{isRaw ? 'Every change event, append only.' : 'Latest version of each document.'}</span>
          )
        }
        actions={
          <>
            {schema.data?.pipeline || isRaw ? (
              <div className="flex rounded-md border bg-card p-0.5 text-xs">
                <button type="button" className={cn('rounded px-2.5 py-1', !isRaw && 'bg-accent font-medium')} onClick={() => isRaw && switchTable(cleanTable)}>
                  Clean
                </button>
                <button type="button" className={cn('rounded px-2.5 py-1', isRaw && 'bg-accent font-medium')} onClick={() => !isRaw && switchTable(rawTable)}>
                  Raw
                </button>
              </div>
            ) : null}
            <Button asChild variant="outline" size="sm">
              <Link to={`/tables/${encodeURIComponent(ns)}/${encodeURIComponent(table)}/health${dest ? `?destination=${dest}` : ''}`}>
                <HeartPulse /> Health
              </Link>
            </Button>
          </>
        }
      />
      <div className="mb-3 flex flex-wrap items-center gap-2">
        <div className="relative w-72">
          <Search className="absolute left-2.5 top-1/2 h-3.5 w-3.5 -translate-y-1/2 text-muted-foreground" />
          <Input className="pl-8" placeholder="Search all text columns" value={search} onChange={(e) => setSearch(e.target.value)} />
        </div>
        <DropdownMenu>
          <DropdownMenuTrigger asChild>
            <Button variant="outline" size="sm">
              <Filter /> Add filter
            </Button>
          </DropdownMenuTrigger>
          <DropdownMenuContent className="w-80 p-3" align="start">
            <div className="space-y-2">
              <Select value={draft?.column || ''} onValueChange={(c) => setDraft({ column: c, op: draft?.op || 'eq', value: draft?.value || '', value2: draft?.value2 || '' })}>
                <SelectTrigger>
                  <SelectValue placeholder="Column" />
                </SelectTrigger>
                <SelectContent>
                  {columns.map((c) => (
                    <SelectItem key={c.name} value={c.name}>
                      {c.name} <span className="text-muted-foreground">· {c.type}</span>
                    </SelectItem>
                  ))}
                </SelectContent>
              </Select>
              <Select value={draft?.op || 'eq'} onValueChange={(o) => setDraft({ column: draft?.column || columns[0]?.name || '', op: o as ApiFilter['op'], value: draft?.value || '', value2: draft?.value2 || '' })}>
                <SelectTrigger>
                  <SelectValue />
                </SelectTrigger>
                <SelectContent>
                  {ops.map((o) => (
                    <SelectItem key={o.v} value={o.v}>
                      {o.label}
                    </SelectItem>
                  ))}
                </SelectContent>
              </Select>
              {(ops.find((o) => o.v === (draft?.op || 'eq'))?.args || 0) >= 1 ? <Input placeholder="Value" value={draft?.value || ''} onChange={(e) => setDraft({ ...(draft || { column: columns[0]?.name || '', op: 'eq', value: '', value2: '' }), value: e.target.value })} onKeyDown={(e) => e.key === 'Enter' && addFilter()} /> : null}
              {ops.find((o) => o.v === draft?.op)?.args === 2 ? <Input placeholder="and" value={draft?.value2 || ''} onChange={(e) => setDraft({ ...draft!, value2: e.target.value })} /> : null}
              <Button size="sm" className="w-full" disabled={!draft?.column} onClick={addFilter}>
                Apply
              </Button>
            </div>
          </DropdownMenuContent>
        </DropdownMenu>
        {filters.map((f, i) => (
          <Badge key={i} variant="secondary" className="gap-1 py-1 font-mono">
            {f.column} {ops.find((o) => o.v === f.op)?.label} {f.value != null ? String(f.value) : ''}
            {f.value2 != null ? ` – ${String(f.value2)}` : ''}
            <button type="button" onClick={() => setFilters((fs) => fs.filter((_, j) => j !== i))} aria-label="Remove filter">
              <X className="h-3 w-3" />
            </button>
          </Badge>
        ))}
        <DropdownMenu>
          <DropdownMenuTrigger asChild>
            <Button variant="outline" size="sm" className="ml-auto">
              <Columns3 /> Columns {hidden.size ? `(${columns.length - hidden.size}/${columns.length})` : ''}
            </Button>
          </DropdownMenuTrigger>
          <DropdownMenuContent align="end" className="max-h-96 w-64 overflow-auto">
            <DropdownMenuLabel className="flex items-center justify-between">
              Show columns
              <button type="button" className="text-[11px] font-normal text-primary" onClick={() => setHidden(new Set())}>
                reset
              </button>
            </DropdownMenuLabel>
            <DropdownMenuSeparator />
            {columns.map((c) => (
              <DropdownMenuCheckboxItem
                key={c.name}
                checked={!hidden.has(c.name)}
                onCheckedChange={(v) =>
                  setHidden((h) => {
                    const n = new Set(h)
                    if (v) n.delete(c.name)
                    else n.add(c.name)
                    return n
                  })
                }
                onSelect={(e) => e.preventDefault()}
              >
                <span className="font-mono text-xs">{c.name}</span>
              </DropdownMenuCheckboxItem>
            ))}
          </DropdownMenuContent>
        </DropdownMenu>
      </div>
      <div className="min-h-[420px] flex-1">
        {rows.isLoading ? (
          <div className="space-y-2">
            {Array.from({ length: 10 }).map((_, i) => (
              <Skeleton key={i} className="h-8 w-full" />
            ))}
          </div>
        ) : rows.error ? (
          <ErrorState error={rows.error} onRetry={() => rows.refetch()} title="Could not load rows" />
        ) : (
          <DataGrid
            columns={visibleColumns}
            rows={allRows}
            sort={sort}
            onSort={setSort}
            pinned={['_id', '_cdc_key'].filter((c) => columns.some((x) => x.name === c) && !hidden.has(c)).slice(0, 1)}
            onRowClick={(row, index) => setSelected({ row, index })}
            onCellClick={(column, value, row) => {
              if (isJsonString(value)) setCell({ column, value })
              else setSelected({ row, index: allRows.indexOf(row) })
            }}
            selectedIndex={selected?.index ?? null}
            hasMore={!!rows.hasNextPage}
            loadingMore={rows.isFetchingNextPage}
            onEndReached={onEnd}
            className="h-full"
            emptyMessage={<EmptyState title="No rows" description={filters.length || debounced ? 'Nothing matches the filters.' : 'The table is empty so far.'} className="border-0" />}
          />
        )}
      </div>
      <Sheet open={!!cell} onOpenChange={(o) => !o && setCell(null)}>
        <SheetContent>
          <SheetHeader>
            <SheetTitle className="font-mono">{cell?.column.name}</SheetTitle>
            <SheetDescription>Nested value stored as JSON text</SheetDescription>
          </SheetHeader>
          <div className="flex-1 overflow-auto p-5">{cell ? <JsonView value={cell.value} /> : null}</div>
        </SheetContent>
      </Sheet>
      <Sheet open={!!selected} onOpenChange={(o) => !o && setSelected(null)}>
        <SheetContent width="sm:max-w-2xl">
          {selected ? <DocumentDetail ns={ns} cleanTable={cleanTable} rawTable={rawTable} dest={dest} columns={columns} row={selected.row} keyValue={String(selected.row[keyIdx >= 0 ? keyIdx : 0])} displayId={idIdx >= 0 ? String(selected.row[idIdx]) : ''} /> : null}
        </SheetContent>
      </Sheet>
    </Page>
  )
}

function DocumentDetail({ ns, cleanTable, rawTable, dest, columns, row, keyValue, displayId }: { ns: string; cleanTable: string; rawTable: string; dest?: string; columns: ColumnMeta[]; row: unknown[]; keyValue: string; displayId: string }) {
  const hist = useQuery({ queryKey: ['history', ns, rawTable, keyValue, dest], queryFn: () => endpoints.history(ns, cleanTable, keyValue, dest) })
  const current = useMemo(() => {
    const o: Record<string, unknown> = {}
    columns.forEach((c, i) => {
      if (c.name.startsWith('_cdc_')) return
      const v = row[i]
      o[c.name] = isJsonString(v) ? JSON.parse(v as string) : v
    })
    return o
  }, [columns, row])
  const meta = useMemo(() => Object.fromEntries(columns.map((c, i) => [c.name, row[i]]).filter(([k]) => String(k).startsWith('_cdc_'))), [columns, row])
  return (
    <>
      <SheetHeader>
        <SheetTitle className="flex items-center gap-2 font-mono text-[13px]">
          <Layers className="h-4 w-4 text-muted-foreground" /> {displayId || keyValue}
        </SheetTitle>
        <SheetDescription className="font-mono text-[11px]">{keyValue}</SheetDescription>
      </SheetHeader>
      <Tabs defaultValue="current" className="flex min-h-0 flex-1 flex-col px-5 pt-3">
        <TabsList className="self-start">
          <TabsTrigger value="current">Current values</TabsTrigger>
          <TabsTrigger value="history">
            <History className="h-3.5 w-3.5" /> Change history {hist.data ? `(${hist.data.events.length})` : ''}
          </TabsTrigger>
        </TabsList>
        <TabsContent value="current" className="min-h-0 flex-1 overflow-auto pb-5">
          <JsonView value={current} />
          <div className="mt-3 grid grid-cols-2 gap-x-4 gap-y-1 text-[11px] text-muted-foreground">
            {Object.entries(meta).map(([k, v]) => (
              <div key={k} className="flex justify-between gap-2 border-b py-0.5">
                <span className="font-mono">{k}</span>
                <span className="truncate font-mono">{v == null ? 'null' : String(v)}</span>
              </div>
            ))}
          </div>
        </TabsContent>
        <TabsContent value="history" className="min-h-0 flex-1 overflow-auto pb-5">
          {hist.isLoading ? <Skeleton className="h-24 w-full" /> : hist.error ? <ErrorState error={hist.error} /> : null}
          {hist.data?.events.length === 0 ? <EmptyState title="No events for this document" description="The raw table has no rows for this key (it may have been compacted away or the key changed)." className="py-8" /> : null}
          <ol className="space-y-3">
            {hist.data?.events.map((e) => (
              <HistoryItem key={e.seq} e={e} />
            ))}
          </ol>
        </TabsContent>
      </Tabs>
    </>
  )
}

const opLabel: Record<string, { label: string; variant: 'success' | 'info' | 'warning' | 'destructive' | 'muted' }> = {
  r: { label: 'copied', variant: 'muted' },
  c: { label: 'insert', variant: 'success' },
  u: { label: 'update', variant: 'info' },
  x: { label: 'replace', variant: 'warning' },
  d: { label: 'delete', variant: 'destructive' },
}

function HistoryItem({ e }: { e: HistoryEvent }) {
  const [open, setOpen] = useState(false)
  const o = opLabel[e.op] || { label: e.op, variant: 'muted' as const }
  return (
    <li className="rounded-md border">
      <button type="button" className="flex w-full items-center gap-2 px-3 py-2 text-left hover:bg-accent/40" onClick={() => setOpen((v) => !v)}>
        <Badge variant={o.variant}>{o.label}</Badge>
        <span className="font-mono text-[11px] text-muted-foreground">seq {e.seq}</span>
        <span className="text-xs" title={exactTime(e.source_ts || e.ingested_at)}>
          {relativeTime(e.source_ts || e.ingested_at)}
        </span>
        {e.changed.length ? (
          <span className="ml-auto truncate text-[11px] text-muted-foreground">
            {e.changed.length} field{e.changed.length > 1 ? 's' : ''}: {e.changed.slice(0, 4).join(', ')}
            {e.changed.length > 4 ? '…' : ''}
          </span>
        ) : null}
        {e.cast_errors ? <Badge variant="warning">cast errors</Badge> : null}
      </button>
      {open ? (
        <div className="space-y-2 border-t p-3">
          {e.changed.length ? (
            <table className="w-full text-xs">
              <tbody>
                {e.changed.map((f) => (
                  <tr key={f} className="border-b last:border-0">
                    <td className="w-32 py-1 pr-2 align-top font-mono text-muted-foreground">{f}</td>
                    <td className="py-1 align-top">
                      <div className="font-mono text-destructive/80 line-through break-all">{fmt(e.diff[f]?.before)}</div>
                      <div className="font-mono text-success break-all">{fmt(e.diff[f]?.after)}</div>
                    </td>
                  </tr>
                ))}
              </tbody>
            </table>
          ) : (
            <div className="text-xs text-muted-foreground">{e.op === 'd' ? 'Document deleted.' : 'No field-level changes versus the previous version.'}</div>
          )}
          {e.doc ? <JsonView value={e.doc} maxHeight="240px" /> : null}
        </div>
      ) : null}
    </li>
  )
}

function fmt(v: unknown) {
  if (v === undefined || v === null) return 'null'
  return typeof v === 'string' ? v : JSON.stringify(v)
}
