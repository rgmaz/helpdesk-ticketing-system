import { useState } from 'react'
import { Link } from 'react-router-dom'
import { api } from '../api/client'
import type { Notification } from '../api/types'
import { Card, Empty, ErrorNote, Pager, Spinner, TimeStamp } from '../components/ui'
import { useAsync } from '../hooks/useAsync'
import { useLiveRefresh } from '../hooks/useLiveRefresh'
import { formatDateTime } from '../lib/format'
import { useNotifications } from '../state/NotificationsContext'

/**
 * Notification history.
 *
 * SSE is the low-latency layer and this list is the durable one. They are both
 * needed: a user who closes the tab for an hour should be able to see what
 * happened, and a client that reconnects after a dropped stream needs a
 * backlog to resync from rather than a gap it cannot detect.
 */
export function NotificationsPage() {
  const [unreadOnly, setUnreadOnly] = useState(false)
  const [page, setPage] = useState(1)
  const { unread, status, markAllRead, markRead } = useNotifications()

  const list = useAsync(
    async () => api.notifications(unreadOnly),
    [unreadOnly, page],
  )

  // A wildcard subscription: every event this user receives also produces a
  // stored notification, so anything arriving on the stream belongs in this
  // list. Filtering by kind would mean missing an event kind added later.
  useLiveRefresh(['*'], list.reload)

  return (
    <>
      <div className="page-head">
        <div>
          <h1>Notifications</h1>
          <p>
            {unread > 0 ? `${unread} unread.` : 'You are up to date.'} Updates arrive live over a
            server-sent stream ({status === 'open' ? 'connected' : status}).
          </p>
        </div>
        <button
          type="button"
          className="btn"
          onClick={markAllRead}
          disabled={unread === 0}
        >
          Mark all read
        </button>
      </div>

      <div className="toolbar">
        <div className="chips" role="group" aria-label="Which notifications to show">
          <button
            type="button"
            className={`chip${!unreadOnly ? ' on' : ''}`}
            aria-pressed={!unreadOnly}
            onClick={() => {
              setUnreadOnly(false)
              setPage(1)
            }}
          >
            Everything
          </button>
          <button
            type="button"
            className={`chip${unreadOnly ? ' on' : ''}`}
            aria-pressed={unreadOnly}
            onClick={() => {
              setUnreadOnly(true)
              setPage(1)
            }}
          >
            Unread{unread > 0 && <span className="count">{unread}</span>}
          </button>
        </div>
        <div className="grow" />
        <button type="button" className="btn btn-sm" onClick={list.reload}>
          Refresh
        </button>
      </div>

      <Card padded={false}>
        {list.initial && !list.data && <Spinner label="Loading notifications…" />}
        {list.error && (
          <div className="card-pad">
            <ErrorNote>{list.error}</ErrorNote>
          </div>
        )}
        {list.data?.items.length === 0 && (
          <Empty
            title={unreadOnly ? 'Nothing unread' : 'No notifications'}
            hint="Ticket changes, comments and SLA escalations for your account appear here."
          />
        )}
        {list.data && list.data.items.length > 0 && (
          <>
            <ul style={{ listStyle: 'none', margin: 0, padding: 0 }}>
              {list.data.items.map((item) => (
                <NotificationRow
                  key={item.id}
                  item={item}
                  onRead={() => markRead(item.id)}
                />
              ))}
            </ul>
            <Pager
              page={page}
              pages={list.data.pages}
              total={list.data.total}
              limit={list.data.limit}
              onChange={setPage}
            />
          </>
        )}
      </Card>
    </>
  )
}

function NotificationRow({ item, onRead }: { item: Notification; onRead: () => void }) {
  const read = Boolean(item.read_at)
  const body = (
    <div
      className="row"
      style={{
        padding: '11px 16px',
        borderBottom: '1px solid var(--border)',
        background: read ? undefined : 'var(--accent-soft)',
        gap: 12,
      }}
    >
      <span
        style={{
          width: 8,
          height: 8,
          borderRadius: '50%',
          flexShrink: 0,
          background: read ? 'var(--border-strong)' : levelColor(item.level),
        }}
        aria-hidden="true"
      />
      <div className="grow">
        <div style={{ fontWeight: read ? 500 : 600 }}>{item.title}</div>
        {item.body && (
          <div className="muted" style={{ fontSize: '0.8125rem' }}>
            {item.body}
          </div>
        )}
        <div className="faint" style={{ fontSize: '0.75rem', marginTop: 2 }}>
          <span className="badge">{item.kind}</span>{' '}
          <TimeStamp value={item.created_at} />
        </div>
      </div>
      {!read && (
        <button
          type="button"
          className="btn btn-sm btn-ghost"
          onClick={(event) => {
            event.preventDefault()
            event.stopPropagation()
            onRead()
          }}
        >
          Mark read
        </button>
      )}
    </div>
  )

  // A notification about a ticket links to it. One without a ticket_id has
  // nothing to link to, so it renders as plain text rather than a dead link.
  return (
    <li>
      {item.ticket_id ? (
        <Link
          to={`/tickets/${item.ticket_id}`}
          style={{ display: 'block', color: 'inherit', textDecoration: 'none' }}
          title={formatDateTime(item.created_at)}
          onClick={() => {
            if (!read) onRead()
          }}
        >
          {body}
        </Link>
      ) : (
        body
      )}
    </li>
  )
}

/** Colour by severity so the eye finds a breach without reading every row. */
function levelColor(level: string): string {
  switch (level) {
    case 'error':
      return 'var(--danger)'
    case 'warning':
      return 'var(--warn)'
    case 'success':
      return 'var(--ok)'
    default:
      return 'var(--accent)'
  }
}
