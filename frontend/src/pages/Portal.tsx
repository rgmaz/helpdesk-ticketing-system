import { useState } from 'react'
import { useNavigate } from 'react-router-dom'
import { NewTicketDialog } from '../components/NewTicketDialog'
import { StatusChips, TicketTable } from '../components/TicketTable'
import { Card, Empty } from '../components/ui'
import { useTicketList } from '../hooks/useTicketList'
import { useAuth } from '../state/AuthContext'

/**
 * The requester portal: my own tickets, and the button to file another.
 *
 * The server pins an end user's queries to their own tickets, so this page
 * never sends a requester filter and cannot accidentally show somebody else's
 * work. Staff reach the same route for the tickets they filed themselves, which
 * is why it is available to every role.
 */
export function PortalPage() {
  const { user } = useAuth()
  const navigate = useNavigate()
  const [composing, setComposing] = useState(false)

  // 'all' shows everything including closed tickets; 'open' is the working set.
  const list = useTicketList({
    base: { requester: 'me', open: true },
  })

  const showClosed = (list.query.open ?? true) === false

  return (
    <>
      <div className="page-head">
        <div>
          <h1>My tickets</h1>
          <p>Everything you have raised, and where each one stands.</p>
        </div>
        <button type="button" className="btn btn-primary" onClick={() => setComposing(true)}>
          New ticket
        </button>
      </div>

      <div className="toolbar">
        <input
          className="grow"
          type="search"
          placeholder="Search your tickets…"
          value={list.search}
          onChange={(event) => list.setSearch(event.target.value)}
          aria-label="Search your tickets"
        />
        <div className="chips" role="group" aria-label="Which tickets to show">
          <button
            type="button"
            className={`chip${!showClosed ? ' on' : ''}`}
            aria-pressed={!showClosed}
            onClick={() => list.setQuery({ open: true })}
          >
            Still open
          </button>
          <button
            type="button"
            className={`chip${showClosed ? ' on' : ''}`}
            aria-pressed={showClosed}
            onClick={() => list.setQuery({ open: false })}
          >
            Everything
          </button>
        </div>
      </div>

      <div style={{ marginBottom: 14 }}>
        <StatusChips
          value={list.query.status ?? []}
          counts={list.counts}
          onChange={(status) => list.setQuery({ status })}
        />
      </div>

      {list.initial && list.tickets.length === 0 ? (
        <Card>
          <Empty
            title={`Hello, ${user?.full_name?.split(' ')[0] || user?.username}`}
            hint="You have not raised any tickets yet. If something is not working, this is the place to say so."
            action={
              <button type="button" className="btn btn-primary" onClick={() => setComposing(true)}>
                Raise your first ticket
              </button>
            }
          />
        </Card>
      ) : (
        <TicketTable
          tickets={list.tickets}
          loading={list.loading}
          error={list.error}
          page={list.page.page}
          pages={list.page.pages}
          total={list.page.total}
          limit={list.page.limit}
          sortBy={list.query.sort_by ?? 'due'}
          sortDir={list.query.sort_dir ?? 'asc'}
          snippets={Boolean(list.search)}
          onSort={list.toggleSort}
          onPage={list.setPage}
          emptyTitle="No tickets match these filters"
          emptyHint="Try clearing the status chips or widening the search."
        />
      )}

      {composing && (
        <NewTicketDialog
          onClose={() => setComposing(false)}
          onCreated={(id) => navigate(`/tickets/${id}`)}
        />
      )}
    </>
  )
}
