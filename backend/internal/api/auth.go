package api

import (
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"net/mail"
	"strings"
	"time"

	"github.com/ryan/ticketing-system/backend/internal/auth"
	"github.com/ryan/ticketing-system/backend/internal/httpx"
	"github.com/ryan/ticketing-system/backend/internal/models"
	"github.com/ryan/ticketing-system/backend/internal/service"
	"github.com/ryan/ticketing-system/backend/internal/store"
)

// actorOf converts the request's bearer-token claims into a service actor.
func actorOf(r *http.Request) (service.Actor, error) {
	u, ok := auth.Identity(r)
	if !ok {
		return service.Actor{}, fmt.Errorf("%w: no authenticated identity on request", models.ErrUnauthorized)
	}
	return service.Actor{ID: u.ID, Username: u.Username, Role: u.Role}, nil
}

type loginRequest struct {
	Identifier string `json:"identifier"`
	Password   string `json:"password"`
}

type authResponse struct {
	Token     string       `json:"token"`
	TokenType string       `json:"token_type"`
	ExpiresAt string       `json:"expires_at"`
	ExpiresIn int          `json:"expires_in"`
	User      userResponse `json:"user"`
}

// userResponse is the public shape of an account. The password hash is never
// serialised.
type userResponse struct {
	ID        int64           `json:"id"`
	Username  string          `json:"username"`
	Email     string          `json:"email"`
	FullName  string          `json:"full_name"`
	Role      models.RoleName `json:"role"`
	Active    bool            `json:"active"`
	CreatedAt *models.Time    `json:"created_at"`
}

func toUserResponse(u models.User) userResponse {
	return userResponse{
		ID: u.ID, Username: u.Username, Email: u.Email,
		FullName: u.DisplayName(), Role: u.Role,
		Active: u.Active, CreatedAt: u.CreatedAt,
	}
}

// handleLogin exchanges credentials for an access token.
func (s *Server) handleLogin(w http.ResponseWriter, r *http.Request) {
	var req loginRequest
	if err := httpx.DecodeJSON(w, r, &req); err != nil {
		httpx.Fail(w, err)
		return
	}
	req.Identifier = strings.TrimSpace(req.Identifier)
	if req.Identifier == "" || req.Password == "" {
		httpx.Error(w, http.StatusBadRequest, "identifier and password are required", "invalid_input")
		return
	}

	user, err := s.store.Users.ByIdentifier(r.Context(), req.Identifier)
	if err != nil {
		if errors.Is(err, models.ErrNotFound) {
			// Run a dummy verification so a missing account and a wrong password
			// take a comparable amount of time.
			auth.VerifyPassword("", req.Password)
			httpx.Error(w, http.StatusUnauthorized, "invalid username or password", "invalid_credentials")
			return
		}
		httpx.Fail(w, err)
		return
	}
	if !auth.VerifyPassword(user.PasswordHash, req.Password) {
		httpx.Error(w, http.StatusUnauthorized, "invalid username or password", "invalid_credentials")
		return
	}
	if !user.Active {
		// 401, not 403: no usable credential was issued. 403 would imply the
		// caller authenticated successfully and was then refused.
		httpx.Error(w, http.StatusUnauthorized, "this account has been deactivated", "account_disabled")
		return
	}

	token, exp, err := s.tokens.Issue(user)
	if err != nil {
		httpx.Fail(w, err)
		return
	}
	slog.Info("login", "user", user.Username, "role", user.Role, "remote", clientIP(r))

	httpx.OK(w, authResponse{
		Token: token, TokenType: "Bearer",
		ExpiresAt: exp.UTC().Format(models.TimeLayout),
		ExpiresIn: int(time.Until(exp).Seconds()),
		User:      toUserResponse(user),
	})
}

type registerRequest struct {
	Username string `json:"username"`
	Email    string `json:"email"`
	Password string `json:"password"`
	FullName string `json:"full_name"`
}

// handleRegister creates an end-user account.
//
// Self-registration is intentionally restricted to the `user` role: creating a
// technician or administrator requires an authenticated admin via POST /users.
func (s *Server) handleRegister(w http.ResponseWriter, r *http.Request) {
	var req registerRequest
	if err := httpx.DecodeJSON(w, r, &req); err != nil {
		httpx.Fail(w, err)
		return
	}
	req.Username = strings.ToLower(strings.TrimSpace(req.Username))
	req.Email = strings.ToLower(strings.TrimSpace(req.Email))
	req.FullName = strings.TrimSpace(req.FullName)

	if err := validateUsername(req.Username); err != nil {
		httpx.Error(w, http.StatusBadRequest, err.Error(), "invalid_input")
		return
	}
	if err := validateEmail(req.Email); err != nil {
		httpx.Error(w, http.StatusBadRequest, err.Error(), "invalid_input")
		return
	}
	if err := auth.ValidatePassword(req.Password); err != nil {
		httpx.Error(w, http.StatusBadRequest, err.Error(), "weak_password")
		return
	}
	if len(req.FullName) > 120 {
		httpx.Error(w, http.StatusBadRequest, "full_name must be at most 120 characters", "invalid_input")
		return
	}

	hash, err := auth.HashPassword(req.Password)
	if err != nil {
		httpx.Fail(w, err)
		return
	}

	user, err := s.store.Users.Create(r.Context(), store.NewUser{
		Username: req.Username, Email: req.Email, PasswordHash: hash,
		FullName: req.FullName, Role: models.RoleUser, Active: true,
	})
	if err != nil {
		httpx.Fail(w, err)
		return
	}
	_ = s.store.Audit.Log(r.Context(), store.AuditEntry{
		ActorID: &user.ID, Action: "user.register",
		Details: strPtrf("self-registered account %s", user.Username),
	})

	httpx.Created(w, map[string]any{"user": toUserResponse(user)})
}

// handleMe returns the caller's own profile.
func (s *Server) handleMe(w http.ResponseWriter, r *http.Request) {
	actor, err := actorOf(r)
	if err != nil {
		httpx.Fail(w, err)
		return
	}
	user, err := s.store.Users.ByID(r.Context(), actor.ID)
	if err != nil {
		httpx.Fail(w, err)
		return
	}
	counts, err := s.store.Tickets.CountsByStatus(r.Context(), &user.ID)
	if err != nil {
		httpx.Fail(w, err)
		return
	}
	httpx.OK(w, map[string]any{
		"user":          toUserResponse(user),
		"ticket_counts": counts,
	})
}

type changePasswordRequest struct {
	CurrentPassword string `json:"current_password"`
	NewPassword     string `json:"new_password"`
}

// handleChangePassword rotates the caller's own password.
func (s *Server) handleChangePassword(w http.ResponseWriter, r *http.Request) {
	actor, err := actorOf(r)
	if err != nil {
		httpx.Fail(w, err)
		return
	}
	var req changePasswordRequest
	if err := httpx.DecodeJSON(w, r, &req); err != nil {
		httpx.Fail(w, err)
		return
	}

	user, err := s.store.Users.ByID(r.Context(), actor.ID)
	if err != nil {
		httpx.Fail(w, err)
		return
	}
	if !auth.VerifyPassword(user.PasswordHash, req.CurrentPassword) {
		httpx.Error(w, http.StatusUnauthorized, "current password is incorrect", "invalid_credentials")
		return
	}
	if req.CurrentPassword == req.NewPassword {
		httpx.Error(w, http.StatusBadRequest, "new password must differ from the current one", "invalid_input")
		return
	}
	hash, err := auth.HashPassword(req.NewPassword)
	if err != nil {
		if errors.Is(err, auth.ErrPasswordTooShort) || errors.Is(err, auth.ErrPasswordWeak) {
			httpx.Error(w, http.StatusBadRequest, err.Error(), "weak_password")
			return
		}
		httpx.Error(w, http.StatusBadRequest, err.Error(), "invalid_input")
		return
	}

	if _, err := s.store.Users.Update(r.Context(), user.ID, store.UpdateUser{PasswordHash: &hash}); err != nil {
		httpx.Fail(w, err)
		return
	}
	_ = s.store.Audit.Log(r.Context(), store.AuditEntry{
		ActorID: &user.ID, Action: "user.password_change",
	})
	httpx.OK(w, map[string]any{"message": "password updated"})
}

// handleListRoles exposes the role vocabulary for the UI.
func (s *Server) handleListRoles(w http.ResponseWriter, r *http.Request) {
	roles, err := s.store.Users.Roles(r.Context())
	if err != nil {
		httpx.Fail(w, err)
		return
	}
	httpx.OK(w, httpx.ListBody{Items: roles, Total: len(roles), Page: 1, Limit: len(roles), Pages: 1})
}

// --- validation helpers ---

func validateUsername(u string) error {
	if len(u) < 3 || len(u) > 32 {
		return errors.New("username must be between 3 and 32 characters")
	}
	for i, r := range u {
		switch {
		case r >= 'a' && r <= 'z', r >= '0' && r <= '9':
		case r == '.' || r == '_' || r == '-':
			if i == 0 {
				return errors.New("username must start with a letter or digit")
			}
		default:
			return errors.New("username may only contain lowercase letters, digits, dot, underscore and hyphen")
		}
	}
	return nil
}

func validateEmail(e string) error {
	if len(e) > 254 {
		return errors.New("email must be at most 254 characters")
	}
	addr, err := mail.ParseAddress(e)
	if err != nil || addr.Address != e {
		return errors.New("a valid email address is required")
	}
	return nil
}

// strPtrf is a small helper for audit Details fields, which are always built
// from a format string.
func strPtrf(format string, args ...any) *string {
	s := fmt.Sprintf(format, args...)
	return &s
}

// strPtr returns a pointer to s, for the many optional audit fields.
func strPtr(s string) *string { return &s }
