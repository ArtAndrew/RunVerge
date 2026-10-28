// SPDX-License-Identifier: Elastic-2.0
package worker

import (
	"errors"

	"agentplatform.local/platform/internal/modelruntime"
)

// Never format an underlying error: URLs, provider bodies and credentials are
// not diagnostic fields. Runtime exposes only internal codes and numeric status.
func (w *Worker) logModelFailure(operation, outcome string, err error) {
	fields := []any{"operation_id", operation, "outcome", outcome}
	var failure *modelruntime.ExecutionError
	if errors.As(err, &failure) {
		fields = append(fields, "reason", failure.Code)
		if failure.GatewayCode != "" {
			fields = append(fields, "gateway_code", failure.GatewayCode)
		}
		if failure.ProviderStatus != 0 {
			fields = append(fields, "provider_status", failure.ProviderStatus)
		}
	}
	w.Log.Warn("model attempt requires attention", fields...)
}
