// SPDX-License-Identifier: Elastic-2.0
package httpapi

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"agentplatform.local/platform/internal/domain"
)

// Preview mounting tests the future module without advertising new routes in
// the0.13 Handler. Final0.14 must separately test the ordinary assembled handler.
func TestWebhookHTTPAuthorPreviewLifecycleAndStrictBoundary(t *testing.T) {
	s := browserIntegrationStore(t)
	ctx := context.Background()
	b, err := s.Bootstrap(ctx, "Webhook API preview", 100000)
	if err != nil {
		t.Fatal(err)
	}
	p, err := s.Authenticate(ctx, b.Token, b.OrganizationID)
	if err != nil {
		t.Fatal(err)
	}
	decode := func(raw []byte, out any) {
		t.Helper()
		if err := json.Unmarshal(raw, out); err != nil {
			t.Fatal(err)
		}
	}
	raw, err := s.CreateAgent(ctx, p, domain.NewID("key"), domain.CreateAgent{Name: "Preview source", Instructions: "Fixture"})
	if err != nil {
		t.Fatal(err)
	}
	var a domain.Agent
	decode(raw, &a)
	raw, err = s.PublishAgent(ctx, p, a.ID, domain.NewID("key"), a.Version)
	if err != nil {
		t.Fatal(err)
	}
	decode(raw, &a)
	raw, err = s.CreateTask(ctx, p, domain.NewID("key"), domain.CreateTask{AgentID: a.ID, Goal: "PRIVATE_GOAL_NEVER_EXPORTED", BudgetMicroUSD: 1000})
	if err != nil {
		t.Fatal(err)
	}
	var task domain.Task
	decode(raw, &task)
	profile := domain.WebhookDestinationProfile{Destination: domain.WebhookDestination{ID: "api_receiver", OrganizationID: p.OrganizationID, Version: 1, Name: "Approved receiver", SigningKeyID: "public_key_1", UnmeteredApproved: true, Active: true}, Endpoint: "https://private-webhook-receiver.invalid/no-export", SigningKeyEnvironment: "PLATFORM_WEBHOOK_KEY_PRIVATE", TimeoutSeconds: 2}
	if _, err = s.InstallWebhookDestination(ctx, profile); err != nil {
		t.Fatal(err)
	}
	server := &Server{Store: s, Log: slog.New(slog.NewTextHandler(io.Discard, nil))}
	mux := http.NewServeMux()
	server.mountWebhooks(mux)
	request := func(method, path, body string, change func(*http.Request), want int) []byte {
		t.Helper()
		r := httptest.NewRequest(method, path, strings.NewReader(body))
		r.Header.Set("Authorization", "Bearer "+b.Token)
		r.Header.Set("X-Organization-ID", b.OrganizationID)
		r.Header.Set(domain.APIContractHeader, domain.ContractVersion)
		if method == "POST" {
			r.Header.Set("Content-Type", "application/json")
			r.Header.Set("Idempotency-Key", domain.NewID("key"))
		}
		if change != nil {
			change(r)
		}
		w := httptest.NewRecorder()
		mux.ServeHTTP(w, r)
		if w.Code != want {
			t.Fatalf("%s %s wanted%d got%d: %s", method, path, want, w.Code, w.Body.String())
		}
		out := w.Body.Bytes()
		if strings.Contains(string(out), profile.Endpoint) || strings.Contains(string(out), profile.SigningKeyEnvironment) || strings.Contains(string(out), b.Token) || strings.Contains(string(out), task.Goal) {
			t.Fatal("private profile, credential or source content leaked")
		}
		return out
	}
	base := "/api/v1/tasks/" + task.ID
	request("GET", "/api/v1/webhook-destinations", "", nil, 200)
	for _, q := range []string{"?", "?limit=01", "?limit=%2B1", "?cursor=", "?limit=1&limit=2", "?endpoint=x", "?x=%zz"} {
		request("GET", base+"/webhook-subscriptions"+q, "", nil, 400)
	}
	input := `{"destination_id":"api_receiver","destination_version":1,"event_types":["material.created"],"ttl_seconds":600,"max_notifications":4,"acknowledge_metadata_export":true,"reason":"Explicit metadata export"}`
	for _, bad := range []string{
		strings.Replace(input, `"destination_version":1`, `"destination_version":2147483648`, 1),
		strings.Replace(input, `"reason":`, `"endpoint":"https://untrusted.invalid", "reason":`, 1),
		strings.Replace(input, `"acknowledge_metadata_export":true`, `"acknowledge_metadata_export":true,"acknowledge_metadata_export":true`, 1),
		strings.Replace(input, `"acknowledge_metadata_export":true`, `"acknowledge_metadata_export":false`, 1),
	} {
		request("POST", base+"/webhook-subscriptions", bad, nil, 400)
	}
	request("POST", base+"/webhook-subscriptions", input, func(r *http.Request) { r.Header.Add("Idempotency-Key", "ambiguous") }, 400)
	request("POST", base+"/webhook-subscriptions", input, func(r *http.Request) { r.Header.Set(domain.APIContractHeader, "unsupported") }, 409)
	key := domain.NewID("key")
	body := request("POST", base+"/webhook-subscriptions", input, func(r *http.Request) { r.Header.Set("Idempotency-Key", key) }, 201)
	var sub struct {
		Data domain.WebhookSubscription `json:"data"`
	}
	decode(body, &sub)
	request("GET", base+"/webhook-subscriptions", "", nil, 200)
	request("GET", base+"/webhook-subscriptions/"+sub.Data.ID, "", nil, 200)
	if _, err = s.CreateMaterial(ctx, p, task.ID, domain.NewID("key"), domain.CreateMaterial{Name: "Private draft", MediaType: "text/plain", Content: "PRIVATE_CONTENT_NEVER_EXPORTED"}); err != nil {
		t.Fatal(err)
	}
	body = request("GET", base+"/webhook-deliveries", "", nil, 200)
	var deliveries struct {
		Data []domain.WebhookDelivery `json:"data"`
	}
	decode(body, &deliveries)
	if len(deliveries.Data) != 1 {
		t.Fatal("expected one queued notification")
	}
	d := deliveries.Data[0]
	request("GET", base+"/webhook-deliveries/"+d.ID, "", nil, 200)
	request("GET", base+"/webhook-deliveries/"+d.ID+"/attempts", "", nil, 200)
	// Controlled stored HTTP fact; this preview probe performs no receiver I/O.
	l, _, err := s.ClaimWebhook(ctx, "preview_worker", time.Second)
	if err != nil || l == nil {
		t.Fatal("claim", err)
	}
	if _, err = s.PrepareWebhookDispatch(ctx, l); err != nil {
		t.Fatal(err)
	}
	d, err = s.RecordWebhookObservation(ctx, domain.WebhookObservation{OrganizationID: l.OrganizationID, TaskID: l.TaskID, DeliveryID: l.DeliveryID, AttemptID: l.AttemptID, EnvelopeDigest: l.EnvelopeDigest, WorkerID: l.WorkerID, Token: l.Token, HTTPStatus: 503})
	if err != nil {
		t.Fatal(err)
	}
	retry, _ := json.Marshal(domain.RetryWebhookDelivery{ExpectedVersion: d.Version, ExpectedAttemptID: d.CurrentAttemptID, EnvelopeDigest: d.EnvelopeDigest, AcknowledgePossibleDuplicate: true, Reason: "Explicit duplicate risk"})
	request("POST", base+"/webhook-deliveries/"+d.ID+"/retry", string(retry), nil, 200)
	if _, err = s.Pool.Exec(ctx, "UPDATE memberships SET role='member' WHERE organization_id=$1 AND principal_id=$2", p.OrganizationID, p.ID); err != nil {
		t.Fatal(err)
	}
	request("POST", base+"/webhook-subscriptions", input, func(r *http.Request) { r.Header.Set("Idempotency-Key", key) }, 403)
	if _, err = s.Pool.Exec(ctx, "UPDATE memberships SET role='owner' WHERE organization_id=$1 AND principal_id=$2", p.OrganizationID, p.ID); err != nil {
		t.Fatal(err)
	}
	current, err := s.WebhookSubscription(ctx, p, task.ID, sub.Data.ID)
	if err != nil {
		t.Fatal(err)
	}
	revoke, _ := json.Marshal(domain.RevokeWebhookSubscription{ExpectedVersion: current.Version, Reason: "Stop exports"})
	request("POST", base+"/webhook-subscriptions/"+sub.Data.ID+"/revoke", string(revoke), nil, 200)
	other, err := s.Bootstrap(ctx, "Other organization", 10000)
	if err != nil {
		t.Fatal(err)
	}
	request("GET", base+"/webhook-deliveries/"+d.ID, "", func(r *http.Request) {
		r.Header.Set("Authorization", "Bearer "+other.Token)
		r.Header.Set("X-Organization-ID", other.OrganizationID)
	}, 404)
	w := httptest.NewRecorder()
	r := httptest.NewRequest("GET", "/api/v1/webhook-destinations", nil)
	server.Handler().ServeHTTP(w, r)
	if w.Code != 404 {
		t.Fatal("preview routes leaked into assembled0.13 API")
	}
	t.Log("all9 preview operations exercised with real PG; strict queries/body/header, current role before replay and tenant scope; no advertised0.13 webhook routes")
}
