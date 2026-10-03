package service

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"math/rand"
	"strings"
	"time"

	"github.com/ryan/ticketing-system/backend/internal/auth"
	"github.com/ryan/ticketing-system/backend/internal/models"
	"github.com/ryan/ticketing-system/backend/internal/store"
)

// BootstrapResult reports what the seed run created, so the operator can log in
// immediately after a cold start.
type BootstrapResult struct {
	CreatedAdmin     bool
	CreatedStaff     int
	CreatedRequester int
	CreatedTickets   int
	AdminUsername    string
	AdminPassword    string
}

// Bootstrap ensures the system has an administrator and, on a fresh database,
// enough sample data for the dashboards and queues to be meaningful.
//
// It is deliberately idempotent and safe to run on every boot:
//   - the admin is created only when no account with admin rights exists;
//   - demo tickets are created only when the tickets table is empty.
//
// No existing data is ever modified or deleted.
func Bootstrap(ctx context.Context, s *store.Store, cfg BootstrapConfig) (BootstrapResult, error) {
	var out BootstrapResult

	adminPassword, err := ensureAdmin(ctx, s, cfg)
	if err != nil {
		return out, err
	}
	out.AdminUsername = cfg.AdminUsername
	out.AdminPassword = adminPassword
	out.CreatedAdmin = adminPassword != ""

	if !cfg.DemoData {
		return out, nil
	}

	tickets, err := s.Tickets.Count(ctx)
	if err != nil {
		return out, err
	}
	if tickets > 0 {
		slog.Info("demo data skipped", "reason", "tickets already exist", "count", tickets)
		return out, nil
	}

	staff, requesters, created, err := seedDemoTickets(ctx, s, cfg.DemoTickets)
	out.CreatedTickets = created
	out.CreatedStaff = len(staff)
	out.CreatedRequester = len(requesters)
	if err != nil {
		return out, err
	}

	slog.Info("demo data seeded",
		"tickets", created, "staff", len(staff), "requesters", len(requesters),
		"password", demoDefaultPassword)
	return out, nil
}

// BootstrapConfig controls the seed run.
type BootstrapConfig struct {
	AdminUsername string
	AdminEmail    string
	AdminPassword string
	DemoPassword  string
	DemoData      bool
	DemoTickets   int
}

// ensureAdmin creates the bootstrap administrator when none exists.
//
// When an explicit password is configured it is used verbatim. Otherwise a
// random one is generated and returned so it can be logged exactly once, rather
// than shipping a well-known default credential.
func ensureAdmin(ctx context.Context, s *store.Store, cfg BootstrapConfig) (string, error) {
	admins, total, err := s.Users.List(ctx, store.UserFilter{Role: models.RoleAdmin, Limit: 1})
	if err != nil {
		return "", err
	}
	if total > 0 {
		slog.Info("administrator exists, skipping bootstrap",
			"username", firstOr(admins, func() string { return cfg.AdminUsername }))
		return "", nil
	}

	password := cfg.AdminPassword
	generated := false
	if password == "" {
		password, err = auth.GeneratePassword(16)
		if err != nil {
			return "", fmt.Errorf("generate bootstrap password: %w", err)
		}
		generated = true
	}
	hash, err := auth.HashPassword(password)
	if err != nil {
		return "", err
	}

	admin, err := s.Users.Create(ctx, store.NewUser{
		Username:     strings.ToLower(cfg.AdminUsername),
		Email:        strings.ToLower(cfg.AdminEmail),
		PasswordHash: hash,
		FullName:     "Service Desk Administrator",
		Role:         models.RoleAdmin,
		Active:       true,
	})
	if err != nil {
		return "", err
	}
	_ = s.Audit.Log(ctx, store.AuditEntry{
		ActorID: &admin.ID,
		Action:  "system.bootstrap",
		Details: strPtr("initial administrator provisioned on first boot"),
	})

	if generated {
		slog.Warn("generated a bootstrap administrator password; store it now",
			"username", admin.Username, "password", password)
	}
	return password, nil
}

// seedDemoTickets creates staff, requesters, and a spread of tickets across
// statuses, priorities, and categories so every chart and filter has data.
func seedDemoTickets(ctx context.Context, s *store.Store, count int) (staff, requesters []models.User, created int, err error) {
	if count <= 0 {
		count = 40
	}

	staff, err = ensureDemoStaff(ctx, s)
	if err != nil {
		return nil, nil, 0, err
	}
	users, err := ensureDemoRequesters(ctx, s)
	if err != nil {
		return nil, nil, 0, err
	}
	requesters = users

	// A fixed seed keeps the demo dataset identical across rebuilds, which makes
	// screenshots and dashboard comparisons meaningful.
	rng := rand.New(rand.NewSource(20240607))

	titles := map[string][]string{
		"hardware": {
			"Laptop will not power on after update",
			"Docking station drops external monitors",
			"Replacement keyboard needed for reception",
			"Printer jams on duplex printing",
		},
		"software": {
			"Spreadsheet macro fails on shared drive",
			"VPN client upgrade request",
			"Excel add-in missing after Office rollout",
			"Design tool license activation failing",
		},
		"network": {
			"Intermittent packet loss to the data centre",
			"Cannot reach the internal wiki from VPN",
			"Wi-Fi drops in the third-floor east wing",
			"DNS resolution slow for external sites",
		},
		"access": {
			"Locked out of the finance reporting folder",
			"New starter needs finance system access",
			"Request read-only access to the CRM",
			"MFA device replacement after phone upgrade",
		},
		"email": {
			"Shared mailbox missing from calendar",
			"Outbound email delayed by several minutes",
			"Distribution list silently dropping members",
			"Outlook rules moving messages to the wrong folder",
		},
		"security": {
			"Suspected phishing message reported by staff",
			"Endpoint flagged by antivirus quarantine",
			"USB device policy exception request",
			"Ex-employee account still has mailbox access",
		},
		"database": {
			"Report query timing out on the orders table",
			"Read replica lag affecting dashboards",
			"Backup job failing since last patch window",
			"Data retention purge needs scheduling",
		},
		"integration": {
			"Webhook deliveries failing with 502",
			"SSO handshake broken for the partner portal",
			"Sync between CRM and billing is double-charging",
			"API rate limit hit during nightly import",
		},
		"general": {
			"Office move requested for two desks",
			"Please add a new starter to the directory",
			"Desk booking system not showing availability",
			"Update the visitor sign-in process",
		},
	}

	categories := make([]string, 0, len(titles))
	for c := range titles {
		categories = append(categories, c)
	}
	// Map iteration order is random; sort so the demo data is deterministic.
	sortStrings(categories)

	priorities := []models.Priority{
		models.PriorityLow, models.PriorityMedium, models.PriorityMedium,
		models.PriorityHigh, models.PriorityCritical,
	}

	now := time.Now().UTC()

	for i := 0; i < count; i++ {
		category := categories[rng.Intn(len(categories))]
		pool := titles[category]
		title := pool[rng.Intn(len(pool))]
		requester := users[rng.Intn(len(users))]
		priority := priorities[rng.Intn(len(priorities))]

		// Spread creation times over the last three weeks, with a bias toward
		// recent tickets so the "today" and "7 day" charts have data.
		ageHours := 1 + rng.Intn(72*21)
		if rng.Intn(3) == 0 {
			ageHours = 1 + rng.Intn(48)
		}
		createdAt := now.Add(-time.Duration(ageHours) * time.Hour)

		// A closed ticket must be created, then progressed, then resolved. The
		// store does not backdate timestamps on update, so the demo ticket is
		// inserted with explicit stamps rather than driven through the service.
		closed := rng.Intn(100) < 45
		status := models.StatusOpen
		switch {
		case closed:
			status = models.StatusClosed
		case rng.Intn(100) < 55:
			status = models.StatusResolved
		case rng.Intn(100) < 50:
			status = models.StatusInProgress
		case rng.Intn(100) < 40:
			status = models.StatusPending
		}

		if err := seedOneTicket(ctx, s, staff, requester, category, priority, status,
			title, createdAt, now, rng, closed); err != nil {
			return staff, users, created, err
		}
		created++
	}
	return staff, users, created, nil
}

// seedOneTicket inserts one ticket with backdated SLA stamps and a comment
// thread, so history, MTTR, and the SLA report all have something to read.
func seedOneTicket(ctx context.Context, s *store.Store, staff []models.User, requester models.User,
	category string, priority models.Priority, status models.TicketStatus,
	title string, createdAt, now time.Time, rng *rand.Rand, closed bool) error {

	policy, err := s.SLA.ForPriority(ctx, priority)
	if err != nil {
		if !errors.Is(err, models.ErrNotFound) {
			return err
		}
	}

	in := store.NewTicket{
		Title:       title,
		Description: describe(category, rng),
		Category:    category,
		Priority:    priority,
		Status:      status,
		RequesterID: requester.ID,
		CreatedAt:   models.NewTime(createdAt),
		UpdatedAt:   models.NewTime(createdAt),
	}
	if policy.ID != 0 {
		id := policy.ID
		in.SLAPolicyID = &id
		in.ResponseDueAt = models.NewTime(createdAt.Add(policy.Response()))
		in.ResolutionDueAt = models.NewTime(createdAt.Add(policy.Resolution()))
	}

	// Assign to a technician for everything that is not brand new, mirroring how
	// the router would have handled it.
	if status != models.StatusOpen || rng.Intn(100) < 70 {
		tech := staff[rng.Intn(len(staff))]
		in.AssigneeID = &tech.ID
	}

	worked := createdAt.Add(time.Duration(20+rng.Intn(300)) * time.Minute)
	if worked.After(now) {
		worked = now
	}

	switch {
	case status == models.StatusInProgress || status == models.StatusPending:
		in.FirstResponseAt = models.NewTime(worked)
	case status == models.StatusResolved || status == models.StatusClosed:
		in.FirstResponseAt = models.NewTime(worked)
		finished := worked.Add(time.Duration(30+rng.Intn(2880)) * time.Minute)
		if finished.After(now) {
			finished = now
		}
		in.ResolvedAt = models.NewTime(finished)
		if closed {
			closedAt := finished.Add(time.Duration(10+rng.Intn(1440)) * time.Minute)
			if closedAt.After(now) {
				closedAt = now
			}
			in.ClosedAt = models.NewTime(closedAt)
		}
	}

	// Breach flags are derived from the same comparison the SLA engine performs,
	// so the seeded history is internally consistent with what a live run would
	// have recorded.
	if in.ResolutionDueAt != nil && in.ResolvedAt != nil {
		in.ResolutionBreached = in.ResolvedAt.Time.After(in.ResolutionDueAt.Time)
	}
	if in.ResponseDueAt != nil && in.FirstResponseAt != nil {
		in.ResponseBreached = in.FirstResponseAt.Time.After(in.ResponseDueAt.Time)
	}

	ticket, err := s.Tickets.Create(ctx, in)
	if err != nil {
		return err
	}

	// Timeline and conversation.
	_ = s.Events.Add(ctx, store.NewEvent{
		TicketID: ticket.ID, Type: string(store.EventTicketCreated),
		ToValue:   strPtr(string(models.StatusOpen)),
		Message:   requester.Username + " created ticket " + ticket.TicketNumber,
		CreatedAt: in.CreatedAt,
	})

	if in.FirstResponseAt != nil {
		tech := staff[rng.Intn(len(staff))]
		_ = s.Events.Add(ctx, store.NewEvent{
			TicketID: ticket.ID, Type: string(store.EventStatusChange),
			FromValue: strPtr(string(models.StatusOpen)),
			ToValue:   strPtr(string(models.StatusInProgress)),
			Message:   tech.Username + " changed status: status open → in_progress",
			CreatedAt: in.FirstResponseAt,
		})
		_, _ = s.Comments.Add(ctx, ticket.ID, tech.ID,
			replyFor(category, rng), false)
	}

	if in.ResolvedAt != nil {
		tech := staff[rng.Intn(len(staff))]
		_ = s.Events.Add(ctx, store.NewEvent{
			TicketID: ticket.ID, Type: string(store.EventTicketResolved),
			Message:   fmt.Sprintf("Ticket %s resolved", ticket.TicketNumber),
			CreatedAt: in.ResolvedAt,
		})
		_ = s.Events.Add(ctx, store.NewEvent{
			TicketID: ticket.ID, Type: string(store.EventStatusChange),
			FromValue: strPtr(string(models.StatusInProgress)),
			ToValue:   strPtr(string(status)),
			Message:   tech.Username + " changed status: status in_progress → " + string(status),
			CreatedAt: in.ResolvedAt,
		})
		if status == models.StatusClosed {
			_ = s.Events.Add(ctx, store.NewEvent{
				TicketID: ticket.ID, Type: string(store.EventTicketClosed),
				Message:   fmt.Sprintf("Ticket %s closed", ticket.TicketNumber),
				CreatedAt: in.ClosedAt,
			})
		}
	}
	return nil
}

// ensureDemoStaff provisions the technician pool the router draws from.
//
// Existing staff are reused, so a real technician who has joined since the seed
// is not shadowed by a demo account.
func ensureDemoStaff(ctx context.Context, s *store.Store) ([]models.User, error) {
	staff, err := s.Users.AllStaff(ctx)
	if err != nil {
		return nil, err
	}
	// One administrator is enough; the router only needs a pool.
	if len(staff) > 1 {
		return staff, nil
	}

	definitions := []struct {
		username, fullName, email string
	}{
		{"tech.avery", "Avery Okafor", "avery.okafor@example.com"},
		{"tech.blake", "Blake Ito", "blake.ito@example.com"},
		{"tech.casey", "Casey Lindqvist", "casey.lindqvist@example.com"},
		{"tech.devon", "Devon Marsh", "devon.marsh@example.com"},
	}

	for _, d := range definitions {
		hash, err := auth.HashPassword(demoDefaultPassword)
		if err != nil {
			return nil, err
		}
		if _, err := s.Users.Create(ctx, store.NewUser{
			Username: d.username, Email: d.email, PasswordHash: hash,
			FullName: d.fullName, Role: models.RoleTechnician, Active: true,
		}); err != nil {
			return nil, err
		}
	}

	staff, err = s.Users.AllStaff(ctx)
	if err != nil {
		return nil, err
	}
	return staff, nil
}

// demoDefaultPassword is the documented credential for seeded demo accounts.
//
// It exists only in a development database created by the -demo flag, and it is
// deliberately stated in the source rather than generated: a developer needs to
// be able to log in as tech.avery without hunting through logs. No production
// deployment runs the demo seeder, and the bootstrap administrator never uses it.
const demoDefaultPassword = "DemoDesk1234"

// ensureDemoRequesters provisions the end users who file tickets.
func ensureDemoRequesters(ctx context.Context, s *store.Store) ([]models.User, error) {
	definitions := []struct {
		username, fullName, email string
	}{
		{"jordan.reed", "Jordan Reed", "jordan.reed@example.com"},
		{"sam.ortiz", "Sam Ortiz", "sam.ortiz@example.com"},
		{"priya.nair", "Priya Nair", "priya.nair@example.com"},
		{"kai.lindgren", "Kai Lindgren", "kai.lindgren@example.com"},
		{"robin.ashby", "Robin Ashby", "robin.ashby@example.com"},
		{"morgan.deluca", "Morgan DeLuca", "morgan.deluca@example.com"},
	}

	existing, _, err := s.Users.List(ctx, store.UserFilter{Limit: 200})
	if err != nil {
		return nil, err
	}
	byName := map[string]models.User{}
	for _, u := range existing {
		byName[u.Username] = u
	}

	users := make([]models.User, 0, len(definitions))
	for _, d := range definitions {
		if u, ok := byName[d.username]; ok {
			users = append(users, u)
			continue
		}
		hash, err := auth.HashPassword(demoDefaultPassword)
		if err != nil {
			return nil, err
		}
		u, err := s.Users.Create(ctx, store.NewUser{
			Username: d.username, Email: d.email, PasswordHash: hash,
			FullName: d.fullName, Role: models.RoleUser, Active: true,
		})
		if err != nil {
			return nil, err
		}
		users = append(users, u)
	}
	return users, nil
}

// describe builds a plausible issue body for the demo ticket.
func describe(category string, rng *rand.Rand) string {
	openers := []string{
		"This started this morning and is affecting the whole team.",
		"It has been happening intermittently since the weekend.",
		"I noticed this during my usual workflow and it blocks my work.",
		"Raising this because the same issue was reported last month.",
		"Apologies if this is already known, I could not find an existing ticket.",
	}
	details := map[string][]string{
		"hardware": {
			"The device powers on but the screen stays black; the fan runs normally.",
			"Ports on one side are unresponsive after a drop.",
			"The unit overheats within minutes and throttles performance.",
		},
		"software": {
			"An error dialog appears when saving; the file is unchanged afterwards.",
			"The feature is missing from the ribbon after the last update.",
			"The application crashes on launch, reproducible every time.",
		},
		"network": {
			"Latency spikes to roughly 400ms and calls drop after a few minutes.",
			"The issue follows the user between networks, so it is likely not local.",
			"Only one site is affected; the rest of the estate is fine.",
		},
		"access": {
			"The account reports insufficient permissions despite being granted access.",
			"I need access before the end of the week for an audit.",
			"Access was never provisioned after the role change.",
		},
		"email": {
			"Messages sent from the shared mailbox never reach external recipients.",
			"The mailbox is missing from the shared calendar view.",
			"Attachments over 10MB are silently dropped.",
		},
		"security": {
			"A staff member received a message asking them to confirm their password.",
			"The endpoint was quarantined by the antivirus with no visible reason.",
			"There may be an account still active for a leaver.",
		},
		"database": {
			"The query completes in under a second manually but times out from the app.",
			"The dashboard shows stale figures by several hours.",
			"The nightly job has been failing since the last maintenance window.",
		},
		"integration": {
			"The partner reports intermittent 502 responses from our endpoint.",
			"The integration stopped syncing records after the schema change.",
			"Requests are rejected once we exceed an undocumented request limit.",
		},
		"general": {
			"Requesting a desk move for the new joiner next week.",
			"Could someone confirm the process for visitor sign-in?",
			"Please update the staff directory with a new starter.",
		},
	}

	pool := details[category]
	if len(pool) == 0 {
		pool = details["general"]
	}
	return openers[rng.Intn(len(openers))] + " " + pool[rng.Intn(len(pool))]
}

// replyFor builds a plausible technician reply for the seeded thread.
func replyFor(category string, rng *rand.Rand) string {
	replies := []string{
		"Thanks for the detail. I have reproduced this and raised it with the platform team.",
		"I have applied a workaround for you in the meantime; the permanent fix is in testing.",
		"Could you confirm whether this affects everyone or only your machine?",
		"Fixed. I cleared a stale session and the issue has not returned.",
		"I have logged a change request; I will confirm once it is deployed.",
	}
	return replies[rng.Intn(len(replies))]
}

// --- small helpers ---

func sortStrings(s []string) {
	for i := 1; i < len(s); i++ {
		for j := i; j > 0 && s[j] < s[j-1]; j-- {
			s[j], s[j-1] = s[j-1], s[j]
		}
	}
}

func firstOr(users []models.User, pick func() string) string {
	if len(users) > 0 {
		return users[0].Username
	}
	return pick()
}
