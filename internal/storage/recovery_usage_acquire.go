// SPDX-License-Identifier: Elastic-2.0
package storage

import (
	"context"

	"agentplatform.local/platform/internal/recoveryaccess"
	"agentplatform.local/platform/internal/recoveryarchive"
)

// AcquireRecoveryUsage derives the request from authenticated history before
// consulting CURRENT host-only account policy. No caller-selected generation,
// model, account or amount can replace the stored bindings. The network request
// holds no database recovery lock. This does not persist or settle observations;
// durable ingestion must revalidate the target and then-current host authority.
func (s *Store) AcquireRecoveryUsage(ctx context.Context, selected, late *recoveryarchive.VerifiedArchive, verified *recoveryarchive.VerifiedCaptureLink, spec RecoveryTargetSpec, limits RecoverySnapshotLimits, selection RecoveryUsageSelection, authorityPath string) (recoveryaccess.AcquiredUsageObservation, error) {
	return s.acquireRecoveryUsage(ctx, selected, late, verified, spec, limits, selection, authorityPath, "")
}

// AcquirePlannedRecoveryUsage refuses a changed reviewed plan before contacting
// the provider. The comparison belongs to the same planning path that derives
// the outgoing request, not to a separate earlier CLI preview. Durable admission
// still revalidates the target and current policy after the network request.
func (s *Store) AcquirePlannedRecoveryUsage(ctx context.Context, selected, late *recoveryarchive.VerifiedArchive, verified *recoveryarchive.VerifiedCaptureLink, spec RecoveryTargetSpec, limits RecoverySnapshotLimits, selection RecoveryUsageSelection, authorityPath, expectedPlanSHA256 string) (recoveryaccess.AcquiredUsageObservation, error) {
	if !validDigest(expectedPlanSHA256) {
		return recoveryaccess.AcquiredUsageObservation{}, ErrRecoveryUsageBinding
	}
	return s.acquireRecoveryUsage(ctx, selected, late, verified, spec, limits, selection, authorityPath, expectedPlanSHA256)
}

func (s *Store) acquireRecoveryUsage(ctx context.Context, selected, late *recoveryarchive.VerifiedArchive, verified *recoveryarchive.VerifiedCaptureLink, spec RecoveryTargetSpec, limits RecoverySnapshotLimits, selection RecoveryUsageSelection, authorityPath, expectedPlanSHA256 string) (recoveryaccess.AcquiredUsageObservation, error) {
	if ctx == nil {
		return recoveryaccess.AcquiredUsageObservation{}, ErrRecoveryUsageBinding
	}
	for _, archive := range []*recoveryarchive.VerifiedArchive{selected, late} {
		if archive == nil {
			return recoveryaccess.AcquiredUsageObservation{}, ErrRecoveryUsageBinding
		}
	}
	plan, err := s.PlanRecoveryUsageReadback(ctx, selected, late, verified, spec, limits, selection)
	if err != nil {
		return recoveryaccess.AcquiredUsageObservation{}, err
	}
	if plan.State != "request_identity_available" || !plan.ExecutionHeld || !plan.RollbackConfirmed || (expectedPlanSHA256 != "" && plan.PlanSHA256 != expectedPlanSHA256) {
		return recoveryaccess.AcquiredUsageObservation{}, ErrRecoveryUsageBinding
	}
	return recoveryaccess.ObserveHostUsage(ctx, authorityPath, recoveryUsageRequestFromPlan(plan))
}

func recoveryUsageRequestFromPlan(plan RecoveryUsageReadbackPlan) recoveryaccess.UsageRequest {
	return recoveryaccess.UsageRequest{
		Binding:          recoveryaccess.Binding{TargetID: plan.TargetID, RestoreID: plan.RestoreID, SnapshotSHA256: plan.SelectedSnapshotSHA256},
		SourcePlanSHA256: plan.PlanSHA256, SourceID: plan.SourceID, OrganizationID: plan.Selection.OrganizationID,
		ModelConnectionID: plan.ModelConnectionID, ModelConnectionVersion: plan.ModelConnectionVersion,
		ConnectionIdentitySHA256: plan.ConnectionIdentitySHA, ArchivedProviderAccount: plan.ArchivedProviderAccount,
		GenerationID: plan.GenerationID, Model: plan.Model,
	}
}
