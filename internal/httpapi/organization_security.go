// SPDX-License-Identifier: Elastic-2.0
package httpapi

import (
	"net/http"

	"agentplatform.local/platform/internal/domain"
)

func (s *Server) loginPolicyRuntime() domain.LoginPolicyRuntime {
	return domain.LoginPolicyRuntime{TrustedIssuer: s.Login.TrustedIssuer()}
}

func (s *Server) mountOrganizationSecurity(mux *http.ServeMux) {
	mux.HandleFunc("GET /api/v1/login-policy", s.auth(func(w http.ResponseWriter, r *http.Request, p domain.Principal) {
		if !s.membershipQuery(w, r) {
			return
		}
		v, e := s.Store.LoginPolicy(r.Context(), p, s.loginPolicyRuntime())
		if e != nil {
			s.fail(w, r, e)
			return
		}
		s.ok(w, 200, v, "")
	}))
	mux.HandleFunc("POST /api/v1/login-policy/update", s.auth(func(w http.ResponseWriter, r *http.Request, p domain.Principal) {
		if !s.membershipQuery(w, r) {
			return
		}
		var in domain.UpdateOrganizationLoginPolicy
		if !s.decode(w, r, &in) {
			return
		}
		v, e := s.Store.UpdateLoginPolicy(r.Context(), p, r.Header.Get("Idempotency-Key"), in, s.loginPolicyRuntime())
		s.mutation(w, r, v, e, 200)
	}))
	mux.HandleFunc("GET /api/v1/service-accounts", s.auth(func(w http.ResponseWriter, r *http.Request, p domain.Principal) {
		if !s.membershipQuery(w, r, "cursor", "limit") {
			return
		}
		n, e := toolListLimit(r, 50)
		if e != nil {
			s.fail(w, r, e)
			return
		}
		v, next, e := s.Store.ServiceAccounts(r.Context(), p, r.URL.Query().Get("cursor"), n)
		if e != nil {
			s.fail(w, r, e)
			return
		}
		s.ok(w, 200, v, next)
	}))
	mux.HandleFunc("POST /api/v1/service-accounts", s.auth(func(w http.ResponseWriter, r *http.Request, p domain.Principal) {
		if !s.membershipQuery(w, r) {
			return
		}
		var in domain.CreateServiceAccount
		if !s.decode(w, r, &in) {
			return
		}
		v, e := s.Store.CreateServiceAccount(r.Context(), p, r.Header.Get("Idempotency-Key"), in)
		s.mutation(w, r, v, e, 201)
	}))
	mux.HandleFunc("GET /api/v1/service-accounts/{principal_id}", s.auth(func(w http.ResponseWriter, r *http.Request, p domain.Principal) {
		if !s.membershipQuery(w, r) {
			return
		}
		v, e := s.Store.ServiceAccount(r.Context(), p, r.PathValue("principal_id"))
		if e != nil {
			s.fail(w, r, e)
			return
		}
		s.ok(w, 200, v, "")
	}))
	mux.HandleFunc("POST /api/v1/service-accounts/{principal_id}/update", s.auth(func(w http.ResponseWriter, r *http.Request, p domain.Principal) {
		if !s.membershipQuery(w, r) {
			return
		}
		var in domain.UpdateServiceAccount
		if !s.decode(w, r, &in) {
			return
		}
		v, e := s.Store.UpdateServiceAccount(r.Context(), p, r.PathValue("principal_id"), r.Header.Get("Idempotency-Key"), in)
		s.mutation(w, r, v, e, 200)
	}))
	mux.HandleFunc("GET /api/v1/service-accounts/{principal_id}/keys", s.auth(func(w http.ResponseWriter, r *http.Request, p domain.Principal) {
		if !s.membershipQuery(w, r, "cursor", "limit") {
			return
		}
		n, e := toolListLimit(r, 50)
		if e != nil {
			s.fail(w, r, e)
			return
		}
		v, next, e := s.Store.ServiceKeys(r.Context(), p, r.PathValue("principal_id"), r.URL.Query().Get("cursor"), n)
		if e != nil {
			s.fail(w, r, e)
			return
		}
		s.ok(w, 200, v, next)
	}))
	mux.HandleFunc("GET /api/v1/service-accounts/{principal_id}/keys/{key_id}", s.auth(func(w http.ResponseWriter, r *http.Request, p domain.Principal) {
		if !s.membershipQuery(w, r) {
			return
		}
		v, e := s.Store.ServiceKey(r.Context(), p, r.PathValue("principal_id"), r.PathValue("key_id"))
		if e != nil {
			s.fail(w, r, e)
			return
		}
		s.ok(w, 200, v, "")
	}))
	mux.HandleFunc("POST /api/v1/service-accounts/{principal_id}/keys/{key_id}/revoke", s.auth(func(w http.ResponseWriter, r *http.Request, p domain.Principal) {
		if !s.membershipQuery(w, r) {
			return
		}
		var in domain.RevokeServiceKey
		if !s.decode(w, r, &in) {
			return
		}
		v, e := s.Store.RevokeServiceKey(r.Context(), p, r.PathValue("principal_id"), r.PathValue("key_id"), r.Header.Get("Idempotency-Key"), in)
		s.mutation(w, r, v, e, 200)
	}))
	mux.HandleFunc("GET /api/v1/security-audit", s.auth(func(w http.ResponseWriter, r *http.Request, p domain.Principal) {
		if !s.membershipQuery(w, r, "principal_id", "cursor", "limit") {
			return
		}
		n, e := toolListLimit(r, 50)
		if e != nil {
			s.fail(w, r, e)
			return
		}
		v, next, e := s.Store.SecurityAudit(r.Context(), p, r.URL.Query().Get("principal_id"), r.URL.Query().Get("cursor"), n)
		if e != nil {
			s.fail(w, r, e)
			return
		}
		s.ok(w, 200, v, next)
	}))
}
