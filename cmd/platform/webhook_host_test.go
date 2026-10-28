// SPDX-License-Identifier: Elastic-2.0
package main

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"agentplatform.local/platform/internal/domain"
	"agentplatform.local/platform/internal/webhookruntime"
)

func TestWebhookHostAuthorPrivateKeyFileAndNoOverwrite(t *testing.T) {
	t.Setenv("DATABASE_URL", "unusable-no-database-needed")
	path := filepath.Join(t.TempDir(), "private.env")
	var output bytes.Buffer
	args := []string{"--environment", "PLATFORM_WEBHOOK_KEY_AUTHOR", "--out", path}
	if err := runGenerateWebhookKey(args, &output); err != nil {
		t.Fatal(err)
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	value := strings.TrimSuffix(strings.TrimPrefix(string(raw), "PLATFORM_WEBHOOK_KEY_AUTHOR="), "\n")
	if key, e := webhookruntime.DecodeSigningKey(value); e != nil || len(key) != 32 {
		t.Fatal("key not canonical32 bytes")
	}
	stat, err := os.Stat(path)
	if err != nil || stat.Mode().Perm() != 0600 {
		t.Fatal("key file not private", err)
	}
	if strings.Contains(output.String(), value) || strings.Contains(output.String(), path) {
		t.Fatal("key/path appeared on stdout")
	}
	if err = runGenerateWebhookKey(args, &output); err == nil {
		t.Fatal("existing private key overwritten")
	}
	after, _ := os.ReadFile(path)
	if !bytes.Equal(raw, after) {
		t.Fatal("key changed on duplicate command")
	}
	if err = runGenerateWebhookKey([]string{"--environment", "DATABASE_URL", "--out", filepath.Join(t.TempDir(), "other.env")}, &output); err == nil {
		t.Fatal("unrestricted environment reference accepted")
	}
}

func TestWebhookHostAuthorInstallRetireAndAudit(t *testing.T) {
	s, _, b := independentCLIDB(t)
	ctx := context.Background()
	p := domain.WebhookDestinationProfile{Destination: domain.WebhookDestination{ID: "host_receiver", OrganizationID: b.OrganizationID, Version: 1, Name: "Approved metadata receiver", SigningKeyID: "public_host_key", UnmeteredApproved: true, Active: true}, Endpoint: "https://host-receiver.invalid/private", SigningKeyEnvironment: "PLATFORM_WEBHOOK_KEY_HOST", TimeoutSeconds: 2}
	raw, _ := json.Marshal(p)
	file := privateProfileFile(t, raw)
	var output bytes.Buffer
	if err := runWebhookHost(ctx, s, "install-webhook-destination", []string{"--file", file}, &output); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(output.String(), p.Endpoint) || strings.Contains(output.String(), p.SigningKeyEnvironment) {
		t.Fatal("private profile appeared on stdout")
	}
	if _, err := s.Pool.Exec(ctx, "UPDATE organizations SET active=false WHERE id=$1", b.OrganizationID); err != nil {
		t.Fatal(err)
	}
	args := []string{"--organization-id", b.OrganizationID, "--id", p.Destination.ID, "--version", "1"}
	if err := runWebhookHost(ctx, s, "retire-webhook-destination", args, &output); err != nil {
		t.Fatal("attenuation of inactive organization failed", err)
	}
	if err := runWebhookHost(ctx, s, "retire-webhook-destination", args, &output); err != nil {
		t.Fatal("idempotent retirement failed", err)
	}
	var active bool
	var audits int
	if err := s.Pool.QueryRow(ctx, "SELECT active FROM webhook_destinations WHERE organization_id=$1 AND id=$2", b.OrganizationID, p.Destination.ID).Scan(&active); err != nil || active {
		t.Fatal("retirement failed", err)
	}
	if err := s.Pool.QueryRow(ctx, "SELECT count(*) FROM configuration_audit WHERE organization_id=$1 AND object_type='webhook_destination'", b.OrganizationID).Scan(&audits); err != nil || audits != 2 {
		t.Fatal("missing install/retire audit or duplicate retirement write", err, audits)
	}
	if _, err := s.Pool.Exec(ctx, "UPDATE organizations SET active=true WHERE id=$1", b.OrganizationID); err != nil {
		t.Fatal(err)
	}
	old, err := s.InstallWebhookDestination(ctx, p)
	if err != nil || old.Active {
		t.Fatal("identical reinstall revived retired version", err)
	}
}
