// SPDX-License-Identifier: Elastic-2.0
package worker

import (
	"context"
	"testing"

	"agentplatform.local/platform/internal/domain"
	"agentplatform.local/platform/internal/storage"
	"agentplatform.local/platform/internal/webhookruntime"
	"github.com/jackc/pgx/v5/pgxpool"
)

// A worker must not read any host credential if it cannot establish current
// recovery admission. The closed lazy pool performs no network or database I/O.
// Signed-import history denial is covered by the storage integration test.
func TestRecoveryAdmissionFailurePrecedesEveryCredentialResolver(t *testing.T) {
	ctx := context.Background()
	pool, err := pgxpool.New(ctx, "postgres://synthetic@127.0.0.1:1/synthetic?sslmode=disable")
	if err != nil {
		t.Fatal(err)
	}
	pool.Close()
	store := &storage.Store{Pool: pool}
	if err := store.CheckRecoveryOutboundProfiles(ctx, storage.RecoveryOutboundProfile{OrganizationID: "org_fixture", Kind: "model", ID: "model_fixture", Version: 1}); err == nil {
		t.Fatal("closed authority store unexpectedly admitted a profile")
	}
	calls := 0
	resolve := func(string) (string, error) { calls++; return "synthetic-not-a-secret", nil }
	w := &Worker{Store: store, ResolveModelCredential: resolve, ResolveToolCredential: resolve, ResolveMCPCredential: resolve, ResolveWebhookSigningKey: resolve}
	w.executeModel(ctx, &domain.Lease{}, domain.ModelConnection{OrganizationID: "org_fixture", ID: "model_fixture", Version: 1, CredentialEnvironment: "PLATFORM_MODEL_KEY_TEST"})
	for _, ref := range []string{"", "PLATFORM_MCP_KEY_TEST"} {
		w.executeTool(ctx, &domain.ToolLease{}, domain.ToolConnection{Tool: domain.ToolInfo{OrganizationID: "org_fixture", ID: "tool_fixture", Version: 1}, CredentialEnvironment: ref})
	}
	w.executeConfigurationVerification(ctx, &domain.ConfigurationVerificationLease{OrganizationID: "org_fixture", Scope: domain.ConfigurationScope{TargetID: "mcp_fixture"}, ResourceVersion: 1}, domain.ConfigurationProbeConfig{})
	profile := domain.WebhookDestinationProfile{Destination: domain.WebhookDestination{ID: "hooks_fixture", OrganizationID: "org_fixture", Version: 1, Name: "Inert receiver", SigningKeyID: "key_v1", UnmeteredApproved: true, Active: true}, Endpoint: "https://hooks.example.invalid/events", SigningKeyEnvironment: "PLATFORM_WEBHOOK_KEY_FIXTURE", TimeoutSeconds: 5}
	if err = webhookruntime.ValidateProfile(profile); err != nil {
		t.Fatal("webhook must reach recovery check", err)
	}
	w.executeWebhook(ctx, &domain.WebhookLease{}, profile)
	if calls != 0 {
		t.Fatal("worker resolved a credential without recovery admission", calls)
	}
}
