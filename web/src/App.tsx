import { Suspense, lazy } from 'react'
import { Navigate, Route, Routes, useLocation } from 'react-router-dom'
import { useAuth } from '@/hooks/use-auth'
import { AppLayout } from '@/components/layout'
import { Skeleton } from '@/components/ui/misc'

const Login = lazy(() => import('@/pages/login'))
const Setup = lazy(() => import('@/pages/setup'))
const Overview = lazy(() => import('@/pages/overview'))
const Connections = lazy(() => import('@/pages/connections'))
const Pipelines = lazy(() => import('@/pages/pipelines'))
const NewPipeline = lazy(() => import('@/pages/new-pipeline'))
const PipelineDetail = lazy(() => import('@/pages/pipeline-detail'))
const Tables = lazy(() => import('@/pages/tables'))
const DataViewer = lazy(() => import('@/pages/data-viewer'))
const TableHealthPage = lazy(() => import('@/pages/table-health'))
const SqlConsole = lazy(() => import('@/pages/sql-console'))
const SettingsPage = lazy(() => import('@/pages/settings'))

function Loading() {
  return (
    <div className="p-6 space-y-3">
      <Skeleton className="h-6 w-48" />
      <Skeleton className="h-24 w-full" />
      <Skeleton className="h-24 w-full" />
    </div>
  )
}

export default function App() {
  const { me, loading } = useAuth()
  const loc = useLocation()
  if (loading) return <Loading />
  if (!me) {
    return (
      <Suspense fallback={<Loading />}>
        <Routes>
          <Route path="/login" element={<Login />} />
          <Route path="*" element={<Navigate to="/login" replace state={{ from: loc.pathname + loc.search }} />} />
        </Routes>
      </Suspense>
    )
  }
  return (
    <Suspense fallback={<Loading />}>
      <Routes>
        <Route path="/login" element={<Navigate to="/" replace />} />
        <Route path="/setup" element={<Setup />} />
        <Route element={<AppLayout />}>
          <Route index element={<Overview />} />
          <Route path="/connections" element={<Connections />} />
          <Route path="/pipelines" element={<Pipelines />} />
          <Route path="/pipelines/new" element={<NewPipeline />} />
          <Route path="/pipelines/:id" element={<PipelineDetail />} />
          <Route path="/tables" element={<Tables />} />
          <Route path="/tables/:ns/:table" element={<DataViewer />} />
          <Route path="/tables/:ns/:table/health" element={<TableHealthPage />} />
          <Route path="/sql" element={<SqlConsole />} />
          <Route path="/settings" element={<SettingsPage />} />
          <Route path="*" element={<Navigate to="/" replace />} />
        </Route>
      </Routes>
    </Suspense>
  )
}
