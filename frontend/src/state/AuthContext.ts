import { createContext, useContext } from 'react'
import type { User } from '../api/types'

/**
 * The signed-in identity and the actions that change it.
 *
 * Split from the provider so this file holds no components: that keeps Fast
 * Refresh able to swap the provider without invalidating every consumer's hook,
 * and it means the shape of the context can be read without wading through the
 * session logic.
 */
export interface AuthState {
  user: User | null
  /** True until the stored token has been checked against the server. */
  initialising: boolean
  /** Technicians and administrators; see internal notes and the queue. */
  isStaff: boolean
  isAdmin: boolean
  signIn: (identifier: string, password: string) => Promise<void>
  register: (input: RegisterInput) => Promise<void>
  signOut: () => void
  /** Re-reads the profile, e.g. after an administrator changes the account. */
  refresh: () => Promise<void>
}

export interface RegisterInput {
  username: string
  email: string
  password: string
  full_name: string
}

export const AuthContext = createContext<AuthState | null>(null)

export function useAuth(): AuthState {
  const ctx = useContext(AuthContext)
  // Throwing beats returning null: a component outside the provider is a wiring
  // mistake that should fail loudly at the point of the bug, not render blank.
  if (!ctx) throw new Error('useAuth must be used inside an AuthProvider')
  return ctx
}