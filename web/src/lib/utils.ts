import { clsx, type ClassValue } from 'clsx'
import { twMerge } from 'tailwind-merge'

export function cn(...inputs: ClassValue[]) {
  return twMerge(clsx(inputs))
}

/** 1234567 -> "1.2M", 15400 -> "15.4k" */
export function formatCount(n: number | null | undefined, opts: { exact?: boolean } = {}): string {
  if (n == null || Number.isNaN(n)) return '–'
  if (opts.exact || Math.abs(n) < 10_000) return n.toLocaleString()
  const units = [
    [1e9, 'B'],
    [1e6, 'M'],
    [1e3, 'k'],
  ] as const
  for (const [v, s] of units) {
    if (Math.abs(n) >= v) return `${(n / v).toFixed(n / v >= 100 ? 0 : 1)}${s}`
  }
  return n.toLocaleString()
}

export function formatBytes(b: number | null | undefined): string {
  if (b == null || Number.isNaN(b)) return '–'
  if (b < 1024) return `${b} B`
  const units = ['KB', 'MB', 'GB', 'TB', 'PB']
  let v = b / 1024
  let i = 0
  while (v >= 1024 && i < units.length - 1) {
    v /= 1024
    i++
  }
  return `${v.toFixed(v >= 100 ? 0 : 1)} ${units[i]}`
}

export function formatDuration(ms: number | null | undefined): string {
  if (ms == null || Number.isNaN(ms)) return '–'
  if (ms < 1000) return `${Math.round(ms)} ms`
  const s = ms / 1000
  if (s < 60) return `${s.toFixed(s < 10 ? 1 : 0)} s`
  const m = Math.floor(s / 60)
  if (m < 60) return `${m} min ${Math.round(s % 60)} s`
  const h = Math.floor(m / 60)
  if (h < 48) return `${h} h ${m % 60} min`
  return `${Math.floor(h / 24)} d ${h % 24} h`
}

export function formatSeconds(s: number | null | undefined): string {
  if (s == null) return '–'
  return formatDuration(s * 1000)
}

export function relativeTime(iso: string | null | undefined, now = Date.now()): string {
  if (!iso) return 'never'
  const t = new Date(iso).getTime()
  if (Number.isNaN(t)) return 'never'
  const diff = Math.max(0, now - t)
  const s = Math.floor(diff / 1000)
  if (s < 5) return 'just now'
  if (s < 60) return `${s} s ago`
  const m = Math.floor(s / 60)
  if (m < 60) return `${m} min ago`
  const h = Math.floor(m / 60)
  if (h < 24) return `${h} h ago`
  const d = Math.floor(h / 24)
  if (d < 30) return `${d} d ago`
  return new Date(iso).toLocaleDateString()
}

export function exactTime(iso: string | null | undefined): string {
  if (!iso) return ''
  const d = new Date(iso)
  if (Number.isNaN(d.getTime())) return iso
  return d.toLocaleString(undefined, { dateStyle: 'medium', timeStyle: 'medium' })
}

export function percent(done: number, total: number): number {
  if (!total || total <= 0) return done > 0 ? 100 : 0
  return Math.min(100, Math.round((done / total) * 100))
}

/** ETA in ms given a rate, or null. */
export function eta(done: number, total: number, startedAt: string | null | undefined): number | null {
  if (!startedAt || !total || done <= 0 || done >= total) return null
  const elapsed = Date.now() - new Date(startedAt).getTime()
  if (elapsed <= 0) return null
  const rate = done / elapsed
  return (total - done) / rate
}

export function isJsonString(s: unknown): boolean {
  if (typeof s !== 'string') return false
  const t = s.trim()
  if (!(t.startsWith('{') || t.startsWith('['))) return false
  try {
    JSON.parse(t)
    return true
  } catch {
    return false
  }
}

export function prettyJson(v: unknown): string {
  try {
    if (typeof v === 'string') return JSON.stringify(JSON.parse(v), null, 2)
    return JSON.stringify(v, null, 2)
  } catch {
    return String(v)
  }
}

export function truncate(s: string, n = 80): string {
  return s.length > n ? s.slice(0, n - 1) + '…' : s
}

export async function copyText(text: string): Promise<boolean> {
  try {
    await navigator.clipboard.writeText(text)
    return true
  } catch {
    return false
  }
}
