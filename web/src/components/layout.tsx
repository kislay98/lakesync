import { useEffect, useState } from 'react'
import { NavLink, Outlet, useLocation } from 'react-router-dom'
import { useQuery } from '@tanstack/react-query'
import { Activity, Database, GitBranch, Home, LogOut, Monitor, Moon, Plug, Search, Settings, Sun, TerminalSquare, Table2, Waves } from 'lucide-react'
import { endpoints } from '@/lib/api'
import { cn } from '@/lib/utils'
import { useAuth } from '@/hooks/use-auth'
import { useTheme } from '@/hooks/use-theme'
import { Button } from '@/components/ui/button'
import { Kbd } from '@/components/ui/misc'
import { DropdownMenu, DropdownMenuContent, DropdownMenuItem, DropdownMenuLabel, DropdownMenuSeparator, DropdownMenuTrigger } from '@/components/ui/dropdown-menu'
import { CommandPalette } from '@/components/command-palette'
import { Tip } from '@/components/ui/tooltip'
import { useLiveInvalidate } from '@/lib/live'

const nav = [
  { to: '/', label: 'Overview', icon: Home, end: true },
  { to: '/pipelines', label: 'Pipelines', icon: GitBranch },
  { to: '/connections', label: 'Connections', icon: Plug },
  { to: '/tables', label: 'Tables', icon: Table2 },
  { to: '/sql', label: 'SQL console', icon: TerminalSquare },
  { to: '/settings', label: 'Settings', icon: Settings },
]

export function AppLayout() {
  const { me, logout } = useAuth()
  const { theme, setTheme } = useTheme()
  const loc = useLocation()
  const [liveOk, setLiveOk] = useState(true)
  useLiveInvalidate((e) => {
    setLiveOk(true)
    switch (e.type) {
      case 'pipeline':
      case 'collection':
        return [['pipelines'], ['pipeline', e.pipeline_id], ['overview'], ['tables']]
      case 'run':
        return [['runs', e.pipeline_id]]
      case 'log':
        return [['logs', e.pipeline_id]]
      case 'maintenance':
        return [['runs', e.pipeline_id], ['health']]
    }
    return null
  })
  const overview = useQuery({ queryKey: ['overview'], queryFn: endpoints.overview, refetchInterval: 30000 })
  useEffect(() => {
    document.title = `${me?.app.name || 'lakesync'}`
  }, [me])
  const attention = overview.data?.totals.attention || 0
  return (
    <div className="flex h-full min-h-screen">
      <aside className="hidden w-56 shrink-0 flex-col border-r bg-sidebar md:flex">
        <div className="flex h-14 items-center gap-2 px-4">
          <div className="flex h-7 w-7 items-center justify-center rounded-md bg-primary text-primary-foreground">
            <Waves className="h-4 w-4" />
          </div>
          <span className="text-sm font-semibold tracking-tight">{me?.app.name || 'lakesync'}</span>
        </div>
        <nav className="flex-1 space-y-0.5 px-2 py-1">
          {nav.map((n) => (
            <NavLink
              key={n.to}
              to={n.to}
              end={n.end}
              className={({ isActive }) => cn('flex items-center gap-2.5 rounded-md px-2.5 py-1.5 text-[13px] font-medium text-muted-foreground transition-colors hover:bg-accent hover:text-foreground', isActive && 'bg-accent text-foreground')}
            >
              <n.icon className="h-4 w-4" />
              {n.label}
              {n.to === '/pipelines' && attention > 0 ? <span className="ml-auto rounded-full bg-destructive/15 px-1.5 text-[10px] font-semibold text-destructive">{attention}</span> : null}
            </NavLink>
          ))}
        </nav>
        <div className="border-t px-3 py-3 text-[11px] text-muted-foreground">
          <div className="flex items-center gap-1.5">
            <span className={cn('h-1.5 w-1.5 rounded-full', liveOk ? 'bg-success' : 'bg-warning')} />
            {liveOk ? 'Live updates' : 'Polling'}
          </div>
          <div className="mt-1 truncate">v{me?.app.version}{me?.app.commit && me.app.commit !== 'unknown' ? ` · ${me.app.commit.slice(0, 7)}` : ''}</div>
        </div>
      </aside>
      <div className="flex min-w-0 flex-1 flex-col">
        <header className="flex h-14 shrink-0 items-center gap-3 border-b bg-background/80 px-4 backdrop-blur md:px-6">
          <div className="flex items-center gap-2 md:hidden">
            <Waves className="h-4 w-4 text-primary" />
            <span className="text-sm font-semibold">{me?.app.name}</span>
          </div>
          <button
            type="button"
            onClick={() => window.dispatchEvent(new Event('lakesync:palette'))}
            className="ml-auto flex h-8 w-64 max-w-[40vw] items-center gap-2 rounded-md border bg-card px-2.5 text-[13px] text-muted-foreground shadow-xs hover:bg-accent"
          >
            <Search className="h-4 w-4" />
            <span className="flex-1 text-left">Jump to…</span>
            <Kbd>⌘K</Kbd>
          </button>
          <Tip label="Theme">
            <DropdownMenu>
              <DropdownMenuTrigger asChild>
                <Button variant="ghost" size="icon" aria-label="Theme">
                  {theme === 'dark' ? <Moon className="h-4 w-4" /> : theme === 'light' ? <Sun className="h-4 w-4" /> : <Monitor className="h-4 w-4" />}
                </Button>
              </DropdownMenuTrigger>
              <DropdownMenuContent align="end">
                <DropdownMenuItem onSelect={() => setTheme('light')}>
                  <Sun /> Light
                </DropdownMenuItem>
                <DropdownMenuItem onSelect={() => setTheme('dark')}>
                  <Moon /> Dark
                </DropdownMenuItem>
                <DropdownMenuItem onSelect={() => setTheme('system')}>
                  <Monitor /> System
                </DropdownMenuItem>
              </DropdownMenuContent>
            </DropdownMenu>
          </Tip>
          <DropdownMenu>
            <DropdownMenuTrigger asChild>
              <Button variant="ghost" size="sm" className="gap-2 px-2">
                <span className="flex h-6 w-6 items-center justify-center rounded-full bg-primary/15 text-[11px] font-semibold uppercase text-primary">{me?.user.email.slice(0, 1)}</span>
                <span className="hidden max-w-[160px] truncate text-[13px] sm:inline">{me?.user.email}</span>
              </Button>
            </DropdownMenuTrigger>
            <DropdownMenuContent align="end" className="w-56">
              <DropdownMenuLabel className="truncate">{me?.user.email}</DropdownMenuLabel>
              <DropdownMenuSeparator />
              <DropdownMenuItem asChild>
                <NavLink to="/settings">
                  <Settings /> Settings
                </NavLink>
              </DropdownMenuItem>
              <DropdownMenuItem asChild>
                <a href="/api/openapi.json" target="_blank" rel="noreferrer">
                  <Activity /> API reference
                </a>
              </DropdownMenuItem>
              <DropdownMenuItem asChild>
                <a href="/metrics" target="_blank" rel="noreferrer">
                  <Database /> Metrics
                </a>
              </DropdownMenuItem>
              <DropdownMenuSeparator />
              <DropdownMenuItem onSelect={() => logout()}>
                <LogOut /> Log out
              </DropdownMenuItem>
            </DropdownMenuContent>
          </DropdownMenu>
        </header>
        <nav className="flex gap-1 overflow-x-auto border-b px-2 py-1 md:hidden">
          {nav.map((n) => (
            <NavLink key={n.to} to={n.to} end={n.end} className={({ isActive }) => cn('rounded-md px-2.5 py-1 text-xs font-medium text-muted-foreground', isActive && 'bg-accent text-foreground')}>
              {n.label}
            </NavLink>
          ))}
        </nav>
        <main key={loc.pathname} className="flex min-h-0 flex-1 flex-col overflow-auto">
          <Outlet />
        </main>
      </div>
      <CommandPalette />
    </div>
  )
}

export function Page({ children, className, full }: { children: React.ReactNode; className?: string; full?: boolean }) {
  return <div className={cn('mx-auto w-full flex-1 px-4 py-5 md:px-6', full ? 'flex min-h-0 flex-col' : 'max-w-7xl', className)}>{children}</div>
}
