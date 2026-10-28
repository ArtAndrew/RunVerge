// SPDX-License-Identifier: Elastic-2.0
package mcpbridge

import (
	"agentplatform.local/platform/internal/domain"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/url"
	"regexp"
	"strconv"
	"strings"
)

const ProtocolVersion = "2026-07-28"
const MaxMessageBytes = 1 << 20

// Credentials belong to the trusted transport configuration, never tool arguments.
type Credentials struct {
	Token          string `json:"-"`
	OrganizationID string `json:"-"`
}
type Request struct {
	Method         string
	Path           string
	Query          url.Values
	Body           json.RawMessage
	IdempotencyKey string
}
type Response struct {
	Status int
	Body   json.RawMessage
}
type Backend interface {
	Call(context.Context, Credentials, Request) (Response, error)
}
type BackendFunc func(context.Context, Credentials, Request) (Response, error)

func (f BackendFunc) Call(ctx context.Context, c Credentials, r Request) (Response, error) {
	return f(ctx, c, r)
}

var safeID = regexp.MustCompile(`^[A-Za-z0-9_-]{1,128}$`)
var errBackend = errors.New("platform API request failed")
var errSize = errors.New("platform API response exceeds the configured limit")

// HandlerBackend invokes the normal API router with bounded buffers. It cannot
// address a network endpoint, and neither authorization nor storage is bypassed.
func HandlerBackend(handler http.Handler) Backend {
	return BackendFunc(func(ctx context.Context, c Credentials, in Request) (Response, error) {
		if handler == nil || !validCredentials(c) || !allowedRequest(in) {
			return Response{}, errBackend
		}
		u := &url.URL{Scheme: "http", Host: "platform.internal", Path: in.Path, RawQuery: in.Query.Encode()}
		req, err := http.NewRequestWithContext(ctx, in.Method, u.String(), bytes.NewReader(in.Body))
		if err != nil {
			return Response{}, errBackend
		}
		req.Header.Set("Authorization", "Bearer "+c.Token)
		req.Header.Set("X-Organization-ID", c.OrganizationID)
		req.Header.Set(domain.APIContractHeader, domain.ContractVersion)
		if in.Body != nil {
			req.Header.Set("Content-Type", "application/json")
		}
		if in.IdempotencyKey != "" {
			req.Header.Set("Idempotency-Key", in.IdempotencyKey)
		}
		w := &boundedWriter{header: make(http.Header)}
		handler.ServeHTTP(w, req)
		if w.overflow {
			return Response{}, errSize
		}
		if w.status == 0 {
			w.status = http.StatusOK
		}
		if !json.Valid(w.body.Bytes()) || w.header.Get(domain.APIContractHeader) != domain.ContractVersion {
			return Response{}, errBackend
		}
		return Response{Status: w.status, Body: append(json.RawMessage(nil), w.body.Bytes()...)}, nil
	})
}
func validCredentials(c Credentials) bool {
	return safeID.MatchString(c.OrganizationID) && len(c.Token) > 0 && len(c.Token) <= 4096 && !strings.ContainsAny(c.Token, "\r\n\x00 \t")
}
func allowedRequest(r Request) bool {
	if len(r.Body) > MaxMessageBytes || (len(r.Body) > 0 && !json.Valid(r.Body)) || len(r.Query.Encode()) > 16384 {
		return false
	}
	for key, values := range r.Query {
		if len(values) != 1 || len(values[0]) > 4096 {
			return false
		}
		switch key {
		case "cursor", "limit", "after", "reservation_cursor", "entry_cursor", "incident_cursor", "quarantine_cursor", "version", "principal_id", "plan_id":
		default:
			return false
		}
	}
	if r.Method == http.MethodGet {
		if len(r.Body) != 0 || r.IdempotencyKey != "" {
			return false
		}
	} else if r.Method != http.MethodPost || len(r.IdempotencyKey) < 1 || len(r.IdempotencyKey) > 128 || strings.ContainsAny(r.IdempotencyKey, "\r\n\x00") {
		return false
	}
	parts := strings.Split(strings.TrimPrefix(r.Path, "/api/v1/"), "/")
	if !strings.HasPrefix(r.Path, "/api/v1/") {
		return false
	}
	if len(parts) == 1 {
		switch parts[0] {
		case "capabilities":
			return r.Method == http.MethodGet && len(r.Query) == 0
		case "agents", "tasks", "members", "service-accounts":
			return r.Method == http.MethodGet || r.Method == http.MethodPost
		case "models", "me", "tools", "membership-audit", "login-policy", "security-audit", "compatibility":
			return r.Method == http.MethodGet
		}
		return false
	}
	if len(parts) < 2 || !safeID.MatchString(parts[1]) {
		return false
	}
	if parts[0] == "configuration" {
		if len(parts) == 2 {
			switch parts[1] {
			case "catalog", "destinations", "resources", "audit", "verifications":
				return r.Method == http.MethodGet
			case "grants", "plans":
				return r.Method == http.MethodGet || r.Method == http.MethodPost
			}
		}
		if len(parts) == 3 && safeID.MatchString(parts[2]) {
			return (parts[1] == "grants" || parts[1] == "plans" || parts[1] == "verifications") && r.Method == http.MethodGet
		}
		if len(parts) == 4 && safeID.MatchString(parts[2]) && r.Method == http.MethodPost {
			if parts[1] == "grants" {
				return parts[3] == "revoke"
			}
			if parts[1] == "plans" {
				switch parts[3] {
				case "validate", "approve", "apply", "verify":
					return true
				}
			}
		}
		return false
	}
	if parts[0] == "login-policy" {
		return len(parts) == 2 && parts[1] == "update" && r.Method == http.MethodPost
	}
	if parts[0] == "service-accounts" {
		switch len(parts) {
		case 2:
			return r.Method == http.MethodGet
		case 3:
			return (parts[2] == "update" && r.Method == http.MethodPost) || (parts[2] == "keys" && r.Method == http.MethodGet)
		case 4:
			return parts[2] == "keys" && safeID.MatchString(parts[3]) && r.Method == http.MethodGet
		case 5:
			return parts[2] == "keys" && safeID.MatchString(parts[3]) && parts[4] == "revoke" && r.Method == http.MethodPost
		}
		return false
	}
	if parts[0] == "agents" {
		return len(parts) == 3 && parts[2] == "publish" && r.Method == http.MethodPost
	}
	if parts[0] == "members" {
		if len(parts) == 2 {
			return r.Method == http.MethodGet
		}
		return len(parts) == 3 && (parts[2] == "update" || parts[2] == "revoke-credentials") && r.Method == http.MethodPost
	}
	if parts[0] != "tasks" {
		return false
	}
	if len(parts) == 2 {
		return r.Method == http.MethodGet
	}

	if len(parts) == 4 && parts[2] == "automation" && parts[3] == "revoke" {
		return r.Method == http.MethodPost
	}
	if len(parts) == 4 && safeID.MatchString(parts[3]) {
		return (parts[2] == "materials" || parts[2] == "artifacts" || parts[2] == "tool-executions" || parts[2] == "model-tool-proposals") && r.Method == http.MethodGet
	}
	if len(parts) == 5 && parts[2] == "tool-executions" && safeID.MatchString(parts[3]) && (parts[4] == "decide" || parts[4] == "dispatch" || parts[4] == "reconcile") {
		return r.Method == http.MethodPost
	}
	if len(parts) >= 5 && parts[2] == "materials" && safeID.MatchString(parts[3]) {
		if len(parts) == 5 && parts[4] == "revisions" {
			return r.Method == http.MethodGet || r.Method == http.MethodPost
		}
		if len(parts) == 5 && parts[4] == "publish" {
			return r.Method == http.MethodPost
		}
		if len(parts) == 6 && parts[4] == "revisions" {
			n, e := strconv.Atoi(parts[5])
			return e == nil && n >= 1 && n <= domain.MaxMaterialRevisions && r.Method == http.MethodGet
		}
	}
	if len(parts) != 3 {
		return false
	}
	switch parts[2] {
	case "materials", "tool-executions", "automation":
		return r.Method == http.MethodGet || r.Method == http.MethodPost
	case "cancel", "review", "retry", "continue-model":
		return r.Method == http.MethodPost
	case "events", "event-snapshot", "event-feed", "artifacts", "runs", "operations", "actions", "approvals", "costs", "candidates", "decisions", "children", "model-tool-proposals", "progress-signals":
		return r.Method == http.MethodGet
	}
	return false
}

type boundedWriter struct {
	header   http.Header
	status   int
	body     bytes.Buffer
	overflow bool
}

func (w *boundedWriter) Header() http.Header { return w.header }
func (w *boundedWriter) WriteHeader(s int) {
	if w.status == 0 {
		w.status = s
	}
}
func (w *boundedWriter) Write(p []byte) (int, error) {
	if w.status == 0 {
		w.status = 200
	}
	if len(p) > MaxMessageBytes-w.body.Len() {
		w.overflow = true
		return 0, errSize
	}
	return w.body.Write(p)
}
