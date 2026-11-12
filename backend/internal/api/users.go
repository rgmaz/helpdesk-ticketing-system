package api

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"sort"
	"strings"

	"github.com/ryan/ticketing-system/backend/internal/auth"
	"github.com/ryan/ticketing-system/backend/internal/httpx"
	"github.com/ryan/ticketing-system/backend/internal/models"
	"github.com/ryan/ticketing-system/backend/internal/service"
	"github.com/ryan/ticketing-system/backend/internal/store"
)

// directoryRow is a user plus their live workload, used by the directory view.
type directoryRow struct {
	userResponse
	ActiveTickets   int `json:"active_tickets"`
	OverdueTickets  int `json:"overdue_tickets"`
	ResolvedTickets int `json:"resolved_30d"`
	// IsLastAdmin marks the only active administrator. The UI uses it to omit
	// actions the server would refuse, such as deactivating the last admin.
	IsLastAdmin bool `json:"is_last_admin"`
}

// handleListUsers returns the account directory.
//
// The workload report is a single grouped query, so the whole page is enriched
// with one extra round-trip rather than one per user.
func (s *Server) handleListUsers(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()

	limit := httpx.QueryInt(r, "limit", 50, 1, 200)
	page := httpx.QueryInt(r, "page", 0, 0, 10000)

	var f store.UserFilter
	if raw := strings.TrimSpace(q.Get("role")); raw != "" {
		role, err := models.ParseRole(raw)
		if err != nil {
			httpx.Error(w, http.StatusBadRequest, err.Error(), "invalid_role")
			return
		}
		f.Role = role
	}
	if active, present := httpx.QueryBool(r, "active"); present {
		f.Active = &active
	}
	f.Search = strings.TrimSpace(q.Get("q"))
	f.SortBy = strings.TrimSpace(q.Get("sort_by"))
	f.SortDir = strings.TrimSpace(q.Get("sort_dir"))
	f.Page, f.Limit = page, limit

	users, total, err := s.store.Users.List(r.Context(), f)
	if err != nil {
		httpx.Fail(w, err)
		return
	}
	workload, err := s.store.Analytics.Workload(r.Context())
	if err != nil {
		httpx.Fail(w, err)
		return
	}

	// Count active administrators once so each row can advertise whether it is
	// the last one. The server refuses to demote or deactivate that account, so
	// the directory hides those controls rather than offering an action that can
	// only fail.
	active := true
	_, activeAdmins, err := s.store.Users.List(r.Context(), store.UserFilter{
		Role: models.RoleAdmin, Active: &active, Limit: 1,
	})
	if err != nil {
		httpx.Fail(w, err)
		return
	}

	byID := make(map[int64]models.TechnicianWorkload, len(workload))
	for _, w := range workload {
		byID[w.UserID] = w
	}

	rows := make([]directoryRow, 0, len(users))
	for _, u := range users {
		w := byID[u.ID]
		rows = append(rows, directoryRow{
			userResponse:    toUserResponse(u),
			ActiveTickets:   w.ActiveTickets,
			OverdueTickets:  w.OverdueTickets,
			ResolvedTickets: w.Resolved30d,
			IsLastAdmin:     u.Role == models.RoleAdmin && u.Active && activeAdmins <= 1,
		})
	}

	httpx.OK(w, httpx.ListBody{
		Items: rows, Total: total, Page: page + 1, Limit: limit,
		Pages: httpx.Pages(total, limit),
	})
}

// handleTechnicians returns assignable staff ordered by least active load, which
// is the same order the automatic router picks from.
func (s *Server) handleTechnicians(w http.ResponseWriter, r *http.Request) {
	users, err := s.store.Users.Technicians(r.Context())
	if err != nil {
		httpx.Fail(w, err)
		return
	}
	load, err := s.store.Tickets.ActiveLoad(r.Context())
	if err != nil {
		httpx.Fail(w, err)
		return
	}

	out := make([]directoryRow, 0, len(users))
	for _, u := range users {
		out = append(out, directoryRow{userResponse: toUserResponse(u), ActiveTickets: load[u.ID]})
	}
	sort.SliceStable(out, func(i, j int) bool {
		if out[i].ActiveTickets != out[j].ActiveTickets {
			return out[i].ActiveTickets < out[j].ActiveTickets
		}
		return out[i].ID < out[j].ID
	})

	httpx.OK(w, httpx.ListBody{
		Items: out, Total: len(out), Page: 1, Limit: len(out), Pages: 1,
		Extra: map[string]any{"sorted_by": "least_active_load"},
	})
}

type createUserRequest struct {
	Username string `json:"username"`
	Email    string `json:"email"`
	Password string `json:"password"`
	FullName string `json:"full_name"`
	Role     string `json:"role"`
	Active   *bool  `json:"active"`
}

// handleCreateUser provisions an account of any role (admin only, by route).
func (s *Server) handleCreateUser(w http.ResponseWriter, r *http.Request) {
	actor, err := actorOf(r)
	if err != nil {
		httpx.Fail(w, err)
		return
	}
	var body createUserRequest
	if err := httpx.DecodeJSON(w, r, &body); err != nil {
		httpx.Fail(w, err)
		return
	}

	body.Username = strings.ToLower(strings.TrimSpace(body.Username))
	body.Email = strings.ToLower(strings.TrimSpace(body.Email))
	body.FullName = strings.TrimSpace(body.FullName)

	if err := validateUsername(body.Username); err != nil {
		httpx.Error(w, http.StatusBadRequest, err.Error(), "invalid_input")
		return
	}
	if err := validateEmail(body.Email); err != nil {
		httpx.Error(w, http.StatusBadRequest, err.Error(), "invalid_input")
		return
	}
	if err := auth.ValidatePassword(body.Password); err != nil {
		httpx.Error(w, http.StatusBadRequest, err.Error(), "weak_password")
		return
	}
	if len(body.FullName) > 120 {
		httpx.Error(w, http.StatusBadRequest, "full_name must be at most 120 characters", "invalid_input")
		return
	}
	// An omitted role means the least-privileged option. Admin and technician
	// accounts must be requested explicitly.
	role := models.RoleUser
	if raw := strings.TrimSpace(body.Role); raw != "" {
		parsed, err := models.ParseRole(raw)
		if err != nil {
			httpx.Error(w, http.StatusBadRequest, err.Error(), "invalid_role")
			return
		}
		role = parsed
	}
	hash, err := auth.HashPassword(body.Password)
	if err != nil {
		httpx.Fail(w, err)
		return
	}
	active := true
	if body.Active != nil {
		active = *body.Active
	}

	user, err := s.store.Users.Create(r.Context(), store.NewUser{
		Username: body.Username, Email: body.Email, PasswordHash: hash,
		FullName: body.FullName, Role: role, Active: active,
	})
	if err != nil {
		httpx.Fail(w, err)
		return
	}
	_ = s.store.Audit.Log(r.Context(), store.AuditEntry{
		ActorID: &actor.ID, Action: "user.create",
		ToValue: strPtr(string(role)),
		Details: strPtrf("created %s account %s", role, user.Username),
	})

	httpx.Created(w, map[string]any{"user": toUserResponse(user)})
}

type updateUserRequest struct {
	FullName        *string `json:"full_name"`
	Email           *string `json:"email"`
	Password        *string `json:"password"`
	Role            *string `json:"role"`
	Active          *bool   `json:"active"`
	CurrentPassword *string `json:"current_password"`
}

// handleUpdateUser edits an account.
func (s *Server) handleUpdateUser(w http.ResponseWriter, r *http.Request) {
	actor, err := actorOf(r)
	if err != nil {
		httpx.Fail(w, err)
		return
	}
	id, err := httpx.PathInt64(r, "user_id")
	if err != nil {
		httpx.Fail(w, err)
		return
	}
	var body updateUserRequest
	if err := httpx.DecodeJSON(w, r, &body); err != nil {
		httpx.Fail(w, err)
		return
	}

	target, err := s.store.Users.ByID(r.Context(), id)
	if err != nil {
		httpx.Fail(w, err)
		return
	}

	var in store.UpdateUser

	if body.FullName != nil {
		name := strings.TrimSpace(*body.FullName)
		if len(name) > 120 {
			httpx.Error(w, http.StatusBadRequest, "full_name must be at most 120 characters", "invalid_input")
			return
		}
		in.FullName = &name
	}
	if body.Email != nil {
		email := strings.ToLower(strings.TrimSpace(*body.Email))
		if err := validateEmail(email); err != nil {
			httpx.Error(w, http.StatusBadRequest, err.Error(), "invalid_input")
			return
		}
		in.Email = &email
	}
	if body.Password != nil {
		if err := s.authorisePasswordChange(actor, target, body.CurrentPassword); err != nil {
			httpx.Fail(w, err)
			return
		}
		hash, err := auth.HashPassword(*body.Password)
		if err != nil {
			if errors.Is(err, auth.ErrPasswordTooShort) || errors.Is(err, auth.ErrPasswordWeak) {
				httpx.Error(w, http.StatusBadRequest, err.Error(), "weak_password")
				return
			}
			httpx.Fail(w, err)
			return
		}
		in.PasswordHash = &hash
	}
	if body.Role != nil {
		role, err := models.ParseRole(*body.Role)
		if err != nil {
			httpx.Error(w, http.StatusBadRequest, err.Error(), "invalid_role")
			return
		}
		if !actor.Role.IsAdmin() {
			httpx.Fail(w, fmt.Errorf("%w: only administrators can change roles", models.ErrForbidden))
			return
		}
		// Removing the last administrator would lock the system out of its own
		// admin surfaces, so it is refused.
		if target.Role == models.RoleAdmin && role != models.RoleAdmin {
			if err := s.assertNotLastAdmin(r.Context(), target.ID); err != nil {
				httpx.Fail(w, err)
				return
			}
		}
		in.Role = &role
	}
	if body.Active != nil {
		if !actor.Role.IsAdmin() {
			httpx.Fail(w, fmt.Errorf("%w: only administrators can activate or deactivate accounts", models.ErrForbidden))
			return
		}
		if !*body.Active && target.ID == actor.ID {
			httpx.Error(w, http.StatusBadRequest, "you cannot deactivate your own account", "invalid_input")
			return
		}
		if !*body.Active && target.Role == models.RoleAdmin {
			if err := s.assertNotLastAdmin(r.Context(), target.ID); err != nil {
				httpx.Fail(w, err)
				return
			}
		}
		in.Active = body.Active
	}

	updated, err := s.store.Users.Update(r.Context(), id, in)
	if err != nil {
		httpx.Fail(w, err)
		return
	}
	_ = s.store.Audit.Log(r.Context(), store.AuditEntry{
		ActorID: &actor.ID, Action: "user.update",
		Details: strPtrf("updated account %s", updated.Username),
	})

	httpx.OK(w, map[string]any{"user": toUserResponse(updated)})
}

// authorisePasswordChange requires the current password unless an administrator
// is resetting someone else's password.
func (s *Server) authorisePasswordChange(actor service.Actor, target models.User, current *string) error {
	if actor.Role.IsAdmin() && actor.ID != target.ID {
		return nil
	}
	if current == nil || *current == "" {
		return fmt.Errorf("%w: current_password is required when changing your own password", models.ErrInvalidInput)
	}
	if !auth.VerifyPassword(target.PasswordHash, *current) {
		return fmt.Errorf("%w: current password is incorrect", models.ErrUnauthorized)
	}
	return nil
}

// assertNotLastAdmin fails when the only active administrator is id.
//
// Without this guard an admin could demote or deactivate the sole account with
// admin rights and permanently lock everyone out of the management screens.
func (s *Server) assertNotLastAdmin(ctx context.Context, id int64) error {
	active := true
	admins, _, err := s.store.Users.List(ctx, store.UserFilter{
		Role:   models.RoleAdmin,
		Active: &active,
		Limit:  200,
	})
	if err != nil {
		return err
	}
	if len(admins) <= 1 {
		return fmt.Errorf("%w: at least one active administrator is required", models.ErrInvalidInput)
	}
	return nil
}

// handleDeleteUser removes an account (admin only, by route).
func (s *Server) handleDeleteUser(w http.ResponseWriter, r *http.Request) {
	actor, err := actorOf(r)
	if err != nil {
		httpx.Fail(w, err)
		return
	}
	id, err := httpx.PathInt64(r, "user_id")
	if err != nil {
		httpx.Fail(w, err)
		return
	}
	if id == actor.ID {
		httpx.Error(w, http.StatusBadRequest, "you cannot delete your own account", "invalid_input")
		return
	}
	target, err := s.store.Users.ByID(r.Context(), id)
	if err != nil {
		httpx.Fail(w, err)
		return
	}
	if target.Role == models.RoleAdmin {
		if err := s.assertNotLastAdmin(r.Context(), id); err != nil {
			httpx.Fail(w, err)
			return
		}
	}
	if err := s.store.Users.Delete(r.Context(), id); err != nil {
		httpx.Fail(w, err)
		return
	}
	_ = s.store.Audit.Log(r.Context(), store.AuditEntry{
		ActorID: &actor.ID, Action: "user.delete",
		Details: strPtrf("deleted account %s", target.Username),
	})
	httpx.NoContent(w)
}
