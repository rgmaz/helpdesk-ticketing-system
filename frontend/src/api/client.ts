import type {
  Comment,
  CreateTicketInput,
  DashboardPayload,
  DirectoryUser,
  LoginResponse,
  Meta,
  MeResponse,
  Notification,
  Page,
  Priority,
  Role,
  SLAEngineStats,
  SLAPolicy,
  Ticket,
  TicketDetail,
  TicketQuery,
  TicketStatus,
  UpdateTicketInput,
  User,
  WorkloadReport,
} from './types'

const BASE = '/api/v1'
const TOKEN_KEY = 'itsm.token'

/**
 * ApiError carries the server's structured failure so a caller can branch on the
 * status rather than pattern-matching a message string. `code` is the stable
 * machine-readable discriminator the backend sets alongside `error`.
 */
export class ApiError extends Error {
  readonly status: number
  readonly code: string

  constructor(status: number, message: string, code = '') {
    super(message)
    this.name = 'ApiError'
    this.status = status
    this.code = code
  }

  /** True when re-authenticating is the right response. */
  get isAuthFailure(): boolean {
    return this.status === 401
  }

  get isConflict(): boolean {
    return this.status === 409
  }

  get isValidation(): boolean {
    return this.status === 400 || this.status === 422
  }
}

/**
 * Listeners for 401s, registered once by the auth provider.
 *
 * Every failed request funnels through here so an expired token bounces the
 * user to the login screen exactly once, instead of each component inventing its
 * own handling and leaving a half-authenticated UI on screen.
 */
type UnauthorizedListener = () => void
const unauthorizedListeners = new Set<UnauthorizedListener>()

export function onUnauthorized(fn: UnauthorizedListener): () => void {
  unauthorizedListeners.add(fn)
  return () => unauthorizedListeners.delete(fn)
}

export function getToken(): string | null {
  try {
    return localStorage.getItem(TOKEN_KEY)
  } catch {
    // Private browsing and blocked storage both make localStorage throw.
    return null
  }
}

export function setToken(token: string | null): void {
  try {
    if (token) localStorage.setItem(TOKEN_KEY, token)
    else localStorage.removeItem(TOKEN_KEY)
  } catch {
    // A session that cannot persist is still usable; it just ends at reload.
  }
}

/** Serialises a ticket filter into the query string the API expects. */
export function buildTicketQuery(query: TicketQuery): string {
  const params = new URLSearchParams()

  // Arrays go over as comma-separated lists, which is what the API expands.
  const list = (key: string, values?: string[]) => {
    if (values?.length) params.set(key, values.join(','))
  }
  if (query.q) params.set('q', query.q)
  list('status', query.status)
  list('priority', query.priority)
  list('category', query.category)
  // The API has two parameters here: assignee accepts the words 'me' and
  // 'none' for the two shortcuts people actually want, while assignee_id takes
  // a specific person. A bare ?assignee=7 is silently ignored by the server, so
  // a numeric filter has to go out under the right name.
  if (typeof query.assignee === 'number') params.set('assignee_id', String(query.assignee))
  else if (query.assignee !== undefined) params.set('assignee', query.assignee)
  if (typeof query.requester === 'number') params.set('requester_id', String(query.requester))
  else if (query.requester !== undefined) params.set('requester', query.requester)
  // Booleans are only sent when true: the API's ?breached=false and an absent
  // parameter mean the same thing, and sending it would be noise.
  if (query.breached) params.set('breached', 'true')
  if (query.escalated) params.set('escalated', 'true')
  if (query.open) params.set('open', 'true')
  if (query.created_from) params.set('created_from', query.created_from)
  if (query.created_to) params.set('created_to', query.created_to)
  if (query.resolved_from) params.set('resolved_from', query.resolved_from)
  if (query.resolved_to) params.set('resolved_to', query.resolved_to)
  if (query.sort_by) params.set('sort_by', query.sort_by)
  if (query.sort_dir) params.set('sort_dir', query.sort_dir)
  // The API's page is zero-based; Page.page is one-based for display.
  if (query.page) params.set('page', String(query.page - 1))
  if (query.limit) params.set('limit', String(query.limit))

  const s = params.toString()
  return s ? `?${s}` : ''
}

interface RequestOptions {
  method?: string
  body?: unknown
  /** Set for endpoints that must work signed out, such as login. */
  anonymous?: boolean
  signal?: AbortSignal
}

async function request<T>(path: string, opts: RequestOptions = {}): Promise<T> {
  const { method = 'GET', body, anonymous = false, signal } = opts
  const headers: Record<string, string> = {}

  const token = getToken()
  if (token && !anonymous) headers.Authorization = `Bearer ${token}`
  if (body !== undefined) headers['Content-Type'] = 'application/json'

  let res: Response
  try {
    res = await fetch(`${BASE}${path}`, {
      method,
      headers,
      body: body === undefined ? undefined : JSON.stringify(body),
      signal,
    })
  } catch (err) {
    // A rejected fetch is a transport failure: offline, DNS, or an aborted
    // request. Surfacing it as an ApiError keeps one error type in the app.
    if ((err as Error).name === 'AbortError') throw err
    throw new ApiError(0, 'Could not reach the server. Is the backend running?')
  }

  if (res.status === 204) return undefined as T

  const text = await res.text()
  let payload: unknown = null
  if (text) {
    try {
      payload = JSON.parse(text)
    } catch {
      // A non-JSON body from an API that always sends JSON means something is
      // in front of it, so the raw text is the most useful thing to show.
      if (!res.ok) throw new ApiError(res.status, text.slice(0, 300))
      throw new ApiError(res.status, 'The server sent a response the app could not read.')
    }
  }

  if (!res.ok) {
    const err = (payload ?? {}) as { error?: string; code?: string }
    const message = err.error || `Request failed with status ${res.status}`
    const apiError = new ApiError(res.status, message, err.code ?? '')
    // A 401 means the token is gone, expired or the account was disabled.
    // Broadcasting once here keeps session teardown in a single place.
    if (apiError.isAuthFailure && !anonymous) {
      for (const listener of unauthorizedListeners) listener()
    }
    throw apiError
  }

  return payload as T
}

/** Every backend endpoint the UI uses, in one typed object. */
export const api = {
  // --- session ---
  login(identifier: string, password: string): Promise<LoginResponse> {
    return request('/auth/login', {
      method: 'POST',
      anonymous: true,
      body: { identifier, password },
    })
  },

  /**
   * Registration returns the account, not a session: issuing a token from an
   * unauthenticated self-service endpoint means an attacker who can create
   * accounts can also mint them. The caller signs in afterwards with the
   * credentials it already has.
   */
  register(input: {
    username: string
    email: string
    password: string
    full_name: string
  }): Promise<{ user: User }> {
    return request('/auth/register', { method: 'POST', anonymous: true, body: input })
  },

  me(): Promise<MeResponse> {
    return request('/auth/me')
  },

  changePassword(current: string, next: string): Promise<{ message: string }> {
    return request('/auth/password', {
      method: 'POST',
      body: { current_password: current, new_password: next },
    })
  },

  // --- reference data ---
  meta(): Promise<Meta> {
    return request('/meta', { anonymous: true })
  },

  // --- tickets ---
  listTickets(query: TicketQuery = {}, signal?: AbortSignal): Promise<Page<Ticket>> {
    return request(`/tickets${buildTicketQuery(query)}`, { signal })
  },

  searchTickets(query: TicketQuery, signal?: AbortSignal): Promise<Page<Ticket>> {
    return request(`/tickets/search${buildTicketQuery(query)}`, { signal })
  },

  getTicket(id: number): Promise<TicketDetail> {
    return request(`/tickets/${id}`)
  },

  createTicket(input: CreateTicketInput): Promise<{ ticket: Ticket }> {
    return request('/tickets', { method: 'POST', body: input })
  },

  updateTicket(id: number, input: UpdateTicketInput): Promise<{ ticket: Ticket }> {
    return request(`/tickets/${id}`, { method: 'PATCH', body: input })
  },

  /** Both deletes answer 204 with no body. */
  deleteTicket(id: number): Promise<void> {
    return request(`/tickets/${id}`, { method: 'DELETE' })
  },

  listComments(id: number): Promise<Page<Comment>> {
    return request(`/tickets/${id}/comments`)
  },

  addComment(id: number, body: string, internal: boolean): Promise<{ comment: Comment }> {
    return request(`/tickets/${id}/comments`, { method: 'POST', body: { body, internal } })
  },

  // --- people ---
  listUsers(
    params: { q?: string; role?: Role; active?: boolean; page?: number; limit?: number } = {},
  ): Promise<Page<DirectoryUser>> {
    const search = new URLSearchParams()
    if (params.q) search.set('q', params.q)
    if (params.role) search.set('role', params.role)
    if (params.active !== undefined) search.set('active', String(params.active))
    if (params.page) search.set('page', String(params.page - 1))
    if (params.limit) search.set('limit', String(params.limit))
    const qs = search.toString()
    return request(`/users${qs ? `?${qs}` : ''}`)
  },

  technicians(): Promise<Page<DirectoryUser>> {
    return request('/technicians')
  },

  createUser(input: {
    username: string
    email: string
    password: string
    full_name: string
    role: Role
    active?: boolean
  }): Promise<{ user: User }> {
    return request('/users', { method: 'POST', body: input })
  },

  updateUser(
    id: number,
    input: {
      email?: string
      full_name?: string
      role?: Role
      active?: boolean
      password?: string
    },
  ): Promise<{ user: User }> {
    return request(`/users/${id}`, { method: 'PATCH', body: input })
  },

  deleteUser(id: number): Promise<void> {
    return request(`/users/${id}`, { method: 'DELETE' })
  },

  // --- SLA ---
  policies(): Promise<Page<SLAPolicy>> {
    return request('/sla-policies')
  },

  updatePolicy(
    id: number,
    input: Partial<
      Pick<
        SLAPolicy,
        'name' | 'description' | 'response_minutes' | 'resolution_minutes' | 'escalation_minutes' | 'active'
      >
    >,
  ): Promise<{ policy: SLAPolicy }> {
    return request(`/sla-policies/${id}`, { method: 'PATCH', body: input })
  },

  slaEngine(): Promise<SLAEngineStats> {
    return request('/system/sla-engine')
  },

  // --- analytics ---
  dashboard(windowDays = 30): Promise<DashboardPayload> {
    return request(`/analytics/dashboard?window_days=${windowDays}`)
  },

  workload(): Promise<WorkloadReport> {
    return request('/analytics/workload')
  },

  // --- notifications ---
  notifications(unread = false): Promise<Page<Notification>> {
    return request(`/notifications${unread ? '?unread=true' : ''}`)
  },

  unreadCount(): Promise<{ unread: number }> {
    return request('/notifications/unread-count')
  },

  markRead(id: number): Promise<{ id: number; read: boolean }> {
    return request(`/notifications/${id}/read`, { method: 'POST' })
  },

  markAllRead(): Promise<{ marked_read: number }> {
    return request('/notifications/read-all', { method: 'POST' })
  },
}

export type { Priority, TicketStatus }