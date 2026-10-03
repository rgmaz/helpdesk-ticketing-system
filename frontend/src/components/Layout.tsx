import { useState } from 'react'
import { NavLink, Outlet, useNavigate } from 'react-router-dom'
import { ROLE_LABEL } from '../lib/format'
import { useAuth } from '../state/AuthContext'
import { useNotifications } from '../state/NotificationsContext'
import { ChangePasswordDialog } from './ChangePasswordDialog'
import { Toaster } from './ui'

/**
 * Navigation is derived from the signed-in role rather than a static list with
 * items hidden. A technician has no admin links to click, so the queue is their
 * landing page; a requester's portal is theirs. An administrator gets all of it
 * because they genuinely do all three jobs.
 */
interface NavItem {
  to: string
  label: string
  roles: ('user' | 'technician' | 'admin')[]
}

const NAV: NavItem[] = [
  { to: '/portal', label: 'My tickets', roles: ['user', 'technician', 'admin'] },
  { to: '/queue', label: 'Queue', roles: ['technician', 'admin'] },
  { to: '/dashboard', label: 'Dashboard', roles: ['admin'] },
  { to: '/team', label: 'People', roles: ['admin'] },
  { to: '/policies', label: 'SLA policies', roles: ['admin'] },
]

export function Layout() {
  const { user, signOut } = useAuth()
  const { unread, status } = useNotifications()
  const navigate = useNavigate()
  const [changingPassword, setChangingPassword] = useState(false)

  if (!user) return null

  const visible = NAV.filter((item) => item.roles.includes(user.role))

  return (
    <div className="app">
      <header className="topbar">
        <NavLink to={visible[0]?.to ?? '/'} className="brand">
          <span className="brand-mark" aria-hidden="true">
            SD
          </span>
          Service Desk
        </NavLink>

        <nav className="nav" aria-label="Sections">
          {visible.map((item) => (
            <NavLink
              key={item.to}
              to={item.to}
              className={({ isActive }) => `nav-link${isActive ? ' active' : ''}`}
            >
              {item.label}
            </NavLink>
          ))}
        </nav>

        <div className="grow" />

        {/* Connection state for the SSE stream. A silent disconnect is
            indistinguishable from an idle system, so it is shown. */}
        <span
          className="row nowrap"
          style={{ fontSize: '0.75rem', color: 'var(--text-muted)' }}
          title={STREAM_HELP[status]}
        >
          <span className={`live-dot ${status}`} />
          <span className="nowrap">{STREAM_SHORT[status]}</span>
        </span>

        <NavLink
          to="/notifications"
          className="btn btn-sm btn-ghost"
          title={`${unread} unread`}
          aria-label={`Notifications, ${unread} unread`}
        >
          <span aria-hidden="true">🔔</span>
          {unread > 0 && <strong style={{ color: 'var(--accent)' }}>{unread}</strong>}
        </NavLink>

        <div className="row">
          <div style={{ textAlign: 'right', lineHeight: 1.25 }}>
            <div style={{ fontWeight: 600 }}>{user.full_name || user.username}</div>
            <div className="faint" style={{ fontSize: '0.75rem' }}>
              {ROLE_LABEL[user.role] ?? user.role}
            </div>
          </div>
          <button
            type="button"
            className="btn btn-sm"
            onClick={() => setChangingPassword(true)}
          >
            Password
          </button>
          <button
            type="button"
            className="btn btn-sm"
            onClick={() => {
              signOut()
              navigate('/login', { replace: true })
            }}
          >
            Sign out
          </button>
        </div>
      </header>

      <main className="main">
        <Outlet />
      </main>

      {changingPassword && <ChangePasswordDialog onClose={() => setChangingPassword(false)} />}
      <Toaster />
    </div>
  )
}

const STREAM_HELP = {
  open: 'Live updates connected',
  connecting: 'Connecting to live updates',
  error: 'Live updates disconnected, reconnecting',
  closed: 'Live updates are not connected',
} as const

const STREAM_SHORT = { open: 'Live', connecting: '…', error: 'Reconnecting', closed: 'Offline' } as const
