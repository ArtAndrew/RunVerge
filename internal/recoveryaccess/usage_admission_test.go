//go:build darwin || linux

// SPDX-License-Identifier: Elastic-2.0
package recoveryaccess

import (
	"context"
	"encoding/json"
	"errors"
	"testing"
	"time"
)

func TestUsageAdmissionCurrentPolicyAndCancellation(t *testing.T) {
	path, info, request := usageHostFixture(t)
	ctx := context.Background()
	o, err := observeHostUsage(ctx, path, request, usageTestRead(t, usageTestKey))
	if err != nil {
		t.Fatal(err)
	}
	called := 0
	admit := func(ctx context.Context, p UsagePolicyAdmission) error {
		called++
		if p.PolicyEpoch != info.Epoch || p.PolicyOperationID != info.OperationID || p.PolicySHA256 != o.Receipt().PolicySHA256 || p.AdmittedAt.Before(o.Receipt().Observation.ObservedAt) {
			t.Fatal("wrong admission policy")
		}
		deadline, ok := ctx.Deadline()
		if !ok || deadline.After(p.ExpiresAt) {
			t.Fatal("unbounded admission")
		}
		return nil
	}
	if err = WithUsageAdmission(ctx, path, o, admit); err != nil || called != 1 {
		t.Fatal("current admission failed", err)
	}
	callbackErr := errors.New("synthetic callback failure")
	if err = WithUsageAdmission(ctx, path, o, func(context.Context, UsagePolicyAdmission) error { return callbackErr }); !errors.Is(err, callbackErr) {
		t.Fatal("callback failure lost")
	}
	cancelled, cancel := context.WithCancel(ctx)
	if err = WithUsageAdmission(cancelled, path, o, func(context.Context, UsagePolicyAdmission) error { cancel(); return nil }); err == nil {
		t.Fatal("post-callback cancellation reported success")
	}
	policy := *info.Usage
	policy.CredentialReference = "rotated-key"
	next, err := ReplaceUsageHost(ctx, path, info.Epoch, policy, usageTestKey2, "operator:rotate")
	if err != nil {
		t.Fatal(err)
	}
	if err = WithUsageAdmission(ctx, path, o, admit); err == nil || called != 1 {
		t.Fatal("stale policy admitted")
	}
	policy = *next.Usage
	policy.Enabled = false
	if _, err = ReplaceUsageHost(ctx, path, next.Epoch, policy, "", "operator:revoke"); err != nil {
		t.Fatal(err)
	}
	if err = WithUsageAdmission(ctx, path, o, admit); err == nil || called != 1 {
		t.Fatal("revoked policy admitted")
	}
	if !UsageReceiptConsistent(o.Receipt(), o.RawResponse()) {
		t.Fatal("historical consistency depends on current policy")
	}
}

func TestUsageAdmissionRetainedReceiptRefusals(t *testing.T) {
	path, _, request := usageHostFixture(t)
	o, err := observeHostUsage(context.Background(), path, request, usageTestRead(t, usageTestKey))
	if err != nil {
		t.Fatal(err)
	}
	mutations := map[string]func(*UsageAcquisitionReceipt){
		"policy_digest":      func(r *UsageAcquisitionReceipt) { r.PolicySHA256 = request.SourcePlanSHA256 },
		"request_account":    func(r *UsageAcquisitionReceipt) { r.Request.ArchivedProviderAccount = "another-account" },
		"request_model":      func(r *UsageAcquisitionReceipt) { r.Request.Model = "other/model" },
		"request_generation": func(r *UsageAcquisitionReceipt) { r.Request.GenerationID = "gen-other" },
		"policy_timestamp": func(r *UsageAcquisitionReceipt) {
			r.PolicyConfirmedAt = r.Observation.ObservedAt.Add(time.Hour).Format(time.RFC3339Nano)
		},
		"cost":     func(r *UsageAcquisitionReceipt) { r.Observation.ExactMicroUSD = "2" },
		"finality": func(r *UsageAcquisitionReceipt) { r.Observation.SettlementState = "settled" },
		"disabled": func(r *UsageAcquisitionReceipt) { r.Policy.Enabled = false },
	}
	for name, mutate := range mutations {
		t.Run(name, func(t *testing.T) {
			wrong := o
			mutate(&wrong.receipt)
			wrong.receipt.ReceiptSHA256 = ""
			raw, _ := json.Marshal(wrong.receipt)
			wrong.receipt.ReceiptSHA256 = stateHash(raw)
			if UsageReceiptConsistent(wrong.receipt, wrong.raw) {
				t.Fatal("resealed inconsistency accepted")
			}
			if err := WithUsageAdmission(context.Background(), path, wrong, func(context.Context, UsagePolicyAdmission) error { t.Fatal("bad receipt reached callback"); return nil }); err == nil {
				t.Fatal("bad admission succeeded")
			}
		})
	}
}

func TestUsageAdmissionRotationDrainsCallback(t *testing.T) {
	path, info, request := usageHostFixture(t)
	o, err := observeHostUsage(context.Background(), path, request, usageTestRead(t, usageTestKey))
	if err != nil {
		t.Fatal(err)
	}
	entered, release := make(chan struct{}), make(chan struct{})
	done := make(chan error, 1)
	go func() {
		done <- WithUsageAdmission(context.Background(), path, o, func(context.Context, UsagePolicyAdmission) error { close(entered); <-release; return nil })
	}()
	select {
	case <-entered:
	case err := <-done:
		t.Fatal("admission ended before callback", err)
	case <-time.After(5 * time.Second):
		close(release)
		t.Fatal("admission did not enter callback")
	}
	policy := *info.Usage
	policy.Enabled = false
	ctx, cancel := context.WithTimeout(context.Background(), 75*time.Millisecond)
	_, rotateErr := ReplaceUsageHost(ctx, path, info.Epoch, policy, "", "operator:revoke")
	cancel()
	close(release)
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	if rotateErr == nil {
		t.Fatal("rotation bypassed in-flight admission")
	}
	if _, err := ReplaceUsageHost(context.Background(), path, info.Epoch, policy, "", "operator:revoke"); err != nil {
		t.Fatal("drained rotation failed", err)
	}
}
