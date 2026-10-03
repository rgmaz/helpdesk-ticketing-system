package models

import (
	"encoding/json"
	"testing"
	"time"
)

// TestParsePriorityAcceptsTheDocumentedSpellings pins the accepted vocabulary.
// Aliases are deliberately not accepted: the API, the database CHECK constraint
// and the UI all speak these four exact strings, so a synonym would introduce a
// value nothing else in the system recognises.
func TestParsePriorityAcceptsTheDocumentedSpellings(t *testing.T) {
	cases := map[string]Priority{
		"low": PriorityLow, "LOW": PriorityLow, "  low  ": PriorityLow,
		"medium": PriorityMedium, "MEDIUM": PriorityMedium,
		"high":     PriorityHigh,
		"critical": PriorityCritical, "Critical": PriorityCritical,
	}
	for input, want := range cases {
		got, err := ParsePriority(input)
		if err != nil {
			t.Errorf("ParsePriority(%q): %v", input, err)
			continue
		}
		if got != want {
			t.Errorf("ParsePriority(%q) = %q, want %q", input, got, want)
		}
	}
	for _, input := range []string{"", "urgent", "med", "normal", "crit", "p1", "highest"} {
		if got, err := ParsePriority(input); err == nil {
			t.Errorf("ParsePriority(%q) = %q, want an error", input, got)
		}
	}
}

func TestPriorityRankIsStrictlyOrdered(t *testing.T) {
	// Rank is what "at least high" and the priority sort key both rely on, so a
	// collision or a gap would silently misroute tickets.
	want := map[Priority]int{PriorityLow: 1, PriorityMedium: 2, PriorityHigh: 3, PriorityCritical: 4}
	seen := map[int]Priority{}
	for _, p := range AllPriorities {
		if got := p.Rank(); got != want[p] {
			t.Errorf("%q.Rank() = %d, want %d", p, got, want[p])
		}
		if other, dup := seen[want[p]]; dup {
			t.Errorf("%q and %q share rank %d", p, other, want[p])
		}
		seen[want[p]] = p
	}

	for _, p := range AllPriorities {
		for _, q := range AllPriorities {
			if p.AtLeast(q) != (p.Rank() >= q.Rank()) {
				t.Errorf("%q.AtLeast(%q) disagrees with rank comparison", p, q)
			}
		}
		if !PriorityHigh.AtLeast(PriorityHigh) || PriorityHigh.AtLeast(PriorityCritical) {
			t.Errorf("AtLeast is not reflexive for high")
		}
	}

	// Rank -> priority must round-trip so a stored rank can be rendered back.
	for _, p := range AllPriorities {
		got, err := PriorityFromRank(p.Rank())
		if err != nil {
			t.Errorf("PriorityFromRank(%d): %v", p.Rank(), err)
			continue
		}
		if got != p {
			t.Errorf("PriorityFromRank(%d) = %q, want %q", p.Rank(), got, p)
		}
	}
	if _, err := PriorityFromRank(0); err == nil {
		t.Error("PriorityFromRank(0) should fail: ranks start at 1")
	}
}

func TestStatusLifecycle(t *testing.T) {
	// closed is the only terminal status: it ends the SLA clocks and leaves
	// the ticket out of open-ticket counts.
	if !StatusClosed.IsTerminal() {
		t.Error("closed must be terminal")
	}
	for _, s := range AllStatuses {
		if s == StatusClosed {
			continue
		}
		if s.IsTerminal() {
			t.Errorf("%q must not be terminal", s)
		}
		if !s.IsOpen() {
			t.Errorf("%q must count as open work", s)
		}
	}

	// A transition must be legal from both directions: NextStatuses is the
	// source of truth for the UI's action buttons, and CanTransitionTo is what
	// the server enforces. If these drift, the UI offers an action that 422s.
	for _, from := range AllStatuses {
		for _, to := range from.NextStatuses() {
			if !from.CanTransitionTo(to) {
				t.Errorf("%q lists %q as next but refuses the transition", from, to)
			}
			found := false
			for _, candidate := range AllStatuses {
				if candidate == to {
					found = true
				}
			}
			if !found {
				t.Errorf("%q advertises unknown status %q", from, to)
			}
		}
		for _, to := range AllStatuses {
			listed := false
			for _, candidate := range from.NextStatuses() {
				if candidate == to {
					listed = true
				}
			}
			if from.CanTransitionTo(to) != listed {
				t.Errorf("%q -> %q: CanTransitionTo=%v but listed=%v",
					from, to, from.CanTransitionTo(to), listed)
			}
		}
	}

	// A ticket that was never resolved cannot be reopened, and a closed ticket
	// has exactly one exit.
	if StatusOpen.CanTransitionTo(StatusReopened) {
		t.Error("open -> reopened must be illegal; reopened means it was resolved")
	}
	if next := StatusClosed.NextStatuses(); len(next) != 1 || next[0] != StatusReopened {
		t.Errorf("closed.NextStatuses() = %v, want [reopened]", next)
	}
	if StatusResolved.CanTransitionTo(StatusOpen) {
		t.Error("resolved -> open must be illegal; use reopened")
	}
	// resolved still counts as open work: the requester has not confirmed.
	if !StatusResolved.IsOpen() {
		t.Error("resolved must count as open until it is closed")
	}
}

func TestEveryEnumValueHasALabel(t *testing.T) {
	// Labels live beside the enums so a new value cannot ship without one, and
	// the UI relies on them rather than prettifying the raw string itself.
	for _, s := range AllStatuses {
		if s.Label() == "" {
			t.Errorf("status %q has no label", s)
		}
	}
	for _, p := range AllPriorities {
		if p.Label() == "" {
			t.Errorf("priority %q has no label", p)
		}
	}
}

func TestParseRoleRejectsUnknownRoles(t *testing.T) {
	for input, want := range map[string]RoleName{
		"user": RoleUser, "technician": RoleTechnician, "admin": RoleAdmin,
		"ADMIN": RoleAdmin, " technician ": RoleTechnician,
	} {
		got, err := ParseRole(input)
		if err != nil {
			t.Errorf("ParseRole(%q): %v", input, err)
			continue
		}
		if got != want {
			t.Errorf("ParseRole(%q) = %q, want %q", input, got, want)
		}
	}
	// Privilege escalation by typo must fail closed rather than defaulting.
	// Surrounding whitespace is tolerated, because it comes from form input.
	for _, input := range []string{"", "root", "administrator", "staff", "user admin"} {
		if got, err := ParseRole(input); err == nil {
			t.Errorf("ParseRole(%q) = %q, want an error", input, got)
		}
	}
}

func TestRoleStaffPredicate(t *testing.T) {
	if RoleUser.IsStaff() {
		t.Error("end users are not staff")
	}
	if !RoleTechnician.IsStaff() || !RoleAdmin.IsStaff() {
		t.Error("technicians and admins are staff")
	}
	if RoleTechnician.IsAdmin() {
		t.Error("a technician is not an administrator")
	}
	if !RoleAdmin.IsAdmin() {
		t.Error("an administrator is an administrator")
	}
	// An empty role must not be staff: a missing claim has to fail closed.
	if RoleName("").IsStaff() || RoleName("").IsAdmin() {
		t.Error("an unknown role must not grant staff privileges")
	}
}

func TestSLAPolicyDurations(t *testing.T) {
	p := SLAPolicy{ResponseMinutes: 15, ResolutionMinutes: 240, EscalationMinutes: 30}
	if p.Response() != 15*time.Minute {
		t.Errorf("Response() = %v, want 15m", p.Response())
	}
	if p.Resolution() != 4*time.Hour {
		t.Errorf("Resolution() = %v, want 4h", p.Resolution())
	}
	if p.EscalationInterval() != 30*time.Minute {
		t.Errorf("EscalationInterval() = %v, want 30m", p.EscalationInterval())
	}

	// escalation_minutes is the gap between steps and the cap is 2, so a policy
	// may never escalate more than twice however long the breach runs.
	if got := p.MaxEscalations(); got != 2 {
		t.Errorf("MaxEscalations() = %d, want 2", got)
	}
	if got := (SLAPolicy{ResolutionMinutes: 240, EscalationMinutes: 720}).MaxEscalations(); got != 0 {
		t.Errorf("MaxEscalations() = %d when the interval exceeds the window, want 0", got)
	}
	if got := (SLAPolicy{ResolutionMinutes: 240, EscalationMinutes: 0}).MaxEscalations(); got != 0 {
		t.Errorf("MaxEscalations() = %d with escalation disabled, want 0", got)
	}
	if got := (SLAPolicy{ResolutionMinutes: 240, EscalationMinutes: 240}).MaxEscalations(); got != 1 {
		t.Errorf("MaxEscalations() = %d for one interval of runway, want 1", got)
	}
}

func TestSLAStateClassification(t *testing.T) {
	now := time.Date(2026, 5, 1, 12, 0, 0, 0, time.UTC)
	const atRisk = 60

	in := func(d time.Duration) *Time { return NewTime(now.Add(d)) }
	mk := func(mutate func(*Ticket)) *Ticket {
		tk := &Ticket{
			Status:          StatusOpen,
			ResolutionDueAt: in(6 * time.Hour),
			CreatedAt:       NewTime(now),
		}
		if mutate != nil {
			mutate(tk)
		}
		return tk
	}

	cases := []struct {
		name string
		tk   *Ticket
		want SLAState
	}{
		{"comfortable runway", mk(nil), SLAOnTrack},
		{"inside the warning window", mk(func(tk *Ticket) {
			tk.ResolutionDueAt = in(30 * time.Minute)
		}), SLAAtRisk},
		{"exactly at the warning boundary is still at risk", mk(func(tk *Ticket) {
			tk.ResolutionDueAt = in(atRisk * time.Minute)
		}), SLAAtRisk},
		{"past the deadline", mk(func(tk *Ticket) {
			tk.ResolutionDueAt = in(-time.Second)
		}), SLABreached},
		{"flagged by the engine", mk(func(tk *Ticket) {
			tk.ResolutionBreached = true
		}), SLABreached},
		{"flagged on the response clock", mk(func(tk *Ticket) {
			tk.ResponseBreached = true
		}), SLABreached},
		{"no deadline at all", mk(func(tk *Ticket) {
			tk.ResolutionDueAt = nil
		}), SLAStopped},
		{"closed on time", mk(func(tk *Ticket) {
			tk.Status = StatusClosed
		}), SLAStopped},
		{"closed late", mk(func(tk *Ticket) {
			tk.Status = StatusClosed
			tk.ResolutionBreached = true
		}), SLABreached},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := tc.tk.SLAStateOf(now, atRisk); got != tc.want {
				t.Errorf("SLAStateOf = %q, want %q", got, tc.want)
			}
		})
	}
}

// TestDeriveSLATimersStopsWhenTerminal checks that a finished ticket does not
// keep counting down, which would show a stale deadline in the UI.
func TestDeriveSLATimersStopsWhenTerminal(t *testing.T) {
	now := time.Date(2026, 5, 1, 12, 0, 0, 0, time.UTC)
	due := NewTime(now.Add(2 * time.Hour))

	tk := &Ticket{Status: StatusOpen, ResponseDueAt: due, ResolutionDueAt: due}
	tk.DeriveSLATimers(now)
	if tk.ResolutionRemain == nil || *tk.ResolutionRemain != 2*time.Hour {
		t.Errorf("remaining = %v, want 2h", tk.ResolutionRemain)
	}

	tk.Status = StatusClosed
	tk.DeriveSLATimers(now)
	if tk.ResolutionRemain != nil || tk.ResponseRemaining != nil {
		t.Error("a terminal ticket must not expose a running countdown")
	}
}

func TestTimeJSONRoundTrip(t *testing.T) {
	want := time.Date(2026, 7, 4, 9, 30, 0, 0, time.UTC)

	encoded, err := json.Marshal(NewTime(want))
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	if string(encoded) != `"2026-07-04T09:30:00Z"` {
		t.Errorf("encoded = %s, want an RFC3339 UTC string", encoded)
	}

	var back Time
	if err := json.Unmarshal(encoded, &back); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if !back.Time.Equal(want) {
		t.Errorf("round trip = %v, want %v", back.Time, want)
	}

	// A zero timestamp is null on the wire, not "0001-01-01T00:00:00Z".
	zero, err := json.Marshal(Time{})
	if err != nil {
		t.Fatalf("marshal zero: %v", err)
	}
	if string(zero) != "null" {
		t.Errorf("zero time encoded as %s, want null", zero)
	}

	// null and the empty string both decode to the zero value, matching the
	// NULL columns they come from.
	for _, in := range []string{"null", `""`} {
		var v Time
		if err := json.Unmarshal([]byte(in), &v); err != nil {
			t.Errorf("unmarshal %s: %v", in, err)
		}
		if !v.Time.IsZero() {
			t.Errorf("%s decoded to %v, want the zero time", in, v.Time)
		}
	}

	var bad Time
	if err := json.Unmarshal([]byte(`"yesterday"`), &bad); err == nil {
		t.Error("a non-timestamp string must fail to decode")
	}
}

func TestTimeScansEveryLayoutTheSchemaProduces(t *testing.T) {
	cases := []struct {
		name string
		src  any
		ok   bool
	}{
		{"null column", nil, true},
		{"empty string", "", true},
		{"the strftime default", "2026-07-04 09:30:00", true},
		{"the Go writer's format", "2026-07-04T09:30:00Z", true},
		{"sqlite driver datetime", time.Date(2026, 7, 4, 9, 30, 0, 0, time.UTC), true},
		{"fractional seconds with offset", "2026-07-04 09:30:00.123456789+02:00", true},
		{"rubbish", "not a timestamp", false},
		{"a number", 12345, false},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var got Time
			err := got.Scan(tc.src)
			if tc.ok && err != nil {
				t.Fatalf("Scan(%v): %v", tc.src, err)
			}
			if !tc.ok && err == nil {
				t.Fatalf("Scan(%v) succeeded, want an error", tc.src)
			}
			if tc.ok && got.Time.IsZero() && tc.src != nil && tc.src != "" {
				t.Errorf("Scan(%v) left the zero time", tc.src)
			}
		})
	}
}

// TestNullTimeStoresNilForZeroValues guards nullable columns: writing the zero
// time as a timestamp would make "not yet answered" look like 1 January 0001.
func TestNullTimeStoresNilForZeroValues(t *testing.T) {
	if got := NullTime(nil); got != nil {
		t.Errorf("NullTime(nil) = %v, want nil", got)
	}
	if got := NullTime(&Time{}); got != nil {
		t.Errorf("NullTime(zero) = %v, want nil", got)
	}
	got, ok := NullTime(NewTime(time.Date(2026, 7, 4, 9, 30, 0, 0, time.UTC))).(string)
	if !ok || got != "2026-07-04T09:30:00Z" {
		t.Errorf("NullTime = %v, want the canonical UTC string", got)
	}
}

func TestJSONMapNeverSerialisesNull(t *testing.T) {
	// The UI does object lookups on these columns, so a null would crash a
	// spread or an Object.keys.
	encoded, err := json.Marshal(JSONMap(nil))
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	if string(encoded) != "{}" {
		t.Errorf("nil JSONMap encoded as %s, want {}", encoded)
	}

	var m JSONMap
	if err := m.Scan([]byte(`{"a":1}`)); err != nil {
		t.Fatalf("scan bytes: %v", err)
	}
	if m["a"] != float64(1) {
		t.Errorf("scan produced %v", m)
	}
	if err := m.Scan(`{"b":2}`); err != nil {
		t.Fatalf("scan string: %v", err)
	}
	if m["b"] != float64(2) {
		t.Errorf("scan produced %v", m)
	}
	for _, src := range []any{nil, ""} {
		if err := m.Scan(src); err != nil {
			t.Errorf("Scan(%v): %v", src, err)
		}
		if m != nil {
			t.Errorf("Scan(%v) left %v, want nil", src, m)
		}
	}
}

func TestUserDisplayNamePrefersTheFullName(t *testing.T) {
	cases := []struct {
		user User
		want string
	}{
		{User{Username: "avery", FullName: "Avery Quinn"}, "Avery Quinn"},
		{User{Username: "avery", FullName: "   "}, "avery"},
		{User{Username: "avery"}, "avery"},
	}
	for _, tc := range cases {
		if got := tc.user.DisplayName(); got != tc.want {
			t.Errorf("DisplayName() = %q, want %q", got, tc.want)
		}
	}
}
