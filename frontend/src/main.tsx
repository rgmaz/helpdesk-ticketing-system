import { StrictMode } from 'react'
import { createRoot } from 'react-dom/client'
import App from './App'
import { AuthProvider } from './state/AuthProvider'
import { MetaProvider } from './state/MetaProvider'
import { NotificationsProvider } from './state/NotificationsProvider'
import { ToastProvider } from './state/ToastProvider'
import './styles.css'

/**
 * Provider order is a dependency chain, not a preference.
 *
 * Toasts wrap everything because any layer may report a failure. Auth sits above
 * notifications because the SSE connection is opened from the signed-in identity
 * and must close when it goes. Meta is outermost of the data providers because
 * every form and badge reads its vocabulary.
 */
createRoot(document.getElementById('root')!).render(
  <StrictMode>
    <ToastProvider>
      <AuthProvider>
        <MetaProvider>
          <NotificationsProvider>
            <App />
          </NotificationsProvider>
        </MetaProvider>
      </AuthProvider>
    </ToastProvider>
  </StrictMode>,
)