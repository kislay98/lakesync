import { useMemo, useRef, useState, type ReactNode } from 'react'
import { useVirtualizer } from '@tanstack/react-virtual'
import { ArrowDown, ArrowUp, ArrowUpDown, Check, Copy, Pin } from 'lucide-react'
import type { ColumnMeta } from '@/lib/api'
import { cn, copyText, isJsonString, truncate } from '@/lib/utils'
import { Tip } from '@/components/ui/tooltip'

export interface GridSort {
  column: string
  desc: boolean
}

export interface DataGridProps {
  /** Every column of the rows, in row order (values are indexed by this list). */
  columns: ColumnMeta[]
  /** Columns to leave out of the display; values stay indexed by `columns`. */
  hiddenColumns?: Set<string>
  rows: unknown[][]
  sort?: GridSort | null
  onSort?: (s: GridSort | null) => void
  onRowClick?: (row: unknown[], index: number) => void
  onCellClick?: (column: ColumnMeta, value: unknown, row: unknown[]) => void
  pinned?: string[]
  loadingMore?: boolean
  onEndReached?: () => void
  hasMore?: boolean
  selectedIndex?: number | null
  className?: string
  emptyMessage?: ReactNode
  columnWidths?: Record<string, number>
  rowHeight?: number
  headerExtra?: (c: ColumnMeta) => ReactNode
}

const DEFAULT_WIDTH = 180

/** Virtualised, column-pinnable grid for tables with many rows and columns. */
export function DataGrid({ columns, hiddenColumns, rows, sort, onSort, onRowClick, onCellClick, pinned = [], loadingMore, onEndReached, hasMore, selectedIndex, className, emptyMessage, columnWidths = {}, rowHeight = 32, headerExtra }: DataGridProps) {
  const parentRef = useRef<HTMLDivElement>(null)
  const ordered = useMemo(() => {
    const shown = columns.filter((c) => !hiddenColumns?.has(c.name))
    return [...shown.filter((c) => pinned.includes(c.name)), ...shown.filter((c) => !pinned.includes(c.name))]
  }, [columns, hiddenColumns, pinned])
  const widths = ordered.map((c) => columnWidths[c.name] || (c.name === '_cdc_key' ? 240 : c.type.includes('TIMESTAMP') ? 200 : c.type === 'BOOLEAN' ? 90 : /INT|DOUBLE|DECIMAL|FLOAT/.test(c.type) ? 110 : DEFAULT_WIDTH))
  const offsets = useMemo(() => {
    const out: number[] = []
    let x = 0
    for (const w of widths) {
      out.push(x)
      x += w
    }
    return out
  }, [widths])
  const totalWidth = offsets.length ? offsets[offsets.length - 1] + widths[widths.length - 1] : 0
  const colIndex = useMemo(() => new Map(columns.map((c, i) => [c.name, i])), [columns])
  const rowVirt = useVirtualizer({
    count: rows.length,
    getScrollElement: () => parentRef.current,
    estimateSize: () => rowHeight,
    overscan: 12,
    onChange: (v) => {
      if (!hasMore || loadingMore || !onEndReached) return
      const items = v.getVirtualItems()
      if (items.length && items[items.length - 1].index >= rows.length - 20) onEndReached()
    },
  })
  const colVirt = useVirtualizer({
    horizontal: true,
    count: ordered.length,
    getScrollElement: () => parentRef.current,
    estimateSize: (i) => widths[i],
    overscan: 4,
  })
  const pinnedCount = ordered.filter((c) => pinned.includes(c.name)).length
  const pinnedWidth = widths.slice(0, pinnedCount).reduce((a, b) => a + b, 0)

  return (
    <div className={cn('relative flex h-full min-h-0 flex-col rounded-md border bg-card', className)}>
      <div ref={parentRef} className="relative flex-1 overflow-auto scrollbar-thin">
        <div style={{ width: totalWidth, height: rowVirt.getTotalSize() + rowHeight, position: 'relative' }}>
          {/* header */}
          <div className="sticky top-0 z-20 flex border-b bg-muted/70 backdrop-blur text-xs font-medium text-muted-foreground" style={{ height: rowHeight, width: totalWidth }}>
            {colVirt.getVirtualItems().map((vc) => {
              const c = ordered[vc.index]
              const isPinned = vc.index < pinnedCount
              const sorted = sort?.column === c.name
              return (
                <div
                  key={c.name}
                  className={cn('group absolute top-0 flex h-full items-center gap-1 border-r px-2', isPinned && 'z-30 bg-muted', onSort && 'cursor-pointer select-none hover:text-foreground')}
                  style={{ left: isPinned ? vc.start : undefined, transform: isPinned ? undefined : `translateX(${vc.start}px)`, width: vc.size, position: isPinned ? 'sticky' : 'absolute' }}
                  onClick={() => onSort && onSort(sorted ? (sort!.desc ? null : { column: c.name, desc: true }) : { column: c.name, desc: false })}
                  title={`${c.name} (${c.type})`}
                >
                  {isPinned ? <Pin className="h-3 w-3 shrink-0 opacity-60" /> : null}
                  <span className="truncate">{c.name}</span>
                  <span className="ml-auto flex items-center gap-1">
                    {headerExtra ? headerExtra(c) : null}
                    {onSort ? sorted ? sort!.desc ? <ArrowDown className="h-3 w-3" /> : <ArrowUp className="h-3 w-3" /> : <ArrowUpDown className="h-3 w-3 opacity-0 group-hover:opacity-50" /> : null}
                  </span>
                </div>
              )
            })}
          </div>
          {/* rows */}
          {rows.length === 0 ? (
            <div className="sticky left-0 flex w-full items-center justify-center p-10 text-[13px] text-muted-foreground" style={{ top: rowHeight }}>
              {emptyMessage || 'No rows.'}
            </div>
          ) : null}
          {rowVirt.getVirtualItems().map((vr) => {
            const row = rows[vr.index]
            const selected = selectedIndex === vr.index
            return (
              <div
                key={vr.key}
                className={cn('absolute left-0 flex border-b', onRowClick && 'cursor-pointer', selected ? 'bg-primary/10' : vr.index % 2 ? 'bg-muted/20 hover:bg-accent/60' : 'hover:bg-accent/60')}
                style={{ top: vr.start + rowHeight, height: vr.size, width: totalWidth }}
                onClick={() => onRowClick?.(row, vr.index)}
                data-row={vr.index}
              >
                {colVirt.getVirtualItems().map((vc) => {
                  const c = ordered[vc.index]
                  const isPinned = vc.index < pinnedCount
                  const value = row[colIndex.get(c.name)!]
                  return (
                    <div
                      key={c.name}
                      className={cn('absolute top-0 flex h-full items-center overflow-hidden border-r px-2', isPinned && 'z-10 bg-card', selected && isPinned && 'bg-primary/10')}
                      style={{ left: isPinned ? vc.start : undefined, transform: isPinned ? undefined : `translateX(${vc.start}px)`, width: vc.size, position: isPinned ? 'sticky' : 'absolute' }}
                      onClick={(e) => {
                        if (onCellClick) {
                          e.stopPropagation()
                          onCellClick(c, value, row)
                        }
                      }}
                    >
                      <Cell value={value} type={c.type} />
                    </div>
                  )
                })}
              </div>
            )
          })}
        </div>
      </div>
      <div className="sticky bottom-0 left-0 z-20 h-px w-full" style={{ marginLeft: pinnedWidth }} />
      {loadingMore ? <div className="absolute bottom-2 left-1/2 -translate-x-1/2 rounded-full border bg-card px-3 py-1 text-xs text-muted-foreground shadow">Loading more…</div> : null}
    </div>
  )
}

export function Cell({ value, type }: { value: unknown; type: string }) {
  const [copied, setCopied] = useState(false)
  if (value === null || value === undefined) return <span className="text-muted-foreground/60 italic text-xs">null</span>
  const t = type.toUpperCase()
  if (typeof value === 'boolean') return <span className={cn('font-mono text-xs', value ? 'text-success' : 'text-muted-foreground')}>{String(value)}</span>
  if (typeof value === 'number' || /INT|DOUBLE|DECIMAL|FLOAT|HUGE/.test(t)) return <span className="ml-auto font-mono text-xs tabular-nums">{typeof value === 'number' ? (Number.isInteger(value) ? value.toLocaleString() : value) : String(value)}</span>
  const s = String(value)
  if (t.includes('TIMESTAMP') || t.includes('DATE')) {
    const d = new Date(s)
    return (
      <Tip label={s}>
        <span className="font-mono text-xs tabular-nums">{Number.isNaN(d.getTime()) ? s : d.toLocaleString(undefined, { dateStyle: 'short', timeStyle: 'medium' })}</span>
      </Tip>
    )
  }
  if (isJsonString(s)) {
    return (
      <span className="flex min-w-0 items-center gap-1">
        <span className="rounded bg-info/15 px-1 font-mono text-[10px] text-info">{s.trim().startsWith('[') ? '[ ]' : '{ }'}</span>
        <span className="truncate font-mono text-xs text-muted-foreground">{truncate(s, 120)}</span>
      </span>
    )
  }
  return (
    <span className="group/cell flex min-w-0 w-full items-center gap-1">
      <span className="truncate" title={s.length > 40 ? s : undefined}>
        {s}
      </span>
      <button
        type="button"
        className="ml-auto hidden shrink-0 text-muted-foreground hover:text-foreground group-hover/cell:inline-flex"
        onClick={async (e) => {
          e.stopPropagation()
          if (await copyText(s)) {
            setCopied(true)
            setTimeout(() => setCopied(false), 1000)
          }
        }}
        aria-label="Copy"
      >
        {copied ? <Check className="h-3 w-3 text-success" /> : <Copy className="h-3 w-3" />}
      </button>
    </span>
  )
}
