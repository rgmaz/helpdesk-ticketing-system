import { useEffect, useMemo, useState, type ReactNode } from 'react'
import { api } from '../api/client'
import type { Meta, Priority, TicketStatus } from '../api/types'
import { humanize } from '../lib/format'
import { MetaContext, type MetaState } from './MetaContext'

/**
 * Fetches the backend's vocabulary once.
 *
 * Statuses, priorities and categories all come from GET /meta rather than a
 * hard-coded list in the client, so the two cannot drift: a status added on the
 * server appears in the dropdowns with no frontend change, and the action
 * buttons are built from the server's own transition table rather than a second
 * copy of the rules.
 *
 * The lookups are built with Maps so a label is an O(1) hit rather than a
 * find, and so the fallback for an unknown value is written exactly once.
 */
export function MetaProvider({ children }: { children: ReactNode }) {
  const [meta, setMeta] = useState<Meta | null>(null)
  const [loading, setLoading] = useState(true)
  const [error, setError] = useState<string | null>(null)

  useEffect(() => {
    let cancelled = false
    api
      .meta()
      .then((data) => {
        if (cancelled) return
        setMeta(data)
        setError(null)
      })
      .catch((err: Error) => {
        if (!cancelled) setError(err.message)
      })
      .finally(() => {
        if (!cancelled) setLoading(false)
      })
    return () => {
      cancelled = true
    }
  }, [])

  const value = useMemo<MetaState>(() => {
    const statusByValue = new Map<TicketStatus, Meta['statuses'][number]>(
      meta?.statuses.map((s) => [s.value, s]) ?? [],
    )
    const priorityByValue = new Map<Priority, Meta['priorities'][number]>(
      meta?.priorities.map((p) => [p.value, p]) ?? [],
    )

    return {
      meta,
      loading,
      error,
      // humanize is the fallback for a value /meta has not described yet, so a
      // status renders sensibly during the first paint rather than blank.
      statusLabel: (status) => statusByValue.get(status)?.label ?? humanize(status),
      priorityLabel: (priority) => priorityByValue.get(priority)?.label ?? humanize(priority),
      nextStatuses: (status) => statusByValue.get(status)?.next ?? [],
      priorityRank: (priority) => priorityByValue.get(priority)?.rank ?? 0,
    }
  }, [meta, loading, error])

  return <MetaContext.Provider value={value}>{children}</MetaContext.Provider>
}