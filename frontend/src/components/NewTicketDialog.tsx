import { useEffect, useState, type FormEvent } from 'react'
import { api } from '../api/client'
import type { CreateTicketInput, Priority, TriageResult } from '../api/types'
import { useDebounced } from '../hooks/useAsync'
import { humanize } from '../lib/format'
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
  const [triage, setTriage] = useState<{ key: string; result: TriageResult } | null>(null)

  // Preflight is debounced so a fast typist produces one request, not one per
  // keystroke. It is also cancelled when the inputs change again, so a slow
  // response cannot overwrite a newer one. The result is tagged with the inputs
  // it answers, so the panel disappears while a new request is in flight rather
  // than showing advice about text the user has already edited.
  const debouncedTitle = useDebounced(title, 700)
  const debouncedDescription = useDebounced(description, 700)
  const enoughText =
    debouncedTitle.trim().length >= 8 || debouncedDescription.trim().length >= 20
  const triageKey = [debouncedTitle.trim(), debouncedDescription.trim(), category, priority].join('\u0000')

  useEffect(() => {
    if (!meta?.ai?.enabled || !enoughText) return

    const key = triageKey
    const controller = new AbortController()
    api
      .triage(
        {
          title: debouncedTitle.trim(),
          description: debouncedDescription.trim(),
          category,
          priority,
        },
        controller.signal,
      )
      .then((result) => {
        if (!controller.signal.aborted) setTriage({ key, result })
      })
      .catch((err: unknown) => {
        // Suggestions are a convenience: a failed or rate-limited preflight must
        // leave the form exactly as it was before AI existed.
        if ((err as Error).name === 'AbortError') return
        if (!controller.signal.aborted) setTriage(null)
      })
    return () => controller.abort()
    // triageKey is derived from the debounced text and the two selects; using it
    // keeps the dependency list honest without re-listing each part.
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [triageKey, meta?.ai?.enabled, enoughText])

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

  const current = triage?.key === triageKey ? triage.result : null
  const categorySuggestion = current?.suggestions.category
  const prioritySuggestion = current?.suggestions.priority
  const priorityCanApply = prioritySuggestion
    ? allowed.includes(prioritySuggestion.value as Priority)
    : false
  const hasSuggestions = Boolean(
    enoughText &&
      current &&
      !current.degraded &&
      ((categorySuggestion && categorySuggestion.value !== category) ||
        (prioritySuggestion && prioritySuggestion.value !== priority) ||
        current.duplicates.length > 0),
  )

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

        {hasSuggestions && current && (
          <div className="ai-panel" role="status" aria-live="polite">
            <div className="ai-panel-head">
              <span className="badge">AI suggestion</span>
              <span className="faint" style={{ fontSize: '0.72rem' }}>
                Advisory only. Nothing changes unless you choose it.
              </span>
            </div>

            {categorySuggestion && categorySuggestion.value !== category && (
              <div className="ai-suggestion">
                <div>
                  <span className="ai-label">Category</span>{' '}
                  <strong>{humanize(categorySuggestion.value)}</strong>{' '}
                  <span className="faint">{Math.round(categorySuggestion.confidence * 100)}%</span>
                </div>
                <p className="muted">{categorySuggestion.rationale}</p>
                <button
                  type="button"
                  className="btn btn-ghost btn-sm"
                  onClick={() => setCategory(categorySuggestion.value)}
                >
                  Use this category
                </button>
              </div>
            )}

            {prioritySuggestion && prioritySuggestion.value !== priority && (
              <div className="ai-suggestion">
                <div>
                  <span className="ai-label">Priority</span>{' '}
                  <strong>{priorityLabel(prioritySuggestion.value as Priority)}</strong>{' '}
                  <span className="faint">{Math.round(prioritySuggestion.confidence * 100)}%</span>
                </div>
                <p className="muted">{prioritySuggestion.rationale}</p>
                {priorityCanApply ? (
                  <button
                    type="button"
                    className="btn btn-ghost btn-sm"
                    onClick={() => setPriority(prioritySuggestion.value as Priority)}
                  >
                    Use this priority
                  </button>
                ) : (
                  <span className="faint" style={{ fontSize: '0.78rem' }}>
                    Staff can confirm a higher priority after submission.
                  </span>
                )}
              </div>
            )}

            {current.duplicates.length > 0 && (
              <div className="ai-suggestion">
                <div className="ai-label">Possible duplicates</div>
                <ul className="ai-duplicates">
                  {current.duplicates.map((duplicate) => (
                    <li key={duplicate.ticket_id}>
                      <span className="mono">{duplicate.ticket_number}</span> — {duplicate.title}
                      {duplicate.snippet && <div className="faint">{duplicate.snippet}</div>}
                    </li>
                  ))}
                </ul>
                <p className="faint" style={{ fontSize: '0.78rem' }}>
                  If one of these is the same issue, you can reference it in your description.
                </p>
              </div>
            )}
          </div>
        )}

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
