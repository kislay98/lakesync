import { useEffect } from 'react'
import { useQueryClient } from '@tanstack/react-query'

export interface LiveEvent {
  type: 'pipeline' | 'collection' | 'run' | 'log' | 'maintenance' | 'hello'
  pipeline_id?: string
  collection?: string
  at: string
  data?: unknown
}

type Listener = (e: LiveEvent) => void
const listeners = new Set<Listener>()
let source: EventSource | null = null
let pollTimer: number | null = null
let connected = false

function connect() {
  if (source) return
  try {
    source = new EventSource('/api/events')
  } catch {
    startPolling()
    return
  }
  const handler = (ev: MessageEvent) => {
    let data: LiveEvent
    try {
      data = JSON.parse(ev.data)
    } catch {
      return
    }
    connected = true
    stopPolling()
    for (const l of listeners) l(data)
  }
  for (const t of ['pipeline', 'collection', 'run', 'log', 'maintenance']) source.addEventListener(t, handler)
  source.onopen = () => {
    connected = true
    stopPolling()
  }
  source.onerror = () => {
    connected = false
    startPolling()
  }
}

function startPolling() {
  if (pollTimer != null) return
  pollTimer = window.setInterval(() => {
    if (connected) return
    for (const l of listeners) l({ type: 'pipeline', at: new Date().toISOString() })
  }, 5000)
}

function stopPolling() {
  if (pollTimer != null) {
    window.clearInterval(pollTimer)
    pollTimer = null
  }
}

export function disconnectLive() {
  source?.close()
  source = null
  connected = false
  stopPolling()
}

/** Subscribe to live events; invalidates the given query keys on matching events. */
export function useLive(listener: Listener, deps: unknown[] = []) {
  useEffect(() => {
    connect()
    listeners.add(listener)
    return () => {
      listeners.delete(listener)
    }
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, deps)
}

/** Invalidate react-query data when the server reports changes. */
export function useLiveInvalidate(keys: (e: LiveEvent) => unknown[][] | null) {
  const qc = useQueryClient()
  useLive((e) => {
    const ks = keys(e)
    if (!ks) return
    for (const k of ks) qc.invalidateQueries({ queryKey: k })
  }, [qc])
}
