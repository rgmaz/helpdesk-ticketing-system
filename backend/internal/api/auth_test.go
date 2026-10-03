package api

import (
	"encoding/base64"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/ryan/ticketing-system/backend/internal/auth"
	"github.com/ryan/ticketing-system/backend/internal/models"
)

func TestLoginSucceedsWithUsernameOrEmail(t *testing.T) {
	h := newHarness(t)
	h.createUser("requester", models.RoleUser)

	for _, identifier := range []string{"requester", "requester@test.local", "REQUESTER@TEST.LOCAL"} {
		t.Run(identifier, func(t *testing.T) {
			res := h.do(http.MethodPost, "/api/v1/auth/login", "", map[string]string{
				"identifier": identifier,
				"password":   "Passw0rd123",
			})
			if res.Status != http.StatusOK {
				t.Fatalf("status = %d, body = %s", res.Status, res.Raw)
			}
			if res.str("token") == "" {
				t.Fatal("expected an access token in the response")
			}
			user, ok := res.Body["user"].(map[string]any)
			if !ok {
				t.Fatalf("expected a user object, got %s", res.Raw)
			}
			if user["role"] != "user" {
				t.Errorf("role = %v, want user", user["role"])
			}
			// The password hash must never leave the server.
			if _, leaked := user["password_hash"]; leaked {
				t.Error("response leaked password_hash")
			}
		})
	}
}

func TestLoginRejectsWrongPasswordAndUnknownAccount(t *testing.T) {
	h := newHarness(t)
	h.createUser("requester", models.RoleUser)

	tests := []struct {
		name       string
		identifier string
		password   string
	}{
		{"wrong password", "requester", "WrongPass123"},
		{"unknown account", "nobody", "Passw0rd123"},
		{"password as identifier", "Passw0rd123", "Passw0rd123"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			res := h.do(http.MethodPost, "/api/v1/auth/login", "", map[string]string{
				"identifier": tc.identifier, "password": tc.password,
			})
			if res.Status != http.StatusUnauthorized {
				t.Fatalf("status = %d, want 401; body = %s", res.Status, res.Raw)
			}
			// A generic message avoids confirming which accounts exist.
			if msg := res.str("error"); !strings.Contains(strings.ToLower(msg), "invalid") {
				t.Errorf("error = %q, want a generic invalid-credentials message", msg)
			}
		})
	}

	// A missing field is a client bug, not a credentials failure, so it is a
	// 400 rather than a 401.
	t.Run("missing password", func(t *testing.T) {
		res := h.do(http.MethodPost, "/api/v1/auth/login", "", map[string]string{"identifier": "requester"})
		if res.Status != http.StatusBadRequest {
			t.Fatalf("status = %d, want 400; body = %s", res.Status, res.Raw)
		}
	})
}

func TestLoginRejectsDeactivatedAccount(t *testing.T) {
	h := newHarness(t)
	user := h.createUser("dormant", models.RoleUser)

	inactive := false
	if _, err := h.store.Users.Update(t.Context(), user.ID, storeUpdateActive(inactive)); err != nil {
		t.Fatalf("deactivate user: %v", err)
	}

	res := h.do(http.MethodPost, "/api/v1/auth/login", "", map[string]string{
		"identifier": "dormant", "password": "Passw0rd123",
	})
	if res.Status != http.StatusUnauthorized {
		t.Fatalf("status = %d, want 401 for a deactivated account; body = %s", res.Status, res.Raw)
	}
}

func TestRegisterRejectsAttemptedPrivilegeEscalation(t *testing.T) {
	h := newHarness(t)

	// Strict JSON decoding means an unknown field is rejected outright, so a
	// client cannot smuggle a role into a self-registration payload.
	res := h.do(http.MethodPost, "/api/v1/auth/register", "", map[string]any{
		"username": "sneaky", "email": "sneaky@test.local",
		"password": "Str0ngPass99", "role": "admin",
	})
	if res.Status != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400 for an unknown field; body = %s", res.Status, res.Raw)
	}
}

func TestRegisterCreatesEndUserOnly(t *testing.T) {
	h := newHarness(t)

	res := h.do(http.MethodPost, "/api/v1/auth/register", "", map[string]any{
		"username":  "new.person",
		"email":     "new.person@test.local",
		"password":  "Str0ngPass99",
		"full_name": "New Person",
	})
	if res.Status != http.StatusCreated {
		t.Fatalf("status = %d, want 201; body = %s", res.Status, res.Raw)
	}
	user, ok := res.Body["user"].(map[string]any)
	if !ok {
		t.Fatalf("expected a user object, got %s", res.Raw)
	}
	if user["role"] != "user" {
		t.Errorf("role = %v, want user (self-registration cannot choose a role)", user["role"])
	}

	// And the new account can actually sign in.
	login := h.do(http.MethodPost, "/api/v1/auth/login", "", map[string]string{
		"identifier": "new.person", "password": "Str0ngPass99",
	})
	if login.Status != http.StatusOK {
		t.Errorf("login after registration = %d, want 200; body = %s", login.Status, login.Raw)
	}
}

func TestRegisterRejectsWeakPasswordAndDuplicateIdentity(t *testing.T) {
	h := newHarness(t)

	weak := h.do(http.MethodPost, "/api/v1/auth/register", "", map[string]any{
		"username": "weak.user", "email": "weak@test.local", "password": "short1",
	})
	if weak.Status != http.StatusBadRequest {
		t.Errorf("weak password: status = %d, want 400; body = %s", weak.Status, weak.Raw)
	}

	first := h.do(http.MethodPost, "/api/v1/auth/register", "", map[string]any{
		"username": "dup.user", "email": "dup@test.local", "password": "Str0ngPass99",
	})
	if first.Status != http.StatusCreated {
		t.Fatalf("first registration: status = %d, body = %s", first.Status, first.Raw)
	}

	dupe := h.do(http.MethodPost, "/api/v1/auth/register", "", map[string]any{
		"username": "dup.user", "email": "other@test.local", "password": "Str0ngPass99",
	})
	if dupe.Status != http.StatusConflict {
		t.Errorf("duplicate username: status = %d, want 409; body = %s", dupe.Status, dupe.Raw)
	}
}

func TestProtectedRoutesRequireAToken(t *testing.T) {
	h := newHarness(t)

	protected := []string{
		"/api/v1/tickets",
		"/api/v1/auth/me",
		"/api/v1/notifications",
		"/api/v1/analytics/dashboard",
	}
	for _, path := range protected {
		t.Run(path, func(t *testing.T) {
			res := h.do(http.MethodGet, path, "", nil)
			if res.Status != http.StatusUnauthorized {
				t.Fatalf("status = %d, want 401; body = %s", res.Status, res.Raw)
			}
			if res.Header.Get("WWW-Authenticate") == "" {
				t.Error("401 response should carry a WWW-Authenticate header")
			}
		})
	}
}

// TestTamperedTokenIsRejected covers the three ways a token can be forged, each
// of which must fail closed.
func TestTamperedTokenIsRejected(t *testing.T) {
	h := newHarness(t)
	h.createUser("requester", models.RoleUser)
	token := h.tokenFor("requester", models.RoleUser)

	parts := strings.Split(token, ".")
	if len(parts) != 3 {
		t.Fatalf("expected a three-part JWT, got %q", token)
	}

	cases := []struct {
		name  string
		token string
	}{
		{
			// Alter the signature itself. The first character is used rather
			// than the last because unpadded base64 leaves spare bits in the
			// final character that decode to the same bytes.
			name:  "mutated signature",
			token: flipChar(parts[0]+"."+parts[1]+"."+parts[2], len(parts[0])+len(parts[1])+2),
		},
		{
			// Swap the payload for a different identity, leaving the signature
			// untouched: the token now says "admin" but was signed for a user.
			name:  "elevated role claim",
			token: strings.Join([]string{parts[0], escalateRoleClaim(t, parts[1]), parts[2]}, "."),
		},
		{
			// A token minted by a different secret must not be accepted here.
			name:  "foreign secret",
			token: foreignToken(t),
		},
		{
			name:  "alg none",
			token: parts[0] + "." + parts[1] + ".",
		},
		{
			name:  "not a jwt",
			token: "Bearer-something-else",
		},
		{
			name:  "empty signature",
			token: parts[0] + "." + parts[1] + ".AAAA",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			res := h.do(http.MethodGet, "/api/v1/tickets", tc.token, nil)
			if res.Status != http.StatusUnauthorized {
				t.Fatalf("status = %d, want 401; body = %s", res.Status, truncate(res.Raw, 200))
			}
		})
	}
}

// flipChar replaces the byte at index i with a different base64url character.
func flipChar(s string, i int) string {
	if i >= len(s) {
		return s
	}
	replacement := byte('A')
	if s[i] == 'A' {
		replacement = 'B'
	}
	return s[:i] + string(replacement) + s[i+1:]
}

// escalateRoleClaim rewrites a payload's role field to "admin" and re-encodes it
// with its original header, producing a structurally valid token whose claims
// disagree with its signature.
func escalateRoleClaim(t *testing.T, payloadSegment string) string {
	t.Helper()
	raw, err := base64.RawURLEncoding.DecodeString(payloadSegment)
	if err != nil {
		t.Fatalf("decode payload: %v", err)
	}
	var claims map[string]any
	if err := json.Unmarshal(raw, &claims); err != nil {
		t.Fatalf("parse claims: %v", err)
	}
	if claims["rol"] != "user" {
		t.Fatalf("expected the fixture to be an end user, got role %v", claims["rol"])
	}
	claims["rol"] = "admin"
	patched, err := json.Marshal(claims)
	if err != nil {
		t.Fatalf("re-encode claims: %v", err)
	}
	return base64.RawURLEncoding.EncodeToString(patched)
}

// foreignToken mints a valid token using a different signing key.
func foreignToken(t *testing.T) string {
	t.Helper()
	other := auth.NewManager("a-completely-different-signing-secret", time.Hour)
	token, _, err := other.Issue(models.User{
		ID: 1, Username: "mallory", Role: models.RoleAdmin, Active: true,
	})
	if err != nil {
		t.Fatalf("mint foreign token: %v", err)
	}
	return token
}

// TestRoleIsReadFromTokenNotTheBody guards against privilege escalation through a
// stale or forged role claim: the role is signed into the token, so editing the
// request body cannot change it.
func TestRoleIsReadFromTokenNotTheBody(t *testing.T) {
	h := newHarness(t)
	_, _, _, userA, _ := h.seed()

	target, err := h.store.Users.ByUsername(t.Context(), userA)
	if err != nil {
		t.Fatalf("load target: %v", err)
	}
	userToken := h.tokenFor(userA, models.RoleUser)

	res := h.do(http.MethodPatch, "/api/v1/users/"+itoa(target.ID), userToken, map[string]any{
		"role": "admin",
	})
	if res.Status == http.StatusOK {
		t.Fatalf("an end user promoted themselves: %s", res.Raw)
	}
	if res.Status != http.StatusForbidden && res.Status != http.StatusNotFound {
		t.Fatalf("status = %d, want 403; body = %s", res.Status, res.Raw)
	}

	stillUser, err := h.store.Users.ByID(t.Context(), target.ID)
	if err != nil {
		t.Fatalf("reload user: %v", err)
	}
	if stillUser.Role != models.RoleUser {
		t.Errorf("role = %q, want user", stillUser.Role)
	}
}

func TestRBACRouteMatrix(t *testing.T) {
	h := newHarness(t)
	admin, techA, techB, userA, _ := h.seed()

	techAUser, _ := h.store.Users.ByUsername(t.Context(), techA)
	ticket := h.createTicketAs(userA, "Router keeps rebooting", "hardware", models.PriorityHigh)
	h.assignTo(ticket.ID, techAUser.ID)

	cases := []struct {
		name         string
		token        string
		method, path string
		body         any
		want         int
	}{
		{"user cannot read the analytics dashboard", userA, http.MethodGet, "/api/v1/analytics/dashboard", nil, http.StatusForbidden},
		{"technician cannot read the analytics dashboard", techA, http.MethodGet, "/api/v1/analytics/dashboard", nil, http.StatusForbidden},
		{"admin can read the analytics dashboard", admin, http.MethodGet, "/api/v1/analytics/dashboard", nil, http.StatusOK},
		{"user cannot read MTTR", userA, http.MethodGet, "/api/v1/analytics/mttr", nil, http.StatusForbidden},
		{"admin can read MTTR", admin, http.MethodGet, "/api/v1/analytics/mttr", nil, http.StatusOK},
		{"user cannot read the audit trail", userA, http.MethodGet, "/api/v1/audit", nil, http.StatusForbidden},
		{"admin can read the audit trail", admin, http.MethodGet, "/api/v1/audit", nil, http.StatusOK},
		{"user cannot list the directory", userA, http.MethodGet, "/api/v1/users", nil, http.StatusForbidden},
		{"technician can list the directory", techA, http.MethodGet, "/api/v1/users", nil, http.StatusOK},
		{"user cannot list technicians", userA, http.MethodGet, "/api/v1/technicians", nil, http.StatusForbidden},
		{"technician can list technicians", techA, http.MethodGet, "/api/v1/technicians", nil, http.StatusOK},
		{"user cannot create accounts", userA, http.MethodPost, "/api/v1/users",
			map[string]any{"username": "x.y", "email": "x@test.local", "password": "Passw0rd123"}, http.StatusForbidden},
		{"admin can create accounts", admin, http.MethodPost, "/api/v1/users",
			map[string]any{"username": "x.y", "email": "x@test.local", "password": "Passw0rd123"}, http.StatusCreated},
		{"user cannot edit SLA policies", userA, http.MethodPatch, "/api/v1/sla-policies/1",
			map[string]any{"resolution_minutes": 60}, http.StatusForbidden},
		{"technician cannot edit SLA policies", techA, http.MethodPatch, "/api/v1/sla-policies/1",
			map[string]any{"resolution_minutes": 60}, http.StatusForbidden},
		{"admin can edit SLA policies", admin, http.MethodPatch, "/api/v1/sla-policies/1",
			map[string]any{"resolution_minutes": 60}, http.StatusOK},
		{"user cannot delete tickets", userA, http.MethodDelete,
			"/api/v1/tickets/" + itoa(ticket.ID), nil, http.StatusForbidden},
		{"technician cannot delete tickets", techB, http.MethodDelete,
			"/api/v1/tickets/" + itoa(ticket.ID), nil, http.StatusForbidden},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			res := h.do(tc.method, tc.path, h.tokenFor(tc.token, mustRole(t, h, tc.token)), tc.body)
			if res.Status != tc.want {
				t.Fatalf("%s %s = %d, want %d; body = %s",
					tc.method, tc.path, res.Status, tc.want, res.Raw)
			}
		})
	}
}

// TestEndUserSeesOnlyOwnTickets is the row-level half of RBAC: an end user must
// not be able to read another requester's ticket even though the route allows any
// authenticated caller.
func TestEndUserSeesOnlyOwnTickets(t *testing.T) {
	h := newHarness(t)
	_, _, _, userA, userB := h.seed()

	mine := h.createTicketAs(userA, "My printer is jammed", "hardware", models.PriorityMedium)
	theirs := h.createTicketAs(userB, "Salary review scheduling", "general", models.PriorityLow)

	tokenA := h.tokenFor(userA, models.RoleUser)

	list := h.do(http.MethodGet, "/api/v1/tickets", tokenA, nil)
	if list.Status != http.StatusOK {
		t.Fatalf("list: %d %s", list.Status, list.Raw)
	}
	if total := int(list.num("total")); total != 1 {
		t.Errorf("user.a sees %d tickets, want 1 (their own only)", total)
	}

	// Reading someone else's ticket by id must fail, not silently succeed.
	peek := h.do(http.MethodGet, "/api/v1/tickets/"+itoa(theirs.ID), tokenA, nil)
	if peek.Status != http.StatusForbidden {
		t.Errorf("reading another requester's ticket = %d, want 403; body = %s", peek.Status, peek.Raw)
	}

	// Comments and events are guarded by the same check.
	for _, suffix := range []string{"/comments", "/events", "/audit"} {
		res := h.do(http.MethodGet, "/api/v1/tickets/"+itoa(theirs.ID)+suffix, tokenA, nil)
		if res.Status != http.StatusForbidden {
			t.Errorf("GET %s on a foreign ticket = %d, want 403", suffix, res.Status)
		}
	}

	// And the search endpoint must not leak other people's tickets either.
	search := h.do(http.MethodGet, "/api/v1/tickets/search?q=salary", tokenA, nil)
	if search.Status != http.StatusOK {
		t.Fatalf("search: %d %s", search.Status, search.Raw)
	}
	if total := int(search.num("total")); total != 0 {
		t.Errorf("search leaked %d tickets belonging to another requester", total)
	}

	// The requester can still read their own ticket in full.
	own := h.do(http.MethodGet, "/api/v1/tickets/"+itoa(mine.ID), tokenA, nil)
	if own.Status != http.StatusOK {
		t.Errorf("reading own ticket = %d, want 200; body = %s", own.Status, own.Raw)
	}
}

// TestEndUserCannotHideInternalNotes verifies that the staff-only comment filter
// is applied server-side rather than trusted to the frontend.
func TestEndUserCannotSeeInternalNotes(t *testing.T) {
	h := newHarness(t)
	_, tech, _, user, _ := h.seed()

	ticket := h.createTicketAs(user, "Cannot log into the payroll system", "access", models.PriorityHigh)

	techToken := h.tokenFor(tech, models.RoleTechnician)
	res := h.do(http.MethodPost, "/api/v1/tickets/"+itoa(ticket.ID)+"/comments", techToken,
		map[string]any{"body": "Suspected credential expiry, not a policy issue.", "internal": true})
	if res.Status != http.StatusCreated {
		t.Fatalf("add internal note = %d, body = %s", res.Status, res.Raw)
	}
	// A requester asking for an internal note is refused outright rather than
	// silently downgraded, so a misbehaving client cannot think it succeeded.
	forced := h.do(http.MethodPost, "/api/v1/tickets/"+itoa(ticket.ID)+"/comments",
		h.tokenFor(user, models.RoleUser),
		map[string]any{"body": "Trying to post an internal note.", "internal": true})
	if forced.Status != http.StatusForbidden {
		t.Errorf("internal note by requester = %d, want 403; body = %s", forced.Status, forced.Raw)
	}

	userToken := h.tokenFor(user, models.RoleUser)

	// An ordinary reply on their own ticket is allowed.
	reply := h.do(http.MethodPost, "/api/v1/tickets/"+itoa(ticket.ID)+"/comments", userToken,
		map[string]any{"body": "It started this morning after I changed my password."})
	if reply.Status != http.StatusCreated {
		t.Fatalf("requester reply = %d, want 201; body = %s", reply.Status, reply.Raw)
	}

	thread := h.do(http.MethodGet, "/api/v1/tickets/"+itoa(ticket.ID)+"/comments", userToken, nil)
	if thread.Status != http.StatusOK {
		t.Fatalf("read thread = %d, body = %s", thread.Status, thread.Raw)
	}
	for _, c := range thread.items() {
		if internal, _ := c["is_internal"].(bool); internal {
			t.Errorf("requester received an internal note: %v", c)
		}
		if body, _ := c["body"].(string); strings.Contains(body, "Suspected credential expiry") {
			t.Error("requester received the body of a staff-only note")
		}
	}

	staffThread := h.do(http.MethodGet, "/api/v1/tickets/"+itoa(ticket.ID)+"/comments", techToken, nil)
	for _, c := range staffThread.items() {
		if body, _ := c["body"].(string); strings.Contains(body, "Suspected credential expiry") {
			return // staff can see it, as expected
		}
	}
	t.Error("technician cannot see their own internal note")
}

func TestChangePasswordRequiresTheCurrentOne(t *testing.T) {
	h := newHarness(t)
	h.createUser("changer", models.RoleUser)
	token := h.tokenFor("changer", models.RoleUser)

	// Missing current_password.
	missing := h.do(http.MethodPost, "/api/v1/auth/password", token, map[string]any{
		"current_password": "", "new_password": "Br4ndNewPass",
	})
	if missing.Status != http.StatusUnauthorized && missing.Status != http.StatusBadRequest {
		t.Errorf("missing current password = %d, want 400 or 401; body = %s", missing.Status, missing.Raw)
	}

	// Wrong current_password.
	wrong := h.do(http.MethodPost, "/api/v1/auth/password", token, map[string]any{
		"current_password": "NotMyPassword1", "new_password": "Br4ndNewPass",
	})
	if wrong.Status != http.StatusUnauthorized {
		t.Errorf("wrong current password = %d, want 401; body = %s", wrong.Status, wrong.Raw)
	}

	// The correct current_password succeeds.
	ok := h.do(http.MethodPost, "/api/v1/auth/password", token, map[string]any{
		"current_password": "Passw0rd123", "new_password": "Br4ndNewPass",
	})
	if ok.Status != http.StatusOK {
		t.Fatalf("change password = %d, body = %s", ok.Status, ok.Raw)
	}

	// The old password must stop working and the new one must start working.
	login := h.do(http.MethodPost, "/api/v1/auth/login", "", map[string]string{
		"identifier": "changer", "password": "Br4ndNewPass",
	})
	if login.Status != http.StatusOK {
		t.Errorf("login with the new password = %d, want 200; body = %s", login.Status, login.Raw)
	}
	stale := h.do(http.MethodPost, "/api/v1/auth/login", "", map[string]string{
		"identifier": "changer", "password": "Passw0rd123",
	})
	if stale.Status != http.StatusUnauthorized {
		t.Errorf("login with the old password = %d, want 401", stale.Status)
	}
}

func TestDeactivatingAnAccountInvalidatesItsTokens(t *testing.T) {
	h := newHarness(t)
	admin := h.createUser("admin", models.RoleAdmin)
	tech := h.createUser("leaver", models.RoleTechnician)
	adminToken := h.tokenFor("admin", models.RoleAdmin)

	techToken := h.tokenFor("leaver", models.RoleTechnician)
	if res := h.do(http.MethodGet, "/api/v1/tickets", techToken, nil); res.Status != http.StatusOK {
		t.Fatalf("technician token rejected before deactivation: %d", res.Status)
	}

	off := false
	res := h.do(http.MethodPatch, "/api/v1/users/"+itoa(tech.ID), adminToken, map[string]any{"active": off})
	if res.Status != http.StatusOK {
		t.Fatalf("deactivate: %d %s", res.Status, res.Raw)
	}

	// The token itself is still cryptographically valid, so the handler must
	// re-check the account on every request.
	after := h.do(http.MethodGet, "/api/v1/tickets", techToken, nil)
	if after.Status != http.StatusUnauthorized {
		t.Errorf("deactivated account still served requests: %d; body = %s", after.Status, after.Raw)
	}

	if _, err := h.store.Users.ByID(t.Context(), admin.ID); err != nil {
		t.Fatalf("admin account missing: %v", err)
	}
}

func TestLastAdministratorIsProtected(t *testing.T) {
	h := newHarness(t)
	admin := h.createUser("admin", models.RoleAdmin)
	second := h.createUser("admin2", models.RoleAdmin)
	adminToken := h.tokenFor("admin", models.RoleAdmin)

	// With two active admins, demoting one is allowed.
	ok := h.do(http.MethodPatch, "/api/v1/users/"+itoa(second.ID), adminToken,
		map[string]any{"role": "technician"})
	if ok.Status != http.StatusOK {
		t.Fatalf("demote second admin = %d, body = %s", ok.Status, ok.Raw)
	}

	// Now only one active admin remains, so the last one cannot be demoted.
	last := h.do(http.MethodPatch, "/api/v1/users/"+itoa(admin.ID), adminToken,
		map[string]any{"role": "technician"})
	if last.Status == http.StatusOK {
		t.Fatalf("the last administrator was demoted, locking the system: %s", last.Raw)
	}

	// Nor deleted.
	del := h.do(http.MethodDelete, "/api/v1/users/"+itoa(admin.ID), adminToken, nil)
	if del.Status == http.StatusNoContent {
		t.Fatal("the last administrator was deleted")
	}
}

func TestCORSAllowsOnlyConfiguredOrigins(t *testing.T) {
	h := newHarness(t)

	origins := []struct {
		origin string
		want   string
	}{
		{"http://localhost:5173", "http://localhost:5173"},
		{"http://127.0.0.1:5173/", "http://127.0.0.1:5173"},
		{"http://evil.example.com", ""},
		{"", ""},
	}
	for _, tc := range origins {
		t.Run("origin="+tc.origin, func(t *testing.T) {
			req := httptest.NewRequest(http.MethodGet, "/health", nil)
			if tc.origin != "" {
				req.Header.Set("Origin", tc.origin)
			}
			rec := httptest.NewRecorder()
			h.handler.ServeHTTP(rec, req)

			if got := rec.Header().Get("Access-Control-Allow-Origin"); got != tc.want {
				t.Errorf("Access-Control-Allow-Origin = %q, want %q", got, tc.want)
			}
			// Caches must key on Origin, otherwise one origin's response is
			// served to another.
			if vary := rec.Header().Get("Vary"); !strings.Contains(vary, "Origin") {
				t.Errorf("Vary = %q, want it to include Origin", vary)
			}
		})
	}
}

func TestCORSPreflightAdvertisesTheRightMethods(t *testing.T) {
	h := newHarness(t)

	req := httptest.NewRequest(http.MethodOptions, "/api/v1/tickets", nil)
	req.Header.Set("Origin", "http://localhost:5173")
	req.Header.Set("Access-Control-Request-Method", "POST")
	req.Header.Set("Access-Control-Request-Headers", "authorization,content-type")
	rec := httptest.NewRecorder()
	h.handler.ServeHTTP(rec, req)

	allowHeaders := strings.ToLower(rec.Header().Get("Access-Control-Allow-Headers"))
	for _, want := range []string{"authorization", "content-type"} {
		if !strings.Contains(allowHeaders, want) {
			t.Errorf("preflight does not allow %q (got %q)", want, allowHeaders)
		}
	}
}
