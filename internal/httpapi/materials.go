// SPDX-License-Identifier: Elastic-2.0
package httpapi

import (
	"net/http"
	"net/url"
	"strconv"

	"agentplatform.local/platform/internal/domain"
)

func (s *Server) materialQuery(w http.ResponseWriter, r *http.Request, names ...string) bool {
	values, e := url.ParseQuery(r.URL.RawQuery)
	allowed := map[string]bool{}
	for _, name := range names {
		allowed[name] = true
	}
	if e == nil {
		for name, entries := range values {
			if !allowed[name] || len(entries) != 1 {
				e = domain.Err("invalid_request", "Некорректные параметры запроса", 400)
				break
			}
		}
	}
	if e != nil {
		s.fail(w, r, domain.Err("invalid_request", "Некорректные параметры запроса", 400))
		return false
	}
	return true
}

func (s *Server) mountMaterials(mux *http.ServeMux) {
	mux.HandleFunc("POST /api/v1/tasks/{id}/materials", s.auth(s.createMaterial))
	mux.HandleFunc("GET /api/v1/tasks/{id}/materials", s.auth(s.listMaterials))
	mux.HandleFunc("GET /api/v1/tasks/{id}/materials/{material_id}", s.auth(s.getMaterial))
	mux.HandleFunc("POST /api/v1/tasks/{id}/materials/{material_id}/revisions", s.auth(s.reviseMaterial))
	mux.HandleFunc("GET /api/v1/tasks/{id}/materials/{material_id}/revisions", s.auth(s.listMaterialRevisions))
	mux.HandleFunc("GET /api/v1/tasks/{id}/materials/{material_id}/revisions/{version}", s.auth(s.materialVersion))
	mux.HandleFunc("POST /api/v1/tasks/{id}/materials/{material_id}/publish", s.auth(s.publishMaterial))
	mux.HandleFunc("GET /api/v1/tasks/{id}/artifacts/{artifact_id}", s.auth(s.artifactVersion))
}

func (s *Server) createMaterial(w http.ResponseWriter, r *http.Request, p domain.Principal) {
	if !s.materialQuery(w, r) {
		return
	}
	var in domain.CreateMaterial
	if !s.decode(w, r, &in) {
		return
	}
	value, e := s.Store.CreateMaterial(r.Context(), p, r.PathValue("id"), r.Header.Get("Idempotency-Key"), in)
	s.mutation(w, r, value, e, 201)
}
func (s *Server) reviseMaterial(w http.ResponseWriter, r *http.Request, p domain.Principal) {
	if !s.materialQuery(w, r) {
		return
	}
	var in domain.ReviseMaterial
	if !s.decode(w, r, &in) {
		return
	}
	value, e := s.Store.ReviseMaterial(r.Context(), p, r.PathValue("id"), r.PathValue("material_id"), r.Header.Get("Idempotency-Key"), in)
	s.mutation(w, r, value, e, 201)
}
func (s *Server) publishMaterial(w http.ResponseWriter, r *http.Request, p domain.Principal) {
	if !s.materialQuery(w, r) {
		return
	}
	var in domain.PublishMaterial
	if !s.decode(w, r, &in) {
		return
	}
	value, e := s.Store.PublishMaterial(r.Context(), p, r.PathValue("id"), r.PathValue("material_id"), r.Header.Get("Idempotency-Key"), in)
	s.mutation(w, r, value, e, 200)
}
func (s *Server) listMaterials(w http.ResponseWriter, r *http.Request, p domain.Principal) {
	if !s.materialQuery(w, r, "cursor", "limit") {
		return
	}
	n, e := limit(r)
	if e != nil {
		s.fail(w, r, e)
		return
	}
	values, next, e := s.Store.Materials(r.Context(), p, r.PathValue("id"), r.URL.Query().Get("cursor"), n)
	if e != nil {
		s.fail(w, r, e)
		return
	}
	s.ok(w, 200, values, next)
}
func (s *Server) getMaterial(w http.ResponseWriter, r *http.Request, p domain.Principal) {
	if !s.materialQuery(w, r) {
		return
	}
	value, e := s.Store.GetMaterial(r.Context(), p, r.PathValue("id"), r.PathValue("material_id"))
	if e != nil {
		s.fail(w, r, e)
		return
	}
	s.ok(w, 200, value, "")
}
func (s *Server) listMaterialRevisions(w http.ResponseWriter, r *http.Request, p domain.Principal) {
	if !s.materialQuery(w, r, "cursor", "limit") {
		return
	}
	n, e := limit(r)
	if e != nil {
		s.fail(w, r, e)
		return
	}
	values, next, e := s.Store.MaterialRevisions(r.Context(), p, r.PathValue("id"), r.PathValue("material_id"), r.URL.Query().Get("cursor"), n)
	if e != nil {
		s.fail(w, r, e)
		return
	}
	s.ok(w, 200, values, next)
}
func (s *Server) materialVersion(w http.ResponseWriter, r *http.Request, p domain.Principal) {
	if !s.materialQuery(w, r) {
		return
	}
	v, e := strconv.Atoi(r.PathValue("version"))
	if e != nil || v < 1 || v > domain.MaxMaterialRevisions {
		s.fail(w, r, domain.Err("invalid_request", "Некорректная версия материала", 400))
		return
	}
	value, e := s.Store.MaterialVersion(r.Context(), p, r.PathValue("id"), r.PathValue("material_id"), v)
	if e != nil {
		s.fail(w, r, e)
		return
	}
	s.ok(w, 200, value, "")
}
func (s *Server) artifactVersion(w http.ResponseWriter, r *http.Request, p domain.Principal) {
	if !s.materialQuery(w, r, "version") {
		return
	}
	v := 1
	if raw := r.URL.Query().Get("version"); r.URL.Query().Has("version") {
		n, e := strconv.Atoi(raw)
		if e != nil || n < 1 {
			s.fail(w, r, domain.Err("invalid_request", "Некорректная версия артефакта", 400))
			return
		}
		v = n
	}
	value, e := s.Store.ArtifactVersion(r.Context(), p, r.PathValue("id"), r.PathValue("artifact_id"), v)
	if e != nil {
		s.fail(w, r, e)
		return
	}
	s.ok(w, 200, value, "")
}
