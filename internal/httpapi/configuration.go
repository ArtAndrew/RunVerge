// SPDX-License-Identifier: Elastic-2.0
package httpapi

import (
	"context"
	"encoding/json"
	"net/http"

	"agentplatform.local/platform/internal/domain"
)

func (s *Server) mountConfiguration(mux *http.ServeMux) {
	const base = "/api/v1/configuration"
	mux.HandleFunc("GET "+base+"/catalog", s.auth(func(w http.ResponseWriter, r *http.Request, p domain.Principal) {
		if !s.membershipQuery(w, r) {
			return
		}
		v, err := s.Store.ConfigurationCatalog(r.Context(), p)
		if err != nil {
			s.fail(w, r, err)
			return
		}
		s.ok(w, 200, v, "")
	}))
	mux.HandleFunc("GET "+base+"/destinations", configurationList(s, s.Store.ConfigurationDestinations))
	mux.HandleFunc("GET "+base+"/resources", configurationList(s, s.Store.ConfigurationResources))
	mux.HandleFunc("GET "+base+"/grants", configurationList(s, s.Store.ConfigurationGrants))
	mux.HandleFunc("GET "+base+"/grants/{grant_id}", configurationExact(s, "grant_id", s.Store.ConfigurationGrant))
	mux.HandleFunc("POST "+base+"/grants", configurationMutation(s, "", 201, func(ctx context.Context, p domain.Principal, _, key string, in domain.CreateConfigurationGrant) (json.RawMessage, error) {
		return s.Store.CreateConfigurationGrant(ctx, p, key, in)
	}))
	mux.HandleFunc("POST "+base+"/grants/{grant_id}/revoke", configurationMutation(s, "grant_id", 200, s.Store.RevokeConfigurationGrant))
	mux.HandleFunc("GET "+base+"/plans", configurationList(s, s.Store.ConfigurationPlans))
	mux.HandleFunc("GET "+base+"/plans/{plan_id}", configurationExact(s, "plan_id", s.Store.ConfigurationPlan))
	mux.HandleFunc("POST "+base+"/plans", configurationMutation(s, "", 201, func(ctx context.Context, p domain.Principal, _, key string, in domain.CreateConfigurationPlan) (json.RawMessage, error) {
		return s.Store.CreateConfigurationPlan(ctx, p, key, in)
	}))
	mux.HandleFunc("POST "+base+"/plans/{plan_id}/validate", configurationMutation(s, "plan_id", 200, s.Store.ValidateConfigurationPlan))
	mux.HandleFunc("POST "+base+"/plans/{plan_id}/approve", configurationMutation(s, "plan_id", 201, s.Store.ApproveConfigurationPlan))
	mux.HandleFunc("POST "+base+"/plans/{plan_id}/apply", configurationMutation(s, "plan_id", 200, s.Store.ApplyConfigurationPlan))
	mux.HandleFunc("POST "+base+"/plans/{plan_id}/verify", configurationMutation(s, "plan_id", 201, s.Store.StartConfigurationVerification))
	mux.HandleFunc("GET "+base+"/verifications/{verification_id}", configurationExact(s, "verification_id", s.Store.ConfigurationVerification))
	mux.HandleFunc("GET "+base+"/verifications", s.auth(func(w http.ResponseWriter, r *http.Request, p domain.Principal) {
		if !s.membershipQuery(w, r, "plan_id", "cursor", "limit") {
			return
		}
		n, err := toolListLimit(r, 50)
		if err != nil {
			s.fail(w, r, err)
			return
		}
		v, next, err := s.Store.ConfigurationVerifications(r.Context(), p, r.URL.Query().Get("plan_id"), r.URL.Query().Get("cursor"), n)
		if err != nil {
			s.fail(w, r, err)
			return
		}
		s.ok(w, 200, v, next)
	}))
	mux.HandleFunc("GET "+base+"/audit", configurationList(s, s.Store.ConfigurationAudit))
}

func configurationList[T any](s *Server, read func(context.Context, domain.Principal, string, int) ([]T, string, error)) http.HandlerFunc {
	return s.auth(func(w http.ResponseWriter, r *http.Request, p domain.Principal) {
		if !s.membershipQuery(w, r, "cursor", "limit") {
			return
		}
		n, err := toolListLimit(r, 50)
		if err != nil {
			s.fail(w, r, err)
			return
		}
		v, next, err := read(r.Context(), p, r.URL.Query().Get("cursor"), n)
		if err != nil {
			s.fail(w, r, err)
			return
		}
		s.ok(w, 200, v, next)
	})
}

func configurationExact[T any](s *Server, key string, read func(context.Context, domain.Principal, string) (T, error)) http.HandlerFunc {
	return s.auth(func(w http.ResponseWriter, r *http.Request, p domain.Principal) {
		if !s.membershipQuery(w, r) {
			return
		}
		v, err := read(r.Context(), p, r.PathValue(key))
		if err != nil {
			s.fail(w, r, err)
			return
		}
		s.ok(w, 200, v, "")
	})
}

func configurationMutation[T any](s *Server, pathKey string, status int, mutate func(context.Context, domain.Principal, string, string, T) (json.RawMessage, error)) http.HandlerFunc {
	return s.auth(func(w http.ResponseWriter, r *http.Request, p domain.Principal) {
		if !s.membershipQuery(w, r) {
			return
		}
		var in T
		if !s.decode(w, r, &in) {
			return
		}
		v, err := mutate(r.Context(), p, r.PathValue(pathKey), r.Header.Get("Idempotency-Key"), in)
		s.mutation(w, r, v, err, status)
	})
}
