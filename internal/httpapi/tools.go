// SPDX-License-Identifier: Elastic-2.0
package httpapi

import (
	"net/http"
	"strconv"

	"agentplatform.local/platform/internal/domain"
)

func (s *Server) mountTools(mux *http.ServeMux) {
	mux.HandleFunc("GET /api/v1/tools", s.auth(s.listTools))
	mux.HandleFunc("GET /api/v1/tasks/{id}/tool-executions", s.auth(s.listToolExecutions))
	mux.HandleFunc("GET /api/v1/tasks/{id}/tool-executions/{execution_id}", s.auth(s.getToolExecution))
	mux.HandleFunc("POST /api/v1/tasks/{id}/tool-executions", s.auth(s.prepareToolExecution))
	mux.HandleFunc("POST /api/v1/tasks/{id}/tool-executions/{execution_id}/decide", s.auth(s.decideToolExecution))
	mux.HandleFunc("POST /api/v1/tasks/{id}/tool-executions/{execution_id}/dispatch", s.auth(s.dispatchToolExecution))
	mux.HandleFunc("POST /api/v1/tasks/{id}/tool-executions/{execution_id}/reconcile", s.auth(s.reconcileToolExecution))
}
func (s *Server) listTools(w http.ResponseWriter, r *http.Request, p domain.Principal) {
	if !s.materialQuery(w, r, "cursor", "limit") {
		return
	}
	n, err := toolListLimit(r, 50)
	if err != nil {
		s.fail(w, r, err)
		return
	}
	values, next, err := s.Store.Tools(r.Context(), p, r.URL.Query().Get("cursor"), n)
	if err != nil {
		s.fail(w, r, err)
		return
	}
	s.ok(w, 200, values, next)
}
func (s *Server) listToolExecutions(w http.ResponseWriter, r *http.Request, p domain.Principal) {
	if !s.materialQuery(w, r, "cursor", "limit") {
		return
	}
	n, err := toolListLimit(r, 20)
	if err != nil {
		s.fail(w, r, err)
		return
	}
	values, next, err := s.Store.ToolExecutions(r.Context(), p, r.PathValue("id"), r.URL.Query().Get("cursor"), n)
	if err != nil {
		s.fail(w, r, err)
		return
	}
	s.ok(w, 200, values, next)
}
func (s *Server) getToolExecution(w http.ResponseWriter, r *http.Request, p domain.Principal) {
	if !s.materialQuery(w, r) {
		return
	}
	value, err := s.Store.GetToolExecution(r.Context(), p, r.PathValue("id"), r.PathValue("execution_id"))
	if err != nil {
		s.fail(w, r, err)
		return
	}
	s.ok(w, 200, value, "")
}
func (s *Server) prepareToolExecution(w http.ResponseWriter, r *http.Request, p domain.Principal) {
	if !s.materialQuery(w, r) {
		return
	}
	var in domain.PrepareToolExecution
	if !s.decode(w, r, &in) {
		return
	}
	value, err := s.Store.PrepareToolExecution(r.Context(), p, r.PathValue("id"), r.Header.Get("Idempotency-Key"), in)
	s.mutation(w, r, value, err, 201)
}
func (s *Server) decideToolExecution(w http.ResponseWriter, r *http.Request, p domain.Principal) {
	if !s.materialQuery(w, r) {
		return
	}
	var in domain.DecideToolExecution
	if !s.decode(w, r, &in) {
		return
	}
	value, err := s.Store.DecideToolExecution(r.Context(), p, r.PathValue("id"), r.PathValue("execution_id"), r.Header.Get("Idempotency-Key"), in)
	s.mutation(w, r, value, err, 200)
}
func (s *Server) dispatchToolExecution(w http.ResponseWriter, r *http.Request, p domain.Principal) {
	if !s.materialQuery(w, r) {
		return
	}
	var in domain.DispatchToolExecution
	if !s.decode(w, r, &in) {
		return
	}
	value, err := s.Store.DispatchToolExecution(r.Context(), p, r.PathValue("id"), r.PathValue("execution_id"), r.Header.Get("Idempotency-Key"), in)
	s.mutation(w, r, value, err, 200)
}
func (s *Server) reconcileToolExecution(w http.ResponseWriter, r *http.Request, p domain.Principal) {
	if !s.materialQuery(w, r) {
		return
	}
	var in domain.ReconcileToolExecution
	if !s.decode(w, r, &in) {
		return
	}
	value, err := s.Store.ReconcileToolExecution(r.Context(), p, r.PathValue("id"), r.PathValue("execution_id"), r.Header.Get("Idempotency-Key"), in)
	s.mutation(w, r, value, err, 200)
}

func toolListLimit(r *http.Request, fallback int) (int, error) {
	if !r.URL.Query().Has("limit") {
		return fallback, nil
	}
	n, e := strconv.Atoi(r.URL.Query().Get("limit"))
	if e != nil || n < 1 || n > 100 {
		return 0, domain.Err("invalid_request", "limit должен быть от 1 до 100", 400)
	}
	return n, nil
}
