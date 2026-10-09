import { useState } from 'react'
import { useMutation, useQuery, useQueryClient } from '@tanstack/react-query'
import { toast } from 'sonner'
import { FlaskConical, MoreHorizontal, Pencil, Plug, Plus, Trash2 } from 'lucide-react'
import { endpoints, type Connection, type TestReport } from '@/lib/api'
import { Page } from '@/components/layout'
import { PageHeader, EmptyState, ErrorState, ListSkeleton } from '@/components/states'
import { Button } from '@/components/ui/button'
import { Badge } from '@/components/ui/badge'
import { Dialog, DialogContent, DialogDescription, DialogHeader, DialogTitle } from '@/components/ui/dialog'
import { AlertDialog, AlertDialogAction, AlertDialogCancel, AlertDialogContent, AlertDialogDescription, AlertDialogFooter, AlertDialogHeader, AlertDialogTitle } from '@/components/ui/alert-dialog'
import { DropdownMenu, DropdownMenuContent, DropdownMenuItem, DropdownMenuTrigger } from '@/components/ui/dropdown-menu'
import { ConnectionForm } from '@/components/connection-form'
import { TestStatusBadge } from '@/components/status-badge'
import { TestChecklist } from '@/components/test-checklist'
import { Tip } from '@/components/ui/tooltip'
import { exactTime, relativeTime } from '@/lib/utils'

export default function Connections() {
  const qc = useQueryClient()
  const q = useQuery({ queryKey: ['connections'], queryFn: () => endpoints.connections() })
  const [dialog, setDialog] = useState<{ kind: 'source' | 'destination'; existing?: Connection } | null>(null)
  const [del, setDel] = useState<Connection | null>(null)
  const [report, setReport] = useState<{ c: Connection; r: TestReport } | null>(null)
  const test = useMutation({
    mutationFn: (c: Connection) => endpoints.testConnection(c.id),
    onSuccess: (r, c) => {
      setReport({ c, r })
      qc.invalidateQueries({ queryKey: ['connections'] })
    },
    onError: (e) => toast.error(e.message),
  })
  const remove = useMutation({
    mutationFn: (c: Connection) => endpoints.deleteConnection(c.id),
    onSuccess: () => {
      toast.success('Connection deleted')
      qc.invalidateQueries({ queryKey: ['connections'] })
      setDel(null)
    },
    onError: (e) => toast.error(e.message),
  })
  const sources = q.data?.filter((c) => c.kind === 'source') || []
  const dests = q.data?.filter((c) => c.kind === 'destination') || []
  return (
    <Page>
      <PageHeader
        title="Connections"
        description="Sources you read from and destinations you write to. Secrets are encrypted at rest and never shown again."
        actions={
          <>
            <Button variant="outline" onClick={() => setDialog({ kind: 'source' })}>
              <Plus /> Source
            </Button>
            <Button onClick={() => setDialog({ kind: 'destination' })}>
              <Plus /> Destination
            </Button>
          </>
        }
      />
      {q.isLoading ? <ListSkeleton /> : q.error ? <ErrorState error={q.error} onRetry={() => q.refetch()} /> : null}
      {q.data ? (
        <div className="grid gap-6 lg:grid-cols-2">
          <Section title="Sources" items={sources} empty="No source yet. Add your MongoDB cluster." onAdd={() => setDialog({ kind: 'source' })} onEdit={(c) => setDialog({ kind: c.kind, existing: c })} onTest={(c) => test.mutate(c)} testing={test.isPending ? test.variables?.id : undefined} onDelete={setDel} />
          <Section title="Destinations" items={dests} empty="No destination yet. Add an Iceberg REST catalog with S3 storage." onAdd={() => setDialog({ kind: 'destination' })} onEdit={(c) => setDialog({ kind: c.kind, existing: c })} onTest={(c) => test.mutate(c)} testing={test.isPending ? test.variables?.id : undefined} onDelete={setDel} />
        </div>
      ) : null}
      <Dialog open={!!dialog} onOpenChange={(o) => !o && setDialog(null)}>
        <DialogContent wide>
          <DialogHeader>
            <DialogTitle>{dialog?.existing ? `Edit ${dialog.existing.name}` : dialog?.kind === 'source' ? 'Add a source' : 'Add a destination'}</DialogTitle>
            <DialogDescription>Test the connection before saving; the checklist explains any failure.</DialogDescription>
          </DialogHeader>
          {dialog ? <ConnectionForm key={`${dialog.kind}:${dialog.existing?.id || "new"}`} kind={dialog.kind} existing={dialog.existing} onSaved={() => setDialog(null)} onCancel={() => setDialog(null)} /> : null}
        </DialogContent>
      </Dialog>
      <Dialog open={!!report} onOpenChange={(o) => !o && setReport(null)}>
        <DialogContent>
          <DialogHeader>
            <DialogTitle>Test: {report?.c.name}</DialogTitle>
            <DialogDescription>{report?.r.ok ? 'Everything passed.' : 'Some checks did not pass.'}</DialogDescription>
          </DialogHeader>
          {report ? <TestChecklist report={report.r} /> : null}
        </DialogContent>
      </Dialog>
      <AlertDialog open={!!del} onOpenChange={(o) => !o && setDel(null)}>
        <AlertDialogContent>
          <AlertDialogHeader>
            <AlertDialogTitle>Delete {del?.name}?</AlertDialogTitle>
            <AlertDialogDescription>
              {del?.in_use ? `This connection is used by ${del.in_use} pipeline(s); delete those first.` : 'The saved configuration and its encrypted secrets are removed. Synced data is not touched.'}
            </AlertDialogDescription>
          </AlertDialogHeader>
          <AlertDialogFooter>
            <AlertDialogCancel>Cancel</AlertDialogCancel>
            <AlertDialogAction destructive disabled={!!del?.in_use} onClick={() => del && remove.mutate(del)}>
              Delete connection
            </AlertDialogAction>
          </AlertDialogFooter>
        </AlertDialogContent>
      </AlertDialog>
    </Page>
  )
}

function Section({ title, items, empty, onAdd, onEdit, onTest, onDelete, testing }: { title: string; items: Connection[]; empty: string; onAdd: () => void; onEdit: (c: Connection) => void; onTest: (c: Connection) => void; onDelete: (c: Connection) => void; testing?: string }) {
  return (
    <section>
      <h2 className="mb-2 text-xs font-semibold uppercase tracking-wide text-muted-foreground">{title}</h2>
      {items.length === 0 ? (
        <EmptyState icon={<Plug className="h-5 w-5" />} title={empty} action={<Button size="sm" onClick={onAdd}><Plus /> Add</Button>} className="py-10" />
      ) : (
        <div className="divide-y rounded-lg border bg-card">
          {items.map((c) => (
            <div key={c.id} className="flex items-center gap-3 px-4 py-3">
              <div className="min-w-0 flex-1">
                <div className="flex items-center gap-2">
                  <span className="truncate text-[13px] font-medium">{c.name}</span>
                  <Badge variant="outline">{c.type}</Badge>
                  {c.in_use ? <span className="text-[11px] text-muted-foreground">{c.in_use} pipeline{c.in_use > 1 ? 's' : ''}</span> : null}
                </div>
                <div className="mt-0.5 flex items-center gap-2 text-xs text-muted-foreground">
                  <TestStatusBadge status={c.last_test_status} />
                  {c.last_tested_at ? (
                    <Tip label={exactTime(c.last_tested_at)}>
                      <span>tested {relativeTime(c.last_tested_at)}</span>
                    </Tip>
                  ) : null}
                </div>
              </div>
              <Button variant="outline" size="sm" onClick={() => onTest(c)} loading={testing === c.id}>
                <FlaskConical /> Test
              </Button>
              <DropdownMenu>
                <DropdownMenuTrigger asChild>
                  <Button variant="ghost" size="icon" aria-label="More">
                    <MoreHorizontal />
                  </Button>
                </DropdownMenuTrigger>
                <DropdownMenuContent align="end">
                  <DropdownMenuItem onSelect={() => onEdit(c)}>
                    <Pencil /> Edit
                  </DropdownMenuItem>
                  <DropdownMenuItem destructive onSelect={() => onDelete(c)}>
                    <Trash2 /> Delete
                  </DropdownMenuItem>
                </DropdownMenuContent>
              </DropdownMenu>
            </div>
          ))}
        </div>
      )}
    </section>
  )
}
