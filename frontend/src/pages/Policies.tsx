import { useState, type FormEvent } from 'react'
import { api } from '../api/client'
import type { SLAPolicy } from '../api/types'
import { Modal } from '../components/Modal'
import { Card, Empty, ErrorNote, Spinner } from '../components/ui'
import { useAsync } from '../hooks/useAsync'
import { hours } from '../lib/format'
import { useToast } from '../state/ToastContext'

/**
 * SLA policy administration.
 *
 * These four numbers are the contract the service desk is judged against, so
 * each edit states what it changes in plain units. A form that accepts "240"
 * without saying whether that is minutes or seconds is how a 4-hour target
 * becomes a 4-second one.
 *
 * Resolution minutes are in minutes because that is the unit the seed data, the
 * database column and the API all use. Converting at the boundary keeps the
 * arithmetic unambiguous.
 */
export function PoliciesPage() {
  const toast = useToast()
  const policies = useAsync(async () => (await api.policies()).items, [])
  const [editing, setEditing] = useState<SLAPolicy | null>(null)

  const toggleActive = async (policy: SLAPolicy) => {
    try {
      await api.updatePolicy(policy.id, { active: !policy.active })
      toast.success(`${policy.name} ${policy.active ? 'deactivated' : 'activated'}.`)
      policies.reload()
    } catch (err) {
      toast.error((err as Error).message)
    }
  }

  return (
    <>
      <div className="page-head">
        <div>
          <h1>SLA policies</h1>
          <p>
            One policy per priority. A ticket inherits the policy for its priority when it is
            created, and keeps it for life, so tightening a policy never rewrites history.
          </p>
        </div>
      </div>

      {policies.initial && !policies.data && <Spinner label="Loading policies…" />}
      {policies.error && <ErrorNote>{policies.error}</ErrorNote>}

      {policies.data && policies.data.length === 0 && (
        <Card>
          <Empty
            title="No policies"
            hint="Without a policy a ticket has no deadline. Seeded policies appear on a fresh database."
          />
        </Card>
      )}

      {policies.data && policies.data.length > 0 && (
        <Card padded={false}>
          <div className="table-wrap" style={{ border: 0, boxShadow: 'none', borderRadius: 0 }}>
            <table className="tbl">
              <thead>
                <tr>
                  <th>Policy</th>
                  <th>Priority</th>
                  <th>First response</th>
                  <th>Resolution</th>
                  <th>Escalates after</th>
                  <th>Status</th>
                  <th />
                </tr>
              </thead>
              <tbody>
                {policies.data.map((policy) => (
                  <tr key={policy.id}>
                    <td>
                      <div style={{ fontWeight: 500 }}>{policy.name}</div>
                      {policy.description && (
                        <div className="faint" style={{ fontSize: '0.75rem' }}>
                          {policy.description}
                        </div>
                      )}
                    </td>
                    <td>
                      <span className={`badge badge-priority-${policy.priority}`}>{policy.priority}</span>
                    </td>
                    <td className="nowrap">{hours(policy.response_minutes / 60)}</td>
                    <td className="nowrap">{hours(policy.resolution_minutes / 60)}</td>
                    <td className="nowrap">
                      {policy.escalation_minutes > 0 ? (
                        hours(policy.escalation_minutes / 60)
                      ) : (
                        <span className="faint">No escalation</span>
                      )}
                    </td>
                    <td>
                      <span className={`badge${policy.active ? ' badge-sla-on_track' : ' badge-sla-stopped'}`}>
                        {policy.active ? 'Active' : 'Inactive'}
                      </span>
                    </td>
                    <td className="nowrap" style={{ textAlign: 'right' }}>
                      <button type="button" className="btn btn-sm" onClick={() => setEditing(policy)}>
                        Edit
                      </button>{' '}
                      <button type="button" className="btn btn-sm" onClick={() => toggleActive(policy)}>
                        {policy.active ? 'Deactivate' : 'Activate'}
                      </button>
                    </td>
                  </tr>
                ))}
              </tbody>
            </table>
          </div>
        </Card>
      )}

      {editing && (
        <PolicyDialog
          policy={editing}
          onClose={() => setEditing(null)}
          onSaved={() => {
            setEditing(null)
            policies.reload()
          }}
        />
      )}
    </>
  )
}

/**
 * Policy editor.
 *
 * Values are entered in hours and converted to minutes on submit, because
 * "0.25" reads as fifteen minutes to nobody and "15" reads as fifteen hours to
 * somebody. The conversion is one place so the two units cannot disagree.
 */
function PolicyDialog({
  policy,
  onClose,
  onSaved,
}: {
  policy: SLAPolicy
  onClose: () => void
  onSaved: () => void
}) {
  const toast = useToast()
  const [name, setName] = useState(policy.name)
  const [description, setDescription] = useState(policy.description)
  const [responseHours, setResponseHours] = useState(String(policy.response_minutes / 60))
  const [resolutionHours, setResolutionHours] = useState(String(policy.resolution_minutes / 60))
  const [escalationHours, setEscalationHours] = useState(String(policy.escalation_minutes / 60))
  const [error, setError] = useState<string | null>(null)
  const [pending, setPending] = useState(false)

  const submit = async (event: FormEvent) => {
    event.preventDefault()
    setPending(true)
    setError(null)

    // Reject an escalation window longer than the resolution window: escalation
    // fires relative to the breach, so a longer interval means the second step
    // can never arrive.
    const response = hoursToMinutes(responseHours)
    const resolution = hoursToMinutes(resolutionHours)
    const escalation = hoursToMinutes(escalationHours)

    if (response === null || resolution === null || escalation === null) {
      setError('Every value must be a number of hours, such as 4 or 0.25.')
      setPending(false)
      return
    }
    if (response > resolution) {
      setError('The first-response target cannot be later than the resolution target.')
      setPending(false)
      return
    }
    if (escalation > resolution) {
      setError('The escalation interval cannot be longer than the resolution window: escalation happens after the deadline has already passed.')
      setPending(false)
      return
    }

    try {
      await api.updatePolicy(policy.id, {
        name: name.trim(),
        description: description.trim(),
        response_minutes: response,
        resolution_minutes: resolution,
        escalation_minutes: escalation,
      })
      toast.success('Policy updated.')
      onSaved()
    } catch (err) {
      setError((err as Error).message)
    } finally {
      setPending(false)
    }
  }

  return (
    <Modal
      title={`Edit ${policy.name}`}
      onClose={onClose}
      footer={
        <>
          <span className="faint" style={{ fontSize: '0.75rem' }}>
            Applies to tickets created from now on.
          </span>
          <div className="row">
            <button type="button" className="btn" onClick={onClose} disabled={pending}>
              Cancel
            </button>
            <button type="submit" form="policy-form" className="btn btn-primary" disabled={pending}>
              {pending ? 'Saving…' : 'Save policy'}
            </button>
          </div>
        </>
      }
    >
      <form id="policy-form" onSubmit={submit}>
        {error && <ErrorNote>{error}</ErrorNote>}

        <div className="field">
          <label htmlFor="p-name">Name</label>
          <input id="p-name" value={name} onChange={(e) => setName(e.target.value)} required maxLength={120} />
        </div>

        <div className="field">
          <label htmlFor="p-desc">Description</label>
          <input
            id="p-desc"
            value={description}
            onChange={(e) => setDescription(e.target.value)}
            maxLength={500}
          />
        </div>

        <div className="row" style={{ gap: 12, alignItems: 'flex-start' }}>
          <div className="grow">
            <div className="field">
              <label htmlFor="p-resp">First response (hours)</label>
              <input
                id="p-resp"
                type="number"
                step="0.25"
                min="0"
                value={responseHours}
                onChange={(e) => setResponseHours(e.target.value)}
                required
              />
              <div className="field-hint">{hours(Number(responseHours) || 0)} to first reply.</div>
            </div>
          </div>
          <div className="grow">
            <div className="field">
              <label htmlFor="p-res">Resolution (hours)</label>
              <input
                id="p-res"
                type="number"
                step="0.5"
                min="0"
                value={resolutionHours}
                onChange={(e) => setResolutionHours(e.target.value)}
                required
              />
              <div className="field-hint">{hours(Number(resolutionHours) || 0)} to a fix.</div>
            </div>
          </div>
          <div className="grow">
            <div className="field">
              <label htmlFor="p-esc">Escalate after (hours)</label>
              <input
                id="p-esc"
                type="number"
                step="0.5"
                min="0"
                value={escalationHours}
                onChange={(e) => setEscalationHours(e.target.value)}
                required
              />
              <div className="field-hint">
                {Number(escalationHours) > 0
                  ? 'Measured from the missed deadline.'
                  : 'Zero disables escalation.'}
              </div>
            </div>
          </div>
        </div>

        <p className="faint" style={{ fontSize: '0.75rem' }}>
          Priority is fixed to {policy.priority} because the policy is selected by it. To change
          which tickets this applies to, deactivate it and edit the policy for the other priority.
        </p>
      </form>
    </Modal>
  )
}

/**
 * Hours to whole minutes.
 *
 * Fractional hours are rounded to the nearest minute and a non-numeric or
 * negative value is rejected, so a typo like "4-8" cannot become a silently
 * plausible target.
 */
function hoursToMinutes(text: string): number | null {
  const value = Number(text)
  if (!Number.isFinite(value) || value < 0) return null
  return Math.round(value * 60)
}
