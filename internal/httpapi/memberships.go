// SPDX-License-Identifier: Elastic-2.0
package httpapi

import (
	"net/http"
	"strings"

	"agentplatform.local/platform/internal/domain"
)

// Membership commands share the ordinary authentication, CSRF, strict JSON and
// durable idempotency boundary. Organization selection is never identity proof.
func (s *Server) mountMemberships(mux *http.ServeMux) {
	mux.HandleFunc("GET /api/v1/members/capabilities", s.auth(func(w http.ResponseWriter, r *http.Request, p domain.Principal) {
		if !s.membershipQuery(w, r) {
			return
		}
		v, e := s.Store.MembershipCapabilities(r.Context(), p)
		if e != nil {
			s.fail(w, r, e)
			return
		}
		s.ok(w, 200, v, "")
	}))
	mux.HandleFunc("GET /api/v1/members", s.auth(func(w http.ResponseWriter, r *http.Request, p domain.Principal) {
		if !s.membershipQuery(w, r, "cursor", "limit") {
			return
		}
		n, e := toolListLimit(r, 50)
		if e != nil {
			s.fail(w, r, e)
			return
		}
		v, next, e := s.Store.Members(r.Context(), p, r.URL.Query().Get("cursor"), n)
		if e != nil {
			s.fail(w, r, e)
			return
		}
		s.ok(w, 200, v, next)
	}))
	mux.HandleFunc("GET /api/v1/members/{principal_id}", s.auth(func(w http.ResponseWriter, r *http.Request, p domain.Principal) {
		if !s.membershipQuery(w, r) {
			return
		}
		v, e := s.Store.Member(r.Context(), p, r.PathValue("principal_id"))
		if e != nil {
			s.fail(w, r, e)
			return
		}
		s.ok(w, 200, v, "")
	}))
	mux.HandleFunc("POST /api/v1/members", s.auth(func(w http.ResponseWriter, r *http.Request, p domain.Principal) {
		if !s.membershipQuery(w, r) {
			return
		}
		var in domain.CreateOrganizationMember
		if !s.decode(w, r, &in) {
			return
		}
		v, e := s.Store.CreateMember(r.Context(), p, r.Header.Get("Idempotency-Key"), in)
		s.mutation(w, r, v, e, 201)
	}))
	mux.HandleFunc("POST /api/v1/members/{principal_id}/update", s.auth(func(w http.ResponseWriter, r *http.Request, p domain.Principal) {
		if !s.membershipQuery(w, r) {
			return
		}
		var in domain.UpdateOrganizationMember
		if !s.decode(w, r, &in) {
			return
		}
		v, e := s.Store.UpdateMember(r.Context(), p, r.PathValue("principal_id"), r.Header.Get("Idempotency-Key"), in)
		s.mutation(w, r, v, e, 200)
	}))
	mux.HandleFunc("POST /api/v1/members/{principal_id}/revoke-credentials", s.auth(func(w http.ResponseWriter, r *http.Request, p domain.Principal) {
		if !s.membershipQuery(w, r) {
			return
		}
		var in domain.RevokeMemberCredentials
		if !s.decode(w, r, &in) {
			return
		}
		v, e := s.Store.RevokeMemberCredentials(r.Context(), p, r.PathValue("principal_id"), r.Header.Get("Idempotency-Key"), in)
		s.mutation(w, r, v, e, 200)
	}))
	mux.HandleFunc("GET /api/v1/membership-audit", s.auth(func(w http.ResponseWriter, r *http.Request, p domain.Principal) {
		if !s.membershipQuery(w, r, "principal_id", "cursor", "limit") {
			return
		}
		n, e := toolListLimit(r, 50)
		if e != nil {
			s.fail(w, r, e)
			return
		}
		v, next, e := s.Store.MembershipAudit(r.Context(), p, r.URL.Query().Get("principal_id"), r.URL.Query().Get("cursor"), n)
		if e != nil {
			s.fail(w, r, e)
			return
		}
		s.ok(w, 200, v, next)
	}))
}

func (s *Server) membershipQuery(w http.ResponseWriter, r *http.Request, names ...string) bool {
	if !s.materialQuery(w, r, names...) {
		return false
	}
	for _, values := range r.URL.Query() {
		if strings.TrimSpace(values[0]) == "" || len(values[0]) > 4096 {
			s.fail(w, r, domain.Err("invalid_request", "Некорректные параметры запроса", 400))
			return false
		}
	}
	return true
}
