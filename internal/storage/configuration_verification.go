// SPDX-License-Identifier: Elastic-2.0
package storage

import (
	"context"
	"encoding/json"
	"errors"
	"time"

	"agentplatform.local/platform/internal/domain"
	"github.com/jackc/pgx/v5"
)

type configurationVerificationRecord struct {
	domain.ConfigurationVerification
	ActorKind, AdmittedRole               string
	ActorAuthorityEpoch, ActorPolicyEpoch int64
	WorkerID                              string
	LeaseUntil, DispatchStartedAt         *time.Time
	FencingToken                          int64
	DispatchedWorkerID                    string
	DispatchedFencingToken                int64
}

const configurationVerificationColumns = "body,status,version,reason_code,created_at,updated_at,actor_kind,admitted_role,actor_authority_epoch,actor_policy_epoch,worker_id,lease_until,fencing_token,dispatch_started_at,COALESCE(dispatched_worker_id,''),COALESCE(dispatched_fencing_token,0),outcome"

func scanConfigurationVerification(row scanner) (v configurationVerificationRecord, err error) {
	var body, outcome []byte
	var status, reason string
	var version int
	var created, updated time.Time
	err = row.Scan(&body, &status, &version, &reason, &created, &updated, &v.ActorKind, &v.AdmittedRole, &v.ActorAuthorityEpoch, &v.ActorPolicyEpoch, &v.WorkerID, &v.LeaseUntil, &v.FencingToken, &v.DispatchStartedAt, &v.DispatchedWorkerID, &v.DispatchedFencingToken, &outcome)
	if err != nil {
		return v, rowExists(err)
	}
	if len(body) > 16384 || json.Unmarshal(body, &v.ConfigurationVerification) != nil {
		return v, configurationProfileConflict()
	}
	v.Status, v.Version, v.ReasonCode, v.CreatedAt, v.UpdatedAt = status, version, reason, created, updated
	if len(outcome) > 0 {
		var observed struct {
			ProtocolVersion string `json:"protocol_version"`
			ToolCount       int    `json:"tool_count"`
			CatalogDigest   string `json:"catalog_digest"`
			ResponseDigest  string `json:"response_digest"`
		}
		if len(outcome) > 16384 || json.Unmarshal(outcome, &observed) != nil {
			return v, configurationProfileConflict()
		}
		v.ProtocolVersion, v.ToolCount, v.CatalogDigest, v.ResponseDigest = observed.ProtocolVersion, observed.ToolCount, observed.CatalogDigest, observed.ResponseDigest
	}
	return v, nil
}
func loadConfigurationVerificationTx(ctx context.Context, tx pgx.Tx, org, id string, lock bool) (configurationVerificationRecord, error) {
	q := "SELECT " + configurationVerificationColumns + " FROM configuration_verifications WHERE organization_id=$1 AND id=$2"
	if lock {
		q += " FOR UPDATE"
	}
	v, err := scanConfigurationVerification(tx.QueryRow(ctx, q, org, id))
	if err == nil && (v.OrganizationID != org || v.ID != id) {
		err = configurationProfileConflict()
	}
	return v, err
}
func loadConfigurationVerificationForModelTx(ctx context.Context, tx pgx.Tx, org, taskID string) (configurationVerificationRecord, error) {
	return scanConfigurationVerification(tx.QueryRow(ctx, "SELECT "+configurationVerificationColumns+" FROM configuration_verifications WHERE organization_id=$1 AND task_id=$2 AND kind='model'", org, taskID))
}

func checkConfigurationVerificationAuthorityTx(ctx context.Context, tx pgx.Tx, v configurationVerificationRecord) error {
	p, epoch, policy, err := currentConfigurationPrincipalTx(ctx, tx, v.OrganizationID, v.ActorID)
	if err != nil {
		return err
	}
	if p.Kind != v.ActorKind || !configurationMember(domain.Principal{Kind: v.ActorKind, Role: v.AdmittedRole}) || epoch != v.ActorAuthorityEpoch || policy != v.ActorPolicyEpoch {
		return domain.Forbidden
	}
	if _, err = configurationAuthorityTx(ctx, tx, p, v.GrantID, "verify", []domain.ConfigurationScope{v.Scope}); err != nil {
		return err
	}
	if err = checkConfigurationResourceAdmissionTx(ctx, tx, v.OrganizationID, v.Kind, v.Scope.TargetID, v.ResourceVersion); err != nil {
		return err
	}
	var active bool
	table := "model_connections"
	if v.Kind == "mcp" {
		table = "mcp_connections"
	}
	if err = tx.QueryRow(ctx, "SELECT active FROM "+table+" WHERE organization_id=$1 AND id=$2 AND version=$3 FOR SHARE", v.OrganizationID, v.Scope.TargetID, v.ResourceVersion).Scan(&active); err != nil {
		return rowExists(err)
	}
	if !active {
		return domain.Forbidden
	}
	return nil
}

func lockConfigurationVerificationAuthorityTx(ctx context.Context, tx pgx.Tx, v configurationVerificationRecord) error {
	if err := admitRecoveryRowTx(ctx, tx, "configuration_verifications", v.OrganizationID, v.ID); err != nil {
		return err
	}
	ids := []string{v.ActorID}
	if v.GrantID != "" {
		chain, err := configurationGrantChainTx(ctx, tx, v.OrganizationID, v.GrantID, false)
		if err != nil {
			return err
		}
		for _, g := range chain {
			ids = append(ids, g.IssuerID, g.SubjectID)
		}
	}
	return lockAuthorityPrincipalsTx(ctx, tx, v.OrganizationID, ids)
}
func lockConfigurationModelProbeAuthorityTx(ctx context.Context, tx pgx.Tx, org, taskID, operationID string) error {
	v, err := loadConfigurationVerificationForModelTx(ctx, tx, org, taskID)
	if errors.Is(err, domain.NotFound) {
		return nil
	}
	if err != nil {
		return err
	}
	var exact bool
	if err = tx.QueryRow(ctx, "SELECT EXISTS(SELECT 1 FROM operations WHERE organization_id=$1 AND task_id=$2 AND id=$3)", org, taskID, operationID).Scan(&exact); err != nil {
		return err
	}
	if !exact {
		return domain.Err("stale_lease", "Проверка не совпадает с операцией", 409)
	}
	return lockConfigurationVerificationAuthorityTx(ctx, tx, v)
}
func checkConfigurationModelProbeAuthorityTx(ctx context.Context, tx pgx.Tx, t domain.Task, operationID string) error {
	v, err := loadConfigurationVerificationForModelTx(ctx, tx, t.OrganizationID, t.ID)
	if errors.Is(err, domain.NotFound) {
		return nil
	}
	if err != nil {
		return err
	}
	if v.Kind != "model" || v.ResourceVersion != t.ModelConnectionVersion || v.Scope.TargetID != t.ModelConnectionID || (v.Status != "queued" && v.Status != "sent" && v.Status != "response_received") {
		return domain.Forbidden
	}
	return checkConfigurationVerificationAuthorityTx(ctx, tx, v)
}
func configurationModelProbeDeadlineTx(ctx context.Context, tx pgx.Tx, org, taskID string) (time.Time, error) {
	v, err := loadConfigurationVerificationForModelTx(ctx, tx, org, taskID)
	if errors.Is(err, domain.NotFound) {
		return time.Time{}, nil
	}
	if err != nil || v.GrantID == "" {
		return time.Time{}, err
	}
	chain, err := configurationGrantChainTx(ctx, tx, org, v.GrantID, false)
	if err != nil {
		return time.Time{}, err
	}
	var deadline time.Time
	for _, g := range chain {
		if deadline.IsZero() || g.ExpiresAt.Before(deadline) {
			deadline = g.ExpiresAt
		}
	}
	return deadline, nil
}

func createConfigurationModelProbeTx(ctx context.Context, tx pgx.Tx, p domain.Principal, v *domain.ConfigurationVerification, model domain.ModelConnection) error {
	var pending, limit int
	if err := tx.QueryRow(ctx, "SELECT pending_task_limit FROM organizations WHERE id=$1", p.OrganizationID).Scan(&limit); err != nil {
		return err
	}
	if err := tx.QueryRow(ctx, "SELECT count(*) FROM tasks WHERE organization_id=$1 AND status NOT IN('succeeded','failed','cancelled')", p.OrganizationID).Scan(&pending); err != nil {
		return err
	}
	if pending >= limit {
		return domain.Err("unavailable", "Достигнут лимит незавершённых задач организации", 503)
	}
	budget := v.MaxChargeMicroUSD
	if budget < 1 {
		budget = 1
	}
	agentID, taskID, runID, operationID := domain.NewID("agt"), domain.NewID("tsk"), domain.NewID("run"), domain.NewID("op")
	if _, err := tx.Exec(ctx, `INSERT INTO agents(organization_id,id,name,instructions,model,tools,budget_microusd,published,model_tools) VALUES($1,$2,'Configuration connection check','Return a short acknowledgement. Do not use tools or include credentials.', $3,'[]',$4,true,'[]')`, p.OrganizationID, agentID, model.Model, budget); err != nil {
		return err
	}
	if _, err := tx.Exec(ctx, `INSERT INTO tasks(organization_id,id,owner_id,agent_id,agent_version,goal,status,budget_microusd,model_connection_id,model_connection_version,max_runs,execution_deadline,tree_depth_limit)
 VALUES($1,$2,$3,$4,1,'Check model connection. A response proves connectivity only.','queued',$5,$6,$7,1,clock_timestamp()+make_interval(secs=>$8::integer),1)`, p.OrganizationID, taskID, p.ID, agentID, budget, model.ID, model.Version, model.TimeoutMilliseconds/1000+60); err != nil {
		return err
	}
	if _, err := tx.Exec(ctx, "INSERT INTO runs(organization_id,id,task_id,principal_id,agent_id,agent_version,status) VALUES($1,$2,$3,$4,$5,1,'queued')", p.OrganizationID, runID, taskID, p.ID, agentID); err != nil {
		return err
	}
	if _, err := tx.Exec(ctx, "INSERT INTO operations(organization_id,id,task_id,run_id,authority_principal_id,status) VALUES($1,$2,$3,$4,$5,'queued')", p.OrganizationID, operationID, taskID, runID, p.ID); err != nil {
		return err
	}
	v.TaskID = taskID
	return emit(ctx, tx, p.OrganizationID, taskID, "task.created", map[string]any{"run_id": runID, "agent_version": 1})
}

func (s *Store) StartConfigurationVerification(ctx context.Context, p domain.Principal, id, key string, in domain.StartConfigurationVerification) (json.RawMessage, error) {
	if err := domain.ValidateStartConfigurationVerification(in); err != nil {
		return nil, err
	}
	return s.mutateWithPreAuthorization(ctx, p, "POST /configuration/plans/"+id+"/verify", key, in, configurationPreAuthorization, configurationPlanGuard(id, "verify"), func(ctx context.Context, tx pgx.Tx, p domain.Principal) (any, error) {
		if err := lockConfigurationOrganizationTx(ctx, tx, p.OrganizationID); err != nil {
			return nil, err
		}
		plan, err := loadConfigurationPlanTx(ctx, tx, p.OrganizationID, id, true)
		if err != nil {
			return nil, err
		}
		if plan.Version != in.ExpectedPlanVersion || plan.Digest != in.PlanDigest || plan.Status != "applied" {
			return nil, configurationProfileConflict()
		}
		var op *domain.ConfigurationOperation
		for i := range plan.Operations {
			if plan.Operations[i].ID == in.OperationID {
				op = &plan.Operations[i]
			}
		}
		if op == nil || !op.Active {
			return nil, configurationProfileConflict()
		}
		var duplicate bool
		if err = tx.QueryRow(ctx, "SELECT EXISTS(SELECT 1 FROM configuration_verifications WHERE organization_id=$1 AND plan_id=$2 AND operation_id=$3)", p.OrganizationID, id, in.OperationID).Scan(&duplicate); err != nil {
			return nil, err
		}
		if duplicate {
			return nil, configurationProfileConflict()
		}
		d, err := loadConfigurationDestinationTx(ctx, tx, p.OrganizationID, op.Scope.DestinationID, op.Scope.DestinationVersion, true)
		if err != nil {
			return nil, err
		}
		if in.MaxChargeMicroUSD > d.Destination.MaxVerifyChargeMicroUSD || (d.Model != nil && in.MaxChargeMicroUSD < d.Model.MaxChargeMicroUSD) || (d.MCP != nil && in.MaxChargeMicroUSD != 0) {
			return nil, domain.Err("budget_exceeded", "Предел проверки не совпадает с разрешённым профилем", 409)
		}
		if err = allocateConfigurationGrantTx(ctx, tx, p.OrganizationID, plan.GrantID, 0, 1, in.MaxChargeMicroUSD); err != nil {
			return nil, err
		}
		_, epoch, policy, err := currentConfigurationPrincipalTx(ctx, tx, p.OrganizationID, p.ID)
		if err != nil {
			return nil, err
		}
		var now time.Time
		if err = tx.QueryRow(ctx, "SELECT clock_timestamp()").Scan(&now); err != nil {
			return nil, err
		}
		v := domain.ConfigurationVerification{ID: domain.NewID("cfg_verify"), OrganizationID: p.OrganizationID, PlanID: id, OperationID: in.OperationID, ActorID: p.ID, GrantID: plan.GrantID, Scope: op.Scope, ResourceVersion: op.ExpectedResourceVersion + 1, Kind: op.Scope.Kind, Status: "queued", Version: 1, MaxChargeMicroUSD: in.MaxChargeMicroUSD, CostStatus: "unmetered", CreatedAt: now, UpdatedAt: now}
		if d.Model != nil {
			model, e := scanModelConnection(tx.QueryRow(ctx, "SELECT profile,active,created_at FROM model_connections WHERE organization_id=$1 AND id=$2 AND version=$3 FOR SHARE", p.OrganizationID, op.Scope.TargetID, v.ResourceVersion))
			if e != nil {
				return nil, e
			}
			v.CostStatus = "not_reserved"
			if e = createConfigurationModelProbeTx(ctx, tx, p, &v, model); e != nil {
				return nil, e
			}
		}
		body, _ := json.Marshal(v)
		created, err := scanConfigurationVerification(tx.QueryRow(ctx, `INSERT INTO configuration_verifications(organization_id,id,plan_id,operation_id,actor_id,grant_id,kind,target_id,resource_version,actor_authority_epoch,actor_policy_epoch,actor_kind,admitted_role,body,task_id,created_at,updated_at)
 VALUES($1,$2,$3,$4,$5,NULLIF($6,''),$7,$8,$9,$10,$11,$12,$13,$14,NULLIF($15,''),$16,$16) RETURNING `+configurationVerificationColumns, p.OrganizationID, v.ID, id, op.ID, p.ID, plan.GrantID, v.Kind, op.Scope.TargetID, v.ResourceVersion, epoch, policy, p.Kind, p.Role, body, v.TaskID, now))
		if err != nil {
			return nil, err
		}
		if err = checkConfigurationVerificationAuthorityTx(ctx, tx, created); err != nil {
			return nil, err
		}
		if err = appendConfigurationAuditTx(ctx, tx, p.OrganizationID, "principal", p.ID, "verification", v.ID, "verification.created", Hash(body), in.Reason); err != nil {
			return nil, err
		}
		return created.ConfigurationVerification, nil
	})
}
