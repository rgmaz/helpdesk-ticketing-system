package api

import (
	"net/http"
	"strconv"
	"strings"

	"github.com/ryan/ticketing-system/backend/internal/httpx"
	"github.com/ryan/ticketing-system/backend/internal/models"
	"github.com/ryan/ticketing-system/backend/internal/notify"
	"github.com/ryan/ticketing-system/backend/internal/service"
	"github.com/ryan/ticketing-system/backend/internal/store"
)

// handleListPolicies returns the SLA policy matrix.
//
// It is readable by any authenticated user so the end-user portal can show the
// commitment attached to their ticket's priority, but only the targets and
// routing flags are meaningful to non-admins; that is acceptable because the
// data is policy configuration, not personal information.
func (s *Server) handleListPolicies(w http.ResponseWriter, r *http.Request) {
	activeOnly := true
	if v, present := httpx.QueryBool(r, "active_only"); present {
		activeOnly = v
	}
	policies, err := s.store.SLA.All(r.Context(), activeOnly)
	if err != nil {
		httpx.Fail(w, err)
		return
	}
	httpx.OK(w, httpx.ListBody{
		Items: policies, Total: len(policies), Page: 1, Limit: len(policies), Pages: 1,
		Extra: map[string]any{
			"note": "targets are stored and displayed in minutes",
		},
	})
}

type updatePolicyRequest struct {
	Name              *string `json:"name"`
	Description       *string `json:"description"`
	ResponseMinutes   *int    `json:"response_minutes"`
	ResolutionMinutes *int    `json:"resolution_minutes"`
	EscalationMinutes *int    `json:"escalation_minutes"`
	AutoAssign        *bool   `json:"auto_assign"`
	Active            *bool   `json:"active"`
}

// handleUpdatePolicy edits an SLA policy (admin only, by route).
//
// Changing a policy does not retroactively re-stamp existing tickets: the
// commitment given to a requester at filing time is the one that binds them.
// New tickets and priority changes pick up the revised targets.
func (s *Server) handleUpdatePolicy(w http.ResponseWriter, r *http.Request) {
	actor, err := actorOf(r)
	if err != nil {
		httpx.Fail(w, err)
		return
	}
	id, err := httpx.PathInt64(r, "policy_id")
	if err != nil {
		httpx.Fail(w, err)
		return
	}
	var body updatePolicyRequest
	if err := httpx.DecodeJSON(w, r, &body); err != nil {
		httpx.Fail(w, err)
		return
	}

	existing, err := s.store.SLA.ByID(r.Context(), id)
	if err != nil {
		httpx.Fail(w, err)
		return
	}

	var in store.UpdatePolicy
	if body.Name != nil {
		name := strings.TrimSpace(*body.Name)
		if name == "" || len(name) > 80 {
			httpx.Error(w, http.StatusBadRequest, "name must be between 1 and 80 characters", "invalid_input")
			return
		}
		in.Name = &name
	}
	if body.Description != nil {
		desc := strings.TrimSpace(*body.Description)
		if len(desc) > 500 {
			httpx.Error(w, http.StatusBadRequest, "description must be at most 500 characters", "invalid_input")
			return
		}
		in.Description = &desc
	}
	if body.ResponseMinutes != nil {
		if err := validateMinutes(*body.ResponseMinutes, "response_minutes", 1, 60*24*30); err != nil {
			httpx.Error(w, http.StatusBadRequest, err.Error(), "invalid_input")
			return
		}
		in.ResponseMinutes = body.ResponseMinutes
	}
	if body.ResolutionMinutes != nil {
		if err := validateMinutes(*body.ResolutionMinutes, "resolution_minutes", 1, 60*24*365); err != nil {
			httpx.Error(w, http.StatusBadRequest, err.Error(), "invalid_input")
			return
		}
		in.ResolutionMinutes = body.ResolutionMinutes
	}
	// A response target longer than the resolution target is always a
	// configuration mistake, so reject it regardless of which field was edited.
	resp := existing.ResponseMinutes
	if in.ResponseMinutes != nil {
		resp = *in.ResponseMinutes
	}
	res := existing.ResolutionMinutes
	if in.ResolutionMinutes != nil {
		res = *in.ResolutionMinutes
	}
	if resp > res {
		httpx.Error(w, http.StatusBadRequest,
			"response_minutes must not exceed resolution_minutes", "invalid_input")
		return
	}
	if body.EscalationMinutes != nil {
		if *body.EscalationMinutes < 0 || *body.EscalationMinutes > 60*24*30 {
			httpx.Error(w, http.StatusBadRequest,
				"escalation_minutes must be between 0 and "+strconv.Itoa(60*24*30), "invalid_input")
			return
		}
		in.EscalationMinutes = body.EscalationMinutes
	}
	in.AutoAssign = body.AutoAssign
	in.Active = body.Active

	if err := s.store.SLA.Update(r.Context(), id, in); err != nil {
		httpx.Fail(w, err)
		return
	}
	updated, err := s.store.SLA.ByID(r.Context(), id)
	if err != nil {
		httpx.Fail(w, err)
		return
	}

	_ = s.store.Audit.Log(r.Context(), store.AuditEntry{
		ActorID:   &actor.ID,
		Action:    "sla.policy_update",
		FromValue: strPtrf("resolution %dm", existing.ResolutionMinutes),
		ToValue:   strPtrf("resolution %dm", updated.ResolutionMinutes),
		Details:   strPtrf("policy %q updated", updated.Name),
	})

	httpx.OK(w, map[string]any{"policy": updated})
}

func validateMinutes(v int, field string, min, max int) error {
	if v < min || v > max {
		return &minutesError{field: field, min: min, max: max}
	}
	return nil
}

// minutesError is a small typed validation failure so the handler can render a
// precise message without string matching.
type minutesError struct {
	field    string
	min, max int
}

func (e *minutesError) Error() string {
	return e.field + " must be between " + strconv.Itoa(e.min) + " and " + strconv.Itoa(e.max)
}

// handleSLAEngineStats reports the background worker's health (staff only).
func (s *Server) handleSLAEngineStats(w http.ResponseWriter, r *http.Request) {
	stats := s.sla.Stats()
	total, staffClients := s.hub.Stats()
	httpx.OK(w, map[string]any{
		"engine": stats,
		"stream": map[string]any{
			"connected_clients": total,
			"staff_clients":     staffClients,
			"keepalive_seconds": int(notify.KeepAliveInterval.Seconds()),
		},
	})
}

// handleMeta exposes the vocabulary the UI needs to render forms and filters.
//
// Shipping this from the server keeps the frontend from hard-coding enum lists
// that could drift from the backend validation. It is deliberately public: none
// of it is sensitive, and fetching it once at boot lets the UI build its
// dropdowns from the server's own rules.
func (s *Server) handleMeta(w http.ResponseWriter, r *http.Request) {
	roles, err := s.store.Users.Roles(r.Context())
	if err != nil {
		httpx.Fail(w, err)
		return
	}
	// The category list comes from the canonical set, not from the data: the
	// "new ticket" form must offer every category even before the first ticket
	// in that category exists. Categories actually in use are reported
	// separately for the filter dropdown.
	inUse, err := s.store.Tickets.Categories(r.Context())
	if err != nil {
		httpx.Fail(w, err)
		return
	}
	policies, err := s.store.SLA.All(r.Context(), true)
	if err != nil {
		httpx.Fail(w, err)
		return
	}

	statuses := make([]map[string]any, 0, len(models.AllStatuses))
	for _, st := range models.AllStatuses {
		statuses = append(statuses, map[string]any{
			"value":       st,
			"label":       st.Label(),
			"terminal":    st.IsTerminal(),
			"next":        st.NextStatuses(),
			"open_bucket": st.IsOpen(),
		})
	}
	priorities := make([]map[string]any, 0, len(models.AllPriorities))
	for _, p := range models.AllPriorities {
		priorities = append(priorities, map[string]any{
			"value":    p,
			"label":    p.Label(),
			"rank":     p.Rank(),
			"escalate": p >= models.PriorityHigh,
		})
	}

	httpx.OK(w, map[string]any{
		"roles":             roles,
		"statuses":          statuses,
		"priorities":        priorities,
		"categories":        service.Categories,
		"categories_in_use": inUse,
		"policies":          policies,
		// Sourced from the store's own whitelist, so a column added there is
		// offered here automatically instead of becoming a 400 for the user.
		"sort_fields":       store.SortFields(),
		"assignment_policy": "least active ticket load; high and critical tickets prefer technicians with fewer overdue tickets; ties break on the lowest user id",
		"sla": map[string]any{
			"unit":          "minutes",
			"scan_interval": s.cfg.SLAScanInterval.String(),
			"warn_before":   s.cfg.SLAWarnBefore.String(),
		},
	})
}
