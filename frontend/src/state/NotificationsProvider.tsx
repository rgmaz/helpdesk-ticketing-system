import { useCallback, useEffect, useMemo, useRef, useState, type ReactNode } from 'react'
import { api } from '../api/client'
import { connectStream, type StreamStatus } from '../api/stream'
import type { StreamEvent } from '../api/types'
import { useAuth } from './AuthContext'
import { NotificationsContext, type NotificationsState } from './NotificationsContext'
import { useToast } from './ToastContext'

/**
 * Holds the SSE connection for the whole app.
 *
 * One connection serves every view: the ticket table, the detail thread and the
 * dashboard all listen to the same stream, so a status change made by a
 * technician updates the administrator's open page without a refresh.
 *
 * Events are also persisted server-side, so a client that was offline replays
 * from the notifications list rather than losing them.
 */
export function NotificationsProvider({ children }: { children: ReactNode }) {
  const { user } = useAuth()
  const toast = useToast()

  /**
   * The badge is stored against the identity it belongs to rather than as a bare
   * count. Signing out must show zero, and signing back in as somebody else must
   * not inherit the previous person's unread total — deriving it from the stored
   * owner means that needs no effect to reset it, so it is true on the very
   * first render after a change rather than one render late.
   */
  const [badge, setBadge] = useState<{ owner: number | null; count: number }>({
    owner: null,
    count: 0,
  })
  const [status, setStatus] = useState<StreamStatus>('connecting')

  const unread = badge.owner === (user?.id ?? null) ? badge.count : 0
  // Likewise, with nobody signed in there is nothing to be connected to.
  const liveStatus: StreamStatus = user ? status : 'closed'

  /** Sets the count, claiming it for whoever is signed in right now. */
  const setUnread = useCallback(
    (next: number | ((current: number) => number)) => {
      const owner = user?.id ?? null
      setBadge((current) => {
        const count = current.owner === owner ? current.count : 0
        return { owner, count: typeof next === 'function' ? next(count) : next }
      })
    },
    [user],
  )

  // Handlers are held in a ref so adding or removing one does not tear down and
  // re-establish the stream, which would drop events during the swap.
  const handlers = useRef(new Set<(event: StreamEvent) => void>())

  const subscribe = useCallback((handler: (event: StreamEvent) => void) => {
    handlers.current.add(handler)
    return () => {
      handlers.current.delete(handler)
    }
  }, [])

  const refreshUnread = useCallback(async () => {
    try {
      const { unread: count } = await api.unreadCount()
      setUnread(count)
    } catch {
      // A failed badge fetch is cosmetic; the next event or refresh retries.
    }
  }, [setUnread])

  useEffect(() => {
    if (!user) return

    // Fetching the badge on (re)connect is synchronising with the server, which
    // is the one thing an effect is for. The lint rule fires because the state
    // write is reachable from here, but refreshUnread is async: setUnread runs
    // after an await, so this is not a synchronous write during the effect and
    // cannot cascade a render.
    // oxlint-disable-next-line react/set-state-in-effect
    void refreshUnread()

    const connection = connectStream({
      onStatus: setStatus,
      onEvent: (event) => {
        // Synchronising with an external system is exactly what an effect is
        // for, and an arriving SSE event is the archetypal case: it happens
        // after the effect has already run, so this is not a cascading render.
        setUnread((n) => n + 1)
        for (const handler of handlers.current) handler(event)

        // Breaches and escalations demand attention regardless of which page
        // is open, so they surface as a toast. Everything else updates the
        // views that care about it without interrupting.
        const urgent = event.level === 'error' || event.type.startsWith('sla_')
        if (urgent) {
          toast.notify(event.body || event.title, event.level === 'error' ? 'error' : 'warning')
        }
      },
    })

    return () => connection.close()
    // Reconnect when the signed-in identity changes, not on every render.
  }, [user, refreshUnread, toast, setUnread])

  const markAllRead = useCallback(() => {
    setUnread(0)
    // Optimistic: the badge clears immediately and a failure re-fetches the real
    // count rather than leaving the user with a wrong number.
    void api.markAllRead().catch(() => {
      void refreshUnread()
    })
  }, [refreshUnread, setUnread])

  const markRead = useCallback(
    (id: number) => {
      setUnread((n) => Math.max(0, n - 1))
      void api.markRead(id).catch(() => {
        void refreshUnread()
      })
    },
    [refreshUnread, setUnread],
  )

  const value = useMemo<NotificationsState>(
    () => ({ unread, status: liveStatus, subscribe, markAllRead, markRead }),
    [unread, liveStatus, subscribe, markAllRead, markRead],
  )

  return <NotificationsContext.Provider value={value}>{children}</NotificationsContext.Provider>
}