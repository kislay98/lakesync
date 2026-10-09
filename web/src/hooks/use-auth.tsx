import { createContext, useCallback, useContext, useEffect, useMemo, useState, type ReactNode } from 'react'
import { ApiError, endpoints, setCsrfToken, type Me } from '@/lib/api'
import { disconnectLive } from '@/lib/live'

interface AuthState {
  me: Me | null
  loading: boolean
  login: (email: string, password: string) => Promise<void>
  logout: () => Promise<void>
  refresh: () => Promise<void>
}

const AuthContext = createContext<AuthState | null>(null)

export function AuthProvider({ children }: { children: ReactNode }) {
  const [me, setMe] = useState<Me | null>(null)
  const [loading, setLoading] = useState(true)

  const refresh = useCallback(async () => {
    try {
      const m = await endpoints.me()
      setCsrfToken(m.csrf_token)
      setMe(m)
    } catch (e) {
      if (e instanceof ApiError && e.status === 401) setMe(null)
    } finally {
      setLoading(false)
    }
  }, [])

  useEffect(() => {
    refresh()
    const onUnauthorized = () => {
      setMe(null)
      disconnectLive()
    }
    window.addEventListener('lakesync:unauthorized', onUnauthorized)
    return () => window.removeEventListener('lakesync:unauthorized', onUnauthorized)
  }, [refresh])

  const login = useCallback(async (email: string, password: string) => {
    const m = await endpoints.login(email, password)
    setCsrfToken(m.csrf_token)
    setMe(m)
  }, [])

  const logout = useCallback(async () => {
    try {
      await endpoints.logout()
    } finally {
      disconnectLive()
      setMe(null)
    }
  }, [])

  const value = useMemo(() => ({ me, loading, login, logout, refresh }), [me, loading, login, logout, refresh])
  return <AuthContext.Provider value={value}>{children}</AuthContext.Provider>
}

export function useAuth(): AuthState {
  const ctx = useContext(AuthContext)
  if (!ctx) throw new Error('useAuth outside AuthProvider')
  return ctx
}
