import { createContext, useContext } from 'react'

/** Split from the provider so this file holds no components; see AuthContext.ts. */

export interface Toast {
  id: number
  message: string
  tone: 'info' | 'success' | 'warning' | 'error'
}

export interface ToastState {
  toasts: Toast[]
  notify: (message: string, tone?: Toast['tone']) => void
  /** Convenience wrappers so callers do not repeat the tone argument. */
  success: (message: string) => void
  error: (message: string) => void
  dismiss: (id: number) => void
}

export const ToastContext = createContext<ToastState | null>(null)

export function useToast(): ToastState {
  const ctx = useContext(ToastContext)
  if (!ctx) throw new Error('useToast must be used inside a ToastProvider')
  return ctx
}