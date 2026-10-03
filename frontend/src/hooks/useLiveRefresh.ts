import { useEffect, useRef } from 'react'
import type { StreamEvent } from '../api/types'
import { useNotifications } from '../state/NotificationsContext'

/**
 * Re-runs a loader when a relevant event arrives on the live stream.
 *
 * Matching on exact names keeps this from firing on everything: a new comment
 * should refresh a ticket's thread and a list's comment counter, but not the
 * administrator's dashboard. A name ending in '*' is a prefix, for the cases
 * where every member of a family matters.
 *
 * The reload function is held in a ref rather than a dependency, so a caller can
 * pass an inline closure without re-subscribing on every render.
 */
export function useLiveRefresh(events: string[], reload: () => void) {
  const { subscribe } = useNotifications()
  const latest = useRef(reload)

  // Synced in an effect rather than during render: writing a ref while rendering
  // is a side effect that a discarded render would already have leaked. The
  // subscription below reads the ref at event time, long after the commit.
  useEffect(() => {
    latest.current = reload
  })

  // Joining the names into one string gives a stable primitive dependency: an
  // array literal would be a new value on every render.
  const key = events.join('|')

  useEffect(() => {
    const wanted = key.split('|').filter(Boolean)

    return subscribe((event: StreamEvent) => {
      const matches = wanted.some((pattern) =>
        pattern.endsWith('*') ? event.type.startsWith(pattern.slice(0, -1)) : event.type === pattern,
      )
      if (matches) latest.current()
    })
  }, [key, subscribe])
}
