import type { Priority, SLAState, TicketStatus } from '../api/types'

/**
 * Presentation helpers.
 *
 * Formatting lives here rather than in components so a duration reads the same
 * in a table cell, a detail panel and a toast.
 */

/**
 * Renders a signed duration compactly: "4h 12m", "2d 3h", "-45m".
 *
 * The sign is preserved because an SLA countdown is most useful precisely when
 * it is negative: "-45m" means 45 minutes overdue, and dropping the sign would
 * make that indistinguishable from "45 minutes left".
 */
export function formatDuration(seconds: number | null | undefined): string {
  if (seconds === null || seconds === undefined || !Number.isFinite(seconds)) return '—'
  const sign = seconds < 0 ? '-' : ''
  const total = Math.abs(Math.round(seconds))

  const days = Math.floor(total / 86400)
  const hours = Math.floor((total % 86400) / 3600)
  const minutes = Math.floor((total % 3600) / 60)

  if (days > 0) return `${sign}${days}d ${hours}h`
  if (hours > 0) return `${sign}${hours}h ${minutes}m`
  if (minutes > 0) return `${sign}${minutes}m`
  return `${sign}${total}s`
}

/** Same idea from a Go duration string such as "1h8m0s" or "15m0s". */
export function formatDurationText(text: string | null | undefined): string {
  if (!text) return '—'
  const match = /^(-)?(?:(\d+)h)?(?:(\d+)m)?(?:(\d+(?:\.\d+)?)s)?$/.exec(text)
  if (!match) return text
  const [, sign, h, m, s] = match
  const hours = Number(h ?? 0)
  const minutes = Number(m ?? 0)
  const secs = Math.round(Number(s ?? 0))
  if (hours > 0) return `${sign ?? ''}${hours}h ${minutes}m`
  if (minutes > 0) return `${sign ?? ''}${minutes}m`
  return `${sign ?? ''}${secs}s`
}

/**
 * Relative time, e.g. "just now", "12m ago", "in 3h".
 *
 * This is what the ticket list uses: an absolute timestamp on every row is
 * harder to scan than a comparison against now.
 */
export function timeAgo(iso: string | null | undefined): string {
  if (!iso) return '—'
  const then = new Date(iso).getTime()
  if (Number.isNaN(then)) return '—'

  const deltaSeconds = Math.round((Date.now() - then) / 1000)
  const magnitude = Math.abs(deltaSeconds)

  if (magnitude < 45) return 'just now'

  // Past a week the relative form stops being useful, so show the date.
  if (magnitude >= 604800) {
    return new Date(then).toLocaleDateString(undefined, { month: 'short', day: 'numeric' })
  }

  const text =
    magnitude < 3600
      ? `${Math.round(magnitude / 60)}m`
      : magnitude < 86400
        ? `${Math.round(magnitude / 3600)}h`
        : `${Math.round(magnitude / 86400)}d`

  // The direction matters for a deadline: an SLA that is still counting reads
  // very differently from one that has already passed.
  return deltaSeconds < 0 ? `in ${text}` : `${text} ago`
}

/** Absolute local timestamp for tooltips and detail panels. */
export function formatDateTime(iso: string | null | undefined): string {
  if (!iso) return '—'
  const d = new Date(iso)
  if (Number.isNaN(d.getTime())) return '—'
  return d.toLocaleString(undefined, {
    year: 'numeric',
    month: 'short',
    day: 'numeric',
    hour: '2-digit',
    minute: '2-digit',
  })
}

export function formatDate(iso: string | null | undefined): string {
  if (!iso) return '—'
  const d = new Date(iso)
  if (Number.isNaN(d.getTime())) return '—'
  return d.toLocaleDateString(undefined, { year: 'numeric', month: 'short', day: 'numeric' })
}

/** Turns a stored slug into a readable label without waiting for /meta. */
export function humanize(value: string): string {
  if (!value) return ''
  const spaced = value.replace(/[_-]+/g, ' ')
  return spaced.charAt(0).toUpperCase() + spaced.slice(1)
}

const PRIORITY_WEIGHT: Record<Priority, number> = {
  critical: 4,
  high: 3,
  medium: 2,
  low: 1,
}

export function priorityWeight(p: Priority): number {
  return PRIORITY_WEIGHT[p] ?? 0
}

export function statusWeight(s: TicketStatus): number {
  const order: TicketStatus[] = [
    'reopened',
    'open',
    'in_progress',
    'pending',
    'resolved',
    'closed',
  ]
  return order.indexOf(s)
}

/** Minutes, as a compact duration. Used by the SLA policy editor. */
export function minutesToDuration(minutes: number): string {
  if (minutes <= 0) return 'none'
  return formatDuration(minutes * 60)
}

export function hours(value: number | null | undefined, digits = 1): string {
  if (value === null || value === undefined || !Number.isFinite(value)) return '—'
  if (value === 0) return '0h'
  if (Math.abs(value) < 1) return `${Math.round(value * 60)}m`
  return `${value.toFixed(digits)}h`
}

/** Copy for each SLA badge, so the meaning is consistent everywhere. */
export const SLA_STATE_LABEL: Record<SLAState, string> = {
  on_track: 'On track',
  at_risk: 'At risk',
  breached: 'Breached',
  met: 'Met',
  stopped: 'Stopped',
}

export const ROLE_LABEL: Record<string, string> = {
  user: 'Requester',
  technician: 'Technician',
  admin: 'Administrator',
}