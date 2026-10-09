import { useEffect, useState } from 'react'
import { useMutation, useQuery, useQueryClient } from '@tanstack/react-query'
import { toast } from 'sonner'
import { endpoints, type Settings as SettingsT } from '@/lib/api'
import { Page } from '@/components/layout'
import { PageHeader, ErrorState, ListSkeleton, InlineError } from '@/components/states'
import { Button } from '@/components/ui/button'
import { Input } from '@/components/ui/input'
import { Label } from '@/components/ui/label'
import { Card, CardContent, CardDescription, CardHeader, CardTitle } from '@/components/ui/card'
import { useTheme } from '@/hooks/use-theme'
import { Select, SelectContent, SelectItem, SelectTrigger, SelectValue } from '@/components/ui/select'

export default function SettingsPage() {
  const qc = useQueryClient()
  const q = useQuery({ queryKey: ['settings'], queryFn: endpoints.settings })
  const { theme, setTheme } = useTheme()
  const [general, setGeneral] = useState<SettingsT['general'] | null>(null)
  const [maint, setMaint] = useState<SettingsT['maintenance'] | null>(null)
  const [pw, setPw] = useState({ current: '', next: '', confirm: '' })
  const [pwErr, setPwErr] = useState<unknown>(null)
  useEffect(() => {
    if (q.data) {
      setGeneral(q.data.general)
      setMaint(q.data.maintenance)
    }
  }, [q.data])
  const save = useMutation({
    mutationFn: () => endpoints.updateSettings({ general: general!, maintenance: maint! }),
    onSuccess: () => {
      toast.success('Settings saved')
      qc.invalidateQueries({ queryKey: ['settings'] })
    },
    onError: (e) => toast.error(e.message),
  })
  const changePw = useMutation({
    mutationFn: () => endpoints.changePassword(pw.current, pw.next),
    onSuccess: () => {
      toast.success('Password changed; other sessions were logged out')
      setPw({ current: '', next: '', confirm: '' })
      setPwErr(null)
    },
    onError: setPwErr,
  })
  if (q.isLoading || !general || !maint)
    return (
      <Page>
        <PageHeader title="Settings" />
        <ListSkeleton />
      </Page>
    )
  if (q.error)
    return (
      <Page>
        <PageHeader title="Settings" />
        <ErrorState error={q.error} onRetry={() => q.refetch()} />
      </Page>
    )
  const a = q.data!.about
  const num = (label: string, key: keyof SettingsT['maintenance'], help: string) => (
    <div className="space-y-1">
      <Label htmlFor={key}>{label}</Label>
      <Input id={key} type="number" min={0} value={maint[key]} onChange={(e) => setMaint({ ...maint, [key]: Number(e.target.value) })} />
      <p className="text-[11px] text-muted-foreground">{help}</p>
    </div>
  )
  return (
    <Page className="max-w-4xl">
      <PageHeader title="Settings" />
      <div className="grid gap-4">
        <Card>
          <CardHeader>
            <CardTitle>Appearance</CardTitle>
            <CardDescription>Follows the system by default.</CardDescription>
          </CardHeader>
          <CardContent>
            <div className="max-w-xs">
              <Select value={theme} onValueChange={(v) => setTheme(v as typeof theme)}>
                <SelectTrigger>
                  <SelectValue />
                </SelectTrigger>
                <SelectContent>
                  <SelectItem value="system">System</SelectItem>
                  <SelectItem value="light">Light</SelectItem>
                  <SelectItem value="dark">Dark</SelectItem>
                </SelectContent>
              </Select>
            </div>
          </CardContent>
        </Card>
        <Card>
          <CardHeader>
            <CardTitle>Sync defaults</CardTitle>
            <CardDescription>Applied to new pipelines. Existing pipelines keep their own interval.</CardDescription>
          </CardHeader>
          <CardContent className="grid gap-4 md:grid-cols-2">
            <div className="space-y-1">
              <Label htmlFor="dsi">Default sync interval (seconds)</Label>
              <Input id="dsi" type="number" min={q.data!.limits.min_sync_interval_s} value={general.default_sync_interval_s} onChange={(e) => setGeneral({ default_sync_interval_s: Number(e.target.value) })} />
              <p className="text-[11px] text-muted-foreground">Minimum {q.data!.limits.min_sync_interval_s}s (server setting MIN_SYNC_INTERVAL_S).</p>
            </div>
          </CardContent>
        </Card>
        <Card>
          <CardHeader>
            <CardTitle>Maintenance thresholds</CardTitle>
            <CardDescription>Minute-level commits create many small files. These thresholds decide when tables are compacted and cleaned.</CardDescription>
          </CardHeader>
          <CardContent className="grid gap-4 md:grid-cols-2 lg:grid-cols-4">
            {num('Small file (MB)', 'small_file_mb', 'Files below this size count as small.')}
            {num('Compact after N small files', 'small_files_trigger', 'Per partition of the raw table.')}
            {num('Target file size (MB)', 'target_file_mb', 'Rewritten files aim for this size.')}
            {num('Rewrite clean after N delete files', 'delete_files_trigger', 'Merges write positional delete files.')}
            {num('Keep snapshots (days)', 'snapshot_keep_days', 'Expire older snapshots daily, when the catalog allows it.')}
            {num('Keep at least N snapshots', 'snapshots_keep_last', '')}
            {num('Orphan files older than (days)', 'orphan_older_days', 'Weekly cleanup of unreferenced files.')}
            {num('Check every (minutes)', 'check_every_minutes', 'How often thresholds are evaluated.')}
          </CardContent>
        </Card>
        <div className="flex justify-end">
          <Button onClick={() => save.mutate()} loading={save.isPending}>
            Save settings
          </Button>
        </div>
        <Card>
          <CardHeader>
            <CardTitle>Change password</CardTitle>
            <CardDescription>At least 8 characters. Other sessions are logged out.</CardDescription>
          </CardHeader>
          <CardContent className="grid gap-3 md:grid-cols-3">
            <div className="space-y-1">
              <Label htmlFor="pw0">Current password</Label>
              <Input id="pw0" type="password" autoComplete="current-password" value={pw.current} onChange={(e) => setPw({ ...pw, current: e.target.value })} />
            </div>
            <div className="space-y-1">
              <Label htmlFor="pw1">New password</Label>
              <Input id="pw1" type="password" autoComplete="new-password" value={pw.next} onChange={(e) => setPw({ ...pw, next: e.target.value })} />
            </div>
            <div className="space-y-1">
              <Label htmlFor="pw2">Confirm</Label>
              <Input id="pw2" type="password" autoComplete="new-password" value={pw.confirm} onChange={(e) => setPw({ ...pw, confirm: e.target.value })} />
            </div>
            <div className="md:col-span-3">
              <InlineError error={pwErr} />
            </div>
            <div className="md:col-span-3 flex justify-end">
              <Button variant="outline" disabled={!pw.current || pw.next.length < 8 || pw.next !== pw.confirm} loading={changePw.isPending} onClick={() => changePw.mutate()}>
                Change password
              </Button>
            </div>
          </CardContent>
        </Card>
        <Card>
          <CardHeader>
            <CardTitle>About</CardTitle>
          </CardHeader>
          <CardContent>
            <dl className="grid grid-cols-[160px_1fr] gap-y-1.5 text-[13px]">
              <dt className="text-muted-foreground">Version</dt>
              <dd className="font-mono">{a.version} ({a.commit})</dd>
              <dt className="text-muted-foreground">Go</dt>
              <dd className="font-mono">{a.go}</dd>
              <dt className="text-muted-foreground">DuckDB</dt>
              <dd className="font-mono">{a.duckdb} · {Object.entries(a.extensions || {}).map(([k, v]) => `${k}@${v}`).join(', ')}</dd>
              <dt className="text-muted-foreground">Worker</dt>
              <dd>{a.worker ? 'in this process' : 'separate process'}</dd>
              <dt className="text-muted-foreground">Egress IP</dt>
              <dd className="font-mono">{a.egress_ip || 'unknown'}</dd>
              <dt className="text-muted-foreground">Private endpoints</dt>
              <dd>{a.private_allowed ? 'allowed' : 'blocked (ALLOW_PRIVATE_ENDPOINTS=false)'}</dd>
              <dt className="text-muted-foreground">SQL console</dt>
              <dd>read-only · {q.data!.limits.query_row_limit.toLocaleString()} rows · {q.data!.limits.query_timeout_s}s timeout</dd>
            </dl>
          </CardContent>
        </Card>
      </div>
    </Page>
  )
}
