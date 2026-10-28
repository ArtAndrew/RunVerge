// SPDX-License-Identifier: Elastic-2.0
package modelruntime_test

import (
	"context"
	"testing"

	"agentplatform.local/platform/internal/domain"
	"agentplatform.local/platform/internal/modelgateway"
	"agentplatform.local/platform/internal/modelruntime"
)

func TestOpenRouterPolicyChangesIntentWithoutDispatch(t *testing.T) {
	lease := testLease()
	lease.Agent.Model = "example/fixed-model"
	c := testConfig(modelgateway.OpenRouterEndpoint)
	c.Gateway.Model, c.Price.Model, c.Gateway.AllowInsecureLoopback = lease.Agent.Model, lease.Agent.Model, false
	legacy := newStore()
	legacy.reserveCommitError = true
	router := newStore()
	router.reserveCommitError = true
	_, _ = newRuntime(t, legacy, c).Execute(context.Background(), lease)
	c.Gateway.WireProfile = domain.ModelWireOpenRouterTextV1
	_, _ = newRuntime(t, router, c).Execute(context.Background(), lease)
	if legacy.intent.IntentHash == "" || router.intent.IntentHash == "" || legacy.intent.IntentHash == router.intent.IntentHash {
		t.Fatal("wire policy was absent from immutable intent")
	}
	if legacy.candidate.Text != "" || router.candidate.Text != "" || legacy.action != "pending" || router.action != "pending" {
		t.Fatal("reserve failure did not stop before network")
	}
	allowData := newStore()
	allowData.reserveCommitError = true
	c.Gateway.OpenRouterDataCollection = "allow"
	_, _ = newRuntime(t, allowData, c).Execute(context.Background(), lease)
	if allowData.intent.IntentHash == "" || router.intent.IntentHash == allowData.intent.IntentHash {
		t.Fatal("data collection policy was absent from immutable intent")
	}
	c.Price.AccountingMode = domain.ModelAccountingLocalUnmetered
	if _, err := modelruntime.New(newStore(), c); err == nil {
		t.Fatal("OpenRouter accepted local-unmetered accounting")
	}
}
