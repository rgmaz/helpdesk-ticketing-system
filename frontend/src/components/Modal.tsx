import { useEffect, useRef, type ReactNode } from 'react'

/**
 * A modal built on the native <dialog> element.
 *
 * Using the platform's own dialog rather than a component library gets focus
 * trapping, Escape handling, inertness of the page behind and the backdrop
 * semantics without shipping any of them. The browser also handles the top
 * layer, so a modal is never clipped by an ancestor's overflow.
 *
 * It is rendered only while open, and opens itself on mount. That is what lets
 * every caller skip the usual "reset the form in an effect when open flips"
 * dance: a closed dialog is unmounted, so the next one starts with fresh state
 * by construction rather than by remembering to clear it.
 */
export function Modal({
  title,
  onClose,
  children,
  footer,
}: {
  title: string
  onClose: () => void
  children: ReactNode
  footer?: ReactNode
}) {
  const ref = useRef<HTMLDialogElement | null>(null)

  useEffect(() => {
    const el = ref.current
    // The element can already be open if the effect re-runs; showModal throws on
    // a second call, so the guard is load-bearing rather than defensive.
    if (el && !el.open) el.showModal()
  }, [])

  return (
    <dialog
      ref={ref}
      onCancel={(event) => {
        // Escape fires `cancel`; taking it over lets the owner's state drive
        // closing so the dialog and the thing that opened it never disagree.
        event.preventDefault()
        onClose()
      }}
      onClick={(event) => {
        // A click lands on the dialog element itself only when it hits the
        // backdrop, which is how <dialog> distinguishes it from inner content.
        if (event.target === ref.current) onClose()
      }}
      style={{
        border: '1px solid var(--border)',
        borderRadius: 'var(--radius-lg)',
        background: 'var(--surface)',
        color: 'var(--text)',
        boxShadow: 'var(--shadow-lg)',
        padding: 0,
        width: 'min(560px, calc(100vw - 32px))',
      }}
    >
      <div className="card-pad">
        <div className="row spread" style={{ marginBottom: 14 }}>
          <h2>{title}</h2>
          <button type="button" className="btn btn-sm btn-ghost" onClick={onClose} aria-label="Close">
            ×
          </button>
        </div>
        {children}
        {footer && (
          <div className="row spread" style={{ marginTop: 6 }}>
            {footer}
          </div>
        )}
      </div>
    </dialog>
  )
}