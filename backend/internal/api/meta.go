package api

import (
	"net/http"
	"sort"

	"github.com/ryan/ticketing-system/backend/internal/httpx"
	"github.com/ryan/ticketing-system/backend/internal/service"
)

// handleCategories returns the request types actually present in the system.
//
// It is derived from the data rather than from a static list so the filter
// dropdown never offers a category with no tickets behind it. The full canonical
// list stays available at GET /api/v1/meta.
func (s *Server) handleCategories(w http.ResponseWriter, r *http.Request) {
	categories, err := s.store.Tickets.Categories(r.Context())
	if err != nil {
		httpx.Fail(w, err)
		return
	}

	inUse := map[string]bool{}
	for _, c := range categories {
		inUse[c] = true
	}

	// Known categories first in canonical order, then anything unexpected found
	// in the data, so a newly added value still shows up.
	merged := make([]string, 0, len(service.Categories)+len(categories))
	for _, c := range service.Categories {
		merged = append(merged, c)
	}
	extra := []string{}
	for _, c := range categories {
		if !inUse[c] {
			continue
		}
		known := false
		for _, k := range service.Categories {
			if k == c {
				known = true
				break
			}
		}
		if !known {
			extra = append(extra, c)
		}
	}
	sort.Strings(extra)
	merged = append(merged, extra...)

	httpx.OK(w, httpx.ListBody{
		Items: merged, Total: len(merged), Page: 1, Limit: len(merged), Pages: 1,
		Extra: map[string]any{"in_use": categories},
	})
}
