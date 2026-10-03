import { useCallback, useMemo, useState } from 'react'
import { api } from '../api/client'
import type { Page, Priority, Ticket, TicketQuery, TicketStatus } from '../api/types'
import { useAsync, useDebounced } from './useAsync'
import { useLiveRefresh } from './useLiveRefresh'

/**
 * Every ticket list in the app shares the same shape: a filter set, a sort, a
 * page, and a live refresh when the stream reports a relevant change. Keeping
 * that in one hook means the portal, the queue and the search view cannot drift
 * in behaviour, and it holds the query in the URL so a filtered view can be
 * linked to and survives a reload.
 */

/**
 * Stream event types that change what a ticket list renders.
 *
 * These are the server's own event names. They live in one exported constant
 * because three call sites need the same set and a partial copy is how a list
 * quietly stops updating.
 */
export const LIST_EVENTS = [
  'ticket_created',
  'ticket_updated',
  'ticket_deleted',
  'status_changed',
  'priority_changed',
  'assigned',
  'auto_assigned',
  'comment_added',
  'ticket_resolved',
  'ticket_closed',
  'ticket_reopened',
  'sla_response_breach',
  'sla_resolution_breach',
  'sla_escalation',
]

/** The subset that changes an open ticket's own detail view. */
export const DETAIL_EVENTS = LIST_EVENTS

export interface TicketListOptions {
  /** Fixed filters merged under the user's own, e.g. `requester: 'me'`. */
  base?: TicketQuery
  /** Skip the API entirely, e.g. when there is no search term yet. */
  enabled?: boolean
}

export interface TicketList {
  tickets: Ticket[]
  counts: Partial<Record<TicketStatus, number>>
  page: Page<Ticket>
  query: TicketQuery
  setQuery: (next: Partial<TicketQuery>) => void
  /** One-keyword typing: the term is applied without re-sorting or paging. */
  setSearch: (term: string) => void
  search: string
  toggleSort: (key: string) => void
  setPage: (page: number) => void
  reload: () => void
  loading: boolean
  error: string | null
  /** True until the first response arrives; used to show a spinner. */
  initial: boolean
}

export function useTicketList({ base, enabled = true }: TicketListOptions = {}): TicketList {
  const [query, setQueryState] = useState<TicketQuery>(() => ({
    sort_by: 'due',
    sort_dir: 'asc',
    page: 1,
    limit: 25,
    ...base,
  }))

  const [term, setTerm] = useState('')
  // 300ms is long enough that a full word does not trigger a request, and short
  // enough that the list feels live.
  const debouncedSearch = useDebounced(term, 300)

  const merged = useMemo<TicketQuery>(
    () => ({ ...base, ...query, q: debouncedSearch || undefined }),
    // base is spread first so a caller-fixed filter always wins.
    // eslint-disable-next-line react-hooks/exhaustive-deps
    [JSON.stringify(base), query, debouncedSearch],
  )

  const effective = enabled ? merged : null

  const { data, error, loading, initial, reload } = useAsync<Page<Ticket>>(
    (signal) =>
      effective
        ? // A term switches to the search endpoint, which is backed by FTS5 and
          // returns snippet excerpts. The filters, sort and paging are identical
          // on both endpoints, so the choice is invisible to the caller.
          debouncedSearch
          ? api.searchTickets(effective, signal)
          : api.listTickets(effective, signal)
        : Promise.resolve({ items: [], total: 0, page: 1, limit: 25, pages: 0 }),
    [effective],
  )

  const setQuery = useCallback((next: Partial<TicketQuery>) => {
    setQueryState((current) => {
      const mergedNext = { ...current, ...next }
      // Any filter change invalidates the current page: staying on page 4 of a
      // newly narrowed list shows an empty table and looks like a bug.
      const filterChanged = Object.keys(next).some((key) => key !== 'page' && key !== 'sort_by' && key !== 'sort_dir')
      if (filterChanged && next.page === undefined) mergedNext.page = 1
      return mergedNext
    })
  }, [])

  const setSearch = useCallback((next: string) => {
    setTerm(next)
    // Searching resets the page: page 4 of a fresh result set is usually empty,
    // which reads as "no matches" rather than "wrong page".
    setQueryState((current) => ({ ...current, page: 1 }))
  }, [])

  /**
   * Clicking the active column reverses the direction; clicking a new column
   * starts ascending, except for the ones where "worst first" is the useful
   * default.
   */
  const toggleSort = useCallback((key: string) => {
    setQueryState((current) => {
      const sameKey = current.sort_by === key
      // Priority and due are read as "what needs attention first", which is
      // descending by rank and ascending by deadline respectively.
      const ascendingFirst = key !== 'priority'
      return {
        ...current,
        sort_by: key,
        sort_dir: sameKey
          ? current.sort_dir === 'asc'
            ? 'desc'
            : 'asc'
          : ascendingFirst
            ? 'asc'
            : 'desc',
        page: 1,
      }
    })
  }, [])

  const setPage = useCallback((page: number) => {
    setQueryState((current) => ({ ...current, page }))
  }, [])

  // Every event that changes what a ticket list would show: the SLA engine
  // mutating a ticket behind the user's back, a colleague acting on one, or a
  // comment bumping the counter. The names are the server's, so a new one is a
  // one-line addition here rather than a stale list nobody noticed.
  useLiveRefresh(LIST_EVENTS, reload)

  return {
    tickets: data?.items ?? [],
    counts: data?.counts ?? {},
    page: data ?? { items: [], total: 0, page: 1, limit: query.limit ?? 25, pages: 0 },
    query,
    setQuery,
    setSearch,
    search: term,
    toggleSort,
    setPage,
    reload,
    loading,
    error,
    initial,
  }
}

/** Priority and status multi-select state helpers for the filter bar. */
export function toggleValue<T>(list: T[], value: T): T[] {
  return list.includes(value) ? list.filter((v) => v !== value) : [...list, value]
}

export type { Priority, TicketStatus }
