package api

import (
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/ryan/ticketing-system/backend/internal/httpx"
)

// defaultWindowDays bounds the trailing reporting window.
const defaultWindowDays = 30

// windowDays reads and clamps the ?window_days parameter.
//
// A wide window keeps the aggregation cheap; a narrow one keeps the reports
// responsive as the dataset grows.
func windowDays(r *http.Request) int {
	return httpx.QueryInt(r, "window_days", defaultWindowDays, 1, 365)
}

// handleDashboard returns every aggregate the admin dashboard renders, in one
// response.
//
// The charts are separate endpoints so a caller can refresh just one panel; this
// combined shape exists for the initial page load, where one round-trip beats
// eight.
func (s *Server) handleDashboard(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	window := windowDays(r)
	atRisk := httpx.QueryInt(r, "at_risk_minutes", 60, 1, 60*24*30)

	stats, err := s.store.Analytics.Dashboard(ctx, atRisk)
	if err != nil {
		httpx.Fail(w, err)
		return
	}
	mttr, err := s.store.Analytics.MTTR(ctx, window)
	if err != nil {
		httpx.Fail(w, err)
		return
	}
	throughput, err := s.store.Analytics.Throughput(ctx, window)
	if err != nil {
		httpx.Fail(w, err)
		return
	}
	workload, err := s.store.Analytics.Workload(ctx)
	if err != nil {
		httpx.Fail(w, err)
		return
	}
	byCategory, err := s.store.Analytics.ByCategory(ctx, window)
	if err != nil {
		httpx.Fail(w, err)
		return
	}
	byPriority, err := s.store.Analytics.ByPriority(ctx, window)
	if err != nil {
		httpx.Fail(w, err)
		return
	}
	distribution, err := s.store.Analytics.StatusDistribution(ctx)
	if err != nil {
		httpx.Fail(w, err)
		return
	}
	histogram, err := s.store.Analytics.ResponseHistogram(ctx, window)
	if err != nil {
		httpx.Fail(w, err)
		return
	}
	engine := s.sla.Stats()

	httpx.OK(w, map[string]any{
		"stats":               stats,
		"mttr":                mttr,
		"throughput":          throughput,
		"workload":            workload,
		"by_category":         byCategory,
		"by_priority":         byPriority,
		"status_distribution": distribution,
		"response_histogram":  histogram,
		"sla_engine":          engine,
		"generated_at":        time.Now().UTC().Format(time.RFC3339),
		"window_days":         window,
		"at_risk_minutes":     atRisk,
	})
}

// handleMTTR reports mean time to resolution.
//
// This is the average of (resolved_at - created_at) over tickets resolved in the
// window. The median and percentiles are included because an average alone
// hides the effect of a few very long tickets.
func (s *Server) handleMTTR(w http.ResponseWriter, r *http.Request) {
	result, err := s.store.Analytics.MTTR(r.Context(), windowDays(r))
	if err != nil {
		httpx.Fail(w, err)
		return
	}
	httpx.OK(w, map[string]any{
		"mttr":       result,
		"definition": "mean time to resolution = average of (resolved_at - created_at) across tickets resolved in the window",
	})
}

// handleThroughput reports created versus resolved volume per day.
func (s *Server) handleThroughput(w http.ResponseWriter, r *http.Request) {
	buckets, err := s.store.Analytics.Throughput(r.Context(), windowDays(r))
	if err != nil {
		httpx.Fail(w, err)
		return
	}
	// Backlog delta is what turns two counts into a trend the UI can plot.
	created, resolved := 0, 0
	for _, b := range buckets {
		created += b.Created
		resolved += b.Resolved
	}
	httpx.OK(w, map[string]any{
		"buckets":        buckets,
		"window_days":    windowDays(r),
		"total_created":  created,
		"total_resolved": resolved,
		"backlog_delta":  created - resolved,
	})
}

// handleWorkload reports active tickets per technician. This is the report the
// assignment router is measured against.
func (s *Server) handleWorkload(w http.ResponseWriter, r *http.Request) {
	workload, err := s.store.Analytics.Workload(r.Context())
	if err != nil {
		httpx.Fail(w, err)
		return
	}

	total, overdue := 0, 0
	max := 0
	for _, w := range workload {
		total += w.ActiveTickets
		overdue += w.OverdueTickets
		if w.ActiveTickets > max {
			max = w.ActiveTickets
		}
	}
	avg := 0.0
	if len(workload) > 0 {
		avg = float64(total) / float64(len(workload))
	}

	httpx.OK(w, map[string]any{
		"technicians":         workload,
		"active_total":        total,
		"overdue_total":       overdue,
		"busiest":             max,
		"average_active_load": avg,
	})
}

// handleByCategory breaks volume down by request type.
func (s *Server) handleByCategory(w http.ResponseWriter, r *http.Request) {
	rows, err := s.store.Analytics.ByCategory(r.Context(), windowDays(r))
	if err != nil {
		httpx.Fail(w, err)
		return
	}
	httpx.OK(w, map[string]any{
		"categories":  rows,
		"window_days": windowDays(r),
	})
}

// handleByPriority breaks volume down by severity.
func (s *Server) handleByPriority(w http.ResponseWriter, r *http.Request) {
	rows, err := s.store.Analytics.ByPriority(r.Context(), windowDays(r))
	if err != nil {
		httpx.Fail(w, err)
		return
	}
	httpx.OK(w, map[string]any{
		"priorities":  rows,
		"window_days": windowDays(r),
	})
}

// handleResponseTimes returns the first-response histogram.
func (s *Server) handleResponseTimes(w http.ResponseWriter, r *http.Request) {
	buckets, err := s.store.Analytics.ResponseHistogram(r.Context(), windowDays(r))
	if err != nil {
		httpx.Fail(w, err)
		return
	}
	httpx.OK(w, map[string]any{
		"buckets":     buckets,
		"window_days": windowDays(r),
	})
}

// handleAudit returns the audit trail (admin only, by route).
func (s *Server) handleAudit(w http.ResponseWriter, r *http.Request) {
	limit := httpx.QueryInt(r, "limit", 100, 1, 500)
	page := httpx.QueryInt(r, "page", 0, 0, 10000)

	// Optional ticket filter, so an admin can drill into one ticket's history.
	var ticketID *int64
	if raw := strings.TrimSpace(r.URL.Query().Get("ticket_id")); raw != "" {
		id, err := strconv.ParseInt(raw, 10, 64)
		if err != nil || id <= 0 {
			httpx.Error(w, http.StatusBadRequest, "ticket_id must be a positive integer", "invalid_input")
			return
		}
		ticketID = &id
	}

	entries, total, err := s.store.Audit.List(r.Context(), ticketID, limit, page*limit)
	if err != nil {
		httpx.Fail(w, err)
		return
	}
	httpx.OK(w, httpx.ListBody{
		Items: entries, Total: total, Page: page + 1, Limit: limit,
		Pages: httpx.Pages(total, limit),
	})
}

// handleHealth is the liveness and readiness probe. It is public and never
// leaks configuration beyond the DB ping result, so it is safe to expose to a
// process supervisor.
func (s *Server) handleHealth(w http.ResponseWriter, r *http.Request) {
	if err := s.store.DB().PingContext(r.Context()); err != nil {
		httpx.Error(w, http.StatusServiceUnavailable, "database unavailable: "+err.Error(), "db_unavailable")
		return
	}
	uptime := time.Since(time.Unix(s.started, 0)).Round(time.Second)
	total, staff := s.hub.Stats()

	httpx.OK(w, map[string]any{
		"status":     "ok",
		"uptime":     uptime.String(),
		"db":         "connected",
		"sla_engine": s.sla.Stats(),
		"stream": map[string]any{
			"connected_clients": total,
			"staff_clients":     staff,
		},
	})
}
