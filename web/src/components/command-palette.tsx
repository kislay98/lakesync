import { useEffect, useState } from 'react'
import { useNavigate } from 'react-router-dom'
import { Command } from 'cmdk'
import { useQuery } from '@tanstack/react-query'
import { Database, GitBranch, Home, Plug, Search, Settings, Table2, TerminalSquare } from 'lucide-react'
import { endpoints } from '@/lib/api'
import { Dialog, DialogContent, DialogTitle } from '@/components/ui/dialog'

export function CommandPalette() {
  const [open, setOpen] = useState(false)
  const nav = useNavigate()
  useEffect(() => {
    const onKey = (e: KeyboardEvent) => {
      if ((e.metaKey || e.ctrlKey) && e.key.toLowerCase() === 'k') {
        e.preventDefault()
        setOpen((o) => !o)
      }
    }
    window.addEventListener('keydown', onKey)
    const onOpen = () => setOpen(true)
    window.addEventListener('lakesync:palette', onOpen)
    return () => {
      window.removeEventListener('keydown', onKey)
      window.removeEventListener('lakesync:palette', onOpen)
    }
  }, [])
  const pipelines = useQuery({ queryKey: ['pipelines'], queryFn: endpoints.pipelines, enabled: open })
  const tables = useQuery({ queryKey: ['tables'], queryFn: () => endpoints.tables(), enabled: open })
  const go = (to: string) => {
    setOpen(false)
    nav(to)
  }
  return (
    <Dialog open={open} onOpenChange={setOpen}>
      <DialogContent className="p-0 gap-0 overflow-hidden max-w-xl top-[20%] translate-y-0" aria-describedby={undefined}>
        <DialogTitle className="sr-only">Command palette</DialogTitle>
        <Command label="Jump to" className="[&_[cmdk-group-heading]]:px-3 [&_[cmdk-group-heading]]:py-1.5 [&_[cmdk-group-heading]]:text-[11px] [&_[cmdk-group-heading]]:font-medium [&_[cmdk-group-heading]]:text-muted-foreground">
          <div className="flex items-center gap-2 border-b px-3">
            <Search className="h-4 w-4 text-muted-foreground" />
            <Command.Input autoFocus placeholder="Jump to a pipeline, table or page…" className="h-11 w-full bg-transparent text-sm outline-none placeholder:text-muted-foreground" />
          </div>
          <Command.List className="max-h-80 overflow-y-auto p-1.5">
            <Command.Empty className="px-3 py-6 text-center text-[13px] text-muted-foreground">Nothing matches.</Command.Empty>
            <Command.Group heading="Pages">
              {[
                ['Overview', '/', Home],
                ['Pipelines', '/pipelines', GitBranch],
                ['Connections', '/connections', Plug],
                ['Tables', '/tables', Table2],
                ['SQL console', '/sql', TerminalSquare],
                ['Settings', '/settings', Settings],
              ].map(([label, to, Icon]) => {
                const I = Icon as typeof Home
                return (
                  <Command.Item key={String(to)} value={`page ${label}`} onSelect={() => go(String(to))} className="flex cursor-default items-center gap-2 rounded-sm px-2 py-1.5 text-[13px] data-[selected=true]:bg-accent">
                    <I className="h-4 w-4 text-muted-foreground" /> {String(label)}
                  </Command.Item>
                )
              })}
            </Command.Group>
            {pipelines.data?.length ? (
              <Command.Group heading="Pipelines">
                {pipelines.data.map((p) => (
                  <Command.Item key={p.id} value={`pipeline ${p.name}`} onSelect={() => go(`/pipelines/${p.id}`)} className="flex cursor-default items-center gap-2 rounded-sm px-2 py-1.5 text-[13px] data-[selected=true]:bg-accent">
                    <GitBranch className="h-4 w-4 text-muted-foreground" /> {p.name}
                    <span className="ml-auto text-[11px] text-muted-foreground">{p.state}</span>
                  </Command.Item>
                ))}
              </Command.Group>
            ) : null}
            {tables.data?.length ? (
              <Command.Group heading="Tables">
                {tables.data.map((t) => (
                  <Command.Item key={`${t.destination_id}/${t.namespace}/${t.name}`} value={`table ${t.namespace}.${t.name}`} onSelect={() => go(`/tables/${encodeURIComponent(t.namespace)}/${encodeURIComponent(t.name)}?destination=${t.destination_id}`)} className="flex cursor-default items-center gap-2 rounded-sm px-2 py-1.5 text-[13px] data-[selected=true]:bg-accent">
                    <Database className="h-4 w-4 text-muted-foreground" /> {t.namespace}.{t.name}
                    <span className="ml-auto text-[11px] text-muted-foreground">{t.kind}</span>
                  </Command.Item>
                ))}
              </Command.Group>
            ) : null}
          </Command.List>
        </Command>
      </DialogContent>
    </Dialog>
  )
}
