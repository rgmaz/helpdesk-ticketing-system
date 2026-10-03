import { createContext, useContext } from 'react'
import type { Meta, Priority, TicketStatus } from '../api/types'

/**
 * The server's vocabulary, plus the lookups built from it.
 *
 * Split from the provider so this file holds no components; see the note in
 * AuthContext.ts.
 */
export interface MetaState {
  meta: Meta | null
  loading: boolean
  error: string | null
  /** Fallback labels so the UI renders before /meta has resolved. */
  statusLabel: (status: TicketStatus) => string
  priorityLabel: (priority: Priority) => string
  /** Legal next states, from the server's own state machine. */
  nextStatuses: (status: TicketStatus) => TicketStatus[]
  priorityRank: (priority: Priority) => number
}

export const MetaContext = createContext<MetaState | null>(null)

export function useMeta(): MetaState {
  const ctx = useContext(MetaContext)
  if (!ctx) throw new Error('useMeta must be used inside a MetaProvider')
  return ctx
}