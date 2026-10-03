package api

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strconv"
	"testing"
	"time"

	"github.com/ryan/ticketing-system/backend/internal/auth"
	"github.com/ryan/ticketing-system/backend/internal/config"
	"github.com/ryan/ticketing-system/backend/internal/db"
	"github.com/ryan/ticketing-system/backend/internal/models"
	"github.com/ryan/ticketing-system/backend/internal/notify"
	"github.com/ryan/ticketing-system/backend/internal/service"
	"github.com/ryan/ticketing-system/backend/internal/store"
)

// harness is a fully wired server backed by a throwaway SQLite file.
//
// A file is used rather than :memory: because modernc.org/sqlite gives each
// connection its own private database when the DSN is "file::memory:", which
// would break the pooled connections the store layer depends on.
type harness struct {
	t       *testing.T
	server  *Server
	store   *store.Store
	handler http.Handler
	tokens  *auth.Manager
}

// newHarness builds a server with a fresh database and the given accounts.
func newHarness(t *testing.T) *harness {
	t.Helper()

	database, err := db.Open(filepath.Join(t.TempDir(), "itsm-test.db"))
	if err != nil {
		t.Fatalf("open database: %v", err)
	}
	t.Cleanup(func() { database.Close() })

	cfg := &config.Config{
		JWTSecret:       "test-secret-value-not-used-anywhere-else",
		AccessTTL:       time.Hour,
		AllowedOrigin:   []string{"http://localhost:5173", "http://127.0.0.1:5173"},
		SLAScanInterval: time.Minute,
		SLAWarnBefore:   15 * time.Minute,
		AdminUsername:   "admin",
		AdminEmail:      "admin@test.local",
		AdminPassword:   "AdminPass123",
		ShutdownTimeout: time.Second,
	}

	st := store.New(database.DB)
	hub := notify.NewHub()
	tokens := auth.NewManager(cfg.JWTSecret, cfg.AccessTTL)
	// Mirrors main.go: a deactivated account must not keep working on an
	// already-issued token.
	tokens.SetActiveChecker(func(ctx context.Context, userID int64) (bool, error) {
		user, err := st.Users.ByID(ctx, userID)
		if err != nil {
			return false, err
		}
		return user.Active, nil
	})
	router := service.NewRouter(st.Users, st.Tickets)
	tickets := service.NewTickets(st, hub, router)
	// A short scan interval lets tests observe several passes of the real ticker
	// loop. Production uses ITSM_SLA_SCAN_INTERVAL, which is a minute.
	sla := service.NewSLAEngine(st, hub, 20*time.Millisecond, 15*time.Minute)

	srv := NewServer(cfg, st, hub, tokens, tickets, sla, time.Now().Unix())
	return &harness{
		t:       t,
		server:  srv,
		store:   st,
		tokens:  tokens,
		handler: srv.Routes(),
	}
}

// --- request helpers ---

type response struct {
	Status int
	Body   map[string]any
	Raw    string
	Header http.Header
}

// jsonArray decodes a JSON array field, returning nil when absent.
func (r response) items() []map[string]any {
	raw, ok := r.Body["items"]
	if !ok || raw == nil {
		return nil
	}
	list, ok := raw.([]any)
	if !ok {
		return nil
	}
	out := make([]map[string]any, 0, len(list))
	for _, v := range list {
		if m, ok := v.(map[string]any); ok {
			out = append(out, m)
		}
	}
	return out
}

func (r response) num(key string) float64 {
	v, _ := r.Body[key].(float64)
	return v
}

func (r response) str(key string) string {
	v, _ := r.Body[key].(string)
	return v
}

// do issues a request against the harness with an optional bearer token.
func (h *harness) do(method, path, token string, body any) response {
	h.t.Helper()

	var reader io.Reader
	if body != nil {
		switch v := body.(type) {
		case []byte:
			reader = bytes.NewReader(v)
		case string:
			reader = bytes.NewReader([]byte(v))
		default:
			raw, err := json.Marshal(v)
			if err != nil {
				h.t.Fatalf("marshal request body: %v", err)
			}
			reader = bytes.NewReader(raw)
		}
	}

	req := httptest.NewRequest(method, path, reader)
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}

	rec := httptest.NewRecorder()
	h.handler.ServeHTTP(rec, req)

	res := response{Status: rec.Code, Raw: rec.Body.String(), Header: rec.Header()}
	if len(rec.Body.Bytes()) > 0 {
		var decoded map[string]any
		if err := json.Unmarshal(rec.Body.Bytes(), &decoded); err == nil {
			res.Body = decoded
		}
	}
	return res
}

// tokenFor mints an access token for an existing account, bypassing the login
// endpoint so tests that are not about authentication stay fast and focused.
func (h *harness) tokenFor(username string, role models.RoleName) string {
	h.t.Helper()
	user, err := h.store.Users.ByUsername(context.Background(), username)
	if err != nil {
		h.t.Fatalf("load user %q: %v", username, err)
	}
	token, _, err := h.tokens.Issue(user)
	if err != nil {
		h.t.Fatalf("sign token for %q: %v", username, err)
	}
	return token
}

// createUser inserts an account directly and returns it.
func (h *harness) createUser(username string, role models.RoleName) models.User {
	h.t.Helper()
	hash, err := auth.HashPassword("Passw0rd123")
	if err != nil {
		h.t.Fatalf("hash password: %v", err)
	}
	user, err := h.store.Users.Create(context.Background(), store.NewUser{
		Username:     username,
		Email:        username + "@test.local",
		PasswordHash: hash,
		FullName:     username,
		Role:         role,
		Active:       true,
	})
	if err != nil {
		h.t.Fatalf("create user %q: %v", username, err)
	}
	return user
}

// seed builds the standard fixture: one admin, two technicians, two end users.
func (h *harness) seed() (admin, techA, techB, userA, userB string) {
	h.t.Helper()
	admin = h.createUser("admin", models.RoleAdmin).Username
	techA = h.createUser("tech.a", models.RoleTechnician).Username
	techB = h.createUser("tech.b", models.RoleTechnician).Username
	userA = h.createUser("user.a", models.RoleUser).Username
	userB = h.createUser("user.b", models.RoleUser).Username
	return
}

// createTicketAs files a ticket through the service layer with the named user as
// the actor, bypassing HTTP so fixtures stay cheap. Because an end user's
// priority is capped at medium by the business rules, use createTicketAsStaff
// when the fixture needs a high or critical ticket.
func (h *harness) createTicketAs(username, title, category string, priority models.Priority) models.Ticket {
	h.t.Helper()
	return h.createTicketAsActor(username, title, category, priority)
}

// createTicketAsStaff files a ticket as an administrator, so the requested
// priority survives the end-user cap.
func (h *harness) createTicketAsStaff(title, category string, priority models.Priority) models.Ticket {
	h.t.Helper()

	admin, err := h.store.Users.ByUsername(context.Background(), adminUsername(h))
	if err != nil {
		h.t.Fatalf("load admin: %v", err)
	}
	actor := service.Actor{ID: admin.ID, Username: admin.Username, Role: admin.Role}

	ticket, err := h.server.tickets.Create(context.Background(), actor, service.CreateRequest{
		Title:       title,
		Description: "Reproduction steps and observed behaviour for " + title,
		Category:    category,
		Priority:    priority,
	})
	if err != nil {
		h.t.Fatalf("create ticket %q: %v", title, err)
	}
	return ticket
}

// createTicketAsActor is the shared implementation behind the two creators.
func (h *harness) createTicketAsActor(username, title, category string, priority models.Priority) models.Ticket {
	h.t.Helper()
	user, err := h.store.Users.ByUsername(context.Background(), username)
	if err != nil {
		h.t.Fatalf("load user %q: %v", username, err)
	}
	actor := service.Actor{ID: user.ID, Username: user.Username, Role: user.Role}

	ticket, err := h.server.tickets.Create(context.Background(), actor, service.CreateRequest{
		Title:       title,
		Description: "Reproduction steps and observed behaviour for " + title,
		Category:    category,
		Priority:    priority,
	})
	if err != nil {
		h.t.Fatalf("create ticket %q: %v", title, err)
	}
	return ticket
}

// adminUsername returns the harness's administrator account name, creating it if
// a test has not seeded one yet.
func adminUsername(h *harness) string {
	if _, err := h.store.Users.ByUsername(context.Background(), "admin"); err == nil {
		return "admin"
	}
	return h.createUser("admin", models.RoleAdmin).Username
}

// assignTo manually assigns a ticket, bypassing the router.
func (h *harness) assignTo(ticketID, assigneeID int64) {
	h.t.Helper()
	if _, err := h.store.Tickets.Update(context.Background(), ticketID, store.UpdateTicket{
		AssigneeID: &assigneeID,
	}); err != nil {
		h.t.Fatalf("assign ticket %d: %v", ticketID, err)
	}
}

// runEngine starts the SLA worker and stops it when the test finishes.
func (h *harness) runEngine() {
	ctx, cancel := context.WithCancel(context.Background())
	h.t.Cleanup(cancel)
	go h.server.sla.Run(ctx)
}

// --- shared test helpers ---

// storeUpdateActive builds the partial user update that toggles the active flag.
func storeUpdateActive(active bool) store.UpdateUser { return store.UpdateUser{Active: &active} }

func itoa(id int64) string { return strconv.FormatInt(id, 10) }

// mustRole looks up a fixture account's role so route-matrix cases can be written
// as (username, expected status) pairs.
func mustRole(t *testing.T, h *harness, username string) models.RoleName {
	t.Helper()
	user, err := h.store.Users.ByUsername(context.Background(), username)
	if err != nil {
		t.Fatalf("load user %q: %v", username, err)
	}
	return user.Role
}
