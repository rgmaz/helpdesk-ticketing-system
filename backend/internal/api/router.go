package api

import (
	"net/http"
	"strings"

	"github.com/ryan/ticketing-system/backend/internal/ai"
	"github.com/ryan/ticketing-system/backend/internal/auth"
	"github.com/ryan/ticketing-system/backend/internal/config"
	"github.com/ryan/ticketing-system/backend/internal/httpx"
	"github.com/ryan/ticketing-system/backend/internal/models"
	"github.com/ryan/ticketing-system/backend/internal/notify"
	"github.com/ryan/ticketing-system/backend/internal/service"
	"github.com/ryan/ticketing-system/backend/internal/store"
)

// demoAccountsFor lists the accounts reachable with the documented demo
// password.
//
// The administrator is included only when it really does use that password: a
// deployment that set ITSM_ADMIN_PASSWORD, or that ran without -demo and got a
// generated one, would otherwise be advertised with a credential that cannot
// sign in.
func demoAccountsFor(cfg *config.Config) []service.DemoAccount {
	accounts := service.DemoAccountList(cfg.DemoData)
	if !cfg.DemoData || cfg.AdminPassword != "" {
		return accounts
	}
	// Admin first: it is the account a reviewer wants most, and it is the only
	// one that reaches the admin dashboard.
	admin := service.DemoAccount{
		Username: strings.ToLower(cfg.AdminUsername),
		FullName: "Service Desk Administrator",
		Role:     string(models.RoleAdmin),
	}
	return append([]service.DemoAccount{admin}, accounts...)
}

// Server holds the dependencies every handler needs.
type Server struct {
	cfg     *config.Config
	store   *store.Store
	hub     *notify.Hub
	tokens  *auth.Manager
	tickets *service.Tickets
	sla     *service.SLAEngine
	ai      *ai.Service
	started int64
}

// NewServer wires the API layer.
func NewServer(cfg *config.Config, s *store.Store, hub *notify.Hub, tokens *auth.Manager,
	tickets *service.Tickets, sla *service.SLAEngine, assistant *ai.Service, startedAt int64) *Server {
	return &Server{
		cfg: cfg, store: s, hub: hub, tokens: tokens,
		tickets: tickets, sla: sla, ai: assistant, started: startedAt,
	}
}

// Routes builds the complete HTTP handler: global middleware, the versioned
// API tree, and the health endpoint.
func (s *Server) Routes() http.Handler {
	mux := httpx.NewRouter()

	// --- public ---
	mux.HandleFunc("GET /health", s.handleHealth)
	mux.HandleFunc("POST /api/v1/auth/login", s.handleLogin)
	mux.HandleFunc("POST /api/v1/auth/register", s.handleRegister)
	mux.HandleFunc("GET /api/v1/meta", s.handleMeta)

	// Go 1.22 ServeMux has no per-subtree middleware, so each authenticated
	// route declares its own middleware stack via the helpers below. Grouping by
	// role keeps that readable at the route table.
	private := func(pattern string, mw []func(http.Handler) http.Handler, h http.HandlerFunc) {
		stack := append([]func(http.Handler) http.Handler{s.tokens.Authenticate()}, mw...)
		mux.Handle(pattern, httpx.Chain(h, stack...))
	}

	any := func(pattern string, h http.HandlerFunc) { private(pattern, nil, h) }
	staff := func(pattern string, h http.HandlerFunc) {
		private(pattern, []func(http.Handler) http.Handler{auth.RequireStaff()}, h)
	}
	admin := func(pattern string, h http.HandlerFunc) {
		private(pattern, []func(http.Handler) http.Handler{auth.RequireAdmin()}, h)
	}

	// Identity
	any("GET /api/v1/auth/me", s.handleMe)
	any("POST /api/v1/auth/password", s.handleChangePassword)

	// Tickets: reads are open to any authenticated user (the handler applies
	// row-level visibility); writes are restricted because an end user may
	// only file tickets for themselves.
	any("GET /api/v1/tickets", s.handleListTickets)
	any("GET /api/v1/tickets/{ticket_id}", s.handleGetTicket)
	any("GET /api/v1/tickets/{ticket_id}/comments", s.handleListComments)
	any("POST /api/v1/tickets/{ticket_id}/comments", s.handleAddComment)
	any("GET /api/v1/tickets/{ticket_id}/events", s.handleListEvents)
	any("GET /api/v1/tickets/{ticket_id}/audit", s.handleTicketAudit)
	any("GET /api/v1/tickets/search", s.handleSearchTickets)
	private("POST /api/v1/tickets", nil, s.handleCreateTicket)
	private("PATCH /api/v1/tickets/{ticket_id}", nil, s.handleUpdateTicket)
	admin("DELETE /api/v1/tickets/{ticket_id}", s.handleDeleteTicket)

	// Reference data
	any("GET /api/v1/categories", s.handleCategories)
	staff("GET /api/v1/technicians", s.handleTechnicians)
	any("GET /api/v1/sla-policies", s.handleListPolicies)
	admin("PATCH /api/v1/sla-policies/{policy_id}", s.handleUpdatePolicy)

	// Directory (staff and admins)
	staff("GET /api/v1/users", s.handleListUsers)
	admin("POST /api/v1/users", s.handleCreateUser)
	admin("PATCH /api/v1/users/{user_id}", s.handleUpdateUser)
	admin("DELETE /api/v1/users/{user_id}", s.handleDeleteUser)
	any("GET /api/v1/roles", s.handleListRoles)

	// Notifications: list/mark are per-user, the stream is the SSE endpoint.
	any("GET /api/v1/notifications", s.handleListNotifications)
	any("POST /api/v1/notifications/{notification_id}/read", s.handleMarkNotificationRead)
	any("POST /api/v1/notifications/read-all", s.handleMarkAllNotificationsRead)
	any("GET /api/v1/notifications/stream", s.handleNotificationStream)
	any("GET /api/v1/notifications/unread-count", s.handleUnreadCount)

	// Reporting (admin)
	admin("GET /api/v1/analytics/dashboard", s.handleDashboard)
	admin("GET /api/v1/analytics/mttr", s.handleMTTR)
	admin("GET /api/v1/analytics/throughput", s.handleThroughput)
	admin("GET /api/v1/analytics/workload", s.handleWorkload)
	admin("GET /api/v1/analytics/by-category", s.handleByCategory)
	admin("GET /api/v1/analytics/by-priority", s.handleByPriority)
	admin("GET /api/v1/analytics/response-times", s.handleResponseTimes)
	admin("GET /api/v1/audit", s.handleAudit)

	// System health for the SLA engine
	staff("GET /api/v1/system/sla-engine", s.handleSLAEngineStats)

	// AI triage: preflight is open to any authenticated user and heavily
	// rate-limited; the suggestion lifecycle is staff-only because accepting a
	// category, priority or duplicate is a triage action.
	any("POST /api/v1/ai/triage", s.handleTriage)
	staff("GET /api/v1/tickets/{ticket_id}/ai/suggestions", s.handleListAISuggestions)
	staff("POST /api/v1/tickets/{ticket_id}/ai/suggestions/{suggestion_id}/accept", s.handleAcceptAISuggestion)
	staff("POST /api/v1/tickets/{ticket_id}/ai/suggestions/{suggestion_id}/reject", s.handleRejectAISuggestion)
	admin("GET /api/v1/ai/health", s.handleAIHealth)

	return httpx.Chain(mux,
		httpx.SecurityHeaders,
		httpx.CORS(s.cfg.AllowedOrigin),
	)
}
