// Wire types mirroring the Go API.
//
// These are hand-written rather than generated so the compiler catches a
// mismatch, but they are the contract: rename a field on the server and it has
// to be renamed here too, or it silently arrives as undefined.

export type Role = 'user' | 'technician' | 'admin'

export type TicketStatus =
  | 'open'
  | 'in_progress'
  | 'pending'
  | 'resolved'
  | 'closed'
  | 'reopened'

export type Priority = 'low' | 'medium' | 'high' | 'critical'

export type SLAState = 'on_track' | 'at_risk' | 'breached' | 'met' | 'stopped'

export interface User {
  id: number
  username: string
  email: string
  full_name: string
  role: Role
  active: boolean
  created_at: string
}

/** A directory row is a user plus their live workload. */
export interface DirectoryUser extends User {
  active_tickets: number
  overdue_tickets: number
  resolved_30d: number
}

export interface LoginResponse {
  token: string
  token_type: string
  expires_at: string
  expires_in: number
  user: User
}

/** GET /auth/me returns the profile plus the caller's per-status ticket counts. */
export interface MeResponse {
  user: User
  ticket_counts: Partial<Record<TicketStatus, number>>
}

export interface Ticket {
  id: number
  ticket_number: string
  title: string
  description: string
  category: string
  priority: Priority
  status: TicketStatus
  requester_id: number
  requester_name: string
  assignee_id: number | null
  assignee_name: string | null
  sla_policy_id: number | null
  sla_policy_name: string | null
  sla_response_due_at: string | null
  sla_resolution_due_at: string | null
  first_response_at: string | null
  resolution_notes: string
  resolved_at: string | null
  closed_at: string | null
  response_breached: boolean
  resolution_breached: boolean
  escalation_level: number
  comment_count: number
  created_at: string
  updated_at: string
  sla_state: SLAState
  /** Seconds left on each clock. Absent once the clock has stopped. */
  sla_resolution_remaining_seconds?: number
  sla_response_remaining_seconds?: number
  /** Search results only: a real FTS5 snippet excerpt from the matched column. */
  snippet?: string
}

export interface Comment {
  id: number
  ticket_id: number
  author_id: number
  author_name: string
  author_role: Role
  body: string
  /** Staff-only notes are filtered out server-side for end users. */
  internal: boolean
  created_at: string
}

export interface TicketEvent {
  id: number
  ticket_id: number
  actor_id: number | null
  actor_name: string
  type: string
  from_value: string | null
  to_value: string | null
  message: string
  created_at: string
}

/** GET /tickets/{id} returns the ticket with its thread and timeline. */
export interface TicketDetail {
  ticket: Ticket
  comments: Comment[]
  events: TicketEvent[]
  sla: {
    state: SLAState
    response_breached: boolean
    resolution_breached: boolean
    escalation_level: number
  }
  /** The legal next states, straight from the server's state machine. */
  available_transitions: TicketStatus[]
  can_delete: boolean
  /** Staff only: how many private notes exist, so the UI can hint at them. */
  internal_note_count?: number
}

export interface SLAPolicy {
  id: number
  name: string
  priority: Priority
  response_minutes: number
  resolution_minutes: number
  escalation_minutes: number
  description: string
  active: boolean
  auto_assign: boolean
}

export interface TechnicianWorkload {
  user_id: number
  username: string
  full_name: string
  active_tickets: number
  overdue_tickets: number
  resolved_30d: number
  avg_resolution_hours_30d: number | null
}

export interface WorkloadReport {
  technicians: TechnicianWorkload[]
  active_total: number
  overdue_total: number
  busiest: number
  average_active_load: number
}

export interface DashboardStats {
  total_tickets: number
  open_tickets: number
  resolved_today: number
  created_today: number
  unassigned_tickets: number
  breached_tickets: number
  at_risk_tickets: number
  avg_overdue_hours: number | null
  total_users: number
  active_technicians: number
  avg_first_response_hours: number | null
  sla_compliance_pct: number | null
}

export interface MTTRResult {
  window_days: number
  resolved_count: number
  avg_resolution_hours: number | null
  median_resolution_hours: number | null
  p50_resolution_hours: number | null
  p90_resolution_hours: number | null
  min_resolution_hours: number | null
  max_resolution_hours: number | null
  avg_first_response_hours: number | null
  sla_met_count: number
  sla_breached_count: number
  sla_compliance_pct: number | null
}

export interface ThroughputBucket {
  bucket: string
  created: number
  resolved: number
  avg_resolution_hours: number | null
}

export interface Breakdown {
  total: number
  open: number
  resolved: number
  breached: number
  avg_resolution_hours: number | null
}

export interface CategoryBreakdown extends Breakdown {
  category: string
}

export interface PriorityBreakdown extends Breakdown {
  priority: Priority
}

export interface HistogramBucket {
  label: string
  count: number
  upper_bound_hours: number | null
}

export interface SLAEngineStats {
  interval_seconds: number
  ticks: number
  breaches_detected: number
  escalations: number
  errors: number
  last_error?: string
  last_run?: string
  next_run_estimate?: string
  warn_before_minutes: number
}

/** GET /analytics/dashboard bundles every panel for the initial page load. */
export interface DashboardPayload {
  stats: DashboardStats
  mttr: MTTRResult
  throughput: ThroughputBucket[]
  workload: TechnicianWorkload[]
  by_category: CategoryBreakdown[]
  by_priority: PriorityBreakdown[]
  status_distribution: Record<string, number>
  response_histogram: HistogramBucket[]
  sla_engine: SLAEngineStats
  generated_at: string
  window_days: number
  at_risk_minutes: number
}

export interface StreamEvent {
  id: string
  type: string
  title: string
  body?: string
  level: 'info' | 'success' | 'warning' | 'error'
  ticket_id?: number
  payload?: Record<string, unknown>
  created_at: string
}

export interface Notification {
  id: number
  user_id: number
  ticket_id: number | null
  kind: string
  title: string
  body: string
  level: string
  read_at: string | null
  created_at: string
}

/** The paged envelope every list endpoint returns. */
export interface Page<T> {
  items: T[]
  total: number
  page: number
  limit: number
  pages: number
  /** Ticket lists also carry a per-status total for the filter chips. */
  counts?: Partial<Record<TicketStatus, number>>
  extra?: Record<string, unknown>
}

export interface StatusMeta {
  value: TicketStatus
  label: string
  terminal: boolean
  next: TicketStatus[]
  /** True while the ticket still counts as live work awaiting closure. */
  open_bucket: boolean
}

export interface PriorityMeta {
  value: Priority
  label: string
  rank: number
  escalate: boolean
}

export interface Meta {
  roles: { id: number; name: string; description: string }[]
  statuses: StatusMeta[]
  priorities: PriorityMeta[]
  categories: string[]
  categories_in_use: string[]
  policies: SLAPolicy[]
  sort_fields: string[]
  assignment_policy: string
  /** Whether the advisory AI assistant is switched on for this deployment. */
  ai?: { enabled: boolean; prompt_version?: string }
  /**
   * True when the backend was started with -demo and seeded the sample accounts.
   * The sign-in screen offers them only then. The password is deliberately not
   * published here: /meta is public, and serving a credential over an
   * unauthenticated endpoint would be a footgun even for a fixed dev value.
   */
  demo: boolean
  /**
   * The usernames the demo seeder created, for the sign-in shortcuts. Sourced
   * from the seeder rather than hardcoded in this client, so a chip can never
   * name an account that does not exist. Empty when `demo` is false.
   */
  demo_accounts: DemoAccount[]
  sla: { unit: string; scan_interval: string; warn_before: string }
}

/** One seeded account published by GET /meta. */
export interface DemoAccount {
  username: string
  full_name: string
  role: string
}

/** Query parameters accepted by GET /tickets and GET /tickets/search. */
export interface TicketQuery {
  q?: string
  status?: TicketStatus[]
  priority?: Priority[]
  category?: string[]
  assignee?: number | 'me' | 'none'
  requester?: number | 'me'
  breached?: boolean
  escalated?: boolean
  open?: boolean
  created_from?: string
  created_to?: string
  resolved_from?: string
  resolved_to?: string
  sort_by?: string
  sort_dir?: 'asc' | 'desc'
  page?: number
  limit?: number
}

export interface CreateTicketInput {
  title: string
  description: string
  category: string
  priority: Priority
  assignee_id?: number | null
}

export interface UpdateTicketInput {
  status?: TicketStatus
  priority?: Priority
  category?: string
  assignee_id?: number
  unassign?: boolean
  resolution_notes?: string
}

// --- AI triage (advisory only) ---

export interface TriageSuggestion {
  value: string
  confidence: number
  rationale: string
}

export interface TriageDuplicate {
  ticket_id: number
  ticket_number: string
  title: string
  snippet: string
  score: number
  score_kind: string
}

/** POST /ai/triage. Degraded results are empty rather than an error. */
export interface TriageResult {
  suggestions: {
    category?: TriageSuggestion
    priority?: TriageSuggestion
  }
  duplicates: TriageDuplicate[]
  model: string
  prompt_version: string
  cached: boolean
  degraded?: boolean
}

export type AISuggestionKind = 'category' | 'priority' | 'duplicate'

export interface AICitation {
  ticket_id: number
  ticket_number: string
}

export interface AISuggestion {
  id: number
  job_id: number
  ticket_id: number | null
  kind: AISuggestionKind
  value: string
  confidence: number
  rationale: string
  citations?: AICitation[]
  model: string
  prompt_version: string
  accepted_at: string | null
  rejected_at: string | null
  created_at: string
}