// SPDX-License-Identifier: Elastic-2.0
package httpapi

import (
	"agentplatform.local/platform/internal/domain"
	"agentplatform.local/platform/internal/identity"
	"agentplatform.local/platform/internal/jsonwire"
	"agentplatform.local/platform/internal/storage"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"mime"
	"net/http"
	"reflect"
	"strconv"
	"strings"
	"time"
)

type Server struct {
	Store        *storage.Store
	Log          *slog.Logger
	StaticDir    string
	Login        *identity.LoginService
	eventStreams eventStreamAdmission
}
type requestKey struct{}

func (s *Server) Handler() http.Handler {
	if s.Log == nil {
		s.Log = slog.Default()
	}
	mux := http.NewServeMux()
	s.mountAuthentication(mux)
	s.mountMaterials(mux)
	s.mountTools(mux)
	s.mountModelLoop(mux)
	s.mountAutomation(mux)
	s.mountMemberships(mux)
	s.mountOrganizationSecurity(mux)
	s.mountConfiguration(mux)
	s.mountEventStream(mux)
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, r *http.Request) {
		s.ok(w, 200, map[string]any{"status": "ok", "contract_version": domain.ContractVersion, "development_fixture": true}, "")
	})
	mux.HandleFunc("GET /readyz", func(w http.ResponseWriter, r *http.Request) {
		ctx, cancel := context.WithTimeout(r.Context(), time.Second)
		defer cancel()
		if e := s.Store.CheckRuntimeAdmission(ctx); e != nil {
			s.fail(w, r, domain.Err("unavailable", "Хранилище недоступно", 503))
			return
		}
		s.ok(w, 200, map[string]string{"status": "ready"}, "")
	})
	mux.HandleFunc("POST /api/v1/agents", s.auth(s.createAgent))
	mux.HandleFunc("GET /api/v1/capabilities", s.auth(s.productCapabilities))
	mux.HandleFunc("GET /api/v1/me", s.auth(func(w http.ResponseWriter, r *http.Request, p domain.Principal) {
		if !s.materialQuery(w, r) {
			return
		}
		s.ok(w, 200, domain.CurrentIdentity{PrincipalID: p.ID, OrganizationID: p.OrganizationID, Role: p.Role, PrincipalKind: p.Kind, Permissions: domain.EffectivePermissions(p)}, "")
	}))
	mux.HandleFunc("GET /api/v1/agents", s.auth(s.listAgents))
	mux.HandleFunc("GET /api/v1/models", s.auth(s.listModels))
	mux.HandleFunc("POST /api/v1/agents/{id}/publish", s.auth(s.publishAgent))
	mux.HandleFunc("POST /api/v1/tasks", s.auth(s.createTask))
	mux.HandleFunc("GET /api/v1/tasks", s.auth(s.listTasks))
	mux.HandleFunc("GET /api/v1/tasks/{id}", s.auth(s.getTask))
	mux.HandleFunc("POST /api/v1/tasks/{id}/cancel", s.auth(s.cancelTask))
	mux.HandleFunc("GET /api/v1/tasks/{id}/events", s.auth(s.events))
	mux.HandleFunc("GET /api/v1/tasks/{id}/artifacts", s.auth(s.artifacts))
	for _, kind := range []string{"runs", "operations", "actions", "approvals", "costs"} {
		mux.HandleFunc("GET /api/v1/tasks/{id}/"+kind, s.auth(s.inspect(kind)))
	}
	mux.HandleFunc("POST /api/v1/tasks/{id}/review", s.auth(s.reviewTask))
	mux.HandleFunc("POST /api/v1/tasks/{id}/retry", s.auth(s.retryTask))
	mux.HandleFunc("GET /api/v1/tasks/{id}/candidates", s.auth(s.candidates))
	mux.HandleFunc("GET /api/v1/tasks/{id}/decisions", s.auth(s.decisions))
	mux.HandleFunc("GET /api/v1/tasks/{id}/children", s.auth(s.children))
	mux.HandleFunc("/api/", func(w http.ResponseWriter, r *http.Request) { s.fail(w, r, domain.NotFound) })
	if s.StaticDir != "" {
		files := http.FileServer(http.Dir(s.StaticDir))
		mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
			if r.Method != http.MethodGet && r.Method != http.MethodHead {
				w.WriteHeader(http.StatusMethodNotAllowed)
				return
			}
			files.ServeHTTP(w, r)
		})
	}
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		id := domain.NewID("req")
		r = r.WithContext(context.WithValue(r.Context(), requestKey{}, id))
		w.Header().Set("X-Request-ID", id)
		if strings.HasPrefix(r.URL.Path, "/api/v1/") {
			w.Header().Set(domain.APIContractHeader, domain.ContractVersion)
		}
		w.Header().Set("X-Content-Type-Options", "nosniff")
		w.Header().Set("Referrer-Policy", "no-referrer")
		// A document's no-referrer policy makes Chromium send Origin:null for
		// ordinary form POST navigation. Preserve exact same-origin form origin;
		// auth/callback/API responses still suppress all referrers.
		if !strings.HasPrefix(r.URL.Path, "/auth/") && !strings.HasPrefix(r.URL.Path, "/api/") {
			w.Header().Set("Referrer-Policy", "same-origin")
		}
		w.Header().Set("Cache-Control", "no-store")
		formAction := "'self'"
		if s.Login != nil {
			formAction += " " + s.Login.AuthorizationOrigin()
		}
		w.Header().Set("Content-Security-Policy", "default-src 'self'; script-src 'self'; style-src 'self' 'unsafe-inline'; img-src 'self' data:; connect-src 'self'; frame-ancestors 'none'; base-uri 'none'; form-action "+formAction)
		if strings.ContainsRune(r.URL.Path, '\x00') {
			s.fail(w, r, domain.Err("invalid_request", "Некорректный путь", 400))
			return
		}
		defer func() {
			if v := recover(); v != nil {
				s.Log.Error("request panic", "request_id", id)
				s.fail(w, r, errors.New("request panic"))
			}
		}()
		mux.ServeHTTP(w, r)
	})
}

type authorizedHandler func(http.ResponseWriter, *http.Request, domain.Principal)

func (s *Server) decode(w http.ResponseWriter, r *http.Request, out any) bool {
	media, _, mediaErr := mime.ParseMediaType(r.Header.Get("Content-Type"))
	if mediaErr != nil || media != "application/json" {
		s.fail(w, r, domain.Err("invalid_request", "Требуется application/json", 400))
		return false
	}
	r.Body = http.MaxBytesReader(w, r.Body, 1<<20)
	body, e := io.ReadAll(r.Body)
	if e != nil || !jsonwire.ValidUnicode(body) || len(bytes.TrimSpace(body)) == 0 || bytes.TrimSpace(body)[0] != '{' {
		s.fail(w, r, domain.Err("invalid_request", "Ожидался JSON-объект допустимого размера", 400))
		return false
	}
	check := json.NewDecoder(bytes.NewReader(body))
	check.UseNumber()
	if e = checkJSONValue(check); e != nil {
		s.fail(w, r, domain.Err("invalid_request", "JSON содержит повторные поля, null или недопустимый текст", 400))
		return false
	}
	if _, e = check.Token(); e != io.EOF {
		s.fail(w, r, domain.Err("invalid_request", "Ожидался один JSON-объект", 400))
		return false
	}
	var fields map[string]json.RawMessage
	if e = json.Unmarshal(body, &fields); e != nil {
		s.fail(w, r, domain.Err("invalid_request", "Некорректный JSON", 400))
		return false
	}
	allowed := map[string]bool{}
	typ := reflect.TypeOf(out).Elem()
	for i := 0; i < typ.NumField(); i++ {
		name := strings.Split(typ.Field(i).Tag.Get("json"), ",")[0]
		allowed[name] = true
	}
	for name := range fields {
		if !allowed[name] {
			s.fail(w, r, domain.Err("invalid_request", "Неизвестное поле JSON", 400))
			return false
		}
	}
	// These security commands replace a complete configuration snapshot. An
	// omitted empty issuer must not silently turn a partial patch into a reset.
	switch typ.Name() {
	case "UpdateOrganizationLoginPolicy", "CreateServiceAccount", "UpdateServiceAccount", "RevokeServiceKey":
		for name := range allowed {
			if _, present := fields[name]; !present {
				s.fail(w, r, domain.Err("invalid_request", "Нужны все поля команды безопасности", 400))
				return false
			}
		}
	}
	if e = canonicalMutationFields(body, typ); e != nil {
		s.fail(w, r, domain.Err("invalid_request", "JSON содержит неизвестное поле или неверное имя вложенного поля", 400))
		return false
	}
	d := json.NewDecoder(bytes.NewReader(body))
	d.DisallowUnknownFields()
	if e := d.Decode(out); e != nil {
		s.fail(w, r, domain.Err("invalid_request", "Некорректный JSON или неизвестное поле", 400))
		return false
	}
	if e := d.Decode(&struct{}{}); e != io.EOF {
		s.fail(w, r, domain.Err("invalid_request", "Ожидался один JSON-объект", 400))
		return false
	}
	return true
}
func checkJSONValue(d *json.Decoder) error { return checkJSONDepth(d, 0) }
func checkJSONDepth(d *json.Decoder, depth int) error {
	if depth > 32 {
		return errors.New("JSON nesting limit exceeded")
	}
	tok, e := d.Token()
	if e != nil {
		return e
	}
	if tok == nil {
		return errors.New("null is not supported")
	}
	switch value := tok.(type) {
	case string:
		if strings.ContainsRune(value, '\x00') {
			return errors.New("invalid text")
		}
	case json.Delim:
		switch value {
		case '{':
			seen := map[string]bool{}
			for d.More() {
				key, e := d.Token()
				if e != nil {
					return e
				}
				name, ok := key.(string)
				if !ok || seen[name] {
					return errors.New("duplicate property")
				}
				seen[name] = true
				if e = checkJSONDepth(d, depth+1); e != nil {
					return e
				}
			}
			end, e := d.Token()
			if e != nil || end != json.Delim('}') {
				return errors.New("invalid object")
			}
		case '[':
			for d.More() {
				if e = checkJSONDepth(d, depth+1); e != nil {
					return e
				}
			}
			end, e := d.Token()
			if e != nil || end != json.Delim(']') {
				return errors.New("invalid array")
			}
		default:
			return errors.New("invalid delimiter")
		}
	}
	return nil
}
func (s *Server) ok(w http.ResponseWriter, status int, data any, next string) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	v := map[string]any{"data": data}
	if next != "" {
		v["next_cursor"] = next
	}
	_ = json.NewEncoder(w).Encode(v)
}
func (s *Server) fail(w http.ResponseWriter, r *http.Request, err error) {
	var e *domain.Error
	if !errors.As(err, &e) {
		e = &domain.Error{Code: "internal_error", Message: "Внутренняя ошибка сервера", Status: 500}
		s.Log.Error("request failed", "request_id", r.Context().Value(requestKey{}), "error_type", errorType(err))
	}
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(e.Status)
	_ = json.NewEncoder(w).Encode(map[string]any{"error": map[string]any{"code": e.Code, "message": e.Message, "retryable": e.Retryable, "request_id": r.Context().Value(requestKey{})}})
}
func errorType(err error) string {
	if err == nil {
		return "nil"
	}
	return "storage_or_runtime_error"
}
func (s *Server) mutation(w http.ResponseWriter, r *http.Request, result json.RawMessage, e error, status int) {
	if e != nil {
		s.fail(w, r, e)
		return
	}
	s.ok(w, status, result, "")
}
func (s *Server) createAgent(w http.ResponseWriter, r *http.Request, p domain.Principal) {
	var v domain.CreateAgent
	if !s.decode(w, r, &v) {
		return
	}
	out, e := s.Store.CreateAgent(r.Context(), p, r.Header.Get("Idempotency-Key"), v)
	s.mutation(w, r, out, e, 201)
}
func (s *Server) publishAgent(w http.ResponseWriter, r *http.Request, p domain.Principal) {
	var v struct {
		ExpectedVersion int `json:"expected_version"`
	}
	if !s.decode(w, r, &v) {
		return
	}
	if v.ExpectedVersion < 1 {
		s.fail(w, r, domain.Err("invalid_request", "Нужна expected_version", 400))
		return
	}
	out, e := s.Store.PublishAgent(r.Context(), p, r.PathValue("id"), r.Header.Get("Idempotency-Key"), v.ExpectedVersion)
	s.mutation(w, r, out, e, 200)
}
func (s *Server) createTask(w http.ResponseWriter, r *http.Request, p domain.Principal) {
	var v domain.CreateTask
	if !s.decode(w, r, &v) {
		return
	}
	out, e := s.Store.CreateTask(r.Context(), p, r.Header.Get("Idempotency-Key"), v)
	s.mutation(w, r, out, e, 201)
}
func (s *Server) cancelTask(w http.ResponseWriter, r *http.Request, p domain.Principal) {
	var v struct{}
	if !s.decode(w, r, &v) {
		return
	}
	out, e := s.Store.CancelTask(r.Context(), p, r.PathValue("id"), r.Header.Get("Idempotency-Key"))
	s.mutation(w, r, out, e, 200)
}
func (s *Server) getTask(w http.ResponseWriter, r *http.Request, p domain.Principal) {
	out, e := s.Store.GetTask(r.Context(), p, r.PathValue("id"))
	if e != nil {
		s.fail(w, r, e)
		return
	}
	s.ok(w, 200, out, "")
}
func limit(r *http.Request) (int, error) {
	if r.URL.Query().Get("limit") == "" {
		return 50, nil
	}
	v, e := strconv.Atoi(r.URL.Query().Get("limit"))
	if e != nil || v < 1 || v > 100 {
		return 0, domain.Err("invalid_request", "limit должен быть от 1 до 100", 400)
	}
	return v, nil
}
func (s *Server) listTasks(w http.ResponseWriter, r *http.Request, p domain.Principal) {
	n, e := limit(r)
	if e != nil {
		s.fail(w, r, e)
		return
	}
	out, next, e := s.Store.ListTasks(r.Context(), p, r.URL.Query().Get("cursor"), n)
	if e != nil {
		s.fail(w, r, e)
		return
	}
	s.ok(w, 200, out, next)
}
func (s *Server) listAgents(w http.ResponseWriter, r *http.Request, p domain.Principal) {
	n, e := limit(r)
	if e != nil {
		s.fail(w, r, e)
		return
	}
	out, next, e := s.Store.ListAgents(r.Context(), p, r.URL.Query().Get("cursor"), n)
	if e != nil {
		s.fail(w, r, e)
		return
	}
	s.ok(w, 200, out, next)
}
func (s *Server) events(w http.ResponseWriter, r *http.Request, p domain.Principal) {
	if !s.eventQuery(w, r, "after", "limit") {
		return
	}
	n, e := limit(r)
	if e != nil {
		s.fail(w, r, e)
		return
	}
	var after int64
	if raw := r.URL.Query().Get("after"); raw != "" {
		after, e = strconv.ParseInt(raw, 10, 64)
		if e != nil || after < 0 || strconv.FormatInt(after, 10) != raw {
			s.fail(w, r, domain.Err("invalid_request", "Некорректная последовательность событий", 400))
			return
		}
	}
	out, next, e := s.Store.Events(r.Context(), p, r.PathValue("id"), after, n)
	if e != nil {
		s.fail(w, r, e)
		return
	}
	s.ok(w, 200, out, next)
}
func (s *Server) artifacts(w http.ResponseWriter, r *http.Request, p domain.Principal) {
	if !s.materialQuery(w, r, "cursor", "limit") {
		return
	}
	n := 20
	if r.URL.Query().Has("limit") {
		var e error
		n, e = limit(r)
		if e != nil {
			s.fail(w, r, e)
			return
		}
	}
	out, next, e := s.Store.ArtifactsPage(r.Context(), p, r.PathValue("id"), r.URL.Query().Get("cursor"), n)
	if e != nil {
		s.fail(w, r, e)
		return
	}
	s.ok(w, 200, out, next)
}
