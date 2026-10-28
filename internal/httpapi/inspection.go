// SPDX-License-Identifier: Elastic-2.0
package httpapi

import (
	"agentplatform.local/platform/internal/domain"
	"agentplatform.local/platform/internal/storage"
	"net/http"
)

func (s *Server) inspect(kind string) authorizedHandler {
	return func(w http.ResponseWriter, r *http.Request, p domain.Principal) {
		n, e := limit(r)
		if e != nil {
			s.fail(w, r, e)
			return
		}
		id := r.PathValue("id")
		cursor := r.URL.Query().Get("cursor")
		var data any
		var next string
		switch kind {
		case "runs":
			data, next, e = s.Store.Runs(r.Context(), p, id, cursor, n)
		case "operations":
			data, next, e = s.Store.Operations(r.Context(), p, id, cursor, n)
		case "actions":
			data, next, e = s.Store.Actions(r.Context(), p, id, cursor, n)
		case "approvals":
			data, next, e = s.Store.Approvals(r.Context(), p, id, cursor, n)
		case "costs":
			data, e = s.Store.Costs(r.Context(), p, id, storage.CostCursors{Reservation: r.URL.Query().Get("reservation_cursor"), Entry: r.URL.Query().Get("entry_cursor"), Incident: r.URL.Query().Get("incident_cursor"), Quarantine: r.URL.Query().Get("quarantine_cursor"), Limit: n})
		default:
			e = domain.NotFound
		}
		if e != nil {
			s.fail(w, r, e)
			return
		}
		s.ok(w, 200, data, next)
	}
}
