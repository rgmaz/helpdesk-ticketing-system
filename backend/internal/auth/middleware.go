package auth

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"strings"

	"github.com/ryan/ticketing-system/backend/internal/httpx"
	"github.com/ryan/ticketing-system/backend/internal/models"
)

// ctxKey is the private type for context keys in this package, preventing
// collisions with other packages' context values.
type ctxKey int

const claimsKey ctxKey = iota

// Authenticate is middleware that validates a bearer token and attaches the
// caller's claims to the request context. Requests without a valid token are
// rejected with 401.
//
// The role is read from the signed claims, so RBAC needs no database round-trip.
// The one exception is the account's active flag: a token issued before an
// offboarding must stop working immediately, not twelve hours later when it
// expires, so that single indexed lookup is performed per request.
func (m *Manager) Authenticate() func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			token, err := bearerToken(r)
			if err != nil {
				unauthorized(w, err.Error())
				return
			}
			claims, err := m.Parse(token)
			if err != nil {
				unauthorized(w, "invalid or expired access token")
				return
			}
			if m.active != nil {
				enabled, err := m.active(r.Context(), claims.UserID)
				if err != nil {
					// A database problem must not silently degrade into
					// granting access.
					slog.Error("account status check failed",
						"user_id", claims.UserID, "error", err)
					httpx.Error(w, http.StatusServiceUnavailable,
						"authentication is temporarily unavailable", "auth_unavailable")
					return
				}
				if !enabled {
					unauthorized(w, "this account has been deactivated")
					return
				}
			}
			ctx := context.WithValue(r.Context(), claimsKey, claims)
			next.ServeHTTP(w, r.WithContext(ctx))
		})
	}
}

// bearerToken extracts the token from the Authorization header. The
// ?access_token= query parameter is accepted as a fallback because the browser
// EventSource API cannot set request headers, and SSE is used for live updates.
func bearerToken(r *http.Request) (string, error) {
	if h := strings.TrimSpace(r.Header.Get("Authorization")); h != "" {
		parts := strings.Fields(h)
		if len(parts) != 2 || !strings.EqualFold(parts[0], "bearer") {
			return "", errors.New("Authorization header must use the 'Bearer <token>' scheme")
		}
		return parts[1], nil
	}
	if q := strings.TrimSpace(r.URL.Query().Get("access_token")); q != "" {
		return q, nil
	}
	return "", errors.New("missing bearer token")
}

// RequireRole returns middleware that rejects callers whose role is not in the
// allowed set. It must be chained after Authenticate.
func RequireRole(allowed ...models.RoleName) func(http.Handler) http.Handler {
	permitted := make(map[models.RoleName]bool, len(allowed))
	for _, r := range allowed {
		permitted[r] = true
	}
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			claims := FromContext(r.Context())
			if claims == nil {
				unauthorized(w, "authentication required")
				return
			}
			if !permitted[claims.Role] {
				httpx.Error(w, http.StatusForbidden,
					fmt.Sprintf("this action requires one of the following roles: %s", roleList(allowed)),
					"insufficient_role")
				return
			}
			next.ServeHTTP(w, r)
		})
	}
}

// RequireStaff is shorthand for the technician and admin roles.
func RequireStaff() func(http.Handler) http.Handler {
	return RequireRole(models.RoleTechnician, models.RoleAdmin)
}

// RequireAdmin is shorthand for the admin role only.
func RequireAdmin() func(http.Handler) http.Handler {
	return RequireRole(models.RoleAdmin)
}

func roleList(roles []models.RoleName) string {
	parts := make([]string, len(roles))
	for i, r := range roles {
		parts[i] = string(r)
	}
	return strings.Join(parts, ", ")
}

func unauthorized(w http.ResponseWriter, msg string) {
	w.Header().Set("WWW-Authenticate", `Bearer realm="itsm"`)
	httpx.Error(w, http.StatusUnauthorized, msg, "unauthorized")
}

// FromContext returns the verified claims, or nil when the request was not
// authenticated.
func FromContext(ctx context.Context) *Claims {
	c, _ := ctx.Value(claimsKey).(*Claims)
	return c
}

// MustUserID returns the authenticated user id, panicking only if used on an
// unauthenticated route (a programming error, not a runtime condition).
func MustUserID(ctx context.Context) int64 {
	c := FromContext(ctx)
	if c == nil {
		panic("auth: MustUserID called on an unauthenticated request")
	}
	return c.UserID
}

// User is the request-scoped identity assembled from the token. Role comes from
// the signed token; name and active flag are resolved from the database by the
// handler when needed so stale roles cannot grant access indefinitely.
type User struct {
	ID       int64
	Username string
	Role     models.RoleName
}

// Identity returns the caller as a User value.
func Identity(r *http.Request) (User, bool) {
	c := FromContext(r.Context())
	if c == nil {
		return User{}, false
	}
	return User{ID: c.UserID, Username: c.Username, Role: c.Role}, true
}
