// SPDX-License-Identifier: Elastic-2.0
package httpapi

import (
	"agentplatform.local/platform/internal/domain"
	"net/http"
)

func (s *Server) mountAutomation(mux *http.ServeMux) {
	mux.HandleFunc("GET /api/v1/tasks/{id}/automation", s.auth(func(w http.ResponseWriter, r *http.Request, p domain.Principal) {
		if !s.materialQuery(w, r) {
			return
		}
		v, e := s.Store.Automation(r.Context(), p, r.PathValue("id"))
		if e != nil {
			s.fail(w, r, e)
			return
		}
		s.ok(w, 200, v, "")
	}))
	mux.HandleFunc("POST /api/v1/tasks/{id}/automation", s.auth(func(w http.ResponseWriter, r *http.Request, p domain.Principal) {
		if !s.materialQuery(w, r) {
			return
		}
		var in domain.CreateTaskAutomation
		if !s.decode(w, r, &in) {
			return
		}
		v, e := s.Store.CreateAutomation(r.Context(), p, r.PathValue("id"), r.Header.Get("Idempotency-Key"), in)
		s.mutation(w, r, v, e, 201)
	}))
	mux.HandleFunc("POST /api/v1/tasks/{id}/automation/revoke", s.auth(func(w http.ResponseWriter, r *http.Request, p domain.Principal) {
		if !s.materialQuery(w, r) {
			return
		}
		var in domain.RevokeTaskAutomation
		if !s.decode(w, r, &in) {
			return
		}
		v, e := s.Store.RevokeAutomation(r.Context(), p, r.PathValue("id"), r.Header.Get("Idempotency-Key"), in)
		s.mutation(w, r, v, e, 200)
	}))
	mux.HandleFunc("GET /api/v1/tasks/{id}/progress-signals", s.auth(func(w http.ResponseWriter, r *http.Request, p domain.Principal) {
		if !s.materialQuery(w, r, "cursor", "limit") {
			return
		}
		n, e := toolListLimit(r, 20)
		if e != nil {
			s.fail(w, r, e)
			return
		}
		v, next, e := s.Store.ProgressSignals(r.Context(), p, r.PathValue("id"), r.URL.Query().Get("cursor"), n)
		if e != nil {
			s.fail(w, r, e)
			return
		}
		s.ok(w, 200, v, next)
	}))
}
