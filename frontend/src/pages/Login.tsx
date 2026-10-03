import { useEffect, useState, type FormEvent } from 'react'
import { Navigate, useLocation, useNavigate } from 'react-router-dom'
import { useAuth } from '../state/AuthContext'
import { useMeta } from '../state/MetaContext'
import { ErrorNote, Field } from '../components/ui'

/**
 * Sign-in and self-service registration on one screen.
 *
 * The two are the same form with different fields rather than separate pages:
 * for a local tool most people only ever need the first tab, and splitting them
 * would put registration two clicks away from where people look for it.
 */
export function LoginPage() {
  const { user, initialising, signIn, register } = useAuth()
  const navigate = useNavigate()
  const location = useLocation()

  const [mode, setMode] = useState<'login' | 'register'>('login')
  const [identifier, setIdentifier] = useState('')
  const [password, setPassword] = useState('')
  const [fullName, setFullName] = useState('')
  const [email, setEmail] = useState('')
  const [error, setError] = useState<string | null>(null)
  const [pending, setPending] = useState(false)

  // Preserve where the user was headed before they were asked to sign in.
  const from = (location.state as { from?: string } | null)?.from ?? '/portal'

  useEffect(() => {
    document.title = 'Sign in · Service Desk'
  }, [])

  if (!initialising && user) return <Navigate to={from} replace />

  const submit = async (event: FormEvent) => {
    event.preventDefault()
    setPending(true)
    setError(null)
    try {
      if (mode === 'login') {
        await signIn(identifier.trim(), password)
      } else {
        await register({
          // The username is derived from the email: asking for both invites a
          // mismatch between them, and nothing in the product needs them apart.
          username: identifier.trim(),
          email: email.trim(),
          password,
          full_name: fullName.trim(),
        })
      }
      navigate(from, { replace: true })
    } catch (err) {
      setError((err as Error).message)
    } finally {
      setPending(false)
    }
  }

  return (
    <div className="auth">
      <div className="card auth-card">
        <div className="row" style={{ marginBottom: 18 }}>
          <span className="brand-mark" aria-hidden="true">
            SD
          </span>
          <div>
            <h1>Service Desk</h1>
            <p className="muted" style={{ fontSize: '0.8125rem' }}>
              {mode === 'login' ? 'Sign in to your account' : 'Create a requester account'}
            </p>
          </div>
        </div>

        <div className="auth-tabs" role="tablist">
          <button
            type="button"
            role="tab"
            aria-selected={mode === 'login'}
            className={`auth-tab${mode === 'login' ? ' on' : ''}`}
            onClick={() => {
              setMode('login')
              setError(null)
            }}
          >
            Sign in
          </button>
          <button
            type="button"
            role="tab"
            aria-selected={mode === 'register'}
            className={`auth-tab${mode === 'register' ? ' on' : ''}`}
            onClick={() => {
              setMode('register')
              setError(null)
            }}
          >
            Register
          </button>
        </div>

        <form onSubmit={submit}>
          {error && (
            <div style={{ marginBottom: 14 }}>
              <ErrorNote>{error}</ErrorNote>
            </div>
          )}

          <Field
            label={mode === 'login' ? 'Username or email' : 'Username'}
            htmlFor="identifier"
          >
            <input
              id="identifier"
              value={identifier}
              onChange={(e) => setIdentifier(e.target.value)}
              autoComplete="username"
              autoCapitalize="none"
              spellCheck={false}
              required
              autoFocus
            />
          </Field>

          {mode === 'register' && (
            <>
              <Field label="Full name" htmlFor="fullname">
                <input
                  id="fullname"
                  value={fullName}
                  onChange={(e) => setFullName(e.target.value)}
                  autoComplete="name"
                  required
                />
              </Field>
              <Field label="Email" htmlFor="email">
                <input
                  id="email"
                  type="email"
                  value={email}
                  onChange={(e) => setEmail(e.target.value)}
                  autoComplete="email"
                  required
                />
              </Field>
            </>
          )}

          <Field
            label="Password"
            htmlFor="password"
            hint={mode === 'register' ? 'At least 10 characters, with a letter and a digit.' : undefined}
          >
            <input
              id="password"
              type="password"
              value={password}
              onChange={(e) => setPassword(e.target.value)}
              autoComplete={mode === 'login' ? 'current-password' : 'new-password'}
              required
            />
          </Field>

          <button
            type="submit"
            className="btn btn-primary btn-block"
            disabled={pending || initialising}
          >
            {pending ? 'Please wait…' : mode === 'login' ? 'Sign in' : 'Create account'}
          </button>
        </form>

        {mode === 'login' && <DemoAccounts onPick={(id) => setIdentifier(id)} />}
      </div>
    </div>
  )
}

/**
 * Demo credentials, offered only when the backend reports it was started with
 * -demo. These accounts do not exist in a normal deployment, and a sign-in
 * button that cannot work is worse than no sign-in button.
 */
function DemoAccounts({ onPick }: { onPick: (identifier: string) => void }) {
  const { meta, loading } = useMeta()
  if (loading || !meta?.demo) return null

  return (
    <div className="hint-box">
      <strong>Demo accounts</strong> — seeded by <code>go run . -demo</code>. Password for all of
      them: <code>DemoDesk1234</code>
      <div className="row row-wrap" style={{ marginTop: 8, gap: 6 }}>
        {DEMO_ACCOUNTS.map((account) => (
          <button key={account} type="button" className="chip" onClick={() => onPick(account)}>
            {account}
          </button>
        ))}
      </div>
    </div>
  )
}

/**
 * The development-only credentials. They exist only in a database seeded by the
 * -demo flag; the bootstrap administrator never uses this password.
 */
const DEMO_ACCOUNTS = ['admin', 'tech', 'tech2', 'avery', 'jordan']
