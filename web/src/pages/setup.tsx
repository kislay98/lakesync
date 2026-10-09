import { useState } from 'react'
import { useNavigate } from 'react-router-dom'
import { useQuery } from '@tanstack/react-query'
import { Check, Database, GitBranch, Plug, Waves } from 'lucide-react'
import { endpoints, type Connection } from '@/lib/api'
import { ConnectionForm } from '@/components/connection-form'
import NewPipeline from '@/pages/new-pipeline'
import { Button } from '@/components/ui/button'
import { cn } from '@/lib/utils'

const steps = [
  { title: 'Add MongoDB', icon: Database, desc: 'The source cluster. Change streams need a replica set.' },
  { title: 'Add a destination', icon: Plug, desc: 'An Iceberg REST catalog plus S3-compatible storage.' },
  { title: 'Create a pipeline', icon: GitBranch, desc: 'Pick collections, set the interval, start streaming.' },
]

export default function Setup() {
  const nav = useNavigate()
  const conns = useQuery({ queryKey: ['connections'], queryFn: () => endpoints.connections() })
  const [source, setSource] = useState<Connection | null>(null)
  const [dest, setDest] = useState<Connection | null>(null)
  const existingSource = source || conns.data?.find((c) => c.kind === 'source') || null
  const existingDest = dest || conns.data?.find((c) => c.kind === 'destination') || null
  const step = !existingSource ? 0 : !existingDest ? 1 : 2
  return (
    <div className="min-h-screen bg-muted/30">
      <header className="flex h-14 items-center gap-2 border-b bg-background px-6">
        <div className="flex h-7 w-7 items-center justify-center rounded-md bg-primary text-primary-foreground">
          <Waves className="h-4 w-4" />
        </div>
        <span className="text-sm font-semibold">lakesync setup</span>
        <Button variant="ghost" size="sm" className="ml-auto" onClick={() => nav('/')}>
          Skip for now
        </Button>
      </header>
      <div className="mx-auto grid max-w-5xl gap-8 px-6 py-8 md:grid-cols-[260px_1fr]">
        <ol className="space-y-4">
          {steps.map((s, i) => (
            <li key={s.title} className={cn('flex gap-3', i > step && 'opacity-50')}>
              <span className={cn('flex h-7 w-7 shrink-0 items-center justify-center rounded-full border bg-card', i < step && 'border-primary bg-primary text-primary-foreground', i === step && 'border-primary text-primary')}>{i < step ? <Check className="h-4 w-4" /> : <s.icon className="h-4 w-4" />}</span>
              <div>
                <div className="text-[13px] font-medium">{s.title}</div>
                <div className="text-xs text-muted-foreground">{s.desc}</div>
                {i === 0 && existingSource ? <div className="mt-0.5 text-xs text-success">{existingSource.name}</div> : null}
                {i === 1 && existingDest ? <div className="mt-0.5 text-xs text-success">{existingDest.name}</div> : null}
              </div>
            </li>
          ))}
        </ol>
        <div className="rounded-lg border bg-card p-6">
          {step === 0 ? (
            <>
              <h2 className="mb-1 text-base font-semibold">Connect MongoDB</h2>
              <p className="mb-4 text-xs text-muted-foreground">Paste an Atlas connection string or enter the fields. The test proves authentication, the replica set and change-stream permissions.</p>
              <ConnectionForm key="source" kind="source" onSaved={setSource} compact autoTest />
            </>
          ) : step === 1 ? (
            <>
              <h2 className="mb-1 text-base font-semibold">Add an Iceberg destination</h2>
              <p className="mb-4 text-xs text-muted-foreground">Any Iceberg REST catalog (Nessie, Lakekeeper, Polaris, R2 Data Catalog, Glue) with S3-compatible storage. The test writes and drops a temporary table.</p>
              <ConnectionForm key="destination" kind="destination" onSaved={setDest} compact autoTest />
            </>
          ) : (
            <>
              <h2 className="mb-1 text-base font-semibold">Create your first pipeline</h2>
              <p className="mb-4 text-xs text-muted-foreground">Choose the collections to sync. Existing documents are copied first, then changes stream in about once a minute.</p>
              <NewPipeline embedded onCreated={(id) => nav(`/pipelines/${id}`)} />
            </>
          )}
        </div>
      </div>
    </div>
  )
}
