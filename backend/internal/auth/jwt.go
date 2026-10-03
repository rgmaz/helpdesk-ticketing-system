package auth

import (
	"context"
	"fmt"
	"time"

	"github.com/golang-jwt/jwt/v5"

	"github.com/ryan/ticketing-system/backend/internal/models"
)

// ErrInvalidToken is returned for any signature, algorithm, expiry or type
// failure. It wraps models.ErrUnauthorized so the shared HTTP error mapper
// classifies it as a 401 without this package having to be imported there.
var ErrInvalidToken = fmt.Errorf("%w: invalid or expired token", models.ErrUnauthorized)

// Claims is the JWT payload. It carries the user id and role so the RBAC
// middleware can authorize without a database round-trip on every request.
type Claims struct {
	UserID    int64           `json:"uid"`
	Username  string          `json:"usr"`
	Role      models.RoleName `json:"rol"`
	TokenType string          `json:"typ"`
	jwt.RegisteredClaims
}

// Manager issues and verifies access tokens.
type Manager struct {
	secret []byte
	ttl    time.Duration
	issuer string
	active ActiveChecker
}

// ActiveChecker reports whether an account is still enabled.
//
// It is a narrow seam rather than a full repository dependency: the token
// manager only needs one boolean per request, and keeping it abstract stops this
// package from importing the storage layer.
type ActiveChecker func(ctx context.Context, userID int64) (bool, error)

// SetActiveChecker installs the account-status check.
//
// Without one, a deactivated account keeps working until its token expires,
// which is unacceptable for offboarding. It is optional so the manager stays
// usable in unit tests that never touch a database.
func (m *Manager) SetActiveChecker(fn ActiveChecker) { m.active = fn }

// NewManager builds a token manager.
func NewManager(secret string, ttl time.Duration) *Manager {
	return &Manager{secret: []byte(secret), ttl: ttl, issuer: "itsm-api"}
}

// TTL exposes the configured access-token lifetime so the login response can
// tell the client when to refresh.
func (m *Manager) TTL() time.Duration { return m.ttl }

// Issue returns a signed access token for the user plus its expiry.
func (m *Manager) Issue(u models.User) (string, time.Time, error) {
	now := time.Now()
	exp := now.Add(m.ttl)
	claims := Claims{
		UserID:    u.ID,
		Username:  u.Username,
		Role:      u.Role,
		TokenType: "access",
		RegisteredClaims: jwt.RegisteredClaims{
			Issuer:    m.issuer,
			Subject:   fmt.Sprintf("%d", u.ID),
			IssuedAt:  jwt.NewNumericDate(now),
			NotBefore: jwt.NewNumericDate(now.Add(-30 * time.Second)),
			ExpiresAt: jwt.NewNumericDate(exp),
			ID:        fmt.Sprintf("%d-%d", u.ID, now.UnixNano()),
		},
	}
	signed, err := jwt.NewWithClaims(jwt.SigningMethodHS256, claims).SignedString(m.secret)
	if err != nil {
		return "", time.Time{}, fmt.Errorf("sign token: %w", err)
	}
	return signed, exp, nil
}

// Parse validates the signature, algorithm and expiry, then returns the claims.
func (m *Manager) Parse(tokenStr string) (*Claims, error) {
	token, err := jwt.ParseWithClaims(tokenStr, &Claims{}, func(t *jwt.Token) (any, error) {
		// Pin the algorithm to prevent alg=none / algorithm-confusion attacks.
		if _, ok := t.Method.(*jwt.SigningMethodHMAC); !ok {
			return nil, fmt.Errorf("unexpected signing method %v", t.Header["alg"])
		}
		return m.secret, nil
	}, jwt.WithIssuer(m.issuer), jwt.WithValidMethods([]string{jwt.SigningMethodHS256.Alg()}))
	if err != nil {
		return nil, fmt.Errorf("%w: %s", ErrInvalidToken, err)
	}
	claims, ok := token.Claims.(*Claims)
	if !ok || !token.Valid {
		return nil, ErrInvalidToken
	}
	if claims.TokenType != "access" {
		return nil, fmt.Errorf("%w: unexpected token type %q", ErrInvalidToken, claims.TokenType)
	}
	return claims, nil
}
