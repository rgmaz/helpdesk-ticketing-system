package api

import (
	"testing"

	"github.com/ryan/ticketing-system/backend/internal/config"
	"github.com/ryan/ticketing-system/backend/internal/service"
)

// The sign-in screen renders one shortcut chip per published demo account. A
// username that is not in the database produces a chip that cannot work, which
// is worse than offering none, so the roster published by GET /meta is asserted
// against the roster the seeder actually creates.
func TestMetaDemoAccountsMatchTheSeededRoster(t *testing.T) {
	h := newHarness(t)
	// Seed for real, exactly as -demo does, so the roster published by /meta
	// is compared against accounts that actually exist in the database.
	if _, err := service.Bootstrap(t.Context(), h.store, service.BootstrapConfig{
		AdminUsername: "admin",
		AdminEmail:    "admin@test.local",
		DemoPassword:  service.DemoPassword,
		DemoData:      true,
		DemoTickets:   5,
	}); err != nil {
		t.Fatalf("bootstrap demo data: %v", err)
	}
	h.server.cfg.DemoData = true
	h.server.cfg.AdminPassword = "" // demo mode: admin uses the demo password

	resp := h.do("GET", "/api/v1/meta", "", nil)
	if resp.Status != 200 {
		t.Fatalf("GET /meta = %d, want 200: %s", resp.Status, resp.Raw)
	}

	published := map[string]bool{}
	for _, raw := range resp.Body["demo_accounts"].([]any) {
		m := raw.(map[string]any)
		published[m["username"].(string)] = true
	}

	// Every published name must actually authenticate with the documented demo
	// password. This is the exact check that the stale hardcoded list failed.
	for username := range published {
		login := h.do("POST", "/api/v1/auth/login", "", map[string]string{
			"identifier": username,
			"password":   service.DemoPassword,
		})
		if login.Status != 200 {
			t.Errorf("published demo account %q cannot sign in: %d %s",
				username, login.Status, login.Raw)
		}
	}

	// And the roster must not have quietly lost its members.
	want := []string{
		"admin",
		"tech.avery", "tech.blake", "tech.casey", "tech.devon",
		"jordan.reed", "sam.ortiz", "priya.nair", "kai.lindgren",
		"robin.ashby", "morgan.deluca",
	}
	for _, username := range want {
		if !published[username] {
			t.Errorf("demo account %q missing from /meta demo_accounts", username)
		}
	}
	if got := len(published); got != len(want) {
		t.Errorf("demo_accounts has %d entries, want %d", got, len(want))
	}
}

// A deployment that supplies its own administrator password must not advertise
// the admin shortcut, because DemoDesk1234 will not open that account.
func TestMetaOmitsDemoAccountsWithoutDemoData(t *testing.T) {
	h := newHarness(t) // harness config has DemoData false and a real password

	resp := h.do("GET", "/api/v1/meta", "", nil)
	if resp.Status != 200 {
		t.Fatalf("GET /meta = %d, want 200: %s", resp.Status, resp.Raw)
	}
	if resp.Body["demo"] != false {
		t.Errorf("demo = %v, want false", resp.Body["demo"])
	}
	accounts, ok := resp.Body["demo_accounts"].([]any)
	if !ok {
		t.Fatalf("demo_accounts missing or not an array: %s", resp.Raw)
	}
	if len(accounts) != 0 {
		t.Errorf("demo_accounts = %v, want empty when not in demo mode", accounts)
	}
}

// Demo mode with an explicit ITSM_ADMIN_PASSWORD keeps the seeded technicians
// but drops the admin shortcut, because the admin is not on the demo password.
func TestMetaDropsAdminWhenAdminPasswordIsExplicit(t *testing.T) {
	cfg := &config.Config{
		DemoData:      true,
		AdminUsername: "admin",
		AdminPassword: "ChosenSecret123",
	}
	accounts := demoAccountsFor(cfg)

	for _, a := range accounts {
		if a.Username == "admin" {
			t.Fatal("admin advertised even though ITSM_ADMIN_PASSWORD was set")
		}
	}
	if len(accounts) != len(service.DemoAccountList(true)) {
		t.Errorf("got %d accounts, want the %d seeded ones",
			len(accounts), len(service.DemoAccountList(true)))
	}
}

// The admin is listed first: it is the account that reaches the dashboard.
func TestMetaListsAdminFirst(t *testing.T) {
	accounts := demoAccountsFor(&config.Config{DemoData: true, AdminUsername: "admin"})
	if len(accounts) == 0 {
		t.Fatal("no demo accounts published")
	}
	if accounts[0].Username != "admin" || accounts[0].Role != "admin" {
		t.Errorf("first account = %+v, want the admin", accounts[0])
	}
}
