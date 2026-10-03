import { useState, type FormEvent } from 'react'
import { api } from '../api/client'
import { useToast } from '../state/ToastContext'
import { Modal } from './Modal'
import { ErrorNote, Field } from './ui'

/**
 * Password rotation.
 *
 * The rules are stated up front because they are enforced server-side: a
 * rejected submit with no explanation is the most common complaint about
 * password forms, and the server's messages are about composition rather than
 * length.
 *
 * Mounted only while open, so the fields are empty and the error gone on every
 * appearance. That matters here more than elsewhere — reopening after a failed
 * attempt must not show the previous password still sitting in the field.
 */
export function ChangePasswordDialog({ onClose }: { onClose: () => void }) {
  const toast = useToast()
  const [current, setCurrent] = useState('')
  const [next, setNext] = useState('')
  const [confirm, setConfirm] = useState('')
  const [error, setError] = useState<string | null>(null)
  const [pending, setPending] = useState(false)

  const submit = async (event: FormEvent) => {
    event.preventDefault()
    // Checked here rather than trusting `pattern`, because the mismatch is
    // between two fields and no single input can express it.
    if (next !== confirm) {
      setError('The new passwords do not match.')
      return
    }
    setPending(true)
    setError(null)
    try {
      await api.changePassword(current, next)
      toast.success('Password updated.')
      onClose()
    } catch (err) {
      setError((err as Error).message)
    } finally {
      setPending(false)
    }
  }

  return (
    <Modal
      title="Change password"
      onClose={onClose}
      footer={
        <>
          <span className="faint" style={{ fontSize: '0.75rem' }}>
            At least 10 characters, with a letter and a digit.
          </span>
          <div className="row">
            <button type="button" className="btn" onClick={onClose} disabled={pending}>
              Cancel
            </button>
            <button
              type="submit"
              form="change-password"
              className="btn btn-primary"
              disabled={pending || !current || !next}
            >
              {pending ? 'Updating…' : 'Update password'}
            </button>
          </div>
        </>
      }
    >
      <form id="change-password" onSubmit={submit}>
        {error && <ErrorNote>{error}</ErrorNote>}
        <Field label="Current password" htmlFor="pw-current">
          <input
            id="pw-current"
            type="password"
            autoComplete="current-password"
            value={current}
            onChange={(e) => setCurrent(e.target.value)}
            required
          />
        </Field>
        <Field label="New password" htmlFor="pw-new">
          <input
            id="pw-new"
            type="password"
            autoComplete="new-password"
            value={next}
            onChange={(e) => setNext(e.target.value)}
            required
          />
        </Field>
        <Field label="Confirm new password" htmlFor="pw-confirm">
          <input
            id="pw-confirm"
            type="password"
            autoComplete="new-password"
            value={confirm}
            onChange={(e) => setConfirm(e.target.value)}
            required
          />
        </Field>
      </form>
    </Modal>
  )
}