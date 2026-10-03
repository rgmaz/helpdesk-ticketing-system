import { lazy, Suspense, type ReactNode } from 'react'
import { BrowserRouter, Link, Navigate, Route, Routes, useLocation } from 'react-router-dom'
import { Layout } from './components/Layout'
import { Spinner } from './components/ui'
import { ROLE_LABEL } from './lib/format'
import { useAuth } from './state/AuthContext'
import { LoginPage } from './pages/Login'
import { NotificationsPage } from './pages/Notifications'
import { PortalPage } from './pages/Portal'
import { QueuePage } from './pages/Queue'
import { TicketPage } from './pages/TicketDetail'

/**
 * The administrator pages are split out.
 *
 * The dashboard pulls in Recharts, which is several times the size of the rest
 * of the app combined, and only an administrator will ever load it. Splitting it
 * keeps that weight out of the bundle a requester or technician downloads —
 * which is every user, on every visit, as opposed to the one who opens the
 * dashboard once. The other two are split for consistency of loading behaviour
 * rather than for size.
 */
const DashboardPage = lazy(() => import('./pages/Dashboard').then((m) => ({ default: m.DashboardPage })))
const PeoplePage = lazy(() => import('./pages/People').then((m) => ({ default: m.PeoplePage })))
const PoliciesPage = lazy(() => import('./pages/Policies').then((m) => ({ default: m.PoliciesPage })))

/** Only administrators may load the above; see the note at the top. */
function AdminOnly({ roles, children }: { roles: string[]; children: ReactNode }) {
  return (
    <RequireRole roles={roles}>
      <Suspense fallback={<Spinner label="Loading…" />}>{children}</Suspense>
    </RequireRole>
  )
}

/**
 * Route table and role gates.
 *
 * The gates here are a convenience, not the security boundary: the server
 * enforces every rule and the end-user scoping is applied in SQL, so an
 * administrator who edits their own way past a guard sees an empty queue rather
 * than somebody else's tickets. What the gate does is stop offering a page that
 * would only ever 403.
 *
 * Each role's default landing page is its own, so signing in as a technician
 * opens the queue rather than an empty personal ticket list.
 */
export default function App() {
  return (
    <BrowserRouter>
      <Routes>
        <Route path="/login" element={<LoginPage />} />

        <Route
          element={
            <RequireAuth>
              <Layout />
            </RequireAuth>
          }
        >
          <Route index element={<Landing />} />
          <Route path="/portal" element={<PortalPage />} />
          <Route
            path="/queue"
            element={
              <RequireRole roles={['technician', 'admin']}>
                <QueuePage />
              </RequireRole>
            }
          />
          <Route path="/tickets/:ticketId" element={<TicketPage />} />
          <Route path="/notifications" element={<NotificationsPage />} />
          <Route
            path="/dashboard"
            element={
              <AdminOnly roles={['admin']}>
                <DashboardPage />
              </AdminOnly>
            }
          />
          <Route
            path="/team"
            element={
              <AdminOnly roles={['admin']}>
                <PeoplePage />
              </AdminOnly>
            }
          />
          <Route
            path="/policies"
            element={
              <AdminOnly roles={['admin']}>
                <PoliciesPage />
              </AdminOnly>
            }
          />
        </Route>

        <Route path="*" element={<NotFound />} />
      </Routes>
    </BrowserRouter>
  )
}

/** Blocks the whole authenticated tree while the stored token is being checked. */
function RequireAuth({ children }: { children: ReactNode }) {
  const { user, initialising } = useAuth()
  const location = useLocation()

  if (initialising) {
    return (
      <div className="auth">
        <Spinner label="Checking your session…" />
      </div>
    )
  }
  if (!user) {
    // Remember where they were headed, so signing in resumes it rather than
    // dumping them on the landing page.
    return <Navigate to="/login" state={{ from: location.pathname }} replace />
  }
  return <>{children}</>
}

/**
 * Renders an explanation rather than silently redirecting.
 *
 * A guard that bounces somebody to their own portal looks like the app decided
 * to send them somewhere else; saying which role is required makes a wrong link
 * debuggable instead of mysterious.
 */
function RequireRole({ roles, children }: { roles: string[]; children: ReactNode }) {
  const { user } = useAuth()
  if (!user || !roles.includes(user.role)) {
    return (
      <div className="card card-pad empty">
        <h3>Not available for your role</h3>
        <p className="muted">
          You are signed in as a {ROLE_LABEL[user?.role ?? ''] ?? user?.role}. This area is for{' '}
          {roles.map((r) => ROLE_LABEL[r] ?? r).join(' and ')} accounts.
        </p>
        <p style={{ marginTop: 12 }}>
          <Link className="btn" to={user?.role === 'admin' ? '/dashboard' : '/portal'}>
            Go to my work
          </Link>
        </p>
      </div>
    )
  }
  return <>{children}</>
}

/** Sends each role to the page it actually works in. */
function Landing() {
  const { user } = useAuth()
  if (user?.role === 'admin') return <Navigate to="/dashboard" replace />
  if (user?.role === 'technician') return <Navigate to="/queue" replace />
  return <Navigate to="/portal" replace />
}

function NotFound() {
  return (
    <div className="auth">
      <div className="card card-pad empty">
        <h3>Page not found</h3>
        <p className="muted">That address does not match anything here.</p>
        <p style={{ marginTop: 12 }}>
          <a className="btn" href="/">
            Go home
          </a>
        </p>
      </div>
    </div>
  )
}
