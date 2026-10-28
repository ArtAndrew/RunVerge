// SPDX-License-Identifier: Elastic-2.0
package main

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"

	"agentplatform.local/platform/internal/capabilities"
	"agentplatform.local/platform/internal/domain"
)

func TestCapabilitiesActualCLIUsesCurrentHTTPWithoutDatabaseOrActions(t *testing.T) {
	const token = "synthetic-capability-cli-credential-8492"
	const org = "org_capability_cli"
	for _, mode := range []string{"valid", "foreign_organization", "wrong_catalog_version", "credential_echo"} {
		t.Run(mode, func(t *testing.T) {
			v, err := capabilities.Observe(domain.Principal{ID: "usr_capability_cli", OrganizationID: org, Kind: "human", Role: "viewer"}, time.Now())
			if err != nil {
				t.Fatal(err)
			}
			switch mode {
			case "foreign_organization":
				v.Identity.OrganizationID = "org_other"
			case "wrong_catalog_version":
				v.CatalogVersion = "999"
			case "credential_echo":
				v.Profiles[0].Summary = token
			}
			var calls atomic.Int64
			peer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				calls.Add(1)
				if r.Method != "GET" || r.URL.Path != "/api/v1/capabilities" || r.URL.RawQuery != "" || r.ContentLength != 0 || r.Header.Get("Authorization") != "Bearer "+token || r.Header.Get("X-Organization-ID") != org || r.Header.Get(domain.APIContractHeader) != domain.ContractVersion || r.Header.Get("Idempotency-Key") != "" {
					t.Error("unexpected capability CLI request")
				}
				w.Header().Set(domain.APIContractHeader, domain.ContractVersion)
				w.Header().Set("Content-Type", "application/json")
				_ = json.NewEncoder(w).Encode(map[string]any{"data": v})
			}))
			defer peer.Close()
			cred := filepath.Join(t.TempDir(), "credential.json")
			raw, _ := json.Marshal(map[string]string{"token": token, "organization_id": org})
			if os.WriteFile(cred, raw, 0600) != nil {
				t.Fatal("private fixture failed")
			}
			ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
			defer cancel()
			cmd := independentCLICommand(t, ctx, "unusable-for-client", "capabilities", "--url", peer.URL, "--credentials", cred, "--json")
			out, err := cmd.Output()
			if calls.Load() != 1 {
				t.Fatal("CLI failed to perform exactly one GET or retried")
			}
			if mode != "valid" {
				if err == nil || len(out) != 0 {
					t.Fatal("invalid observation was printed as successful")
				}
				return
			}
			if err != nil {
				t.Fatal("valid catalogue CLI failed before normal HTTP output")
			}
			var envelope struct {
				Data domain.ProductCapabilities `json:"data"`
			}
			if json.Unmarshal(out, &envelope) != nil || !capabilities.Valid(envelope.Data) || envelope.Data.Identity.OrganizationID != org {
				t.Fatal("valid observation changed")
			}
		})
	}
}
