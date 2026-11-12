package api

import (
	"net/http"
	"testing"

	"github.com/ryan/ticketing-system/backend/internal/models"
)

// TestDirectoryFlagsTheLastActiveAdmin covers the capability the People page
// uses to hide controls the server would refuse.
func TestDirectoryFlagsTheLastActiveAdmin(t *testing.T) {
	h := newHarness(t)
	admin, _, _, _, _ := h.seed()
	token := h.tokenFor(admin, models.RoleAdmin)

	if !directoryUser(t, h, token, admin)["is_last_admin"].(bool) {
		t.Fatal("the only active administrator should be flagged as the last one")
	}

	second := h.createUser("admin.two", models.RoleAdmin)
	if directoryUser(t, h, token, admin)["is_last_admin"].(bool) {
		t.Fatal("with two active administrators neither is the last one")
	}
	if directoryUser(t, h, token, second.Username)["is_last_admin"].(bool) {
		t.Fatal("with two active administrators neither is the last one")
	}

	// Deactivating one administrator leaves the other as the only one, so the
	// flag must follow the data rather than be fixed at seed time.
	res := h.do(http.MethodPatch, "/api/v1/users/"+itoa(second.ID), token,
		map[string]any{"active": false})
	if res.Status != http.StatusOK {
		t.Fatalf("deactivate second admin = %d, body = %s", res.Status, res.Raw)
	}
	if !directoryUser(t, h, token, admin)["is_last_admin"].(bool) {
		t.Fatal("the remaining active administrator should be flagged as the last one")
	}
}

// directoryUser fetches the directory and returns one row by username.
func directoryUser(t *testing.T, h *harness, token, username string) map[string]any {
	t.Helper()
	res := h.do(http.MethodGet, "/api/v1/users?limit=200", token, nil)
	if res.Status != http.StatusOK {
		t.Fatalf("list users = %d, body = %s", res.Status, res.Raw)
	}
	for _, item := range res.items() {
		if item["username"] == username {
			return item
		}
	}
	t.Fatalf("user %q missing from the directory", username)
	return nil
}
