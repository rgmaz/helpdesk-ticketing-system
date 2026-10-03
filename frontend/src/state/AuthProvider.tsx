import { useCallback, useEffect, useMemo, useState, type ReactNode } from 'react'
import { api, getToken, onUnauthorized, setToken } from '../api/client'
import type { MeResponse, User } from '../api/types'
import { AuthContext, type AuthState, type RegisterInput } from './AuthContext'

/**
 * Owns the session for the whole app.
 *
 * The token lives in localStorage so a reload does not sign you out, but it is
 * never trusted on its own: the boot effect below checks it against the server
 * before the first authenticated paint.
 */
export function AuthProvider({ children }: { children: ReactNode }) {
  const [user, setUser] = useState<User | null>(null)
  const [initialising, setInitialising] = useState(true)

  /**
   * Validate the stored token on boot.
   *
   * Trusting a token straight out of localStorage would show a signed-in shell
   * for an expired or revoked session until the first request failed. Checking
   * it once here means the first paint is already correct.
   */
  useEffect(() => {
    let cancelled = false

    const boot = async () => {
      if (!getToken()) {
        setInitialising(false)
        return
      }
      try {
        const me: MeResponse = await api.me()
        if (!cancelled) setUser(me.user)
      } catch {
        // Expired, revoked, or the account was disabled. Start signed out.
        setToken(null)
        if (!cancelled) setUser(null)
      } finally {
        if (!cancelled) setInitialising(false)
      }
    }

    void boot()
    return () => {
      cancelled = true
    }
  }, [])

  /**
   * A 401 from anywhere in the app ends the session once, centrally. Without
   * this, a disabled account or a rotated secret would leave a shell UI that
   * silently fails on every action.
   */
  useEffect(() => {
    return onUnauthorized(() => {
      setToken(null)
      setUser(null)
    })
  }, [])

  const signIn = useCallback(async (identifier: string, password: string) => {
    const session = await api.login(identifier, password)
    setToken(session.token)
    setUser(session.user)
  }, [])

  /**
   * Register, then sign in.
   *
   * The register endpoint deliberately returns no token, so a second request is
   * needed. Doing it here rather than in the form keeps the caller from having
   * to remember to follow up.
   */
  const register = useCallback(async (input: RegisterInput) => {
    await api.register(input)
    const session = await api.login(input.username, input.password)
    setToken(session.token)
    setUser(session.user)
  }, [])

  const signOut = useCallback(() => {
    setToken(null)
    setUser(null)
  }, [])

  const refresh = useCallback(async () => {
    const me = await api.me()
    setUser(me.user)
  }, [])

  const value = useMemo<AuthState>(
    () => ({
      user,
      initialising,
      isStaff: user?.role === 'technician' || user?.role === 'admin',
      isAdmin: user?.role === 'admin',
      signIn,
      register,
      signOut,
      refresh,
    }),
    [user, initialising, signIn, register, signOut, refresh],
  )

  return <AuthContext.Provider value={value}>{children}</AuthContext.Provider>
}