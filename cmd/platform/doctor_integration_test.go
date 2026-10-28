// SPDX-License-Identifier: Elastic-2.0
package main

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"agentplatform.local/platform/internal/doctor"
	"agentplatform.local/platform/internal/domain"
	"agentplatform.local/platform/internal/httpapi"
	"agentplatform.local/platform/internal/storage"
	"github.com/jackc/pgx/v5"
)

// Hash actual rows in every table, including credential, ledger and audit
// tables. Never print row contents or secrets on a mismatch.
func doctorDatabaseFingerprint(t *testing.T, s *storage.Store) map[string]string {
	t.Helper()
	ctx := context.Background()
	rows, err := s.Pool.Query(ctx, "SELECT tablename FROM pg_tables WHERE schemaname=current_schema() ORDER BY tablename")
	if err != nil {
		t.Fatal("cannot inspect isolated table inventory")
	}
	names := []string{}
	for rows.Next() {
		var name string
		if rows.Scan(&name) != nil {
			t.Fatal("cannot read isolated table inventory")
		}
		names = append(names, name)
	}
	rows.Close()
	if rows.Err() != nil {
		t.Fatal("isolated table inventory failed")
	}
	fingerprints := map[string]string{}
	for _, name := range names {
		var raw []byte
		if s.Pool.QueryRow(ctx, "SELECT COALESCE(jsonb_agg(to_jsonb(t) ORDER BY to_jsonb(t)::text),'[]'::jsonb) FROM "+pgx.Identifier{name}.Sanitize()+" t").Scan(&raw) != nil {
			t.Fatal("cannot fingerprint isolated table", name)
		}
		fingerprints[name] = storage.Hash(raw)
	}
	return fingerprints
}

func TestDoctorActualCLIAndPostgresPreserveStateAcrossConfigurationLifecycle(t *testing.T) {
	s, _, b := independentCLIDB(t)
	ctx := context.Background()
	owner, err := s.Authenticate(ctx, b.Token, b.OrganizationID)
	if err != nil {
		t.Fatal("owner authentication failed")
	}
	file := independentCredentialFile(t, b)
	var externalCalls, writes atomic.Int32
	provider := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { externalCalls.Add(1); w.WriteHeader(503) }))
	defer provider.Close()
	api := (&httpapi.Server{Store: s, Log: slog.New(slog.NewTextHandler(io.Discard, nil))}).Handler()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet {
			writes.Add(1)
			w.WriteHeader(405)
			return
		}
		api.ServeHTTP(w, r)
	}))
	defer server.Close()
	run := func(expected string, planID string, privateFile string) doctor.Report {
		t.Helper()
		before := doctorDatabaseFingerprint(t, s)
		bounded, cancel := context.WithTimeout(ctx, 15*time.Second)
		defer cancel()
		args := []string{"doctor", "--url", server.URL, "--credentials", privateFile, "--json"}
		if planID != "" {
			args = append(args, "--plan-id", planID)
		}
		cmd := independentCLICommand(t, bounded, "deliberately-unusable-database", args...)
		var stdout, stderr bytes.Buffer
		cmd.Stdout, cmd.Stderr = &stdout, &stderr
		err := cmd.Run()
		var report doctor.Report
		if json.Unmarshal(stdout.Bytes(), &report) != nil {
			t.Fatal("actual doctor child did not return a report")
		}
		found := false
		for _, f := range report.Findings {
			found = found || f.Code == expected
		}
		if !found || report.Status == "observed" && err != nil || report.Status != "observed" && err == nil {
			t.Fatal("unexpected doctor lifecycle result", expected, report.Status)
		}
		if !reflect.DeepEqual(before, doctorDatabaseFingerprint(t, s)) {
			t.Fatal("doctor changed persistent database state")
		}
		if externalCalls.Load() != 0 || writes.Load() != 0 || !report.ReadOnly || report.ExternalProbes != 0 {
			t.Fatal("doctor performed an external or mutating action")
		}
		if strings.Contains(stdout.String()+stderr.String(), b.Token) {
			t.Fatal("doctor leaked credential")
		}
		return report
	}
	run("no_visible_active_destination", "", file)
	purpose, err := s.InstallCredentialPurposeProfile(ctx, domain.CredentialPurposeProfile{ID: domain.NewID("purpose"), OrganizationID: b.OrganizationID, Version: 1, Kind: "mcp", EndpointDigest: storage.Hash([]byte(provider.URL)), CredentialEnvironment: "PLATFORM_MCP_KEY_DOCTOR_TEST", Active: true})
	if err != nil {
		t.Fatal("purpose fixture installation failed")
	}
	destination, err := s.InstallConfigurationDestination(ctx, domain.ConfigurationDestinationProfile{Destination: domain.ConfigurationDestination{ID: domain.NewID("destination"), OrganizationID: b.OrganizationID, Version: 1, Kind: "mcp", Name: "Doctor lifecycle fixture", TargetID: domain.NewID("mcp"), CredentialPurposeID: purpose.ID, CredentialPurposeVersion: 1, DiscoveryUnmeteredApproved: true, Active: true}, MCP: &domain.MCPConnectionProfile{Endpoint: provider.URL, AllowPrivateNetwork: true, TimeoutSeconds: 2}})
	if err != nil {
		t.Fatal("destination fixture installation failed")
	}
	run("destination_not_applied", "", file)
	catalog, err := s.ConfigurationCatalog(ctx, owner)
	if err != nil {
		t.Fatal("catalog fixture read failed")
	}
	yes := true
	raw, err := s.CreateConfigurationPlan(ctx, owner, domain.NewID("key"), domain.CreateConfigurationPlan{BaseRevision: catalog.Revision, TTLSeconds: 600, Operations: []domain.ConfigurationOperationInput{{ID: "connect_mcp", DestinationID: destination.Destination.ID, DestinationVersion: 1, Active: &yes}}, Reason: "Explicit isolated doctor lifecycle fixture"})
	var plan domain.ConfigurationPlan
	if err != nil || json.Unmarshal(raw, &plan) != nil {
		t.Fatal("plan fixture failed")
	}
	run("plan_not_applied", plan.ID, file)
	raw, err = s.ValidateConfigurationPlan(ctx, owner, plan.ID, domain.NewID("key"), domain.ValidateConfigurationPlan{ExpectedVersion: plan.Version, PlanDigest: plan.Digest})
	var validation domain.ConfigurationValidation
	if err != nil || json.Unmarshal(raw, &validation) != nil || !validation.Valid {
		t.Fatal("validation fixture failed")
	}
	raw, err = s.ApplyConfigurationPlan(ctx, owner, plan.ID, domain.NewID("key"), domain.ApplyConfigurationPlan{ExpectedVersion: plan.Version, PlanDigest: plan.Digest, ValidationID: validation.ID, Reason: "Explicit isolated doctor apply fixture"})
	if err != nil || json.Unmarshal(raw, &plan) != nil {
		t.Fatal("apply fixture failed")
	}
	run("plan_operation_unverified", plan.ID, file)
	_, err = s.StartConfigurationVerification(ctx, owner, plan.ID, domain.NewID("key"), domain.StartConfigurationVerification{ExpectedPlanVersion: plan.Version, PlanDigest: plan.Digest, OperationID: "connect_mcp", AcknowledgeCost: &yes, Reason: "Explicit pending probe, no worker started"})
	if err != nil {
		t.Fatal("pending verification fixture failed")
	}
	run("probe_pending", plan.ID, file)
	service := independentCLIService(t, s, b)
	token, _, err := s.IssueServiceToken(ctx, b.OrganizationID, service.PrincipalID, "member", time.Minute, "Bounded doctor denial fixture")
	if err != nil {
		t.Fatal("service fixture issuance failed")
	}
	serviceFile := independentCredentialFile(t, storage.Bootstrap{OrganizationID: b.OrganizationID, PrincipalID: service.PrincipalID, Token: token})
	report := run("forbidden", "", serviceFile)
	if report.Complete {
		t.Fatal("ungranted service was reported fully inspected")
	}
	t.Log("six actual CLI lifecycle observations, all isolated PostgreSQL tables unchanged, zero external calls, zero API writes; service denial retained")
}
