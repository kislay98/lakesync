import { useId } from 'react'
import { cn } from '@/lib/utils'

export function Sparkline({ values, className, height = 28, width = 120 }: { values: number[]; className?: string; height?: number; width?: number }) {
  const id = useId()
  if (!values.length) return <div className={cn('text-xs text-muted-foreground', className)}>no data</div>
  const max = Math.max(1, ...values)
  const step = width / Math.max(1, values.length - 1)
  const pts = values.map((v, i) => [i * step, height - (v / max) * (height - 2) - 1] as const)
  const d = pts.map(([x, y], i) => `${i === 0 ? 'M' : 'L'}${x.toFixed(1)},${y.toFixed(1)}`).join(' ')
  const area = `${d} L${width},${height} L0,${height} Z`
  return (
    <svg width={width} height={height} viewBox={`0 0 ${width} ${height}`} className={cn('overflow-visible', className)} aria-hidden>
      <defs>
        <linearGradient id={id} x1="0" x2="0" y1="0" y2="1">
          <stop offset="0%" stopColor="currentColor" stopOpacity="0.25" />
          <stop offset="100%" stopColor="currentColor" stopOpacity="0" />
        </linearGradient>
      </defs>
      <path d={area} fill={`url(#${id})`} />
      <path d={d} fill="none" stroke="currentColor" strokeWidth="1.5" strokeLinejoin="round" strokeLinecap="round" />
    </svg>
  )
}
