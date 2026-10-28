// SPDX-License-Identifier: Elastic-2.0
package httpapi

import (
	"agentplatform.local/platform/internal/domain"
	"net/http"
)

func (s *Server) mountModelLoop(mux *http.ServeMux) {
	mux.HandleFunc("GET /api/v1/tasks/{id}/model-tool-proposals", s.auth(s.listModelProposals))
	mux.HandleFunc("GET /api/v1/tasks/{id}/model-tool-proposals/{proposal_id}", s.auth(s.getModelProposal))
	mux.HandleFunc("POST /api/v1/tasks/{id}/continue-model", s.auth(s.continueModel))
}
func (s *Server) listModelProposals(w http.ResponseWriter, r *http.Request, p domain.Principal) {
	if !s.materialQuery(w, r, "cursor", "limit") {
		return
	}
	n, err := toolListLimit(r, 20)
	if err != nil {
		s.fail(w, r, err)
		return
	}
	values, next, err := s.Store.ModelProposals(r.Context(), p, r.PathValue("id"), r.URL.Query().Get("cursor"), n)
	if err != nil {
		s.fail(w, r, err)
		return
	}
	s.ok(w, 200, values, next)
}
func (s *Server) getModelProposal(w http.ResponseWriter, r *http.Request, p domain.Principal) {
	if !s.materialQuery(w, r) {
		return
	}
	value, err := s.Store.ModelProposal(r.Context(), p, r.PathValue("id"), r.PathValue("proposal_id"))
	if err != nil {
		s.fail(w, r, err)
		return
	}
	s.ok(w, 200, value, "")
}
func (s *Server) continueModel(w http.ResponseWriter, r *http.Request, p domain.Principal) {
	if !s.materialQuery(w, r) {
		return
	}
	var input domain.ContinueModelRequest
	if !s.decode(w, r, &input) {
		return
	}
	value, err := s.Store.ContinueModel(r.Context(), p, r.PathValue("id"), r.Header.Get("Idempotency-Key"), input)
	s.mutation(w, r, value, err, 200)
}
