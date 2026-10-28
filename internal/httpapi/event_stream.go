// SPDX-License-Identifier: Elastic-2.0
package httpapi

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"

	"agentplatform.local/platform/internal/domain"
)

const eventStreamLifetime = 20 * time.Second
const eventStreamWriteTimeout = 3 * time.Second
const eventStreamHeartbeat = 2 * time.Second
const maxEventStreams = 32
const maxPrincipalEventStreams = 4

type eventStreamAdmission struct {
	sync.Mutex
	total      int
	principals map[string]int
}

func (a *eventStreamAdmission) acquire(principal string) bool {
	a.Lock()
	defer a.Unlock()
	if a.total >= maxEventStreams || a.principals[principal] >= maxPrincipalEventStreams {
		return false
	}
	if a.principals == nil {
		a.principals = map[string]int{}
	}
	a.total++
	a.principals[principal]++
	return true
}
func (a *eventStreamAdmission) release(principal string) {
	a.Lock()
	defer a.Unlock()
	a.total--
	a.principals[principal]--
	if a.principals[principal] == 0 {
		delete(a.principals, principal)
	}
}

func (s *Server) checkAPIContract(w http.ResponseWriter, r *http.Request, required bool) bool {
	values := r.Header.Values(domain.APIContractHeader)
	if !required && len(values) == 0 {
		return true
	}
	if len(values) != 1 || values[0] != domain.ContractVersion {
		s.fail(w, r, domain.Err("incompatible_client", "Клиент должен выбрать поддерживаемую версию API: "+domain.ContractVersion, 409))
		return false
	}
	return true
}
func (s *Server) eventQuery(w http.ResponseWriter, r *http.Request, names ...string) bool {
	if len(r.URL.RawQuery) > 2048 || r.URL.ForceQuery || !s.materialQuery(w, r, names...) {
		if len(r.URL.RawQuery) > 2048 || r.URL.ForceQuery {
			s.fail(w, r, domain.Err("invalid_request", "Некорректные параметры потока", 400))
		}
		return false
	}
	for name, values := range r.URL.Query() {
		invalid := values[0] == ""
		if name == "limit" {
			n, err := strconv.Atoi(values[0])
			invalid = invalid || err != nil || n < 1 || n > 100 || strconv.Itoa(n) != values[0]
		}
		if invalid {
			s.fail(w, r, domain.Err("invalid_request", "Некорректный параметр потока", 400))
			return false
		}
	}
	return true
}
func (s *Server) mountEventStream(mux *http.ServeMux) {
	mux.HandleFunc("GET /api/v1/compatibility", func(w http.ResponseWriter, r *http.Request) {
		if !s.checkAPIContract(w, r, false) || !s.eventQuery(w, r) {
			return
		}
		s.ok(w, 200, domain.APICompatibility{APIVersion: domain.ContractVersion, SupportedVersions: []string{domain.ContractVersion}, VersionHeader: domain.APIContractHeader, HeaderRequired: true, EventPayloadVersions: []int{0, 1}, LegacyEventPolicy: "opaque_json_string_read_only", CommandRetryPolicy: "explicit_idempotency_key_no_automatic_mutation_retry"}, "")
	})
	mux.HandleFunc("GET /api/v1/tasks/{id}/event-snapshot", s.auth(s.taskEventSnapshot))
	mux.HandleFunc("GET /api/v1/tasks/{id}/event-feed", s.auth(s.taskEventFeed))
	mux.HandleFunc("GET /api/v1/tasks/{id}/event-stream", s.auth(s.taskEventStream))
}
func (s *Server) taskEventSnapshot(w http.ResponseWriter, r *http.Request, p domain.Principal) {
	if !s.eventQuery(w, r) {
		return
	}
	value, err := s.Store.TaskEventSnapshot(r.Context(), p, r.PathValue("id"))
	if err != nil {
		s.fail(w, r, err)
		return
	}
	if err = s.Store.CheckEventReadAccess(r.Context(), p, r.PathValue("id")); err != nil {
		s.fail(w, r, err)
		return
	}
	s.ok(w, 200, value, "")
}
func (s *Server) taskEventFeed(w http.ResponseWriter, r *http.Request, p domain.Principal) {
	if !s.eventQuery(w, r, "cursor", "limit") {
		return
	}
	n, err := limit(r)
	if err != nil {
		s.fail(w, r, err)
		return
	}
	value, err := s.Store.ReplayTaskEvents(r.Context(), p, r.PathValue("id"), r.URL.Query().Get("cursor"), n)
	if err != nil {
		s.fail(w, r, err)
		return
	}
	if err = s.Store.CheckEventReadAccess(r.Context(), p, r.PathValue("id")); err != nil {
		s.fail(w, r, err)
		return
	}
	s.ok(w, 200, value, "")
}

// This bounded SSE protocol uses fetch/SDK readers, never native EventSource:
// authentication and wire-version headers remain ordinary API headers. Every
// connection expires; clients may reconnect reads from the last complete batch.
func (s *Server) taskEventStream(w http.ResponseWriter, r *http.Request, p domain.Principal) {
	if !s.eventQuery(w, r, "cursor") {
		return
	}
	cursor := r.URL.Query().Get("cursor")
	ids := r.Header.Values("Last-Event-ID")
	if len(ids) > 1 || (len(ids) == 1 && (ids[0] == "" || len(ids[0]) > 1024 || strings.ContainsAny(ids[0], "\r\n\x00") || (cursor != "" && cursor != ids[0]))) {
		s.fail(w, r, domain.Err("invalid_request", "Неоднозначный курсор потока", 400))
		return
	}
	if len(ids) == 1 {
		cursor = ids[0]
	}
	if !s.eventStreams.acquire(p.ID) {
		s.fail(w, r, &domain.Error{Code: "unavailable", Message: "Достигнут предел одновременных потоков", Status: 503, Retryable: true})
		return
	}
	defer s.eventStreams.release(p.ID)
	ctx, cancel := context.WithTimeout(r.Context(), eventStreamLifetime)
	defer cancel()
	wake, err := s.Store.SubscribeTaskEvents(ctx, p.OrganizationID, r.PathValue("id"))
	if err != nil {
		s.fail(w, r, err)
		return
	}
	batch, err := s.Store.ReplayTaskEvents(ctx, p, r.PathValue("id"), cursor, 50)
	if err != nil {
		s.fail(w, r, err)
		return
	}
	if err = s.Store.CheckEventReadAccess(ctx, p, r.PathValue("id")); err != nil {
		s.fail(w, r, err)
		return
	}
	controller := http.NewResponseController(w)
	if err = controller.SetWriteDeadline(time.Now().Add(eventStreamWriteTimeout)); err != nil {
		s.fail(w, r, domain.Err("unavailable", "Транспорт не поддерживает ограниченный поток", 503))
		return
	}
	w.Header().Set("Content-Type", "text/event-stream; charset=utf-8")
	w.Header().Set("X-Accel-Buffering", "no")
	w.WriteHeader(200)
	write := func(event, id string, frame domain.TaskEventStreamFrame) error {
		body, err := json.Marshal(frame)
		if err != nil {
			return err
		}
		if len(body)+len(id)+len(event)+32 > domain.MaxEventFrameBytes {
			return errors.New("event frame exceeds bounded protocol")
		}
		if err = controller.SetWriteDeadline(time.Now().Add(eventStreamWriteTimeout)); err != nil {
			return err
		}
		var text strings.Builder
		fmt.Fprintf(&text, "event: %s\n", event)
		if id != "" {
			fmt.Fprintf(&text, "id: %s\n", id)
		}
		fmt.Fprintf(&text, "data: %s\n\n", body)
		if _, err = fmt.Fprint(w, text.String()); err != nil {
			return err
		}
		return controller.Flush()
	}
	writeError := func(err error) {
		if ctx.Err() != nil {
			return
		}
		e := &domain.Error{Code: "internal_error", Message: "Поток прерван; состояние можно запросить повторно", Status: 500}
		var public *domain.Error
		if errors.As(err, &public) {
			e = public
		}
		_ = write("platform.error", "", domain.TaskEventStreamFrame{FrameVersion: 1, Kind: "error", Error: &domain.EventStreamError{Code: e.Code, Message: e.Message, Retryable: e.Retryable}})
	}
	ticker := time.NewTicker(eventStreamHeartbeat)
	defer ticker.Stop()
	for {
		// A completed short read is followed by a fresh authorization check.
		// Revocation linearizes at that check; no transaction is held for I/O.
		if err = s.Store.CheckEventReadAccess(ctx, p, r.PathValue("id")); err != nil {
			writeError(err)
			return
		}
		kind, name, id := "batch", "platform.events", batch.Cursor
		if batch.ResetRequired {
			kind, name, id = "reset_required", "platform.reset", ""
		}
		if err = write(name, id, domain.TaskEventStreamFrame{FrameVersion: 1, Kind: kind, Batch: &batch}); err != nil {
			return
		}
		if batch.ResetRequired {
			return
		}
		cursor = batch.Cursor
		if !batch.HasMore {
			select {
			case <-ctx.Done():
				return
			case <-wake:
			case <-ticker.C:
				if err = s.Store.CheckEventReadAccess(ctx, p, r.PathValue("id")); err != nil {
					writeError(err)
					return
				}
				if err = controller.SetWriteDeadline(time.Now().Add(eventStreamWriteTimeout)); err != nil {
					return
				}
				if _, err = fmt.Fprint(w, ": heartbeat\n\n"); err != nil {
					return
				}
				if err = controller.Flush(); err != nil {
					return
				}
			}
		}
		batch, err = s.Store.ReplayTaskEvents(ctx, p, r.PathValue("id"), cursor, 50)
		if err != nil {
			writeError(err)
			return
		}
	}
}
