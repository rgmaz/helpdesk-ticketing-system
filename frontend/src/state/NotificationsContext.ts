import { createContext, useContext } from 'react'
// The status union is the stream module's to define, not a second copy here.
import type { StreamStatus } from '../api/stream'
import type { StreamEvent } from '../api/types'

/** Split from the provider so this file holds no components; see AuthContext.ts. */

export interface NotificationsState {
  unread: number
  /** The live connection's state, for showing it in the UI. */
  status: StreamStatus
  /** Subscribers refresh on relevant events instead of polling. */
  subscribe: (handler: (event: StreamEvent) => void) => () => void
  markAllRead: () => void
  markRead: (id: number) => void
}

export const NotificationsContext = createContext<NotificationsState | null>(null)

export function useNotifications(): NotificationsState {
  const ctx = useContext(NotificationsContext)
  if (!ctx) throw new Error('useNotifications must be used inside a NotificationsProvider')
  return ctx
}