import { Loader2 } from 'lucide-react'
import { Badge } from '@/components/ui/badge'
import type { CollectionState } from '@/lib/api'
import { cn } from '@/lib/utils'

const meta: Record<string, { label: string; variant: 'success' | 'info' | 'muted' | 'destructive' | 'warning' | 'secondary'; spin?: boolean; dot?: string }> = {
  streaming: { label: 'Streaming', variant: 'success', dot: 'bg-success' },
  copying: { label: 'Copying', variant: 'info', spin: true },
  resyncing: { label: 'Resyncing', variant: 'info', spin: true },
  paused: { label: 'Paused', variant: 'muted', dot: 'bg-muted-foreground' },
  stopped: { label: 'Stopped', variant: 'muted', dot: 'bg-muted-foreground' },
  created: { label: 'Not started', variant: 'secondary', dot: 'bg-muted-foreground/60' },
  failed: { label: 'Failed', variant: 'destructive', dot: 'bg-destructive' },
  source_removed: { label: 'Source removed', variant: 'warning', dot: 'bg-warning' },
}

export function StatusBadge({ state, className }: { state: CollectionState | string; className?: string }) {
  const m = meta[state] || { label: state, variant: 'secondary' as const }
  return (
    <Badge variant={m.variant} className={cn('gap-1.5', className)}>
      {m.spin ? <Loader2 className="h-3 w-3 animate-spin" /> : <span className={cn('h-1.5 w-1.5 rounded-full', m.dot)} />}
      {m.label}
    </Badge>
  )
}

export function stateLabel(state: string) {
  return meta[state]?.label || state
}

export function TestStatusBadge({ status }: { status: 'ok' | 'warn' | 'fail' | null | undefined }) {
  if (!status) return <Badge variant="muted">Never tested</Badge>
  if (status === 'ok') return <Badge variant="success">Passed</Badge>
  if (status === 'warn') return <Badge variant="warning">Warnings</Badge>
  return <Badge variant="destructive">Failed</Badge>
}

export function HealthBadge({ status }: { status: string }) {
  if (status === 'healthy') return <Badge variant="success">Healthy</Badge>
  if (status === 'attention') return <Badge variant="warning">Needs attention</Badge>
  if (status === 'unhealthy') return <Badge variant="destructive">Unhealthy</Badge>
  return <Badge variant="muted">Unknown</Badge>
}
