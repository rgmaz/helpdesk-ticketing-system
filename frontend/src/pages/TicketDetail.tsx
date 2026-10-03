import { useEffect, useRef, useState, type FormEvent } from 'react'
import { Link, useNavigate, useParams } from 'react-router-dom'
import { api } from '../api/client'
import type { TicketDetail, TicketStatus } from '../api/types'
import { Modal } from '../components/Modal'
import {
  Card,
  Empty,
  ErrorNote,
  MetaList,
  MetaRow,
  PriorityBadge,
  SLABadge,
  Spinner,
  StatusBadge,
  TimeStamp,
} from '../components/ui'
import { useAsync } from '../hooks/useAsync'
import { useLiveRefresh } from '../hooks/useLiveRefresh'
import { DETAIL_EVENTS } from '../hooks/useTicketList'
import { formatDateTime, formatDuration, humanize, timeAgo } from '../lib/format'
import { useAuth } from '../state/AuthContext'
import { useMeta } from '../state/MetaContext'
import { useToast } from '../state/ToastContext'

/**
 * One ticket, with its thread, its SLA clock and its immutable timeline.
 *
 * Status buttons are built from `available_transitions`, which the server
 * derives from the same transition table it enforces on write. That is the
 * point: the UI cannot offer an illegal move, and a rule change on the server
 * reaches this screen with no frontend change.
 */
export function TicketPage() {
  const { ticketId } = useParams()
  const navigate = useNavigate()
  const id = Number(ticketId)

  const { isStaff: staff, user } = useAuth()
  const { meta } = useMeta()
  const toast = useToast()

  const detail = useAsync<TicketDetail>(() => api.getTicket(id), [id])
  // Refresh on any ticket-scoped event, so a change made by somebody else in
  // another tab appears without a reload.
  useLiveRefresh(DETAIL_EVENTS, detail.reload)

  // The status being resolved, or null. The value is the target status so the
  // dialog knows whether it is resolving or closing, rather than a bare flag it
  // would have to guess the intent of.
  const [resolving, setResolving] = useState<TicketStatus | null>(null)
  const [assigning, setAssigning] = useState(false)
  const [deleting, setDeleting] = useState(false)
  const [busy, setBusy] = useState<string | null>(null)

  if (!Number.isFinite(id)) {
    return (
      <Card>
        <Empty title="That is not a ticket" hint="The link is malformed." />
      </Card>
    )
  }
  if (detail.initial) return <Spinner label="Loading ticket…" />
  if (detail.error) {
    return (
      <Card>
        <Empty
          title="Ticket unavailable"
          hint={detail.error}
          action={
            <Link className="btn" to={staff ? '/queue' : '/portal'}>
              Back to the list
            </Link>
          }
        />
      </Card>
    )
  }

  const { ticket, events, sla, available_transitions, can_delete } = detail.data!
  const isMine = ticket.requester_id === user?.id

  /**
   * Status changes go through this one path so every move gets the same
   * pending state, the same error handling and the same refresh. A resolved or
   * closed move additionally requires resolution notes, which is collected in
   * the dialog rather than by blocking here.
   */
  const changeStatus = async (next: TicketStatus, notes?: string) => {
    setBusy(next)
    try {
      const payload: { status: TicketStatus; resolution_notes?: string } = { status: next }
      if (notes !== undefined) payload.resolution_notes = notes
      await api.updateTicket(id, payload)
      toast.success(`Moved to ${humanize(next)}.`)
      detail.reload()
    } catch (err) {
      toast.error((err as Error).message)
    } finally {
      setBusy(null)
      setResolving(null)
    }
  }

  const onTransitionClick = (next: TicketStatus) => {
    // Resolution notes are mandatory when the work stops: without them the
    // requester cannot tell what was done, and the MTTR figure still counts the
    // ticket as resolved.
    if (next === 'resolved' || next === 'closed') {
      setResolving(next)
      return
    }
    void changeStatus(next)
  }

  return (
    <>
      <div className="page-head">
        <div className="grow">
          <div className="row" style={{ marginBottom: 4 }}>
            <Link to={staff && !isMine ? '/queue' : '/portal'} className="muted">
              ← Back
            </Link>
            <span className="mono muted">{ticket.ticket_number}</span>
            <StatusBadge value={ticket.status} />
            <PriorityBadge value={ticket.priority} />
            <SLABadge
              state={ticket.sla_state}
              detail={
                ticket.sla_resolution_due_at
                  ? `Resolution due ${formatDateTime(ticket.sla_resolution_due_at)}`
                  : 'No resolution deadline'
              }
            />
            {ticket.escalation_level > 0 && (
              <span className="badge badge-sla-breached">Escalated L{ticket.escalation_level}</span>
            )}
          </div>
          <h1>{ticket.title}</h1>
          <p className="muted">
            Raised by {ticket.requester_name} {timeAgo(ticket.created_at)} ·{' '}
            {humanize(ticket.category)}
          </p>
        </div>

        <div className="row row-wrap">
          {available_transitions.map((next) => (
            <button
              key={next}
              type="button"
              className={`btn${next === 'closed' ? '' : next === 'resolved' ? ' btn-primary' : ''}`}
              disabled={busy !== null}
              onClick={() => onTransitionClick(next)}
            >
              {busy === next ? 'Working…' : `Mark ${humanize(next).toLowerCase()}`}
            </button>
          ))}
          {staff && (
            <>
              <button type="button" className="btn" onClick={() => setAssigning(true)} disabled={busy !== null}>
                {ticket.assignee_id ? 'Reassign' : 'Assign'}
              </button>
              {can_delete && (
                <button type="button" className="btn btn-ghost" onClick={() => setDeleting(true)}>
                  Delete
                </button>
              )}
            </>
          )}
        </div>
      </div>

      <div className="detail">
        <div className="stack">
          <Card title="Description">
            <p style={{ whiteSpace: 'pre-wrap', overflowWrap: 'anywhere' }}>{ticket.description}</p>
            {ticket.resolution_notes && (
              <>
                <h3 style={{ marginTop: 18, marginBottom: 6 }}>Resolution</h3>
                <p
                  style={{
                    whiteSpace: 'pre-wrap',
                    overflowWrap: 'anywhere',
                    padding: 12,
                    background: 'var(--ok-soft)',
                    borderRadius: 'var(--radius)',
                  }}
                >
                  {ticket.resolution_notes}
                </p>
              </>
            )}
          </Card>

          <CommentThread ticketId={id} detail={detail.data!} onPosted={detail.reload} />

          <Card title="History" padded={false}>
            <div className="card-pad">
              {events.length === 0 ? (
                <p className="muted">Nothing recorded yet.</p>
              ) : (
                <div className="timeline">
                  {events.map((event) => (
                    <TimelineEntry key={event.id} event={event} />
                  ))}
                </div>
              )}
            </div>
          </Card>
        </div>

        <div className="stack">
          <Card title="SLA">
            <MetaList>
              <MetaRow label="Policy">
                {ticket.sla_policy_name ?? <span className="faint">None attached</span>}
              </MetaRow>
              <MetaRow label="State">
                <SLABadge state={sla.state} />
              </MetaRow>
              <MetaRow label="First response">
                <Deadline
                  due={ticket.sla_response_due_at}
                  remaining={ticket.sla_response_remaining_seconds}
                  met={ticket.first_response_at}
                  breached={sla.response_breached}
                />
              </MetaRow>
              <MetaRow label="Resolution">
                <Deadline
                  due={ticket.sla_resolution_due_at}
                  remaining={ticket.sla_resolution_remaining_seconds}
                  met={ticket.resolved_at}
                  breached={sla.resolution_breached}
                />
              </MetaRow>
              <MetaRow label="Escalation level">{sla.escalation_level}</MetaRow>
            </MetaList>
          </Card>

          <Card title="Details">
            <MetaList>
              <MetaRow label="Requester">{ticket.requester_name}</MetaRow>
              <MetaRow label="Assignee">
                {ticket.assignee_name ?? <span className="faint">Unassigned</span>}
              </MetaRow>
              <MetaRow label="Category">{humanize(ticket.category)}</MetaRow>
              <MetaRow label="Created">
                <TimeStamp value={ticket.created_at} />
              </MetaRow>
              <MetaRow label="Updated">
                <TimeStamp value={ticket.updated_at} />
              </MetaRow>
              {ticket.resolved_at && (
                <MetaRow label="Resolved">
                  <TimeStamp value={ticket.resolved_at} />
                </MetaRow>
              )}
              {ticket.closed_at && (
                <MetaRow label="Closed">
                  <TimeStamp value={ticket.closed_at} />
                </MetaRow>
              )}
              {staff && detail.data!.internal_note_count !== undefined && (
                <MetaRow label="Internal notes">
                  <span className="muted">{detail.data!.internal_note_count} hidden from the requester</span>
                </MetaRow>
              )}
            </MetaList>
          </Card>

          {meta?.assignment_policy && staff && (
            <Card title="Routing">
              <p className="muted" style={{ fontSize: '0.8125rem' }}>
                {meta.assignment_policy}
              </p>
            </Card>
          )}
        </div>
      </div>

      {resolving && (
        <ResolveDialog
          target={resolving}
          onClose={() => setResolving(null)}
          onSubmit={changeStatus}
        />
      )}

      {assigning && (
        <AssignDialog
          ticketId={id}
          current={ticket.assignee_id}
          onClose={() => setAssigning(false)}
          onAssigned={() => {
            setAssigning(false)
            detail.reload()
          }}
        />
      )}

      {deleting && (
        <DeleteDialog
          ticketId={id}
          ticketNumber={ticket.ticket_number}
          onClose={() => setDeleting(false)}
          onDeleted={() => {
            toast.success(`${ticket.ticket_number} deleted.`)
            navigate(staff ? '/queue' : '/portal', { replace: true })
          }}
        />
      )}
    </>
  )
}

/** A single SLA clock line: the deadline, and how much of it is left or gone. */
function Deadline({
  due,
  remaining,
  met,
  breached,
}: {
  due: string | null
  remaining?: number
  met: string | null
  breached: boolean
}) {
  if (!due) {
    return <span className="faint">{met ? 'Not applicable' : 'No deadline'}</span>
  }
  if (met) {
    return (
      <span>
        <TimeStamp value={met} />
        {breached && <span className="badge badge-sla-breached" style={{ marginLeft: 6 }}>late</span>}
      </span>
    )
  }
  const overdue = remaining !== undefined && remaining <= 0
  return (
    <span>
      <span
        style={{
          color: overdue ? 'var(--danger)' : remaining !== undefined && remaining < 3600 ? 'var(--warn)' : undefined,
          fontWeight: 600,
        }}
        title={formatDateTime(due)}
      >
        {overdue ? `${formatDuration(remaining)} overdue` : `${formatDuration(remaining)} left`}
      </span>
    </span>
  )
}

function TimelineEntry({ event }: { event: TicketDetail['events'][number] }) {
  // The dot colour follows consequence, not category: an escalation is the only
  // thing on this list anyone needs to notice at a glance.
  const tone =
    event.type === 'sla_escalation'
      ? 'alert'
      : event.type.endsWith('_breach') || event.type === 'ticket_reopened'
        ? 'warn'
        : ''

  return (
    <div className={`timeline-item ${tone}`}>
      <span className="timeline-dot" />
      <div>
        <div>{event.message}</div>
        <div className="faint" style={{ fontSize: '0.75rem' }}>
          {event.actor_name || 'System'} · <TimeStamp value={event.created_at} />
        </div>
      </div>
    </div>
  )
}

/**
 * The comment thread.
 *
 * Internal notes are offered only to staff and are the server's job to hide
 * from a requester, so this component never has to filter the list it is given.
 * A new comment arriving over the stream appends in place rather than replacing
 * the thread, which keeps scroll position and any half-typed reply.
 */
function CommentThread({
  ticketId,
  detail,
  onPosted,
}: {
  ticketId: number
  detail: TicketDetail
  onPosted: () => void
}) {
  const { user, isStaff: staff } = useAuth()
  const [body, setBody] = useState('')
  const [internal, setInternal] = useState(false)
  const [posting, setPosting] = useState(false)
  const endRef = useRef<HTMLDivElement>(null)

  // A live update should scroll into view so the reply is not missed, but only
  // when the reader is already at the bottom: yanking the page out from someone
  // reading history is worse than requiring a small scroll.
  const [pinned, setPinned] = useState(true)
  useEffect(() => {
    if (pinned) endRef.current?.scrollIntoView({ block: 'nearest' })
  }, [detail.comments.length, pinned])

  const submit = async (event: FormEvent) => {
    event.preventDefault()
    const text = body.trim()
    if (!text) return

    setPosting(true)
    try {
      await api.addComment(ticketId, text, staff && internal)
      setBody('')
      setInternal(false)
      onPosted()
      endRef.current?.scrollIntoView({ behavior: 'smooth', block: 'nearest' })
    } catch (err) {
      // The textarea keeps its content, so a failed post is recoverable by
      // pressing the button again.
      window.alert((err as Error).message)
    } finally {
      setPosting(false)
    }
  }

  return (
    <Card title={`Conversation (${detail.comments.length})`}>
      <div
        className="thread"
        onScroll={(event) => {
          const el = event.currentTarget
          setPinned(el.scrollHeight - el.scrollTop - el.clientHeight < 40)
        }}
        style={{ maxHeight: 520, overflowY: 'auto', paddingRight: 4 }}
      >
        {detail.comments.length === 0 ? (
          <p className="muted">
            No comments yet.{' '}
            {user?.id === detail.ticket.requester_id
              ? 'Add anything that would help someone reproduce the problem.'
              : 'Reply here to update the requester.'}
          </p>
        ) : (
          detail.comments.map((comment) => (
            <article key={comment.id} className={`comment${comment.internal ? ' internal' : ''}`}>
              <div className="comment-head">
                <strong>{comment.author_name}</strong>
                <span className="badge">{humanize(comment.author_role)}</span>
                {comment.internal && (
                  <span className="badge badge-priority-high">Internal note</span>
                )}
                <span className="grow" />
                <span title={formatDateTime(comment.created_at)}>
                  <TimeStamp value={comment.created_at} />
                </span>
              </div>
              <div className="comment-body">{comment.body}</div>
            </article>
          ))
        )}
        <div ref={endRef} />
      </div>

      <form onSubmit={submit} style={{ marginTop: 14 }}>
        <textarea
          value={body}
          onChange={(event) => setBody(event.target.value)}
          placeholder={staff ? 'Reply to the requester…' : 'Add a comment…'}
          rows={3}
          maxLength={20000}
          aria-label="Comment"
        />
        <div className="row spread" style={{ marginTop: 8 }}>
          {staff ? (
            <label className="row" style={{ marginBottom: 0, fontWeight: 500 }}>
              <input
                type="checkbox"
                checked={internal}
                onChange={(event) => setInternal(event.target.checked)}
                style={{ width: 'auto' }}
              />
              Internal note (hidden from the requester)
            </label>
          ) : (
            <span className="faint" style={{ fontSize: '0.75rem' }}>
              Comments are visible to everyone working on this ticket.
            </span>
          )}
          <button
            type="submit"
            className="btn btn-primary"
            disabled={posting || body.trim().length === 0}
          >
            {posting ? 'Posting…' : internal ? 'Add note' : 'Post comment'}
          </button>
        </div>
      </form>
    </Card>
  )
}

/**
 * Resolution notes are required when the work stops.
 *
 * The target status is passed in rather than the dialog being told to resolve
 * generally, so "close" and "resolve" share one form and label themselves
 * correctly. The parent mounts this only while a target is chosen, so the notes
 * are always blank on open.
 */
function ResolveDialog({
  target,
  onClose,
  onSubmit,
}: {
  target: TicketStatus
  onClose: () => void
  onSubmit: (status: TicketStatus, notes: string) => void
}) {
  const [notes, setNotes] = useState('')
  const closing = target === 'closed'

  return (
    <Modal
      title={closing ? 'Close ticket' : 'Resolve ticket'}
      onClose={onClose}
      footer={
        <>
          <span className="faint" style={{ fontSize: '0.75rem' }}>
            The requester sees this text.
          </span>
          <div className="row">
            <button type="button" className="btn" onClick={onClose}>
              Cancel
            </button>
            <button
              type="submit"
              form="resolve-ticket"
              className="btn btn-primary"
              disabled={notes.trim().length === 0}
            >
              {closing ? 'Close ticket' : 'Mark resolved'}
            </button>
          </div>
        </>
      }
    >
      <form
        id="resolve-ticket"
        onSubmit={(event) => {
          event.preventDefault()
          onSubmit(target, notes.trim())
        }}
      >
        <div className="field">
          <label htmlFor="resolution">What was done?</label>
          <textarea
            id="resolution"
            value={notes}
            onChange={(event) => setNotes(event.target.value)}
            rows={5}
            maxLength={8000}
            required
            autoFocus
            placeholder="Describe the fix, and anything the requester needs to do."
          />
          <div className="field-hint">
            This becomes the resolution note and is searchable. It is required: a closed ticket
            without one tells the requester nothing.
          </div>
        </div>
      </form>
    </Modal>
  )
}

/**
 * Manual assignment, which overrides the automatic routing.
 *
 * Mounted only while open, so the directory is fetched once per appearance and
 * the selection starts unset — which is deliberate, because a pre-selected
 * technician would be reassigned to on a stray Enter.
 */
function AssignDialog({
  ticketId,
  current,
  onClose,
  onAssigned,
}: {
  ticketId: number
  current: number | null
  onClose: () => void
  onAssigned: () => void
}) {
  const toast = useToast()
  const [selected, setSelected] = useState('')
  const [technicians, setTechnicians] = useState<{ id: number; name: string; load: number }[]>([])
  const [pending, setPending] = useState(false)

  useEffect(() => {
    let cancelled = false
    api
      .technicians()
      .then((page) => {
        if (cancelled) return
        setTechnicians(
          page.items
            .filter((t) => t.active)
            .map((t) => ({ id: t.id, name: t.full_name || t.username, load: t.active_tickets })),
        )
      })
      .catch(() => {
        if (!cancelled) setTechnicians([])
      })
    return () => {
      cancelled = true
    }
  }, [])

  const assign = async () => {
    setPending(true)
    try {
      await api.updateTicket(ticketId, { assignee_id: Number(selected) })
      toast.success('Assignment updated.')
      onAssigned()
    } catch (err) {
      toast.error((err as Error).message)
    } finally {
      setPending(false)
    }
  }

  return (
    <Modal
      title="Assign ticket"
      onClose={onClose}
      footer={
        <div className="row">
          <button type="button" className="btn" onClick={onClose}>
            Cancel
          </button>
          <button
            type="button"
            className="btn btn-primary"
            disabled={pending || selected === '' || Number(selected) === current}
            onClick={assign}
          >
            {pending ? 'Assigning…' : 'Assign'}
          </button>
        </div>
      }
    >
      <div className="field">
        <label htmlFor="assign-to">Technician</label>
        <select id="assign-to" value={selected} onChange={(event) => setSelected(event.target.value)}>
          <option value="">Choose…</option>
          {technicians.map((tech) => (
            <option key={tech.id} value={tech.id}>
              {tech.name} — {tech.load} active
            </option>
          ))}
        </select>
        {technicians.length === 0 && (
          <div className="field-hint">
            No active technicians are available. Create one under People, or activate an existing
            account.
          </div>
        )}
      </div>
      {current !== null && (
        <p className="faint" style={{ fontSize: '0.75rem' }}>
          Currently assigned. Choosing somebody else transfers ownership and notifies them.
        </p>
      )}
    </Modal>
  )
}

/**
 * Deletion is irreversible, so it is confirmed by typing the ticket number.
 *
 * Typing beats a yes/no confirmation for something this permanent: a
 * mis-click on a generic "Are you sure?" costs a ticket and its whole comment
 * history, and there is no undo. Requiring the number means the intent has to
 * be deliberate.
 */
function DeleteDialog({
  ticketId,
  ticketNumber,
  onClose,
  onDeleted,
}: {
  ticketId: number
  ticketNumber: string
  onClose: () => void
  onDeleted: () => void
}) {
  const toast = useToast()
  const [typed, setTyped] = useState('')
  const [pending, setPending] = useState(false)

  const remove = async () => {
    setPending(true)
    try {
      await api.deleteTicket(ticketId)
      onDeleted()
    } catch (err) {
      toast.error((err as Error).message)
    } finally {
      setPending(false)
    }
  }

  return (
    <Modal
      title="Delete ticket"
      onClose={onClose}
      footer={
        <div className="row">
          <button type="button" className="btn" onClick={onClose}>
            Cancel
          </button>
          <button
            type="button"
            className="btn btn-danger"
            disabled={typed !== ticketNumber || pending}
            onClick={remove}
          >
            {pending ? 'Deleting…' : 'Delete permanently'}
          </button>
        </div>
      }
    >
      <ErrorNote>
        This removes the ticket, its comments and its history. It cannot be undone. Type{' '}
        <strong>{ticketNumber}</strong> to confirm.
      </ErrorNote>
      <div className="field" style={{ marginTop: 14 }}>
        <label htmlFor="confirm-delete">Ticket number</label>
        <input
          id="confirm-delete"
          value={typed}
          onChange={(event) => setTyped(event.target.value.toUpperCase())}
          className="mono"
          autoFocus
        />
      </div>
    </Modal>
  )
}
