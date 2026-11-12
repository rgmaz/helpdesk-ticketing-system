import { useState, type FormEvent } from 'react'
import { api } from '../api/client'
import type { DirectoryUser, Role } from '../api/types'
import { Modal } from '../components/Modal'
import { Card, Empty, ErrorNote, Field, Spinner } from '../components/ui'
import { useAsync, useDebounced } from '../hooks/useAsync'
import { ROLE_LABEL, formatDate, timeAgo } from '../lib/format'
import { useAuth } from '../state/AuthContext'
import { useToast } from '../state/ToastContext'

/**
 * The account directory.
 *
 * Provisioning an account is the first thing an administrator does and the last
 * thing a user can do for themselves beyond the requester role, so create and
 * edit share one form: two forms for the same fields is two places for the role
 * list to drift.
 *
 * Deactivation is offered instead of deletion for a person who has history.
 * Deleting a technician would orphan every ticket they touched; the API refuses
 * it, and hiding the button explains why.
 */
export function PeoplePage() {
  const { user: me } = useAuth()
  const toast = useToast()
  const [search, setSearch] = useState('')
  const [role, setRole] = useState<Role | ''>('')
  const [active, setActive] = useState<'' | 'true' | 'false'>('')
  const [editing, setEditing] = useState<DirectoryUser | 'new' | null>(null)

  const term = useDebounced(search, 300)

  const directory = useAsync(
    async () =>
      api.listUsers({
        q: term || undefined,
        role: role || undefined,
        active: active === '' ? undefined : active === 'true',
        limit: 100,
      }),
    [term, role, active],
  )

  const setUserActive = async (target: DirectoryUser, next: boolean) => {
    try {
      await api.updateUser(target.id, { active: next })
      toast.success(`${target.username} ${next ? 'reactivated' : 'deactivated'}.`)
      directory.reload()
    } catch (err) {
      toast.error((err as Error).message)
    }
  }

  return (
    <>
      <div className="page-head">
        <div>
          <h1>People</h1>
          <p>
            Technicians receive automatically routed tickets, so an account has to be a technician
            before it joins the routing pool.
          </p>
        </div>
        <button type="button" className="btn btn-primary" onClick={() => setEditing('new')}>
          Add person
        </button>
      </div>

      <div className="toolbar">
        <input
          className="grow"
          type="search"
          placeholder="Search by name, username or email…"
          value={search}
          onChange={(event) => setSearch(event.target.value)}
          aria-label="Search people"
        />
        <select
          value={role}
          onChange={(event) => setRole(event.target.value as Role | '')}
          aria-label="Role"
          style={{ width: 'auto' }}
        >
          <option value="">All roles</option>
          <option value="user">Requester</option>
          <option value="technician">Technician</option>
          <option value="admin">Administrator</option>
        </select>
        <select
          value={active}
          onChange={(event) => setActive(event.target.value as '' | 'true' | 'false')}
          aria-label="Active"
          style={{ width: 'auto' }}
        >
          <option value="">Any</option>
          <option value="true">Active</option>
          <option value="false">Deactivated</option>
        </select>
      </div>

      <Card padded={false}>
        {directory.initial && !directory.data && <Spinner label="Loading people…" />}
        {directory.error && (
          <div className="card-pad">
            <ErrorNote>{directory.error}</ErrorNote>
          </div>
        )}
        {directory.data && directory.data.items.length === 0 && (
          <Empty title="Nobody matches" hint="Try a different search or clear the filters." />
        )}
        {directory.data && directory.data.items.length > 0 && (
          <div className="table-wrap" style={{ border: 0, boxShadow: 'none', borderRadius: 0 }}>
            <table className="tbl">
              <thead>
                <tr>
                  <th>Name</th>
                  <th>Username</th>
                  <th>Email</th>
                  <th>Role</th>
                  <th>Load</th>
                  <th>Status</th>
                  <th>Created</th>
                  <th />
                </tr>
              </thead>
              <tbody>
                {directory.data.items.map((person) => (
                  <tr key={person.id}>
                    <td style={{ fontWeight: 500 }}>
                      {person.full_name || person.username}
                      {person.id === me?.id && <span className="badge" style={{ marginLeft: 6 }}>you</span>}
                    </td>
                    <td className="mono">{person.username}</td>
                    <td className="truncate" style={{ maxWidth: 200 }}>
                      {person.email}
                    </td>
                    <td className="nowrap">{ROLE_LABEL[person.role] ?? person.role}</td>
                    <td className="nowrap muted">
                      {person.active_tickets} active
                      {person.overdue_tickets > 0 && (
                        <span style={{ color: 'var(--danger)' }}> · {person.overdue_tickets} late</span>
                      )}
                    </td>
                    <td>
                      <span
                        className={`badge${person.active ? ' badge-sla-on_track' : ' badge-sla-stopped'}`}
                      >
                        {person.active ? 'Active' : 'Deactivated'}
                      </span>
                    </td>
                    <td className="nowrap muted" title={formatDate(person.created_at)}>
                      {timeAgo(person.created_at)}
                    </td>
                    <td className="nowrap" style={{ textAlign: 'right' }}>
                      <button
                        type="button"
                        className="btn btn-sm"
                        onClick={() => setEditing(person)}
                      >
                        Edit
                      </button>{' '}
                      {person.id !== me?.id && !person.is_last_admin && (
                        <button
                          type="button"
                          className="btn btn-sm"
                          onClick={() => setUserActive(person, !person.active)}
                          title={
                            person.active
                              ? 'Deactivating revokes access immediately'
                              : 'Restore this account'
                          }
                        >
                          {person.active ? 'Deactivate' : 'Reactivate'}
                        </button>
                      )}
                    </td>
                  </tr>
                ))}
              </tbody>
            </table>
          </div>
        )}
      </Card>

      {editing && (
        <UserDialog
          target={editing}
          onClose={() => setEditing(null)}
          onSaved={() => {
            setEditing(null)
            directory.reload()
          }}
        />
      )}
    </>
  )
}

/**
 * Create or edit, depending on the target.
 *
 * Mounted only while open and its fields are seeded by lazy initialisers, so
 * editing two people in a row cannot carry the first one's values into the
 * second. That was previously done with a "have I changed target yet?" state
 * write during render, which is the kind of cleverness that reads as a bug the
 * first time React changes when it re-renders.
 */
function UserDialog({
  target,
  onClose,
  onSaved,
}: {
  target: DirectoryUser | 'new'
  onClose: () => void
  onSaved: () => void
}) {
  const toast = useToast()
  const creating = target === 'new'
  const existing = target === 'new' ? null : target
  // The last active administrator cannot be demoted: the server refuses it to
  // avoid locking everyone out of the admin surfaces.
  const roleLocked = existing?.is_last_admin === true

  const [username, setUsername] = useState(() => existing?.username ?? '')
  const [fullName, setFullName] = useState(() => existing?.full_name ?? '')
  const [email, setEmail] = useState(() => existing?.email ?? '')
  const [role, setRole] = useState<Role>(() => existing?.role ?? 'user')
  const [password, setPassword] = useState('')
  const [error, setError] = useState<string | null>(null)
  const [pending, setPending] = useState(false)

  const submit = async (event: FormEvent) => {
    event.preventDefault()
    setPending(true)
    setError(null)
    try {
      // Branched on `existing` rather than on the `creating` flag: TypeScript
      // cannot correlate two separately derived booleans, so narrowing on the
      // value that is actually dereferenced is both clearer and checked.
      if (!existing) {
        await api.createUser({
          username: username.trim(),
          email: email.trim(),
          password,
          full_name: fullName.trim(),
          role,
        })
        toast.success(`${username.trim()} created.`)
      } else {
        await api.updateUser(existing.id, {
          email: email.trim(),
          full_name: fullName.trim(),
          role,
          // A blank password field means "unchanged", so fixing an email
          // address does not require retyping somebody's password.
          ...(password ? { password } : {}),
        })
        toast.success('Account updated.')
      }
      onSaved()
    } catch (err) {
      setError((err as Error).message)
    } finally {
      setPending(false)
    }
  }

  return (
    <Modal
      title={existing ? `Edit ${existing.username}` : 'Add person'}
      onClose={onClose}
      footer={
        <>
          <span className="faint" style={{ fontSize: '0.75rem' }}>
            {creating ? 'The person signs in with these credentials.' : 'Leave the password blank to keep it.'}
          </span>
          <div className="row">
            <button type="button" className="btn" onClick={onClose} disabled={pending}>
              Cancel
            </button>
            <button type="submit" form="user-form" className="btn btn-primary" disabled={pending}>
              {pending ? 'Saving…' : creating ? 'Create account' : 'Save changes'}
            </button>
          </div>
        </>
      }
    >
      <form id="user-form" onSubmit={submit}>
        {error && <ErrorNote>{error}</ErrorNote>}

        <div className="row" style={{ gap: 12, alignItems: 'flex-start' }}>
          <div className="grow">
            <Field label="Username" htmlFor="u-username">
              <input
                id="u-username"
                value={username}
                onChange={(e) => setUsername(e.target.value)}
                // Lowercase letters, digits, dot, underscore and dash only.
                // Changing it after creation would break the audit trail, which
                // records the username as it was at the time.
                disabled={!creating}
                pattern="[a-z0-9._-]{3,32}"
                title="3 to 32 characters: lowercase letters, digits, dot, underscore or dash"
                required
                autoCapitalize="none"
                spellCheck={false}
              />
            </Field>
          </div>
          <div className="grow">
            <Field label="Full name" htmlFor="u-fullname">
              <input
                id="u-fullname"
                value={fullName}
                onChange={(e) => setFullName(e.target.value)}
                maxLength={120}
              />
            </Field>
          </div>
        </div>

        <Field label="Email" htmlFor="u-email">
          <input
            id="u-email"
            type="email"
            value={email}
            onChange={(e) => setEmail(e.target.value)}
            required
          />
        </Field>

        <Field
          label="Role"
          htmlFor="u-role"
          hint={
            roleLocked
              ? 'The last active administrator must stay an administrator.'
              : role === 'technician'
                ? 'Technicians are in the automatic routing pool.'
                : role === 'admin'
                  ? 'Administrators manage people, policies and reporting, and are escalation targets rather than front-line handlers.'
                  : 'Requesters see and comment on their own tickets only.'
          }
        >
          <select
            id="u-role"
            value={role}
            onChange={(e) => setRole(e.target.value as Role)}
            disabled={roleLocked}
          >
            <option value="user">Requester</option>
            <option value="technician">Technician</option>
            <option value="admin">Administrator</option>
          </select>
        </Field>

        <Field
          label={creating ? 'Password' : 'New password'}
          htmlFor="u-password"
          hint="At least 10 characters, with a letter and a digit."
        >
          <input
            id="u-password"
            type="text"
            value={password}
            onChange={(e) => setPassword(e.target.value)}
            // Shown as text rather than masked: an administrator is setting
            // somebody else's password and has to be able to read it out to
            // them. This is only ever displayed on this form, never stored in
            // the browser.
            required={creating}
            minLength={creating ? 10 : undefined}
            autoComplete="new-password"
          />
        </Field>
      </form>
    </Modal>
  )
}
