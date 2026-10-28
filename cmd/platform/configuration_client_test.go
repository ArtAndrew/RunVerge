// SPDX-License-Identifier: Elastic-2.0
package main

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"

	"agentplatform.local/platform/internal/domain"
)

func configurationCLITestIdentity(t *testing.T) {
	t.Helper()
	t.Setenv("PLATFORM_TOKEN", "synthetic-cli-test-token")
	t.Setenv("PLATFORM_ORGANIZATION_ID", "org_cli_test")
	t.Setenv("DATABASE_URL", "deliberately-unusable-database")
}

func TestConfigurationCLIUsesCurrentHTTPHeadersWithoutDatabaseAndRejectsPrivateResponse(t *testing.T) {
	configurationCLITestIdentity(t)
	var calls atomic.Int32
	var private atomic.Bool
	peer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		if r.URL.Path != "/api/v1/configuration/catalog" || r.Header.Get(domain.APIContractHeader) != domain.ContractVersion || r.Header.Get("X-Organization-ID") != "org_cli_test" || r.Header.Get("Authorization") != "Bearer synthetic-cli-test-token" {
			t.Error("incorrect ordinary client boundary")
		}
		w.Header().Set(domain.APIContractHeader, domain.ContractVersion)
		if private.Load() {
			_, _ = io.WriteString(w, `{"data":{"token":"should-never-be-printed"}}`)
			return
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"data": domain.ConfigurationCatalog{OrganizationID: "org_cli_test", Revision: 1, Audience: domain.ConfigurationAudience, Actions: []string{}, AvailableCommands: []string{}, MaxOperations: 8, MaxGrantDepth: 4}})
	}))
	defer peer.Close()
	var out bytes.Buffer
	if err := runConfigurationClient(context.Background(), []string{"catalog", "--url", peer.URL, "--json"}, &out); err != nil {
		t.Fatal(err)
	}
	if calls.Load() != 1 || !strings.Contains(out.String(), `"revision":"1"`) {
		t.Fatal("missing exact HTTP result")
	}
	private.Store(true)
	out.Reset()
	if err := runConfigurationClient(context.Background(), []string{"catalog", "--url", peer.URL, "--json"}, &out); err == nil || out.Len() != 0 || calls.Load() != 2 {
		t.Fatal("private response was exposed or request retried")
	}
}

func TestConfigurationCLINeverFollowsRedirectOrRetriesMutation(t *testing.T) {
	configurationCLITestIdentity(t)
	var redirected, calls atomic.Int32
	target := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { redirected.Add(1) }))
	defer target.Close()
	body := `{"base_revision":"1","ttl_seconds":60,"operations":[{"id":"one","destination_id":"approved","destination_version":1,"expected_resource_version":0,"active":true}],"reason":"Exact client request"}`
	path := filepath.Join(t.TempDir(), "plan.json")
	if err := os.WriteFile(path, []byte(body), 0600); err != nil {
		t.Fatal(err)
	}
	peer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		raw, _ := io.ReadAll(r.Body)
		if r.Method != "POST" || r.URL.Path != "/api/v1/configuration/plans" || string(raw) != body || r.Header.Get("Idempotency-Key") != "explicit-one" {
			t.Error("mutation intent changed")
		}
		w.Header().Set("Location", target.URL)
		w.WriteHeader(http.StatusTemporaryRedirect)
	}))
	defer peer.Close()
	var out bytes.Buffer
	err := runConfigurationClient(context.Background(), []string{"plans", "create", "--url", peer.URL, "--file", path, "--key", "explicit-one", "--json"}, &out)
	if err == nil || calls.Load() != 1 || redirected.Load() != 0 || !strings.Contains(out.String(), `"automatic_retry":false`) {
		t.Fatal("redirect/retry or unsafe error result")
	}
}

func TestConfigurationCLICommandDispatchPrecedesDatabaseRequirement(t *testing.T) {
	configurationCLITestIdentity(t)
	previous := os.Args
	os.Args = []string{"platform", "config", "catalog", "--url", "http://non-loopback.example"}
	defer func() { os.Args = previous }()
	err := run()
	if err == nil || !strings.Contains(err.Error(), "requires HTTPS") || strings.Contains(err.Error(), "PostgreSQL") {
		t.Fatal("ordinary configuration client attempted host/database access")
	}
}

func TestConfigurationCLIRejectsAmbiguousSuccessVersionHeaders(t *testing.T) {
	configurationCLITestIdentity(t)
	for _, versions := range [][]string{{domain.ContractVersion, domain.ContractVersion}, {domain.ContractVersion, "0.11.0"}} {
		peer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			for _, version := range versions {
				w.Header().Add(domain.APIContractHeader, version)
			}
			_ = json.NewEncoder(w).Encode(map[string]any{"data": domain.ConfigurationCatalog{OrganizationID: "org_cli_test", Revision: 1, Audience: domain.ConfigurationAudience, Actions: []string{}, AvailableCommands: []string{}, MaxOperations: 8, MaxGrantDepth: 4}})
		}))
		var out bytes.Buffer
		err := runConfigurationClient(context.Background(), []string{"catalog", "--url", peer.URL, "--json"}, &out)
		peer.Close()
		if err == nil || out.Len() != 0 {
			t.Fatal("ambiguous success version accepted")
		}
	}
}
