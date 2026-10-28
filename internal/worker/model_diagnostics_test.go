// SPDX-License-Identifier: Elastic-2.0
package worker

import (
	"bytes"
	"encoding/json"
	"fmt"
	"log/slog"
	"strings"
	"testing"

	"agentplatform.local/platform/internal/modelruntime"
)

func TestModelFailureLogDoesNotFormatPrivateUnderlyingError(t *testing.T) {
	var output bytes.Buffer
	w := Worker{Log: slog.New(slog.NewJSONHandler(&output, nil))}
	err := fmt.Errorf("private-body-and-key-sentinel: %w", &modelruntime.ExecutionError{Code: "provider_outcome_unknown", Unknown: true, GatewayCode: "provider_rejected", ProviderStatus: 403})
	w.logModelFailure("op_test", "outcome_unknown", err)
	var entry map[string]any
	if e := json.Unmarshal(output.Bytes(), &entry); e != nil {
		t.Fatal(e)
	}
	if entry["gateway_code"] != "provider_rejected" || entry["provider_status"] != float64(403) || entry["reason"] != "provider_outcome_unknown" {
		t.Fatal("typed diagnosis missing")
	}
	if strings.Contains(output.String(), "private-body") {
		t.Fatal("underlying error disclosed")
	}
}
