//go:build darwin || linux

// SPDX-License-Identifier: Elastic-2.0
package recoveryaccess

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"agentplatform.local/platform/internal/modelgateway"
)

const usageTestKey = "synthetic-private-usage-key-one"
const usageTestKey2 = "synthetic-private-usage-key-two"

type usageTestObservation struct {
	record modelgateway.UsageObservationRecord
	raw    []byte
}

func (o usageTestObservation) Record() modelgateway.UsageObservationRecord { return o.record }
func (o usageTestObservation) RawResponse() []byte                         { return bytes.Clone(o.raw) }
func usageTestResponse(id string) usageTestObservation {
	raw := []byte(`{"data":{"id":"` + id + `","model":"example/test-model","total_cost":0.000001,"is_byok":false}}`)
	return usageTestObservation{record: modelgateway.UsageObservationRecord{Profile: "openrouter-generation-observation-v1", GenerationID: id, Model: "example/test-model", TotalCostCredits: "0.000001", MicroUSDStatus: "exact", ExactMicroUSD: "1", ResponseSHA256: stateHash(raw), ObservedAt: time.Now().UTC(), SettlementState: "unverified"}, raw: raw}
}
func usageTestRead(t *testing.T, key string) usageRead {
	t.Helper()
	return func(_ context.Context, actual, id string) (usageObservation, error) {
		if actual != key || id != "gen-test-request" {
			t.Fatal("unexpected credential or request")
		}
		return usageTestResponse(id), nil
	}
}
func usageHostFixture(t *testing.T) (string, HostInfo, UsageRequest) {
	t.Helper()
	p, _, _, _ := fixture(t)
	parent := t.TempDir()
	if os.Chmod(parent, 0700) != nil {
		t.Fatal("private parent")
	}
	policy := UsageHostPolicy{SourceID: "inst_" + strings.Repeat("a", 32), OrganizationID: "org_test", ModelConnectionID: "router-test", ModelConnectionVersion: 1, ConnectionIdentitySHA256: strings.Repeat("c", 64), ArchivedProviderAccount: "archive-account", CredentialReference: "current-account-key-1", ExpiresAt: time.Now().UTC().Add(time.Hour), Enabled: true}
	path := filepath.Join(parent, "usage")
	i, err := InitUsageHost(context.Background(), path, p.Binding, policy, usageTestKey, "operator:test")
	if err != nil {
		t.Fatal(err)
	}
	r := UsageRequest{Binding: p.Binding, SourcePlanSHA256: strings.Repeat("b", 64), SourceID: policy.SourceID, OrganizationID: policy.OrganizationID, ModelConnectionID: policy.ModelConnectionID, ModelConnectionVersion: policy.ModelConnectionVersion, ConnectionIdentitySHA256: policy.ConnectionIdentitySHA256, ArchivedProviderAccount: policy.ArchivedProviderAccount, GenerationID: "gen-test-request", Model: "example/test-model"}
	return path, i, r
}

func TestUsageHostRotationAndPurposeIsolation(t *testing.T) {
	path, info, r := usageHostFixture(t)
	ctx := context.Background()
	o, err := observeHostUsage(ctx, path, r, usageTestRead(t, usageTestKey))
	if err != nil {
		t.Fatal(err)
	}
	receipt := o.Receipt()
	if receipt.Request != r || receipt.PolicyEpoch != 1 || receipt.Policy.CredentialSHA256 != stateHash([]byte(usageTestKey)) || receipt.AccountMappingMethod != "current_host_operator_attestation" || len(receipt.ReceiptSHA256) != 64 {
		t.Fatal("incomplete acquisition attribution")
	}
	copy := o.RawResponse()
	copy[0] = 'X'
	edited := o.Receipt()
	edited.Policy.CredentialReference = "changed"
	if o.RawResponse()[0] != '{' || o.Receipt() != receipt {
		t.Fatal("mutable acquisition")
	}
	inspection, err := InspectHost(ctx, path)
	if err != nil {
		t.Fatal(err)
	}
	raw, _ := json.Marshal(inspection)
	if bytes.Contains(raw, []byte(usageTestKey)) {
		t.Fatal("inspection leaked credential")
	}
	if _, err = IssueHost(ctx, path, "member", r.OrganizationID, time.Minute); err == nil {
		t.Fatal("usage issued bearer")
	}
	if err = WithHostAuthority(ctx, path, func(context.Context, *Authority) error { t.Fatal("usage became read authority"); return nil }); err == nil {
		t.Fatal("usage read allowed")
	}
	if _, err = ReplaceHost(ctx, path, 1, []Scope{{"member", r.OrganizationID}}, "operator:test"); err == nil {
		t.Fatal("purpose changed")
	}
	readPath, readInfo := hostFixture(t)
	if _, err = ReplaceUsageHost(ctx, readPath, readInfo.Epoch, *info.Usage, usageTestKey, "operator:test"); err == nil {
		t.Fatal("read converted to usage")
	}
	policy := *info.Usage
	policy.Enabled = false
	policy.CredentialReference = usageTestKey
	if _, err = ReplaceUsageHost(ctx, path, 1, policy, "", "operator:revoke"); err == nil {
		t.Fatal("revocation exposed previous credential")
	}
	policy = *info.Usage
	policy.CredentialReference = usageTestKey
	if _, err = ReplaceUsageHost(ctx, path, 1, policy, usageTestKey2, "operator:rotation"); err == nil {
		t.Fatal("rotation exposed previous credential")
	}
	policy = *info.Usage
	policy.CredentialReference = "current-account-key-2"
	next, err := ReplaceUsageHost(ctx, path, 1, policy, usageTestKey2, "operator:rotation")
	if err != nil {
		t.Fatal(err)
	}
	if next.Epoch != 2 || next.OperationID == info.OperationID {
		t.Fatal("rotation attribution")
	}
	usageChild(t, path, r, stateHash([]byte(usageTestKey2)), false)
	policy = *next.Usage
	policy.Enabled = false
	if _, err = ReplaceUsageHost(ctx, path, 2, policy, "", "operator:revoke"); err != nil {
		t.Fatal(err)
	}
	usageChild(t, path, r, "", true)
}

func TestUsageHostScopeAndAcquisitionRefusals(t *testing.T) {
	path, _, r := usageHostFixture(t)
	mutations := map[string]func(*UsageRequest){
		"target":            func(r *UsageRequest) { r.Binding.TargetID = "inst_" + strings.Repeat("d", 32) },
		"restore":           func(r *UsageRequest) { r.Binding.RestoreID = "restore_" + strings.Repeat("d", 32) },
		"snapshot":          func(r *UsageRequest) { r.Binding.SnapshotSHA256 = strings.Repeat("d", 64) },
		"source":            func(r *UsageRequest) { r.SourceID = "inst_" + strings.Repeat("d", 32) },
		"organization":      func(r *UsageRequest) { r.OrganizationID = "org_other" },
		"connection":        func(r *UsageRequest) { r.ModelConnectionID = "other" },
		"version":           func(r *UsageRequest) { r.ModelConnectionVersion++ },
		"connection_digest": func(r *UsageRequest) { r.ConnectionIdentitySHA256 = strings.Repeat("d", 64) },
		"account":           func(r *UsageRequest) { r.ArchivedProviderAccount = "other" },
		"plan":              func(r *UsageRequest) { r.SourcePlanSHA256 = "" },
		"generation":        func(r *UsageRequest) { r.GenerationID = "../invalid" },
	}
	for name, mutate := range mutations {
		t.Run(name, func(t *testing.T) {
			wrong := r
			mutate(&wrong)
			o, err := observeHostUsage(context.Background(), path, wrong, func(context.Context, string, string) (usageObservation, error) {
				t.Fatal("unscoped request sent")
				return nil, nil
			})
			if err == nil || !reflect.DeepEqual(o, AcquiredUsageObservation{}) {
				t.Fatal("scope refusal missing")
			}
		})
	}
	for _, mode := range []string{"wrong_model", "wrong_id", "wrong_hash", "cancelled", "expired"} {
		t.Run(mode, func(t *testing.T) {
			p, i, request := usageHostFixture(t)
			if mode == "expired" {
				policy := *i.Usage
				policy.ExpiresAt = time.Now().UTC().Add(150 * time.Millisecond)
				if _, err := ReplaceUsageHost(context.Background(), p, 1, policy, usageTestKey, "operator:short"); err != nil {
					t.Fatal(err)
				}
			}
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			o, err := observeHostUsage(ctx, p, request, func(_ context.Context, _ string, id string) (usageObservation, error) {
				if mode == "expired" {
					time.Sleep(180 * time.Millisecond)
				}
				v := usageTestResponse(id)
				switch mode {
				case "wrong_model":
					v.record.Model = "example/alias"
				case "wrong_id":
					v.record.GenerationID = "gen-other"
				case "wrong_hash":
					v.record.ResponseSHA256 = strings.Repeat("f", 64)
				case "cancelled":
					cancel()
				}
				return v, nil
			})
			if err == nil || !reflect.DeepEqual(o, AcquiredUsageObservation{}) {
				t.Fatal("failed acquisition returned observation")
			}
		})
	}
}

func TestUsageHostPendingAndCrossPurpose(t *testing.T) {
	for _, cross := range []bool{false, true} {
		t.Run(fmt.Sprint(cross), func(t *testing.T) {
			path, info, r := usageHostFixture(t)
			ctx := context.Background()
			d, err := openHostDirectory(ctx, path, true)
			if err != nil {
				t.Fatal(err)
			}
			s, _, err := d.load()
			if err != nil {
				d.close()
				t.Fatal(err)
			}
			next, err := freshUsageState(info.Binding, 2, *info.Usage, usageTestKey2, "operator:rotation")
			if cross {
				next, err = freshState(info.Binding, 2, []Scope{{"member", r.OrganizationID}}, "operator:wrong-purpose")
			}
			if err != nil {
				d.close()
				t.Fatal(err)
			}
			if err = d.writeIntent(hostIntent{PreviousSHA256: stateHash(stateBytes(s)), Next: next}); err != nil {
				d.close()
				t.Fatal(err)
			}
			d.close()
			if _, err = observeHostUsage(ctx, path, r, func(context.Context, string, string) (usageObservation, error) {
				t.Fatal("pending acquisition")
				return nil, nil
			}); err == nil {
				t.Fatal("pending state accepted")
			}
			inspection, err := InspectHost(ctx, path)
			if cross {
				if err == nil {
					t.Fatal("cross-purpose pending accepted")
				}
				if _, err = ResolveHost(ctx, path, next.Info.OperationID); err == nil {
					t.Fatal("cross-purpose pending resolved")
				}
			} else {
				if err != nil || inspection.State != "pending" || inspection.Pending.OperationID != next.Info.OperationID {
					t.Fatal("pending inspection", err)
				}
				if _, err = ResolveHost(ctx, path, next.Info.OperationID); err != nil {
					t.Fatal(err)
				}
				usageChild(t, path, r, stateHash([]byte(usageTestKey2)), false)
			}
		})
	}
}

func TestUsageHostDrainsBeforeRotation(t *testing.T) {
	path, info, r := usageHostFixture(t)
	entered, release := make(chan struct{}), make(chan struct{})
	done := make(chan error, 1)
	go func() {
		_, err := observeHostUsage(context.Background(), path, r, func(_ context.Context, _ string, id string) (usageObservation, error) {
			close(entered)
			<-release
			return usageTestResponse(id), nil
		})
		done <- err
	}()
	select {
	case <-entered:
	case <-time.After(5 * time.Second):
		t.Fatal("acquisition did not start")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	_, err := ReplaceUsageHost(ctx, path, info.Epoch, *info.Usage, usageTestKey2, "operator:rotate")
	cancel()
	close(release)
	if err == nil {
		t.Fatal("rotation overtook active acquisition")
	}
	if err = <-done; err != nil {
		t.Fatal(err)
	}
	next, err := ReplaceUsageHost(context.Background(), path, info.Epoch, *info.Usage, usageTestKey2, "operator:rotate")
	if err != nil || next.Epoch != 2 {
		t.Fatal("rotation after drain", err)
	}
}

func TestUsageHostStateAndLegacyEncoding(t *testing.T) {
	readPath, _ := hostFixture(t)
	legacy := mustReadHostTestFile(t, filepath.Join(readPath, "access.json"))
	// Explicit old wire shape: optional new fields must not alter durable v1 bytes.
	var old struct {
		Info struct {
			Version           int     `json:"version"`
			Binding           Binding `json:"binding"`
			Epoch             uint64  `json:"epoch"`
			OperationID       string  `json:"operation_id"`
			Scopes            []Scope `json:"scopes"`
			OperatorReference string  `json:"operator_reference"`
			ConfirmedAt       string  `json:"confirmed_at"`
			PublicKeySHA256   string  `json:"public_key_sha256"`
		} `json:"info"`
		Seed []byte `json:"seed"`
	}
	if json.Unmarshal(legacy, &old) != nil {
		t.Fatal("legacy decode")
	}
	raw, _ := json.Marshal(old)
	if !bytes.Equal(raw, legacy) {
		t.Fatal("old authority encoding changed")
	}
	if _, err := decodeState(raw); err != nil {
		t.Fatal("old authority refused", err)
	}
	path, _, _ := usageHostFixture(t)
	base := mustReadHostTestFile(t, filepath.Join(path, "access.json"))
	for _, mode := range []string{"seed", "scopes", "purpose", "credential", "fingerprint", "public_key_echo", "escaped_key_echo"} {
		t.Run(mode, func(t *testing.T) {
			var s hostState
			if json.Unmarshal(base, &s) != nil {
				t.Fatal("decode")
			}
			switch mode {
			case "seed":
				s.Seed = make([]byte, 32)
			case "scopes":
				s.Info.Scopes = []Scope{{"member", "org_test"}}
			case "purpose":
				s.Info.Purpose = ""
			case "credential":
				s.UsageCredential = usageTestKey2
			case "fingerprint":
				s.Info.Usage.CredentialSHA256 = strings.Repeat("d", 64)
			case "public_key_echo":
				s.Info.OperatorReference = usageTestKey
			case "escaped_key_echo":
				s.UsageCredential = `synthetic-<private>-key`
				s.Info.Usage.CredentialSHA256 = stateHash([]byte(s.UsageCredential))
				s.Info.Usage.ArchivedProviderAccount = "prefix:" + s.UsageCredential
			}
			if _, err := decodeState(stateBytes(s)); err == nil {
				t.Fatal("mixed or inconsistent state accepted")
			}
		})
	}
}

func usageChild(t *testing.T, path string, r UsageRequest, keySHA string, denied bool) {
	t.Helper()
	exe, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	raw, _ := json.Marshal(r)
	cmd := exec.CommandContext(ctx, exe, "-test.run=^TestUsageHostProcessHelper$")
	cmd.Env = append(os.Environ(), "RUNVERGE_USAGE_TEST_CHILD=1", "RUNVERGE_USAGE_TEST_PATH="+path, "RUNVERGE_USAGE_TEST_REQUEST="+string(raw), "RUNVERGE_USAGE_TEST_KEY_SHA="+keySHA, "RUNVERGE_USAGE_TEST_DENIED="+fmt.Sprint(denied))
	if output, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("fresh-process usage check failed: %v: %s", err, output)
	}
}
func TestUsageHostProcessHelper(t *testing.T) {
	if os.Getenv("RUNVERGE_USAGE_TEST_CHILD") != "1" {
		return
	}
	var r UsageRequest
	if json.Unmarshal([]byte(os.Getenv("RUNVERGE_USAGE_TEST_REQUEST")), &r) != nil {
		t.Fatal("child request")
	}
	denied := os.Getenv("RUNVERGE_USAGE_TEST_DENIED") == "true"
	o, err := observeHostUsage(context.Background(), os.Getenv("RUNVERGE_USAGE_TEST_PATH"), r, func(_ context.Context, key, id string) (usageObservation, error) {
		if denied || stateHash([]byte(key)) != os.Getenv("RUNVERGE_USAGE_TEST_KEY_SHA") {
			t.Fatal("child used revoked/stale credential")
		}
		return usageTestResponse(id), nil
	})
	if denied {
		if err == nil || !reflect.DeepEqual(o, AcquiredUsageObservation{}) {
			t.Fatal("child accepted revoked policy")
		}
	} else if err != nil || o.Receipt().PolicyEpoch != 2 {
		t.Fatal("child current policy", err)
	}
}
