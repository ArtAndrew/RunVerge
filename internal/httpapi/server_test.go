// SPDX-License-Identifier: Elastic-2.0
package httpapi

import (
	"agentplatform.local/platform/internal/domain"
	"io"
	"log/slog"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestRoutesRegisterWithStaticAndAPIFallback(t *testing.T) {
	s := Server{Log: slog.New(slog.NewTextHandler(io.Discard, nil)), StaticDir: t.TempDir()}
	h := s.Handler()
	for path, want := range map[string]int{"/healthz": 200, "/api/missing": 404, "/api/v1/tasks": 401} {
		r := httptest.NewRequest("GET", path, nil)
		w := httptest.NewRecorder()
		h.ServeHTTP(w, r)
		if w.Code != want {
			t.Fatalf("%s: %d", path, w.Code)
		}
		if w.Header().Get("X-Request-ID") == "" {
			t.Fatal("missing request ID")
		}
	}
}
func TestMutationJSONIsUnambiguous(t *testing.T) {
	s := Server{Log: slog.New(slog.NewTextHandler(io.Discard, nil))}
	for _, body := range []string{`null`, `[]`, `{"name":null}`, `{"name":"a","name":"b"}`, `{"name":"a\u0000b"}`, `{} {}`, `{"unknown":true}`, `{"Name":"case_alias"}`} {
		r := httptest.NewRequest("POST", "/", strings.NewReader(body))
		r.Header.Set("Content-Type", "application/json")
		w := httptest.NewRecorder()
		var input domain.CreateAgent
		if s.decode(w, r, &input) || w.Code != 400 {
			t.Errorf("accepted invalid JSON: %s", body)
		}
	}
	r := httptest.NewRequest("POST", "/", strings.NewReader(`{"name":"Agent","instructions":"Use evidence"}`))
	r.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	var input domain.CreateAgent
	if !s.decode(w, r, &input) || input.BudgetMicroUSD != nil {
		t.Fatal("valid optional field rejected")
	}
}
