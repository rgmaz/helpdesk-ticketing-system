import { useCallback, useEffect, useRef, useState } from 'react'
import { ApiError } from '../api/client'

interface AsyncState<T> {
  data: T | null
  error: string | null
  loading: boolean
  /** True only for the very first load, so refreshes do not blank the page. */
  initial: boolean
  reload: () => void
}

type Fetcher<T> = (signal: AbortSignal) => Promise<T>

/**
 * Loads data with cancellation and stale-response protection.
 *
 * Every list view needs the same three things: abort the previous request when
 * the dependencies change so a fast typist does not have an older response
 * overwrite a newer one, keep the existing data visible while reloading so the
 * page does not flash empty, and stop entirely when the component unmounts.
 */
export function useAsync<T>(fetcher: Fetcher<T>, deps: unknown[]): AsyncState<T> {
  const [data, setData] = useState<T | null>(null)
  const [error, setError] = useState<string | null>(null)
  const [loading, setLoading] = useState(true)
  const [initial, setInitial] = useState(true)

  // The fetcher is usually an inline arrow, so it changes on every render.
  // Holding it in a ref and driving the effect off `deps` keeps that from
  // causing an infinite request loop.
  const latest = useRef(fetcher)
  latest.current = fetcher

  const [nonce, setNonce] = useState(0)
  const mounted = useRef(true)

  useEffect(() => {
    mounted.current = true
    return () => {
      mounted.current = false
    }
  }, [])

  useEffect(() => {
    const controller = new AbortController()
    setLoading(true)

    latest
      .current(controller.signal)
      .then((result) => {
        if (controller.signal.aborted || !mounted.current) return
        setData(result)
        setError(null)
      })
      .catch((err: unknown) => {
        if (controller.signal.aborted || !mounted.current) return
        if ((err as Error).name === 'AbortError') return
        setError(err instanceof ApiError ? err.message : String(err))
      })
      .finally(() => {
        if (controller.signal.aborted || !mounted.current) return
        setLoading(false)
        setInitial(false)
      })

    return () => controller.abort()
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [...deps, nonce])

  const reload = useCallback(() => setNonce((n) => n + 1), [])

  return { data, error, loading, initial, reload }
}

/**
 * Wraps a mutating call: tracks its own pending flag and normalises the error,
 * so every form does not reimplement try/catch and a "Saving…" state.
 */
export function useMutation<TArgs extends unknown[], TResult>(
  action: (...args: TArgs) => Promise<TResult>,
) {
  const [pending, setPending] = useState(false)
  const [error, setError] = useState<string | null>(null)

  const run = useCallback(
    async (...args: TArgs): Promise<TResult | null> => {
      setPending(true)
      setError(null)
      try {
        return await action(...args)
      } catch (err) {
        setError(err instanceof ApiError ? err.message : String(err))
        return null
      } finally {
        setPending(false)
      }
    },
    [action],
  )

  return { run, pending, error, clearError: () => setError(null) }
}

/** Delays a rapidly-changing value, for search-as-you-type inputs. */
export function useDebounced<T>(value: T, delayMs = 300): T {
  const [debounced, setDebounced] = useState(value)

  useEffect(() => {
    const timer = setTimeout(() => setDebounced(value), delayMs)
    return () => clearTimeout(timer)
  }, [value, delayMs])

  return debounced
}