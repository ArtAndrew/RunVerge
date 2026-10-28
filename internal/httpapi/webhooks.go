// SPDX-License-Identifier: Elastic-2.0
package httpapi

import (
	"context"
	"encoding/json"
	"net/http"
	"strconv"

	"agentplatform.local/platform/internal/domain"
)

// Wired into the main handler only with the future complete webhook consumer contract. The
// module itself uses ordinary auth/version/CSRF/query/body/idempotency guards.
func (s *Server) mountWebhooks(mux *http.ServeMux) {
	mux.HandleFunc("GET /api/v1/webhook-destinations", webhookList(s, "", func(ctx context.Context, p domain.Principal, _, _ string, cursor string, limit int) ([]domain.WebhookDestination, string, error) {
		return s.Store.WebhookDestinations(ctx, p, cursor, limit)
	}))
	const base = "/api/v1/tasks/{id}"
	mux.HandleFunc("GET "+base+"/webhook-subscriptions", webhookList(s, "", func(ctx context.Context, p domain.Principal, task, _ string, cursor string, limit int) ([]domain.WebhookSubscription, string, error) {
		return s.Store.WebhookSubscriptions(ctx, p, task, cursor, limit)
	}))
	mux.HandleFunc("POST "+base+"/webhook-subscriptions", webhookMutation(s, "", 201, func(ctx context.Context, p domain.Principal, task, _, key string, in domain.CreateWebhookSubscription) (json.RawMessage, error) {
		return s.Store.CreateWebhookSubscription(ctx, p, task, key, in)
	}))
	mux.HandleFunc("GET "+base+"/webhook-subscriptions/{subscription_id}", webhookExact(s, "subscription_id", s.Store.WebhookSubscription))
	mux.HandleFunc("POST "+base+"/webhook-subscriptions/{subscription_id}/revoke", webhookMutation(s, "subscription_id", 200, s.Store.RevokeWebhookSubscription))
	mux.HandleFunc("GET "+base+"/webhook-deliveries", webhookList(s, "", func(ctx context.Context, p domain.Principal, task, _ string, cursor string, limit int) ([]domain.WebhookDelivery, string, error) {
		return s.Store.WebhookDeliveries(ctx, p, task, cursor, limit)
	}))
	mux.HandleFunc("GET "+base+"/webhook-deliveries/{delivery_id}", webhookExact(s, "delivery_id", s.Store.WebhookDelivery))
	mux.HandleFunc("GET "+base+"/webhook-deliveries/{delivery_id}/attempts", webhookList(s, "delivery_id", s.Store.WebhookAttempts))
	mux.HandleFunc("POST "+base+"/webhook-deliveries/{delivery_id}/retry", webhookMutation(s, "delivery_id", 200, s.Store.RetryWebhookDelivery))
}
func webhookList[T any](s *Server, key string, read func(context.Context, domain.Principal, string, string, string, int) ([]T, string, error)) http.HandlerFunc {
	return s.auth(func(w http.ResponseWriter, r *http.Request, p domain.Principal) {
		if !s.webhookQuery(w, r, "cursor", "limit") {
			return
		}
		limit, err := toolListLimit(r, 50)
		if err != nil {
			s.fail(w, r, err)
			return
		}
		v, next, err := read(r.Context(), p, r.PathValue("id"), r.PathValue(key), r.URL.Query().Get("cursor"), limit)
		if err != nil {
			s.fail(w, r, err)
			return
		}
		s.ok(w, 200, v, next)
	})
}
func webhookExact[T any](s *Server, key string, read func(context.Context, domain.Principal, string, string) (T, error)) http.HandlerFunc {
	return s.auth(func(w http.ResponseWriter, r *http.Request, p domain.Principal) {
		if !s.webhookQuery(w, r) {
			return
		}
		v, err := read(r.Context(), p, r.PathValue("id"), r.PathValue(key))
		if err != nil {
			s.fail(w, r, err)
			return
		}
		s.ok(w, 200, v, "")
	})
}
func webhookMutation[T any](s *Server, key string, status int, mutate func(context.Context, domain.Principal, string, string, string, T) (json.RawMessage, error)) http.HandlerFunc {
	return s.auth(func(w http.ResponseWriter, r *http.Request, p domain.Principal) {
		if !s.webhookQuery(w, r) {
			return
		}
		if len(r.Header.Values("Idempotency-Key")) != 1 {
			s.fail(w, r, domain.Err("invalid_request", "Нужен один Idempotency-Key", 400))
			return
		}
		var in T
		if !s.decode(w, r, &in) {
			return
		}
		v, err := mutate(r.Context(), p, r.PathValue("id"), r.PathValue(key), r.Header.Get("Idempotency-Key"), in)
		s.mutation(w, r, v, err, status)
	})
}

func (s *Server) webhookQuery(w http.ResponseWriter, r *http.Request, names ...string) bool {
	if r.URL.ForceQuery || len(r.URL.RawQuery) > 2048 {
		s.fail(w, r, domain.Err("invalid_request", "Некорректные параметры уведомлений", 400))
		return false
	}
	if !s.materialQuery(w, r, names...) {
		return false
	}
	for name, values := range r.URL.Query() {
		invalid := values[0] == ""
		if name == "limit" {
			n, err := strconv.Atoi(values[0])
			invalid = invalid || err != nil || n < 1 || n > 100 || strconv.Itoa(n) != values[0]
		}
		if name == "cursor" {
			invalid = invalid || len(values[0]) > 512
		}
		if invalid {
			s.fail(w, r, domain.Err("invalid_request", "Некорректные параметры уведомлений", 400))
			return false
		}
	}
	return true
}
