import { useState } from 'react'
import { useNavigate } from 'react-router-dom'
import { NewTicketDialog } from '../components/NewTicketDialog'
import { StatusChips, TicketTable } from '../components/TicketTable'
import { Card } from '../components/ui'
import { toggleValue, useTicketList } from '../hooks/useTicketList'
import { humanize } from '../lib/format'
import { useAuth } from '../state/AuthContext'
import { useMeta } from '../state/MetaContext'
import { useAsync } from '../hooks/useAsync'
import { useLiveRefresh } from '../hooks/useLiveRefresh'
import { api } from '../api/client'

/**
 * The technician queue: everything, filtered by what is assigned and what is
 * about to breach.
 *
 * Two saved views are one click away rather than pre-selected, because a shared
 * default hides work: "my open tickets" is the natural starting point but it
 * also means the unassigned backlog nobody is looking at is invisible until you
 * go looking for it. The counts make the size of each visible.
 */
export function QueuePage() {
  const { user, isAdmin } = useAuth()
  const { meta } = useMeta()
  const navigate = useNavigate()
  const [composing, setComposing] = useState(false)

  const list = useTicketList({ base: { open: true } })
  const { status = [], priority = [], category = [] } = list.query

  // The technician directory rather than the admin workload report: that
  // endpoint is administrator-only, and a technician still needs to see who
  // else is holding work. It is a second query rather than a derivation of the
  // current page, because the page is filtered and paged and so cannot answer
  // "how loaded is each technician overall".
  const directory = useAsync(async () => (await api.technicians()).items, [])
  useLiveRefresh(['assigned', 'auto_assigned', 'status_changed', 'ticket_closed'], directory.reload)

  return (
    <>
      <div className="page-head">
        <div>
          <h1>Queue</h1>
          <p>
            Sorted by the nearest deadline first. New tickets arrive assigned to the least-loaded
            technician.
          </p>
        </div>
        <button type="button" className="btn btn-primary" onClick={() => setComposing(true)}>
          New ticket
        </button>
      </div>

      <div className="toolbar">
        <input
          className="grow"
          type="search"
          placeholder="Search titles, descriptions and resolution notes…"
          value={list.search}
          onChange={(event) => list.setSearch(event.target.value)}
          aria-label="Search tickets"
        />

        <select
          value={
            list.query.assignee === undefined
              ? ''
              : list.query.assignee === 'me'
                ? 'me'
                : list.query.assignee === 'none'
                  ? 'none'
                  : 'all'
          }
          onChange={(event) => {
            const value = event.target.value
            list.setQuery({ assignee: value === 'all' ? undefined : (value as 'me' | 'none') })
          }}
          aria-label="Assignee"
          style={{ width: 'auto' }}
        >
          <option value="">Anyone</option>
          <option value="me">Assigned to me</option>
          <option value="none">Unassigned</option>
        </select>

        <select
          value={category[0] ?? ''}
          onChange={(event) =>
            list.setQuery({ category: event.target.value ? [event.target.value] : [] })
          }
          aria-label="Category"
          style={{ width: 'auto' }}
        >
          <option value="">All categories</option>
          {(meta?.categories ?? []).map((c) => (
            <option key={c} value={c}>
              {humanize(c)}
            </option>
          ))}
        </select>

        <button
          type="button"
          className={`chip${list.query.breached ? ' on' : ''}`}
          aria-pressed={Boolean(list.query.breached)}
          onClick={() => list.setQuery({ breached: !list.query.breached })}
        >
          Breached only
        </button>
        <button
          type="button"
          className={`chip${list.query.escalated ? ' on' : ''}`}
          aria-pressed={Boolean(list.query.escalated)}
          onClick={() => list.setQuery({ escalated: !list.query.escalated })}
        >
          Escalated
        </button>
        <button
          type="button"
          className={`chip${list.query.open ? ' on' : ''}`}
          aria-pressed={Boolean(list.query.open)}
          onClick={() => list.setQuery({ open: !list.query.open })}
        >
          Open only
        </button>
      </div>

      <div style={{ marginBottom: 14 }}>
        <StatusChips
          value={status}
          counts={list.counts}
          onChange={(next) => list.setQuery({ status: next })}
        />
      </div>

      <div className="toolbar">
        <span className="faint" style={{ fontSize: '0.8125rem' }}>
          Priority:
        </span>
        {(meta?.priorities ?? []).map((p) => (
          <button
            key={p.value}
            type="button"
            className={`chip${priority.includes(p.value) ? ' on' : ''}`}
            aria-pressed={priority.includes(p.value)}
            onClick={() => list.setQuery({ priority: toggleValue(priority, p.value) })}
          >
            {p.label}
          </button>
        ))}
      </div>

      <div style={{ marginBottom: 16 }}>
        <Card title="Technician load" padded={false}>
          {directory.error && (
            <div className="card-pad">
              <span className="muted">Could not load the technician directory: {directory.error}</span>
            </div>
          )}
          {!directory.data && !directory.error && <div className="card-pad muted">Loading…</div>}
          {directory.data?.length === 0 && (
            <div className="card-pad muted">
              No technicians are active. New tickets will queue unassigned until one is created.
            </div>
          )}
          {directory.data && directory.data.length > 0 && (
            <div className="stat-grid card-pad">
              {directory.data.map((tech) => (
                <LoadTile
                  key={tech.id}
                  name={tech.full_name || tech.username}
                  active={tech.active_tickets}
                  overdue={tech.overdue_tickets}
                  resolved={tech.resolved_30d}
                  mine={tech.id === user?.id}
                  maxActive={Math.max(...directory.data!.map((t) => t.active_tickets))}
                  onClick={() => list.setQuery({ assignee: tech.id })}
                />
              ))}
            </div>
          )}
        </Card>
      </div>

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
        showRequester={isAdmin}
        snippets={Boolean(list.search)}
        onSort={list.toggleSort}
        onPage={list.setPage}
        emptyTitle="The queue is clear"
        emptyHint="No tickets match these filters."
      />

      {composing && (
        <NewTicketDialog
          onClose={() => setComposing(false)}
          onCreated={(id) => navigate(`/tickets/${id}`)}
          allowPriorityEscalation
        />
      )}
    </>
  )
}

/** One technician's load, with a bar so the distribution is readable at a glance. */
function LoadTile({
  name,
  active,
  overdue,
  resolved,
  mine,
  maxActive,
  onClick,
}: {
  name: string
  active: number
  overdue: number
  resolved: number
  mine: boolean
  /** The busiest load in the set, so the bars are relative to each other. */
  maxActive: number
  onClick: () => void
}) {
  // A relative bar is the right call here: on an absolute scale a queue where
  // everyone holds one ticket looks identical to a queue where one person holds
  // twenty, which is exactly the imbalance this strip exists to reveal.
  const pct = Math.min(100, Math.round((active / Math.max(1, maxActive)) * 100))

  return (
    <button
      type="button"
      onClick={onClick}
      className="stat"
      style={{ textAlign: 'left', cursor: 'pointer', font: 'inherit', color: 'inherit' }}
      title={`Show tickets assigned to ${name}`}
    >
      <div className="stat-label">
        {name}
        {mine && <span className="badge" style={{ marginLeft: 6 }}>you</span>}
      </div>
      <div className="stat-value" style={{ fontSize: '1.3rem' }}>
        {active}
        <span style={{ fontSize: '0.75rem', fontWeight: 400 }}> active</span>
      </div>
      <div className="stat-sub">
        {overdue > 0 ? (
          <span style={{ color: 'var(--danger)' }}>
            {overdue} overdue
          </span>
        ) : (
          'none overdue'
        )}
        {` · ${resolved} resolved`}
      </div>
      <div className="bar-track" style={{ marginTop: 8 }}>
        <div
          className={`bar-fill${overdue > 0 ? ' overdue' : pct > 60 ? ' warn' : ''}`}
          style={{ width: `${pct}%` }}
        />
      </div>
    </button>
  )
}
