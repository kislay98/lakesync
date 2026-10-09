import { useCallback, useEffect, useMemo, useRef, useState } from 'react'
import { useMutation, useQuery } from '@tanstack/react-query'
import { EditorView, keymap, placeholder as cmPlaceholder } from '@codemirror/view'
import { EditorState, Compartment } from '@codemirror/state'
import { defaultKeymap, history, historyKeymap } from '@codemirror/commands'
import { sql, PostgreSQL } from '@codemirror/lang-sql'
import { autocompletion, closeBrackets } from '@codemirror/autocomplete'
import { syntaxHighlighting, defaultHighlightStyle, bracketMatching } from '@codemirror/language'
import { oneDark } from '@codemirror/theme-one-dark'
import { Clock, Lock, Play } from 'lucide-react'
import { endpoints, type QueryResult } from '@/lib/api'
import { Page } from '@/components/layout'
import { PageHeader, EmptyState, InlineError } from '@/components/states'
import { Button } from '@/components/ui/button'
import { Kbd } from '@/components/ui/misc'
import { Select, SelectContent, SelectItem, SelectTrigger, SelectValue } from '@/components/ui/select'
import { DataGrid } from '@/components/data-grid'
import { useTheme } from '@/hooks/use-theme'
import { formatDuration, relativeTime } from '@/lib/utils'
import { Badge } from '@/components/ui/badge'

const KEY = 'lakesync.sql.draft'

export default function SqlConsole() {
  const dests = useQuery({ queryKey: ['connections', 'destination'], queryFn: () => endpoints.connections('destination') })
  const [destChoice, setDestId] = useState('')
  // the first destination is preselected (derived, so no effect is needed)
  const destId = destChoice || dests.data?.[0]?.id || ''
  const schema = useQuery({ queryKey: ['query-schema', destId], queryFn: () => endpoints.querySchema(destId), enabled: !!destId, staleTime: 60000 })
  const history_ = useQuery({ queryKey: ['query-history'], queryFn: endpoints.queryHistory })
  const [result, setResult] = useState<QueryResult | null>(null)
  const [limit, setLimit] = useState(0)
  const run = useMutation({
    mutationFn: (q: string) => endpoints.query(destId, q),
    onSuccess: (r) => {
      setResult(r.result)
      setLimit(r.row_limit)
      history_.refetch()
    },
    onError: () => history_.refetch(),
  })
  const editorRef = useRef<HTMLDivElement>(null)
  const viewRef = useRef<EditorView | null>(null)
  const themeComp = useRef(new Compartment())
  const schemaComp = useRef(new Compartment())
  const { isDark } = useTheme()
  const schemaMap = useMemo(() => {
    const m: Record<string, string[]> = {}
    for (const t of schema.data?.tables || []) {
      m[`${t.namespace}.${t.table}`] = t.columns
      m[t.table] = t.columns
    }
    return m
  }, [schema.data])
  const runCurrent = useCallback(() => {
    const v = viewRef.current
    if (!v) return
    const sel = v.state.selection.main
    const text = sel.empty ? v.state.doc.toString() : v.state.sliceDoc(sel.from, sel.to)
    if (text.trim()) run.mutate(text)
  }, [run])
  const runRef = useRef(runCurrent)
  useEffect(() => {
    runRef.current = runCurrent
  })
  useEffect(() => {
    if (!editorRef.current || viewRef.current) return
    let draft = ''
    try {
      draft = localStorage.getItem(KEY) || ''
    } catch {
      /* ignore */
    }
    const state = EditorState.create({
      doc: draft || 'SELECT * FROM shop.customers LIMIT 100',
      extensions: [
        keymap.of([{ key: 'Mod-Enter', run: () => (runRef.current(), true) }, ...defaultKeymap, ...historyKeymap]),
        history(),
        closeBrackets(),
        bracketMatching(),
        autocompletion(),
        schemaComp.current.of(sql({ dialect: PostgreSQL, schema: {}, upperCaseKeywords: true })),
        syntaxHighlighting(defaultHighlightStyle, { fallback: true }),
        themeComp.current.of(document.documentElement.classList.contains('dark') ? oneDark : []),
        cmPlaceholder('SELECT … (read-only, Cmd/Ctrl+Enter to run)'),
        EditorView.updateListener.of((u) => {
          if (u.docChanged) {
            try {
              localStorage.setItem(KEY, u.state.doc.toString())
            } catch {
              /* ignore */
            }
          }
        }),
        EditorView.theme({ '&': { height: '100%' }, '.cm-scroller': { fontFamily: 'var(--font-mono)' } }),
      ],
    })
    viewRef.current = new EditorView({ state, parent: editorRef.current })
    return () => {
      viewRef.current?.destroy()
      viewRef.current = null
    }
  }, [])
  useEffect(() => {
    viewRef.current?.dispatch({ effects: themeComp.current.reconfigure(isDark ? oneDark : []) })
  }, [isDark])
  useEffect(() => {
    viewRef.current?.dispatch({ effects: schemaComp.current.reconfigure(sql({ dialect: PostgreSQL, schema: schemaMap, upperCaseKeywords: true })) })
  }, [schemaMap])
  const load = (q: string) => {
    const v = viewRef.current
    if (!v) return
    v.dispatch({ changes: { from: 0, to: v.state.doc.length, insert: q } })
    v.focus()
  }
  return (
    <Page full className="max-w-none">
      <PageHeader
        title="SQL console"
        description={
          <span className="inline-flex items-center gap-1.5">
            <Lock className="h-3 w-3" /> Read-only: SELECT and EXPLAIN against any attached table. Results are capped at {limit ? limit.toLocaleString() : '10,000'} rows.
          </span>
        }
        actions={
          <>
            <Select value={destId} onValueChange={setDestId}>
              <SelectTrigger className="w-56">
                <SelectValue placeholder="Destination" />
              </SelectTrigger>
              <SelectContent>
                {(dests.data || []).map((d) => (
                  <SelectItem key={d.id} value={d.id}>
                    {d.name}
                  </SelectItem>
                ))}
              </SelectContent>
            </Select>
            <Button onClick={runCurrent} loading={run.isPending} disabled={!destId}>
              <Play /> Run <Kbd className="ml-1 bg-primary-foreground/20 text-primary-foreground border-0">⌘↵</Kbd>
            </Button>
          </>
        }
      />
      <div className="grid min-h-0 flex-1 gap-4 lg:grid-cols-[1fr_260px]">
        <div className="flex min-h-0 flex-col gap-3">
          <div ref={editorRef} className="h-44 shrink-0 overflow-hidden rounded-md border bg-card" />
          <InlineError error={run.error} />
          <div className="flex min-h-0 flex-1 flex-col">
            {result ? (
              <>
                <div className="mb-2 flex items-center gap-3 text-xs text-muted-foreground">
                  <span className="tabular-nums">
                    {result.row_count.toLocaleString()} row{result.row_count === 1 ? '' : 's'}
                    {result.truncated ? ` (truncated at ${limit.toLocaleString()})` : ''}
                  </span>
                  <span className="tabular-nums">{formatDuration(result.duration_ms)}</span>
                  <span>{result.columns.length} columns</span>
                </div>
                <DataGrid columns={result.columns} rows={result.rows} className="min-h-[300px] flex-1" emptyMessage="The query returned no rows." />
              </>
            ) : (
              <EmptyState title="Run a query to see results" description="Table and column names autocomplete. Select part of the editor to run only that." className="flex-1" />
            )}
          </div>
        </div>
        <aside className="hidden min-h-0 flex-col gap-3 lg:flex">
          <div className="rounded-md border bg-card">
            <div className="border-b px-3 py-2 text-xs font-medium text-muted-foreground">Tables</div>
            <div className="max-h-60 overflow-auto p-1 text-xs">
              {(schema.data?.tables || []).map((t) => (
                <button key={`${t.namespace}.${t.table}`} className="block w-full truncate rounded px-2 py-1 text-left font-mono hover:bg-accent" onClick={() => load(`SELECT * FROM ${t.namespace}.${t.table} LIMIT 100`)} title={t.columns.join(', ')}>
                  {t.namespace}.{t.table}
                </button>
              ))}
              {schema.data && !schema.data.tables.length ? <div className="px-2 py-3 text-muted-foreground">No tables yet.</div> : null}
            </div>
          </div>
          <div className="flex min-h-0 flex-1 flex-col rounded-md border bg-card">
            <div className="flex items-center gap-1.5 border-b px-3 py-2 text-xs font-medium text-muted-foreground">
              <Clock className="h-3.5 w-3.5" /> History
            </div>
            <div className="min-h-0 flex-1 overflow-auto p-1 text-xs">
              {(history_.data || []).map((h) => (
                <button key={h.id} className="block w-full rounded px-2 py-1.5 text-left hover:bg-accent" onClick={() => load(h.sql)}>
                  <div className="truncate font-mono">{h.sql}</div>
                  <div className="mt-0.5 flex items-center gap-2 text-[11px] text-muted-foreground">
                    {h.error ? <Badge variant="destructive">error</Badge> : <span>{h.rows} rows · {formatDuration(h.duration_ms)}</span>}
                    <span>{relativeTime(h.at)}</span>
                  </div>
                </button>
              ))}
              {history_.data && !history_.data.length ? <div className="px-2 py-3 text-muted-foreground">Queries you run appear here.</div> : null}
            </div>
          </div>
        </aside>
      </div>
    </Page>
  )
}
