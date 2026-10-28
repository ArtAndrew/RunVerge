// SPDX-License-Identifier: Elastic-2.0
package storage

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"time"

	"agentplatform.local/platform/internal/domain"
	"agentplatform.local/platform/internal/mcpbridge"
	"github.com/jackc/pgx/v5"
)

func (s *Store) ClaimConfigurationVerification(ctx context.Context, worker string, duration time.Duration) (*domain.ConfigurationVerificationLease, domain.ConfigurationProbeConfig, error) {
	var empty domain.ConfigurationProbeConfig
	if !configurationIdentifier.MatchString(worker) || duration < time.Second || duration > time.Minute {
		return nil, empty, configurationProfileInvalid()
	}
	tx, err := s.beginRuntime(ctx)
	if err != nil {
		return nil, empty, err
	}
	defer tx.Rollback(ctx)
	v, err := scanConfigurationVerification(tx.QueryRow(ctx, "SELECT "+configurationVerificationColumns+" FROM configuration_verifications WHERE kind='mcp' AND status='queued' AND (lease_until IS NULL OR lease_until<=clock_timestamp()) ORDER BY created_at,id LIMIT 1"))
	if errors.Is(err, domain.NotFound) {
		return nil, empty, nil
	}
	if err != nil {
		return nil, empty, err
	}
	if err = lockConfigurationVerificationAuthorityTx(ctx, tx, v); err != nil {
		return nil, empty, err
	}
	if err = lockConfigurationOrganizationTx(ctx, tx, v.OrganizationID); err != nil {
		return nil, empty, err
	}
	v, err = loadConfigurationVerificationTx(ctx, tx, v.OrganizationID, v.ID, true)
	if err != nil {
		return nil, empty, err
	}
	var now time.Time
	if err = tx.QueryRow(ctx, "SELECT clock_timestamp()").Scan(&now); err != nil {
		return nil, empty, err
	}
	if v.Status != "queued" || v.DispatchStartedAt != nil || (v.LeaseUntil != nil && v.LeaseUntil.After(now)) {
		return nil, empty, nil
	}
	if err = checkConfigurationVerificationAuthorityTx(ctx, tx, v); err != nil {
		if !configurationAuthorityDenied(err) {
			return nil, empty, err
		}
		if _, err = tx.Exec(ctx, "UPDATE configuration_verifications SET status='cancelled',reason_code='authorization_revoked' WHERE organization_id=$1 AND id=$2", v.OrganizationID, v.ID); err != nil {
			return nil, empty, err
		}
		return nil, empty, tx.Commit(ctx)
	}
	d, err := loadConfigurationDestinationTx(ctx, tx, v.OrganizationID, v.Scope.DestinationID, v.Scope.DestinationVersion, true)
	if err != nil {
		return nil, empty, err
	}
	purpose, err := loadCredentialPurposeProfileTx(ctx, tx, v.OrganizationID, v.Scope.CredentialPurposeID, v.Scope.CredentialPurposeVersion, true)
	if err != nil {
		return nil, empty, err
	}
	if d.MCP == nil || !d.Destination.DiscoveryUnmeteredApproved {
		return nil, empty, domain.Forbidden
	}
	v, err = scanConfigurationVerification(tx.QueryRow(ctx, "UPDATE configuration_verifications SET worker_id=$3,lease_until=clock_timestamp()+make_interval(secs=>$4::double precision),fencing_token=fencing_token+1 WHERE organization_id=$1 AND id=$2 RETURNING "+configurationVerificationColumns, v.OrganizationID, v.ID, worker, duration.Seconds()))
	if err != nil {
		return nil, empty, err
	}
	config := domain.ConfigurationProbeConfig{Destination: d, Purpose: purpose}
	lease := &domain.ConfigurationVerificationLease{OrganizationID: v.OrganizationID, ID: v.ID, PlanID: v.PlanID, OperationID: v.OperationID, WorkerID: worker, Token: v.FencingToken, LeaseUntil: *v.LeaseUntil, Scope: v.Scope, ResourceVersion: v.ResourceVersion, ProfileDigest: domain.ConfigurationProbeDigest(config, v.ResourceVersion)}
	return lease, config, tx.Commit(ctx)
}
func configurationAuthorityDenied(err error) bool {
	return errors.Is(err, domain.Forbidden) || errors.Is(err, domain.NotFound)
}
func configurationLeaseMatches(v configurationVerificationRecord, l *domain.ConfigurationVerificationLease) bool {
	return l != nil && v.OrganizationID == l.OrganizationID && v.ID == l.ID && v.PlanID == l.PlanID && v.OperationID == l.OperationID && v.WorkerID == l.WorkerID && v.FencingToken == l.Token && l.Token > 0 && l.Scope == v.Scope && l.ResourceVersion == v.ResourceVersion
}

func (s *Store) PrepareConfigurationVerificationDispatch(ctx context.Context, l *domain.ConfigurationVerificationLease) (domain.ConfigurationDispatchPermit, error) {
	var empty domain.ConfigurationDispatchPermit
	if l == nil {
		return empty, domain.Forbidden
	}
	tx, err := s.beginRuntime(ctx)
	if err != nil {
		return empty, err
	}
	defer tx.Rollback(ctx)
	v, err := loadConfigurationVerificationTx(ctx, tx, l.OrganizationID, l.ID, false)
	if err != nil {
		return empty, err
	}
	if err = lockConfigurationVerificationAuthorityTx(ctx, tx, v); err != nil {
		return empty, err
	}
	if err = lockConfigurationOrganizationTx(ctx, tx, v.OrganizationID); err != nil {
		return empty, err
	}
	v, err = loadConfigurationVerificationTx(ctx, tx, l.OrganizationID, l.ID, true)
	if err != nil {
		return empty, err
	}
	var now time.Time
	if err = tx.QueryRow(ctx, "SELECT clock_timestamp()").Scan(&now); err != nil {
		return empty, err
	}
	if v.Kind != "mcp" || !configurationLeaseMatches(v, l) || v.Status != "queued" || v.DispatchStartedAt != nil || v.LeaseUntil == nil || !now.Before(*v.LeaseUntil) {
		return empty, domain.Err("stale_lease", "Проверка потеряла исходную аренду", 409)
	}
	if err = checkConfigurationVerificationAuthorityTx(ctx, tx, v); err != nil {
		return empty, err
	}
	destination, e := loadConfigurationDestinationTx(ctx, tx, v.OrganizationID, v.Scope.DestinationID, v.Scope.DestinationVersion, true)
	if e != nil {
		return empty, e
	}
	purpose, e := loadCredentialPurposeProfileTx(ctx, tx, v.OrganizationID, v.Scope.CredentialPurposeID, v.Scope.CredentialPurposeVersion, true)
	if e != nil {
		return empty, e
	}
	profileDigest := domain.ConfigurationProbeDigest(domain.ConfigurationProbeConfig{Destination: destination, Purpose: purpose}, v.ResourceVersion)
	if configurationScope(destination.Destination) != v.Scope || l.ProfileDigest != profileDigest {
		return empty, domain.Forbidden
	}
	until := now.Add(2 * time.Second)
	if v.LeaseUntil.Before(until) {
		until = *v.LeaseUntil
	}
	if v.GrantID != "" {
		chain, e := configurationGrantChainTx(ctx, tx, v.OrganizationID, v.GrantID, false)
		if e != nil {
			return empty, e
		}
		for _, g := range chain {
			if g.ExpiresAt.Before(until) {
				until = g.ExpiresAt
			}
		}
	}
	if !now.Before(until) {
		return empty, domain.Forbidden
	}
	v, err = scanConfigurationVerification(tx.QueryRow(ctx, "UPDATE configuration_verifications SET status='sent',dispatch_started_at=$3,dispatched_worker_id=worker_id,dispatched_fencing_token=fencing_token WHERE organization_id=$1 AND id=$2 RETURNING "+configurationVerificationColumns, v.OrganizationID, v.ID, now))
	if err != nil {
		return empty, err
	}
	permit := domain.ConfigurationDispatchPermit{OrganizationID: v.OrganizationID, VerificationID: v.ID, WorkerID: l.WorkerID, FencingToken: l.Token, IssuedAt: now, NotAfter: until, ValidFor: until.Sub(now), Scope: v.Scope, ResourceVersion: v.ResourceVersion, ProfileDigest: profileDigest}
	return permit, tx.Commit(ctx)
}

func (s *Store) RecordConfigurationVerificationObservation(ctx context.Context, in domain.ConfigurationProbeObservation) (domain.ConfigurationVerification, error) {
	var empty domain.ConfigurationVerification
	if !configurationIdentifier.MatchString(in.OrganizationID) || !configurationIdentifier.MatchString(in.VerificationID) || !configurationIdentifier.MatchString(in.WorkerID) || in.FencingToken < 1 || (in.Outcome != "unknown" && in.Outcome != "response_received") {
		return empty, configurationProfileInvalid()
	}
	tx, err := s.beginRuntime(ctx)
	if err != nil {
		return empty, err
	}
	defer tx.Rollback(ctx)
	if err = lockConfigurationFactOrganizationTx(ctx, tx, in.OrganizationID); err != nil {
		return empty, err
	}
	v, err := loadConfigurationVerificationTx(ctx, tx, in.OrganizationID, in.VerificationID, true)
	if err != nil {
		return empty, err
	}
	if v.Kind != "mcp" || v.DispatchStartedAt == nil || v.DispatchedWorkerID != in.WorkerID || v.DispatchedFencingToken != in.FencingToken {
		return empty, domain.Forbidden
	}
	if in.Outcome == "unknown" {
		if v.Status == "response_received" || v.Status == "unknown" {
			return v.ConfigurationVerification, tx.Commit(ctx)
		}
		if v.Status != "sent" {
			return empty, configurationProfileConflict()
		}
		v, err = scanConfigurationVerification(tx.QueryRow(ctx, "UPDATE configuration_verifications SET status='unknown',reason_code='probe_outcome_unknown' WHERE organization_id=$1 AND id=$2 RETURNING "+configurationVerificationColumns, v.OrganizationID, v.ID))
		if err != nil {
			return empty, err
		}
		return v.ConfigurationVerification, tx.Commit(ctx)
	}
	if in.ProtocolVersion != mcpbridge.ProtocolVersion || in.ToolCount < 0 || in.ToolCount > 128 || !configurationDigest.MatchString(in.CatalogDigest) || (in.ResponseDigest != "" && !configurationDigest.MatchString(in.ResponseDigest)) {
		return empty, configurationProfileInvalid()
	}
	outcome := map[string]any{"outcome": "response_received", "protocol_version": in.ProtocolVersion, "tool_count": in.ToolCount, "catalog_digest": in.CatalogDigest}
	if in.ResponseDigest != "" {
		outcome["response_digest"] = in.ResponseDigest
	}
	body, _ := json.Marshal(outcome)
	digest := Hash(body)
	var previous *string
	if err = tx.QueryRow(ctx, "SELECT evidence_digest FROM configuration_verifications WHERE organization_id=$1 AND id=$2", v.OrganizationID, v.ID).Scan(&previous); err != nil {
		return empty, err
	}
	if previous != nil {
		if *previous != digest {
			return empty, configurationProfileConflict()
		}
		return v.ConfigurationVerification, tx.Commit(ctx)
	}
	if v.Status != "sent" && v.Status != "unknown" {
		return empty, configurationProfileConflict()
	}
	v, err = scanConfigurationVerification(tx.QueryRow(ctx, "UPDATE configuration_verifications SET status='response_received',outcome=$3,evidence_digest=$4,reason_code='' WHERE organization_id=$1 AND id=$2 RETURNING "+configurationVerificationColumns, v.OrganizationID, v.ID, body, digest))
	if err != nil {
		return empty, err
	}
	if err = appendConfigurationAuditTx(ctx, tx, v.OrganizationID, "worker", in.WorkerID, "verification", v.ID, "response_received", digest, "A bounded MCP catalog response was observed; no tool was called"); err != nil {
		return empty, err
	}
	return v.ConfigurationVerification, tx.Commit(ctx)
}

func (s *Store) PauseConfigurationVerification(ctx context.Context, l *domain.ConfigurationVerificationLease, reason string) error {
	if l == nil || !validText(reason, 128) {
		return configurationProfileInvalid()
	}
	tx, err := s.beginRuntime(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx)
	if err = lockConfigurationFactOrganizationTx(ctx, tx, l.OrganizationID); err != nil {
		return err
	}
	v, err := loadConfigurationVerificationTx(ctx, tx, l.OrganizationID, l.ID, true)
	if err != nil {
		return err
	}
	if v.Kind != "mcp" || !configurationLeaseMatches(v, l) {
		return domain.Err("stale_lease", "Проверка потеряла исходную аренду", 409)
	}
	status := "failed"
	if v.DispatchStartedAt != nil {
		status = "unknown"
		reason = "probe_dispatch_unconfirmed"
	}
	if v.Status == "response_received" || v.Status == "unknown" || v.Status == "failed" || v.Status == "cancelled" {
		return tx.Commit(ctx)
	}
	_, err = tx.Exec(ctx, "UPDATE configuration_verifications SET status=$3,reason_code=$4 WHERE organization_id=$1 AND id=$2", v.OrganizationID, v.ID, status, reason)
	if err != nil {
		return err
	}
	return tx.Commit(ctx)
}

func configurationVerificationAdmissionSQL() string {
	grant := configurationGrantScopeSQL("verify", "v.body->'scope'")
	grant = strings.ReplaceAll(grant, "g.subject_id=$2", "g.subject_id=$2 AND g.id=v.grant_id")
	grant = strings.ReplaceAll(grant, "$1", "v.organization_id")
	grant = strings.ReplaceAll(grant, "$2", "v.actor_id")
	return `EXISTS(SELECT 1 FROM organizations WHERE id=v.organization_id AND active) AND EXISTS(SELECT 1 FROM memberships m JOIN principals p ON p.id=m.principal_id JOIN organization_login_policies q ON q.organization_id=m.organization_id
 WHERE m.organization_id=v.organization_id AND m.principal_id=v.actor_id AND m.active AND p.active AND p.kind=v.actor_kind AND m.role IN('member','admin','owner')
 AND (p.kind='human' OR (p.kind='service' AND p.service_organization_id=m.organization_id AND q.service_access_enabled))
 AND m.security_epoch=v.actor_authority_epoch AND current_principal_policy_epoch(m.organization_id,m.principal_id)=v.actor_policy_epoch
 AND ((v.grant_id IS NULL AND p.kind='human' AND m.role IN('owner','admin')) OR ` + grant + `))
 AND EXISTS(SELECT 1 FROM configuration_resource_bindings b JOIN configuration_destinations d ON d.organization_id=b.organization_id AND d.id=b.destination_id AND d.version=b.destination_version
 JOIN credential_purpose_profiles p ON p.organization_id=b.organization_id AND p.id=b.credential_purpose_id AND p.version=b.credential_purpose_version
 WHERE b.organization_id=v.organization_id AND b.kind=v.kind AND b.target_id=v.target_id AND b.resource_version=v.resource_version AND d.active AND p.active)
 AND ((v.kind='model' AND EXISTS(SELECT 1 FROM model_connections WHERE organization_id=v.organization_id AND id=v.target_id AND version=v.resource_version AND active))
 OR (v.kind='mcp' AND EXISTS(SELECT 1 FROM mcp_connections WHERE organization_id=v.organization_id AND id=v.target_id AND version=v.resource_version AND active)))`
}

func (s *Store) ReconcileConfigurationVerifications(ctx context.Context) (bool, error) {
	tx, err := s.beginRuntime(ctx)
	if err != nil {
		return false, err
	}
	defer tx.Rollback(ctx)
	var org, id string
	err = tx.QueryRow(ctx, `SELECT v.organization_id,v.id FROM configuration_verifications v WHERE
 (v.status='sent' AND v.lease_until<=clock_timestamp()) OR (v.status='queued' AND (NOT (`+configurationVerificationAdmissionSQL()+`) OR (v.kind='model' AND EXISTS(SELECT 1 FROM tasks WHERE organization_id=v.organization_id AND id=v.task_id AND status IN('blocked','failed','cancelled'))))) ORDER BY v.created_at,v.id LIMIT 1`).Scan(&org, &id)
	if errors.Is(err, pgx.ErrNoRows) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	v, err := loadConfigurationVerificationTx(ctx, tx, org, id, false)
	if err != nil {
		return false, err
	}
	if err = lockConfigurationVerificationAuthorityTx(ctx, tx, v); err != nil {
		return false, err
	}
	if err = lockConfigurationFactOrganizationTx(ctx, tx, org); err != nil {
		return false, err
	}
	v, err = loadConfigurationVerificationTx(ctx, tx, org, id, true)
	if err != nil {
		return false, err
	}
	status, reason := "", ""
	var now time.Time
	if err = tx.QueryRow(ctx, "SELECT clock_timestamp()").Scan(&now); err != nil {
		return false, err
	}
	if v.Status == "sent" && v.LeaseUntil != nil && !now.Before(*v.LeaseUntil) {
		status, reason = "unknown", "probe_lease_expired"
	} else if v.Status == "queued" && v.DispatchStartedAt == nil {
		if e := checkConfigurationVerificationAuthorityTx(ctx, tx, v); e != nil {
			if !configurationAuthorityDenied(e) {
				return false, e
			}
			status, reason = "cancelled", "authorization_revoked"
		} else if v.Kind == "model" {
			var stopped bool
			if err = tx.QueryRow(ctx, "SELECT status IN('blocked','failed','cancelled') FROM tasks WHERE organization_id=$1 AND id=$2", org, v.TaskID).Scan(&stopped); err != nil {
				return false, err
			}
			if stopped {
				status, reason = "cancelled", "verification_task_stopped"
			}
		}
	}
	if status == "" {
		return false, tx.Commit(ctx)
	}
	if _, err = tx.Exec(ctx, "UPDATE configuration_verifications SET status=$3,reason_code=$4 WHERE organization_id=$1 AND id=$2", org, id, status, reason); err != nil {
		return false, err
	}
	return true, tx.Commit(ctx)
}

// Facts and proven-unsent cleanup retain tenant lock order after deactivation.
// This is deliberately not an authorization or active-organization check.
func lockConfigurationFactOrganizationTx(ctx context.Context, tx pgx.Tx, org string) error {
	var id string
	return rowExists(tx.QueryRow(ctx, "SELECT id FROM organizations WHERE id=$1 FOR UPDATE", org).Scan(&id))
}
