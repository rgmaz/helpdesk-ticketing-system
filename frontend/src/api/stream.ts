import { getToken } from './client'
import type { StreamEvent } from './types'

/**
 * Server-sent events over fetch rather than EventSource.
 *
 * EventSource cannot set an Authorization header, and putting a JWT in the URL
 * leaks it into proxy logs and browser history. Reading the response body as a
 * stream gives us the header without either compromise, at the cost of parsing
 * the SSE grammar by hand, which is a handful of lines.
 */

export type StreamStatus = 'connecting' | 'open' | 'closed' | 'error'

export interface StreamHandlers {
  onEvent: (event: StreamEvent) => void
  onStatus?: (status: StreamStatus) => void
  /** Surfaced to the UI so a persistent disconnect is visible, not silent. */
  onError?: (error: Error) => void
}

export interface StreamConnection {
  close: () => void
}

const STREAM_PATH = '/api/v1/notifications/stream'

/**
 * Reconnect backoff. The first retry is quick because a dropped connection is
 * usually the dev server restarting; later retries back off so a backend that is
 * genuinely down does not get hammered.
 */
const RETRY_DELAYS_MS = [1000, 2000, 4000, 8000, 15000]

export function connectStream(handlers: StreamHandlers): StreamConnection {
  const controller = new AbortController()
  let attempt = 0
  let retryTimer: ReturnType<typeof setTimeout> | undefined
  let stopped = false

  const open = async () => {
    if (stopped) return

    const token = getToken()
    if (!token) {
      handlers.onStatus?.('closed')
      return
    }

    handlers.onStatus?.(attempt === 0 ? 'connecting' : 'connecting')

    try {
      const res = await fetch(STREAM_PATH, {
        headers: { Authorization: `Bearer ${token}`, Accept: 'text/event-stream' },
        signal: controller.signal,
        // The stream must not be served from cache on reconnect.
        cache: 'no-store',
      })

      if (res.status === 401 || res.status === 403) {
        // The session is gone; retrying cannot help, and the client's 401
        // handler has already torn the session down.
        handlers.onStatus?.('closed')
        return
      }
      if (!res.ok || !res.body) {
        throw new Error(`stream refused with status ${res.status}`)
      }

      handlers.onStatus?.('open')
      attempt = 0

      await readFrames(res.body, handlers.onEvent)

      // A clean end-of-stream still means the server closed the connection,
      // which happens on deploys and on keep-alive timeouts. Reconnect.
      if (!stopped) scheduleRetry()
    } catch (err) {
      if (stopped || (err as Error).name === 'AbortError') return
      handlers.onError?.(err as Error)
      handlers.onStatus?.('error')
      scheduleRetry()
    }
  }

  const scheduleRetry = () => {
    if (stopped) return
    const delay = RETRY_DELAYS_MS[Math.min(attempt, RETRY_DELAYS_MS.length - 1)]
    attempt += 1
    retryTimer = setTimeout(open, delay)
  }

  void open()

  return {
    close() {
      stopped = true
      controller.abort()
      if (retryTimer) clearTimeout(retryTimer)
      handlers.onStatus?.('closed')
    },
  }
}

/**
 * Parses the SSE wire format out of a byte stream.
 */
async function readFrames(
  body: ReadableStream<Uint8Array>,
  onEvent: (event: StreamEvent) => void,
): Promise<void> {
  const reader = body.getReader()
  const decoder = new TextDecoder()
  let buffer = ''

  try {
    for (;;) {
      const { done, value } = await reader.read()
      if (done) return

      buffer += decoder.decode(value, { stream: true })

      // Frames end at a blank line ("\n\n", or "\r\n\r\n" from a proxy). A frame
      // is only dispatched once that terminator arrives, so a payload split
      // across two network chunks is still delivered whole.
      for (;;) {
        const split = findFrameEnd(buffer)
        if (!split) break
        dispatch(buffer.slice(0, split.index), onEvent)
        buffer = buffer.slice(split.index + split.length)
      }
    }
  } finally {
    reader.releaseLock()
  }
}

/**
 * Locates the next frame terminator, or returns null when the buffer holds only
 * a partial frame.
 *
 * Both line endings are accepted because a reverse proxy is allowed to rewrite
 * them, and a parser that only understood one would silently stop delivering.
 */
function findFrameEnd(buffer: string): { index: number; length: number } | null {
  const lf = buffer.indexOf('\n\n')
  const crlf = buffer.indexOf('\r\n\r\n')
  if (lf === -1 && crlf === -1) return null
  if (crlf !== -1 && (lf === -1 || crlf < lf)) return { index: crlf, length: 4 }
  return { index: lf, length: 2 }
}

function dispatch(frame: string, onEvent: (event: StreamEvent) => void) {
  // Comment lines (":" prefixed) are keep-alives and carry nothing to dispatch.
  let id = ''
  let event = 'message'
  const data: string[] = []

  for (const rawLine of frame.split(/\r?\n/)) {
    if (!rawLine || rawLine.startsWith(':')) continue
    const colon = rawLine.indexOf(':')
    const field = colon === -1 ? rawLine : rawLine.slice(0, colon)
    // A single leading space after the colon is part of the framing.
    const value = colon === -1 ? '' : rawLine.slice(colon + 1).replace(/^ /, '')

    switch (field) {
      case 'id':
        id = value
        break
      case 'event':
        event = value
        break
      case 'data':
        data.push(value)
        break
      default:
        // retry: and unknown fields are ignored, as the spec requires.
        break
    }
  }

  if (!data.length) return

  try {
    const parsed = JSON.parse(data.join('\n')) as StreamEvent
    onEvent({ ...parsed, id: parsed.id || id, type: parsed.type || event })
  } catch {
    // A malformed payload must not kill the stream; the next frame may be fine.
  }
}