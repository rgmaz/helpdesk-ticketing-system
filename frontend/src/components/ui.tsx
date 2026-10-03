import type { ReactNode } from 'react'
import type { Priority, SLAState, TicketStatus } from '../api/types'
import { SLA_STATE_LABEL, formatDateTime, humanize, timeAgo } from '../lib/format'
import { useToast } from '../state/ToastContext'

/**
 * Presentational primitives.
 *
 * These stay deliberately dumb: no data fetching, no API awareness. Anything
 * that needs to load belongs in a page or a hook, which keeps these reusable
 * across the three role views that render the same badges and stats.
 */

export function PriorityBadge({ value }: { value: Priority }) {
  return <span className={`badge badge-priority-${value}`}>{humanize(value)}</span>
}

export function StatusBadge({ value }: { value: TicketStatus }) {
  return <span className={`badge badge-status-${value}`}>{humanize(value)}</span>
}

/**
 * The SLA badge is the densest signal in the app, so it carries a title with
 * the underlying state and the deadline text rather than only a colour.
 */
export function SLABadge({
  state,
  detail,
}: {
  state: SLAState
  detail?: string
}) {
  return (
    <span className={`badge badge-sla-${state}`} title={detail ?? SLA_STATE_LABEL[state]}>
      {SLA_STATE_LABEL[state]}
    </span>
  )
}

export function Card({
  title,
  actions,
  children,
  padded = true,
}: {
  title?: ReactNode
  actions?: ReactNode
  children: ReactNode
  padded?: boolean
}) {
  return (
    <section className="card">
      {(title || actions) && (
        <header
          className="row spread card-pad"
          style={{ borderBottom: '1px solid var(--border)', padding: '11px 16px' }}
        >
          {typeof title === 'string' ? <h3>{title}</h3> : title}
          {actions && <div className="row">{actions}</div>}
        </header>
      )}
      <div className={padded ? 'card-pad' : undefined}>{children}</div>
    </section>
  )
}

export function Stat({
  label,
  value,
  sub,
  tone = 'neutral',
}: {
  label: string
  value: ReactNode
  sub?: ReactNode
  tone?: 'neutral' | 'good' | 'warn' | 'alert'
}) {
  return (
    <div className={`stat ${tone === 'neutral' ? '' : tone}`}>
      <div className="stat-label">{label}</div>
      <div className="stat-value">{value}</div>
      {sub !== undefined && <div className="stat-sub">{sub}</div>}
    </div>
  )
}

export function Empty({
  title,
  hint,
  action,
}: {
  title: string
  hint?: ReactNode
  action?: ReactNode
}) {
  return (
    <div className="empty">
      <h3>{title}</h3>
      {hint && <p className="muted">{hint}</p>}
      {action && <div style={{ marginTop: 14 }}>{action}</div>}
    </div>
  )
}

export function ErrorNote({ children }: { children: ReactNode }) {
  return (
    <div className="alert" role="alert">
      <div>{children}</div>
    </div>
  )
}

export function Spinner({ label }: { label?: string }) {
  return (
    <div className="loading-row">
      <span className="spinner" />
      {label && <span>{label}</span>}
    </div>
  )
}

/** Definition list for the ticket detail sidebar. */
export function MetaRow({ label, children }: { label: string; children: ReactNode }) {
  return (
    <div className="meta-row">
      <dt>{label}</dt>
      <dd>{children}</dd>
    </div>
  )
}

export function MetaList({ children }: { children: ReactNode }) {
  return <dl className="meta-list">{children}</dl>
}

/**
 * Pagination.
 *
 * The API's page index is zero-based while the buttons are one-based, so the
 * conversion is confined here rather than leaking into every caller's state.
 */
export function Pager({
  page,
  pages,
  total,
  limit,
  onChange,
}: {
  page: number
  pages: number
  total: number
  limit: number
  onChange: (page: number) => void
}) {
  if (total === 0) return null
  const first = (page - 1) * limit + 1
  const last = Math.min(page * limit, total)

  return (
    <div className="pager">
      <span>
        {first}–{last} of {total}
      </span>
      <div className="row">
        <button
          type="button"
          className="btn btn-sm"
          disabled={page <= 1}
          onClick={() => onChange(page - 1)}
        >
          Previous
        </button>
        <span className="mono">
          {page} / {Math.max(pages, 1)}
        </span>
        <button
          type="button"
          className="btn btn-sm"
          disabled={page >= pages}
          onClick={() => onChange(page + 1)}
        >
          Next
        </button>
      </div>
    </div>
  )
}

/** A labelled form row, so spacing stays consistent across every form. */
export function Field({
  label,
  hint,
  htmlFor,
  children,
}: {
  label: string
  hint?: ReactNode
  htmlFor?: string
  children: ReactNode
}) {
  return (
    <div className="field">
      <label htmlFor={htmlFor}>{label}</label>
      {children}
      {hint && <div className="field-hint">{hint}</div>}
    </div>
  )
}

/** Toast host. Mounted once by the layout. */
export function Toaster() {
  const { toasts, dismiss } = useToast()
  if (!toasts.length) return null

  return (
    <div className="toasts" role="status" aria-live="polite">
      {toasts.map((toast) => (
        <div key={toast.id} className={`toast ${toast.tone}`}>
          <div className="grow">{toast.message}</div>
          <button
            type="button"
            className="btn-ghost btn btn-sm"
            onClick={() => dismiss(toast.id)}
            aria-label="Dismiss"
          >
            ×
          </button>
        </div>
      ))}
    </div>
  )
}

/**
 * Relative time with the absolute value in a tooltip.
 *
 * A relative label alone is ambiguous in a table of mixed ages; the tooltip
 * removes the ambiguity without spending a second column on the exact time.
 */
export function TimeStamp({ value }: { value: string | null | undefined }) {
  if (!value) return <span className="faint">—</span>
  return (
    <time dateTime={value} title={formatDateTime(value)}>
      {timeAgo(value)}
    </time>
  )
}
