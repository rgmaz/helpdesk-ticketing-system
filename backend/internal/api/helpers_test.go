package api

import (
	"context"
	"net/url"
	"strconv"
	"testing"
	"time"

	"github.com/ryan/ticketing-system/backend/internal/models"
	"github.com/ryan/ticketing-system/backend/internal/store"
)

// parseTime decodes an RFC3339 timestamp from a decoded JSON body.
func parseTime(t *testing.T, v any) time.Time {
	t.Helper()
	raw, ok := v.(string)
	if !ok || raw == "" {
		t.Fatalf("expected an RFC3339 timestamp, got %v", v)
	}
	parsed, err := time.Parse(time.RFC3339, raw)
	if err != nil {
		t.Fatalf("parse timestamp %q: %v", raw, err)
	}
	return parsed
}

// storeUpdateResolution returns a partial update that records the fix summary.
func storeUpdateResolution(notes string) store.UpdateTicket {
	return store.UpdateTicket{ResolutionNotes: &notes}
}

// makeOverdue backdates a ticket's deadlines by the given amount so the SLA
// engine treats it as breached. Going through the store keeps the timestamps in
// the same format the rest of the system uses.
func makeOverdue(t *testing.T, h *harness, ticketID int64, overdue time.Duration) {
	t.Helper()
	past := models.NewTime(time.Now().UTC().Add(-overdue))
	if _, err := h.store.Tickets.Update(context.Background(), ticketID, store.UpdateTicket{
		ResponseDueAt:   past,
		ResolutionDueAt: past,
	}); err != nil {
		t.Fatalf("backdate deadlines for ticket %d: %v", ticketID, err)
	}
}

// waitForBreach blocks until the SLA engine has recorded at least one breach.
func waitForBreach(t *testing.T, h *harness) {
	t.Helper()
	deadline := time.After(5 * time.Second)
	for {
		if h.server.sla.Stats().Breaches > 0 {
			return
		}
		select {
		case <-deadline:
			t.Fatalf("the engine did not detect a breach; stats = %+v", h.server.sla.Stats())
		case <-time.After(10 * time.Millisecond):
		}
	}
}

// waitForScan blocks until the SLA engine has completed at least n passes.
func waitForScan(t *testing.T, h *harness, n uint64) {
	t.Helper()
	deadline := time.After(5 * time.Second)
	for h.server.sla.Stats().Ticks < n {
		select {
		case <-deadline:
			t.Fatalf("the engine ran %d scans, want at least %d", h.server.sla.Stats().Ticks, n)
		case <-time.After(10 * time.Millisecond):
		}
	}
}

// urlEscape percent-encodes a query-string fragment for use in a test URL.
func urlEscape(s string) string { return url.QueryEscape(s) }

// findStatus locates a status entry in the /meta payload.
func findStatus(t *testing.T, statuses []any, value string) map[string]any {
	t.Helper()
	for _, raw := range statuses {
		m := raw.(map[string]any)
		if m["value"] == value {
			return m
		}
	}
	t.Fatalf("status %q is missing from the meta payload", value)
	return nil
}

// truncate shortens a response body for a failure message.
func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "..."
}

// itoa64 is a readability wrapper for building path segments.
func itoa64(v int64) string { return strconv.FormatInt(v, 10) }
