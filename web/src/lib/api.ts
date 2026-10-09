// Typed client for the lakesync JSON API. Errors always carry {code, message, hint}.

export class ApiError extends Error {
  code: string
  hint: string
  status: number
  constructor(status: number, code: string, message: string, hint = '') {
    super(message)
    this.status = status
    this.code = code
    this.hint = hint
  }
}

let csrfToken = ''
export function setCsrfToken(t: string) {
  csrfToken = t
}

export async function api<T>(path: string, init: RequestInit & { json?: unknown } = {}): Promise<T> {
  const headers: Record<string, string> = { Accept: 'application/json', ...(init.headers as Record<string, string>) }
  let body = init.body
  if (init.json !== undefined) {
    headers['Content-Type'] = 'application/json'
    body = JSON.stringify(init.json)
  }
  const method = (init.method || 'GET').toUpperCase()
  if (method !== 'GET' && method !== 'HEAD' && csrfToken) headers['X-CSRF-Token'] = csrfToken
  const res = await fetch(path, { ...init, method, headers, body, credentials: 'same-origin' })
  if (res.status === 204) return undefined as T
  const text = await res.text()
  let data: unknown = null
  try {
    data = text ? JSON.parse(text) : null
  } catch {
    data = null
  }
  if (!res.ok) {
    const e = (data as { error?: { code?: string; message?: string; hint?: string } } | null)?.error
    if (res.status === 401 && !path.startsWith('/api/auth/login')) {
      window.dispatchEvent(new CustomEvent('lakesync:unauthorized'))
    }
    throw new ApiError(res.status, e?.code || 'error', e?.message || `Request failed (${res.status})`, e?.hint || '')
  }
  return data as T
}

// ---------- types ----------

export interface Me {
  user: { id: string; email: string; created_at: string }
  csrf_token: string
  app: { name: string; version: string; commit: string; dev_mode: boolean }
  egress_ip: string
  allow_private_endpoints: boolean
  worker_in_process: boolean
  local_stack_hints?: Record<string, { config: Record<string, unknown>; secrets: Record<string, string> }> | null
}

export interface JsonSchemaProperty {
  type?: string
  title?: string
  description?: string
  default?: unknown
  enum?: string[]
  format?: string
  'x-secret'?: boolean
  'x-order'?: number
  'x-placeholder'?: string
  'x-group'?: string
  'x-depends-on'?: Record<string, string>
}

export interface JsonSchema {
  type: string
  properties: Record<string, JsonSchemaProperty>
  required?: string[]
  'x-parse'?: { title?: string; description?: string; placeholder?: string }
}

export interface TypeInfo {
  type: string
  kind: 'source' | 'destination'
  title: string
  description: string
  schema: JsonSchema
  can_parse: boolean
}

export interface TestStep {
  name: string
  status: 'ok' | 'warn' | 'fail'
  duration_ms: number
  message?: string
  hint?: string
}

export interface TestReport {
  ok: boolean
  steps: TestStep[]
  info?: Record<string, string>
}

export interface Connection {
  id: string
  kind: 'source' | 'destination'
  type: string
  name: string
  config: Record<string, unknown>
  secret_fields: string[]
  in_use: number
  last_test_status: 'ok' | 'warn' | 'fail' | null
  last_test_report?: TestReport | null
  last_tested_at: string | null
  created_at: string
  updated_at: string
}

export interface Entity {
  namespace: string
  name: string
  estimated_count: number
  estimated_bytes: number
}

export type CollectionState = 'created' | 'copying' | 'streaming' | 'paused' | 'resyncing' | 'failed' | 'source_removed' | 'stopped'

export interface PipelineCollection {
  pipeline_id: string
  collection: string
  enabled: boolean
  state: CollectionState
  raw_table: string
  clean_table: string
  generation: number
  last_seq: number
  merged_seq: number
  snapshot_total_est: number
  snapshot_done: number
  last_event_at: string | null
  last_source_ts: string | null
  last_commit_at: string | null
  last_merge_at: string | null
  last_error: string | null
  state_message: string | null
  resync_reason: string | null
  requested_action: string | null
  failures: number
  schema?: { columns: { name: string; source_field: string; type: string }[] } | null
  updated_at: string
}

export interface Pipeline {
  id: string
  name: string
  source_id: string
  destination_id: string
  namespace: string
  sync_interval_s: number
  state: CollectionState
  desired_state: 'stopped' | 'running' | 'paused'
  settings: Record<string, unknown>
  created_at: string
  updated_at: string
  source: { id: string; name: string; type: string }
  destination: { id: string; name: string; type: string }
  collections: PipelineCollection[]
  stats: { rows_synced: number; events_last_hour: number; lag_seconds: number | null; collections: number; attention: string[] }
  oplog?: { window_seconds: number; oldest: string; newest: string; size_bytes?: number; warning?: string; paused_seconds?: number; error?: string } | null
}

export interface Run {
  id: number
  pipeline_id: string
  collection: string
  kind: 'copy' | 'stream' | 'merge' | 'maintenance' | 'resync'
  started_at: string
  ended_at: string | null
  status: string
  rows: number
  bytes: number
  error: string | null
  details?: Record<string, unknown>
}

export interface LogEntry {
  id: number
  pipeline_id: string
  collection: string
  level: 'debug' | 'info' | 'warn' | 'error'
  message: string
  fields?: Record<string, unknown>
  at: string
}

export interface FieldMapping {
  source_field: string
  column_name: string
  type: string
  conflict_count: number
  reason: string | null
}

export interface TableView {
  destination_id: string
  destination_name: string
  namespace: string
  name: string
  kind: 'raw' | 'clean' | 'other'
  pipeline_id?: string
  pipeline_name?: string
  collection?: string
  state?: CollectionState
  row_count: number
  size_bytes: number
  last_updated: string
  snapshots: number
  health: string
}

export interface ColumnMeta {
  name: string
  type: string
}

export interface RowsResult {
  columns: ColumnMeta[]
  rows: unknown[][]
  next_cursor?: string
  has_more: boolean
  total?: number
  duration_ms: number
}

export interface Filter {
  column: string
  op: 'eq' | 'neq' | 'contains' | 'starts' | 'gt' | 'gte' | 'lt' | 'lte' | 'between' | 'isnull' | 'notnull' | 'in'
  value?: unknown
  value2?: unknown
}

export interface HistoryEvent {
  seq: number
  op: string
  generation: number
  source_ts: string | null
  ingested_at: string
  cast_errors: string | null
  doc: Record<string, unknown> | null
  changed: string[]
  diff: Record<string, { before: unknown; after: unknown }>
}

export interface TableSchema {
  columns: { name: string; type: string; required: boolean; id: number; source_field?: string; conflicts: number; meta: boolean }[]
  mappings: FieldMapping[]
  namespace: string
  table: string
  destination_id: string
  pipeline?: { id: string; name: string; collection: string; raw_table: string; clean_table: string; state: CollectionState }
}

export interface Health {
  data_files: number
  small_files: number
  avg_file_bytes: number
  delete_files: number
  snapshots: number
  total_bytes: number
  row_count: number
  last_commit: string
  last_compaction: string
  partitions: number
  orphan_files: number
  orphan_bytes: number
  gc_enabled: boolean
  format_version: number
}

export interface TableHealth {
  health: Health
  status: 'healthy' | 'attention' | 'unhealthy'
  reason: string
  thresholds: Record<string, number>
  destination_id: string
  namespace: string
  table: string
  mappings?: FieldMapping[]
  conflicts?: number
  pipeline?: { id: string; name: string; collection: string; state: CollectionState; raw_table: string; clean_table: string }
  maintenance_runs?: Run[]
}

export interface SnapshotInfo {
  id: number
  parent_id?: number
  timestamp: string
  operation: string
  summary: Record<string, string>
  schema_id?: number
  is_current: boolean
}

export interface TypeShare {
  type: string
  count: number
  percent: number
  first_seen: string | null
  last_seen: string | null
  sample: string
}

export interface FieldDrift {
  field: string
  documents: number
  types: TypeShare[]
  first_seen: string | null
  last_seen: string | null
  conflicts: { key: string; seq: number; value: string }[]
}

export interface QueryResult {
  columns: ColumnMeta[]
  rows: unknown[][]
  row_count: number
  truncated: boolean
  duration_ms: number
}

export interface QueryHistoryEntry {
  id: number
  destination: string | null
  sql: string
  duration_ms: number
  rows: number
  error: string | null
  at: string
}

export interface Overview {
  pipelines: Pipeline[]
  connections: number
  totals: { rows_synced: number; events_last_hour: number; lag_seconds: number | null; attention: number }
  attention: { pipeline_id: string; pipeline: string; collection: string; table: string; reason: string; state: string }[]
  first_run: boolean
}

export interface Settings {
  general: { default_sync_interval_s: number }
  maintenance: {
    small_file_mb: number
    small_files_trigger: number
    target_file_mb: number
    delete_files_trigger: number
    snapshot_keep_days: number
    snapshots_keep_last: number
    orphan_older_days: number
    check_every_minutes: number
  }
  limits: { min_sync_interval_s: number; query_row_limit: number; query_timeout_s: number }
  about: { version: string; commit: string; go: string; duckdb: string; extensions: Record<string, string>; worker: boolean; egress_ip: string; private_allowed: boolean }
}

export interface MinuteCount {
  minute: string
  events: number
}

// ---------- calls ----------

export const endpoints = {
  me: () => api<Me>('/api/me'),
  login: (email: string, password: string) => api<Me>('/api/auth/login', { method: 'POST', json: { email, password } }),
  logout: () => api<{ ok: boolean }>('/api/auth/logout', { method: 'POST' }),
  changePassword: (current_password: string, new_password: string) => api<{ ok: boolean }>('/api/auth/password', { method: 'POST', json: { current_password, new_password } }),
  types: () => api<{ sources: TypeInfo[]; destinations: TypeInfo[] }>('/api/types'),
  parse: (type: string, text: string) => api<{ config: Record<string, unknown>; secrets: Record<string, string> }>(`/api/types/${type}/parse`, { method: 'POST', json: { text } }),
  connections: (kind?: string) => api<Connection[]>(`/api/connections${kind ? `?kind=${kind}` : ''}`),
  connection: (id: string) => api<Connection>(`/api/connections/${id}`),
  createConnection: (body: { kind: string; type: string; name: string; config: Record<string, unknown>; secrets: Record<string, string> }) => api<Connection>('/api/connections', { method: 'POST', json: body }),
  updateConnection: (id: string, body: { name: string; config: Record<string, unknown>; secrets: Record<string, string> }) => api<Connection>(`/api/connections/${id}`, { method: 'PUT', json: body }),
  deleteConnection: (id: string) => api<{ ok: boolean }>(`/api/connections/${id}`, { method: 'DELETE' }),
  testConnection: (id: string) => api<TestReport>(`/api/connections/${id}/test`, { method: 'POST' }),
  testDraft: (body: { kind: string; type: string; name: string; config: Record<string, unknown>; secrets: Record<string, string> }, id?: string) => api<TestReport>(`/api/connections/test${id ? `?id=${id}` : ''}`, { method: 'POST', json: body }),
  entities: (id: string) => api<Entity[]>(`/api/connections/${id}/entities`),
  pipelines: () => api<Pipeline[]>('/api/pipelines'),
  pipeline: (id: string) => api<Pipeline>(`/api/pipelines/${id}`),
  createPipeline: (body: Record<string, unknown>) => api<Pipeline>('/api/pipelines', { method: 'POST', json: body }),
  updatePipeline: (id: string, body: Record<string, unknown>) => api<Pipeline>(`/api/pipelines/${id}`, { method: 'PUT', json: body }),
  deletePipeline: (id: string, dropTables: boolean) => api<{ ok: boolean; tables_dropped: boolean; drop_errors: string[] }>(`/api/pipelines/${id}?drop_tables=${dropTables}`, { method: 'DELETE' }),
  pipelineAction: (id: string, action: 'start' | 'pause' | 'stop' | 'resync' | 'retry') => api<Pipeline>(`/api/pipelines/${id}/${action}`, { method: 'POST' }),
  collectionAction: (id: string, name: string, action: 'resync' | 'retry' | 'enable' | 'disable') => api<Pipeline>(`/api/pipelines/${id}/collections/${encodeURIComponent(name)}/${action}`, { method: 'POST' }),
  runs: (id: string, limit = 100) => api<Run[]>(`/api/pipelines/${id}/runs?limit=${limit}`),
  logs: (id: string, p: { level?: string; collection?: string; q?: string; before?: number; limit?: number }) => {
    const qs = new URLSearchParams()
    if (p.level) qs.set('level', p.level)
    if (p.collection) qs.set('collection', p.collection)
    if (p.q) qs.set('q', p.q)
    if (p.before) qs.set('before', String(p.before))
    if (p.limit) qs.set('limit', String(p.limit))
    return api<LogEntry[]>(`/api/pipelines/${id}/logs?${qs}`)
  },
  series: (id: string, minutes = 60, collection = '') => api<MinuteCount[]>(`/api/pipelines/${id}/series?minutes=${minutes}&collection=${encodeURIComponent(collection)}`),
  mappings: (id: string, collection: string) => api<FieldMapping[]>(`/api/pipelines/${id}/mappings?collection=${encodeURIComponent(collection)}`),
  overview: () => api<Overview>('/api/overview'),
  tables: (refresh = false) => api<TableView[]>(`/api/tables${refresh ? '?refresh=true' : ''}`),
  tableSchema: (ns: string, table: string, dest?: string) => api<TableSchema>(`/api/tables/${enc(ns)}/${enc(table)}/schema${dest ? `?destination=${dest}` : ''}`),
  tableRows: (ns: string, table: string, p: { limit?: number; cursor?: string; sort?: string; filters?: Filter[]; q?: string; columns?: string[]; destination?: string; total?: boolean }) => {
    const qs = new URLSearchParams()
    if (p.limit) qs.set('limit', String(p.limit))
    if (p.cursor) qs.set('cursor', p.cursor)
    if (p.sort) qs.set('sort', p.sort)
    if (p.filters && p.filters.length) qs.set('filter', JSON.stringify(p.filters))
    if (p.q) qs.set('q', p.q)
    if (p.columns && p.columns.length) qs.set('columns', p.columns.join(','))
    if (p.destination) qs.set('destination', p.destination)
    if (p.total === false) qs.set('total', 'false')
    return api<RowsResult>(`/api/tables/${enc(ns)}/${enc(table)}/rows?${qs}`)
  },
  tableHealth: (ns: string, table: string, dest?: string) => api<TableHealth>(`/api/tables/${enc(ns)}/${enc(table)}/health${dest ? `?destination=${dest}` : ''}`),
  tableSnapshots: (ns: string, table: string, dest?: string) => api<SnapshotInfo[]>(`/api/tables/${enc(ns)}/${enc(table)}/snapshots${dest ? `?destination=${dest}` : ''}`),
  tableDrift: (ns: string, table: string, dest?: string) => api<{ raw_table: string; fields: FieldDrift[] }>(`/api/tables/${enc(ns)}/${enc(table)}/drift${dest ? `?destination=${dest}` : ''}`),
  history: (ns: string, table: string, key: string, dest?: string) => api<{ key: string; raw_table: string; events: HistoryEvent[] }>(`/api/tables/${enc(ns)}/${enc(table)}/keys/${enc(key)}/history${dest ? `?destination=${dest}` : ''}`),
  maintenance: (ns: string, table: string, dest?: string) => api<{ queued: boolean; message?: string; results?: unknown[]; errors?: string[] }>(`/api/tables/${enc(ns)}/${enc(table)}/maintenance${dest ? `?destination=${dest}` : ''}`, { method: 'POST' }),
  query: (destination_id: string, sql: string) => api<{ result: QueryResult; row_limit: number; read_only: boolean }>('/api/query', { method: 'POST', json: { destination_id, sql } }),
  querySchema: (destination_id: string) => api<{ catalog: string; tables: { namespace: string; table: string; columns: string[] }[] }>(`/api/query/schema?destination_id=${destination_id}`),
  queryHistory: () => api<QueryHistoryEntry[] | null>('/api/query/history'),
  settings: () => api<Settings>('/api/settings'),
  updateSettings: (body: Partial<Pick<Settings, 'general' | 'maintenance'>>) => api<Settings>('/api/settings', { method: 'PUT', json: body }),
}

function enc(s: string) {
  return encodeURIComponent(s)
}
