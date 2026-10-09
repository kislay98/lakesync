import { AlertTriangle, Check, X } from 'lucide-react'
import type { TestReport } from '@/lib/api'
import { cn, formatDuration } from '@/lib/utils'

export function TestChecklist({ report, className }: { report: TestReport; className?: string }) {
  return (
    <div className={cn('rounded-md border divide-y', className)}>
      {report.steps.map((s, i) => (
        <div key={i} className="flex items-start gap-3 px-3 py-2">
          <div
            className={cn(
              'mt-0.5 flex h-4 w-4 shrink-0 items-center justify-center rounded-full',
              s.status === 'ok' && 'bg-success/15 text-success',
              s.status === 'warn' && 'bg-warning/20 text-warning',
              s.status === 'fail' && 'bg-destructive/15 text-destructive',
            )}
          >
            {s.status === 'ok' ? <Check className="h-3 w-3" /> : s.status === 'warn' ? <AlertTriangle className="h-2.5 w-2.5" /> : <X className="h-3 w-3" />}
          </div>
          <div className="min-w-0 flex-1">
            <div className="flex items-center justify-between gap-2">
              <div className="text-[13px] font-medium">{s.name}</div>
              <div className="text-[11px] tabular-nums text-muted-foreground">{formatDuration(s.duration_ms)}</div>
            </div>
            {s.message ? <div className="text-xs text-muted-foreground break-words">{s.message}</div> : null}
            {s.hint && s.status !== 'ok' ? <div className={cn('mt-1 rounded-sm px-2 py-1 text-xs', s.status === 'fail' ? 'bg-destructive/8 text-destructive' : 'bg-warning/10 text-warning')}>{s.hint}</div> : null}
          </div>
        </div>
      ))}
      {report.info && Object.keys(report.info).length ? (
        <div className="flex flex-wrap gap-x-4 gap-y-1 px-3 py-2 text-[11px] text-muted-foreground">
          {Object.entries(report.info).map(([k, v]) => (
            <span key={k}>
              <span className="font-medium">{k.replace(/_/g, ' ')}:</span> {v}
            </span>
          ))}
        </div>
      ) : null}
    </div>
  )
}
