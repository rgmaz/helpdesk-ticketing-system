import { useEffect, useState, type FormEvent } from 'react'
import { api } from '../api/client'
import type { CreateTicketInput, Priority } from '../api/types'
import { useMeta } from '../state/MetaContext'
import { useToast } from '../state/ToastContext'
import { Modal } from './Modal'
import { ErrorNote, Field } from './ui'

/**
 * New ticket form.
 *
 * End users may only file at medium priority or below: the server enforces that
 * cap, so offering the higher options here would just produce a rejection. The
 * cap is applied on the client for the same reason it is enforced on the
 * server — hiding an action that will be refused is better than refusing it.
 *
 * Mounted only while open, so the form arrives blank instead of needing to be
 * cleared on every appearance.
 */
export function NewTicketDialog({
  onClose,
  onCreated,
  allowPriorityEscalation,
}: {
  onClose: () => void
  onCreated: (ticketId: number) => void
  /** True for staff, who may raise priority themselves. */
  allowPriorityEscalation?: boolean
}) {
  const { meta, priorityLabel } = useMeta()
  const toast = useToast()
  const [title, setTitle] = useState('')
  const [description, setDescription] = useState('')
  // Seeded from meta so the first category matches whatever the server considers
  // canonical, rather than hard-coding a name here that could drift.
  const [category, setCategory] = useState(() => meta?.categories[0] ?? 'general')
  const [priority, setPriority] = useState<Priority>('medium')
  const [assignee, setAssignee] = useState('')
  const [technicians, setTechnicians] = useState<{ id: number; full_name: string; active_tickets: number }[]>([])
  const [error, setError] = useState<string | null>(null)
  const [pending, setPending] = useState(false)

  const allowed: Priority[] = allowPriorityEscalation
    ? ['low', 'medium', 'high', 'critical']
    : ['low', 'medium']

  // The technician list is staff-only, so it is fetched only when it can
  // actually be used. Leaving the ticket unassigned is legal: the server routes
  // it to the least-loaded technician on creation.
  useEffect(() => {
    if (!allowPriorityEscalation) return
    let cancelled = false
    api
      .technicians()
      .then((page) => {
        if (cancelled) return
        setTechnicians(
          page.items
            .filter((t) => t.active)
            .map((t) => ({ id: t.id, full_name: t.full_name || t.username, active_tickets: t.active_tickets })),
        )
      })
      // A failed directory lookup must not block filing a ticket; the form falls
      // back to automatic routing, which is the common case anyway.
      .catch(() => {
        if (!cancelled) setTechnicians([])
      })
    return () => {
      cancelled = true
    }
  }, [allowPriorityEscalation])

  const submit = async (event: FormEvent) => {
    event.preventDefault()
    setPending(true)
    setError(null)

    const input: CreateTicketInput = {
      title: title.trim(),
      description: description.trim(),
      category,
      priority,
    }
    if (allowPriorityEscalation && assignee) input.assignee_id = Number(assignee)

    try {
      const { ticket } = await api.createTicket(input)
      toast.success(`${ticket.ticket_number} created.`)
      onCreated(ticket.id)
      onClose()
    } catch (err) {
      setError((err as Error).message)
    } finally {
      setPending(false)
    }
  }

  return (
    <Modal
      title="New ticket"
      onClose={onClose}
      footer={
        <>
          <span className="faint" style={{ fontSize: '0.75rem' }}>
            An SLA deadline is applied as soon as the ticket is created.
          </span>
          <div className="row">
            <button type="button" className="btn" onClick={onClose} disabled={pending}>
              Cancel
            </button>
            <button type="submit" form="new-ticket" className="btn btn-primary" disabled={pending}>
              {pending ? 'Creating…' : 'Create ticket'}
            </button>
          </div>
        </>
      }
    >
      <form id="new-ticket" onSubmit={submit}>
        {error && <ErrorNote>{error}</ErrorNote>}

        <Field
          label="Summary"
          htmlFor="nt-title"
          hint="One line that says what is wrong, as specifically as you can."
        >
          <input
            id="nt-title"
            value={title}
            onChange={(e) => setTitle(e.target.value)}
            maxLength={200}
            required
            autoFocus
          />
        </Field>

        <Field
          label="Description"
          htmlFor="nt-desc"
          hint="What you expected, what happened instead, and anything you already tried."
        >
          <textarea
            id="nt-desc"
            value={description}
            onChange={(e) => setDescription(e.target.value)}
            rows={6}
            maxLength={8000}
            required
          />
        </Field>

        <div className="row" style={{ gap: 12, alignItems: 'flex-start' }}>
          <div className="grow">
            <Field label="Category" htmlFor="nt-cat">
              <select id="nt-cat" value={category} onChange={(e) => setCategory(e.target.value)}>
                {meta?.categories.map((c) => (
                  <option key={c} value={c}>
                    {c}
                  </option>
                ))}
              </select>
            </Field>
          </div>
          <div className="grow">
            <Field
              label="Priority"
              htmlFor="nt-pri"
              hint={allowPriorityEscalation ? undefined : 'Requesters can raise this later.'}
            >
              <select
                id="nt-pri"
                value={priority}
                onChange={(e) => setPriority(e.target.value as Priority)}
              >
                {allowed.map((p) => (
                  <option key={p} value={p}>
                    {priorityLabel(p)}
                  </option>
                ))}
              </select>
            </Field>
          </div>
        </div>

        {allowPriorityEscalation && (
          <Field
            label="Assign to"
            htmlFor="nt-assignee"
            hint="Leave as auto to send it to the least-loaded technician."
          >
            <select id="nt-assignee" value={assignee} onChange={(e) => setAssignee(e.target.value)}>
              <option value="">Auto (least loaded)</option>
              {technicians.map((tech) => (
                <option key={tech.id} value={tech.id}>
                  {tech.full_name} — {tech.active_tickets} active
                </option>
              ))}
            </select>
          </Field>
        )}
      </form>
    </Modal>
  )
}
