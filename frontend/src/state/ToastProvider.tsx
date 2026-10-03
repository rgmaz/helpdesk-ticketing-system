import { useCallback, useEffect, useMemo, useRef, useState, type ReactNode } from 'react'
import { ToastContext, type Toast, type ToastState } from './ToastContext'

const DISMISS_AFTER_MS = 5000
const MAX_VISIBLE = 4

/**
 * Transient messages.
 *
 * Sits outside every other provider so any layer can report a failure,
 * including the notification stream handler.
 */
export function ToastProvider({ children }: { children: ReactNode }) {
  const [toasts, setToasts] = useState<Toast[]>([])
  const nextId = useRef(1)
  const timers = useRef(new Map<number, ReturnType<typeof setTimeout>>())

  const dismiss = useCallback((id: number) => {
    setToasts((current) => current.filter((t) => t.id !== id))
    const timer = timers.current.get(id)
    if (timer) {
      clearTimeout(timer)
      timers.current.delete(id)
    }
  }, [])

  const notify = useCallback(
    (message: string, tone: Toast['tone'] = 'info') => {
      const id = nextId.current++
      setToasts((current) => {
        // Cap the stack: a burst of failures should not bury the page, and the
        // newest is the one the user most likely needs to read.
        const next = [...current, { id, message, tone }]
        if (next.length <= MAX_VISIBLE) return next

        // The evicted toasts will never be seen, so their pending timers are
        // cancelled here rather than left to fire against ids that are gone.
        for (const evicted of next.slice(0, next.length - MAX_VISIBLE)) {
          const timer = timers.current.get(evicted.id)
          if (timer) clearTimeout(timer)
          timers.current.delete(evicted.id)
        }
        return next.slice(next.length - MAX_VISIBLE)
      })
      timers.current.set(
        id,
        // Errors linger: they usually need acting on, unlike a success
        // confirmation the user has already registered.
        setTimeout(() => dismiss(id), tone === 'error' ? DISMISS_AFTER_MS * 2 : DISMISS_AFTER_MS),
      )
    },
    [dismiss],
  )

  // Clear pending timers on unmount so a dismissed provider does not fire.
  useEffect(() => {
    const pending = timers.current
    return () => {
      for (const timer of pending.values()) clearTimeout(timer)
      pending.clear()
    }
  }, [])

  const value = useMemo<ToastState>(
    () => ({
      toasts,
      notify,
      success: (message: string) => notify(message, 'success'),
      error: (message: string) => notify(message, 'error'),
      dismiss,
    }),
    [toasts, notify, dismiss],
  )

  return <ToastContext.Provider value={value}>{children}</ToastContext.Provider>
}