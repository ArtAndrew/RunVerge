// SPDX-License-Identifier: Elastic-2.0
package domain

// ErrorCatalog describes stable public codes, independent of localized messages.
// Retryability is contextual and is carried on each response, never inferred
// from the HTTP status alone.
type ErrorDescription struct {
	HTTPStatus int    `json:"http_status"`
	Meaning    string `json:"meaning"`
}

var ErrorCatalog = map[string]ErrorDescription{
	"invalid_request":     {400, "Input is malformed, unsupported or outside the supported bounds."},
	"unauthenticated":     {401, "Credentials are missing, invalid, expired or revoked."},
	"forbidden":           {403, "The authenticated subject lacks current authority."},
	"not_found":           {404, "The object is absent or outside the selected organization."},
	"conflict":            {409, "A version, intent, idempotency key or recorded outcome conflicts."},
	"incompatible_client": {409, "The client must explicitly select the supported API contract before a command or read is admitted."},
	"budget_exceeded":     {409, "Admission would exceed a scope budget or a financial hold is active."},
	"invalid_transition":  {409, "The requested change is not allowed in the current state."},
	"stale_lease":         {409, "Execution authority or its fencing token is no longer valid."},
	"unavailable":         {503, "A dependency or admission capacity is currently unavailable."},
	"internal_error":      {500, "An unexpected internal failure occurred; details are not exposed."},
}
