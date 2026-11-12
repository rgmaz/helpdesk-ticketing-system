package service

import (
	"reflect"
	"testing"

	"github.com/ryan/ticketing-system/backend/internal/models"
)

// TestAvailableTransitionsByRole pins the advertised actions to the rules the
// write path enforces. End users may only close or reopen; staff get the full
// state machine.
func TestAvailableTransitionsByRole(t *testing.T) {
	requester := Actor{ID: 1, Username: "user", Role: models.RoleUser}
	technician := Actor{ID: 2, Username: "tech", Role: models.RoleTechnician}
	admin := Actor{ID: 3, Username: "admin", Role: models.RoleAdmin}

	cases := []struct {
		status    models.TicketStatus
		requester []models.TicketStatus
		staff     []models.TicketStatus
	}{
		{
			models.StatusOpen,
			[]models.TicketStatus{models.StatusClosed},
			[]models.TicketStatus{models.StatusInProgress, models.StatusPending, models.StatusResolved, models.StatusClosed},
		},
		{
			models.StatusInProgress,
			[]models.TicketStatus{models.StatusClosed},
			[]models.TicketStatus{models.StatusPending, models.StatusResolved, models.StatusClosed},
		},
		{
			models.StatusPending,
			[]models.TicketStatus{models.StatusClosed},
			[]models.TicketStatus{models.StatusInProgress, models.StatusResolved, models.StatusClosed},
		},
		{
			models.StatusResolved,
			[]models.TicketStatus{models.StatusClosed, models.StatusReopened},
			[]models.TicketStatus{models.StatusClosed, models.StatusReopened},
		},
		{
			models.StatusClosed,
			[]models.TicketStatus{models.StatusReopened},
			[]models.TicketStatus{models.StatusReopened},
		},
		{
			models.StatusReopened,
			[]models.TicketStatus{models.StatusClosed},
			[]models.TicketStatus{models.StatusInProgress, models.StatusPending, models.StatusResolved, models.StatusClosed},
		},
	}

	var tickets Tickets
	for _, tc := range cases {
		t.Run(string(tc.status), func(t *testing.T) {
			if got := tickets.AvailableTransitions(requester, tc.status); !reflect.DeepEqual(got, tc.requester) {
				t.Errorf("requester transitions from %s = %v, want %v", tc.status, got, tc.requester)
			}
			if got := tickets.AvailableTransitions(technician, tc.status); !reflect.DeepEqual(got, tc.staff) {
				t.Errorf("technician transitions from %s = %v, want %v", tc.status, got, tc.staff)
			}
			if got := tickets.AvailableTransitions(admin, tc.status); !reflect.DeepEqual(got, tc.staff) {
				t.Errorf("admin transitions from %s = %v, want %v", tc.status, got, tc.staff)
			}

			// Whatever is advertised must pass the same predicate the write path
			// uses; anything that fails it must not be in the list.
			for _, next := range tc.status.NextStatuses() {
				advertised := false
				for _, allowed := range tickets.AvailableTransitions(requester, tc.status) {
					if allowed == next {
						advertised = true
					}
				}
				if CanTransition(requester, tc.status, next) != advertised {
					t.Errorf("status %s: CanTransition(%s) = %v but advertised = %v",
						tc.status, next, CanTransition(requester, tc.status, next), advertised)
				}
			}
		})
	}
}

func TestCanTransitionRefusesUnknownEdges(t *testing.T) {
	staff := Actor{ID: 1, Username: "tech", Role: models.RoleTechnician}
	if CanTransition(staff, models.StatusResolved, models.StatusInProgress) {
		t.Fatal("resolved → in_progress is not a legal edge")
	}
	if CanTransition(staff, models.StatusOpen, models.StatusOpen) {
		t.Fatal("a status must not transition to itself")
	}
}
