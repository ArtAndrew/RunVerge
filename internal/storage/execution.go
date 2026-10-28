// SPDX-License-Identifier: Elastic-2.0
package storage

import (
	"agentplatform.local/platform/internal/domain"
	"context"
	"errors"
	"github.com/jackc/pgx/v5"
	"time"
)

// ClaimFixture only dispatches a known deterministic, synthetic adapter.
// Real external attempts use their own Action/Reservation and cannot replay this way.
func (s *Store) ClaimFixture(ctx context.Context, worker string, duration time.Duration) (*domain.Lease, error) {
	if duration < time.Millisecond || duration > 5*time.Minute {
		return nil, domain.Err("invalid_request", "Некорректный срок аренды", 400)
	}
	tx, e := s.beginRuntime(ctx)
	if e != nil {
		return nil, e
	}
	defer tx.Rollback(ctx)
	l := &domain.Lease{}
	var attempts, maxAttempts int
	e = tx.QueryRow(ctx, `SELECT o.organization_id,o.id,o.task_id,o.run_id,o.fencing_token,o.attempts,o.max_attempts
 FROM operations o JOIN tasks t ON t.organization_id=o.organization_id AND t.id=o.task_id
 JOIN agents a ON a.organization_id=t.organization_id AND a.id=t.agent_id AND a.version=t.agent_version
 WHERE (o.status='queued' OR (o.status='leased' AND o.lease_until<clock_timestamp())) AND t.status IN('queued','running') AND o.run_id=t.current_run_id AND a.model=$1
 ORDER BY o.created_at,o.id LIMIT 1`, domain.FixtureModel).Scan(&l.OrganizationID, &l.OperationID, &l.TaskID, &l.RunID, &l.Token, &attempts, &maxAttempts)
	if errors.Is(e, pgx.ErrNoRows) {
		return nil, nil
	}
	if e != nil {
		return nil, e
	}
	authorityErr := lockOperationAuthorityTx(ctx, tx, l.OrganizationID, l.TaskID, l.OperationID)
	if authorityErr != nil && !errors.Is(authorityErr, domain.Forbidden) {
		return nil, authorityErr
	}
	e = tx.QueryRow(ctx, `SELECT o.run_id,o.fencing_token,o.attempts,o.max_attempts FROM operations o JOIN tasks t ON t.organization_id=o.organization_id AND t.id=o.task_id
 WHERE o.organization_id=$1 AND o.id=$2 AND (o.status='queued' OR (o.status='leased' AND o.lease_until<clock_timestamp()))
 AND t.status IN('queued','running') AND o.run_id=t.current_run_id FOR UPDATE OF o SKIP LOCKED`, l.OrganizationID, l.OperationID).Scan(&l.RunID, &l.Token, &attempts, &maxAttempts)
	if errors.Is(e, pgx.ErrNoRows) {
		return nil, nil
	}
	if e != nil {
		return nil, e
	}
	scopes, e := lockCostScopes(ctx, tx, l.OrganizationID, l.TaskID)
	if e != nil {
		return nil, e
	}
	l.Task = scopes.tasks[len(scopes.tasks)-1]
	l.RunInput, e = loadRunInputTx(ctx, tx, l.OrganizationID, l.TaskID, l.RunID)
	if e != nil {
		return nil, e
	}
	reason := ""
	if !scopes.now.Before(l.Task.ExecutionDeadline) {
		reason = "execution_deadline"
	}
	if len("Тестовый результат\n"+domain.ExecutionInput(*l)) > 262144 {
		reason = "fixture_input_too_large"
	}
	if authorityErr == nil {
		authorityErr = checkOperationAuthorityTx(ctx, tx, l.OrganizationID, l.TaskID, l.OperationID)
	}
	if e = authorityErr; e != nil {
		if !errors.Is(e, domain.Forbidden) {
			return nil, e
		}
		reason = "authorization_revoked"
	}
	if !scopes.active {
		reason = "authorization_revoked"
	}
	if reason == "" && attempts >= maxAttempts {
		reason = "attempt_limit"
	}
	if reason != "" {
		if e = block(ctx, tx, l, reason); e != nil {
			return nil, e
		}
		return nil, tx.Commit(ctx)
	}
	reservation, e := scanReservation(tx.QueryRow(ctx, "SELECT "+reservationColumns+" FROM reservations WHERE organization_id=$1 AND operation_id=$2 AND category='fixture_legacy' FOR UPDATE", l.OrganizationID, l.OperationID))
	if errors.Is(e, domain.NotFound) {
		if e = scopes.admit(domain.FixtureCost); e != nil {
			if e = block(ctx, tx, l, "budget_exceeded"); e != nil {
				return nil, e
			}
			return nil, tx.Commit(ctx)
		}
		reservation, e = scanReservation(tx.QueryRow(ctx, "INSERT INTO reservations(organization_id,id,task_id,operation_id,amount_microusd,status) VALUES($1,$2,$3,$4,$5,'reserved') RETURNING "+reservationColumns, l.OrganizationID, domain.NewID("res"), l.TaskID, l.OperationID, domain.FixtureCost))
		if e != nil {
			return nil, e
		}
		if e = applyCostDelta(ctx, tx, &scopes, domain.FixtureCost, 0, false); e != nil {
			return nil, e
		}
		if e = recordLedger(ctx, tx, reservation, "fixture.reserve", digestValue(l.OperationID), "reserve", domain.FixtureCost, 0); e != nil {
			return nil, e
		}
	} else if e != nil {
		return nil, e
	} else if reservation.Status != "reserved" {
		if e = block(ctx, tx, l, "cost_reconciliation_required"); e != nil {
			return nil, e
		}
		return nil, tx.Commit(ctx)
	} else if e = scopes.admit(0); e != nil {
		if e = block(ctx, tx, l, "budget_exceeded"); e != nil {
			return nil, e
		}
		return nil, tx.Commit(ctx)
	}
	l.ReservationID = reservation.ID
	l.Token++
	l.OwnerID = worker
	if _, e = tx.Exec(ctx, "UPDATE operations SET status='leased',lease_owner=$3,lease_until=clock_timestamp()+$4::interval,fencing_token=$5,attempts=attempts+1 WHERE organization_id=$1 AND id=$2", l.OrganizationID, l.OperationID, worker, duration.String(), l.Token); e != nil {
		return nil, e
	}
	l.Task, e = scanTask(tx.QueryRow(ctx, "UPDATE tasks SET status='running',version=version+1,updated_at=now(),waiting_reason='' WHERE organization_id=$1 AND id=$2 RETURNING "+taskColumns, l.OrganizationID, l.TaskID))
	if e != nil {
		return nil, e
	}
	if _, e = tx.Exec(ctx, "UPDATE runs SET status='running' WHERE organization_id=$1 AND id=$2", l.OrganizationID, l.RunID); e != nil {
		return nil, e
	}
	l.Agent, e = scanAgent(tx.QueryRow(ctx, "SELECT "+agentColumns+" FROM agents WHERE organization_id=$1 AND id=$2 AND version=$3", l.OrganizationID, l.Task.AgentID, l.Task.AgentVersion))
	if e != nil {
		return nil, e
	}
	if e = emit(ctx, tx, l.OrganizationID, l.TaskID, "operation.claimed", map[string]any{"operation_id": l.OperationID, "fencing_token": l.Token, "attempt": attempts + 1, "adapter": "fixture", "synthetic_cost": true}); e != nil {
		return nil, e
	}
	return l, tx.Commit(ctx)
}
func block(ctx context.Context, tx pgx.Tx, l *domain.Lease, reason string) error {
	if _, e := tx.Exec(ctx, "UPDATE operations SET status='blocked',lease_owner=NULL,lease_until=NULL WHERE organization_id=$1 AND id=$2", l.OrganizationID, l.OperationID); e != nil {
		return e
	}
	if _, e := tx.Exec(ctx, "UPDATE tasks SET status='blocked',version=version+1,updated_at=now(),waiting_reason=$3 WHERE organization_id=$1 AND id=$2", l.OrganizationID, l.TaskID, reason); e != nil {
		return e
	}
	if _, e := tx.Exec(ctx, "UPDATE runs SET status='blocked' WHERE organization_id=$1 AND id=$2", l.OrganizationID, l.RunID); e != nil {
		return e
	}
	return emit(ctx, tx, l.OrganizationID, l.TaskID, "task.blocked", map[string]any{"reason": reason})
}
func (s *Store) CheckLease(ctx context.Context, l *domain.Lease) error {
	if l == nil {
		return domain.Err("stale_lease", "Выполнение больше не разрешено", 409)
	}
	tx, err := s.beginRuntime(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx)
	if err = lockOperationAuthorityTx(ctx, tx, l.OrganizationID, l.TaskID, l.OperationID); err != nil {
		if errors.Is(err, domain.Forbidden) {
			return domain.Err("stale_lease", "Выполнение больше не разрешено", 409)
		}
		return err
	}
	if err = checkLeaseTx(ctx, tx, l); err != nil {
		return err
	}
	var running bool
	if err = tx.QueryRow(ctx, "SELECT status='running' FROM tasks WHERE organization_id=$1 AND id=$2", l.OrganizationID, l.TaskID).Scan(&running); err != nil {
		return err
	}
	if !running {
		return domain.Err("stale_lease", "Выполнение больше не разрешено", 409)
	}
	return tx.Commit(ctx)
}
func (s *Store) CompleteFixture(ctx context.Context, l *domain.Lease, content string) error {
	if len(content) > 262144 {
		return domain.Err("invalid_request", "Результат превышает лимит размера", 400)
	}
	tx, e := s.beginRuntime(ctx)
	if e != nil {
		return e
	}
	defer tx.Rollback(ctx)
	// Lock the original authority rows before money locks without rejecting a
	// revoked delegation: exact late synthetic accounting must still be recorded.
	if e = lockOperationAuthorityRowsTx(ctx, tx, l.OrganizationID, l.TaskID, l.OperationID); e != nil {
		// An authority row is selected by the exact claimed task/operation pair.
		// A forged pairing is a stale lease, never a tenant object lookup result.
		if errors.Is(e, domain.NotFound) {
			return domain.Err("stale_lease", "Исполнитель потерял право на действие", 409)
		}
		return e
	}

	// This deterministic adapter records its exact known synthetic observation
	// under the original fence, then refuses publication if authority changed.
	if e = checkLeaseFenceTx(ctx, tx, l); e != nil {
		return e
	}
	scopes, e := lockCostScopes(ctx, tx, l.OrganizationID, l.TaskID)
	if e != nil {
		return e
	}
	t := scopes.tasks[len(scopes.tasks)-1]
	input, e := loadRunInputTx(ctx, tx, l.OrganizationID, l.TaskID, l.RunID)
	if e != nil {
		return e
	}
	if content != "Тестовый результат\n"+domain.ExecutionInput(domain.Lease{Task: t, RunInput: input}) {
		return domain.Err("invalid_request", "Результат не прошёл проверку против сохранённой цели", 400)
	}
	r, e := scanReservation(tx.QueryRow(ctx, "SELECT "+reservationColumns+" FROM reservations WHERE organization_id=$1 AND id=$2 AND operation_id=$3 AND category='fixture_legacy' FOR UPDATE", l.OrganizationID, l.ReservationID, l.OperationID))
	if e != nil {
		return e
	}
	if r.Status != "reserved" {
		return domain.Err("conflict", "Резерв уже обработан", 409)
	}
	if _, e = tx.Exec(ctx, "UPDATE reservations SET status='settled',actual_microusd=$3,revision=revision+1 WHERE organization_id=$1 AND id=$2", l.OrganizationID, r.ID, r.AmountMicroUSD); e != nil {
		return e
	}
	if e = applyCostDelta(ctx, tx, &scopes, -r.AmountMicroUSD, r.AmountMicroUSD, false); e != nil {
		return e
	}
	if e = recordLedger(ctx, tx, r, "fixture.settle", digestValue(l.OperationID), "settle", -r.AmountMicroUSD, r.AmountMicroUSD); e != nil {
		return e
	}
	if e = emit(ctx, tx, l.OrganizationID, l.TaskID, "cost.settled", map[string]any{"reservation_id": r.ID, "actual_microusd": r.AmountMicroUSD, "synthetic_cost": true}); e != nil {
		return e
	}
	if t.Status == "cancelled" {
		if _, e = tx.Exec(ctx, "UPDATE operations SET status='cancelled',lease_owner=NULL,lease_until=NULL WHERE organization_id=$1 AND id=$2", l.OrganizationID, l.OperationID); e != nil {
			return e
		}
		return tx.Commit(ctx)
	}
	if e = checkOperationAuthorityTx(ctx, tx, l.OrganizationID, l.TaskID, l.OperationID); e != nil || !scopes.active {
		if e != nil && !errors.Is(e, domain.Forbidden) {
			return e
		}
		if e = block(ctx, tx, l, "authorization_revoked"); e != nil {
			return e
		}
		return tx.Commit(ctx)
	}
	provenance := map[string]any{"adapter": "fixture:echo", "verified": true, "verification": "exact_goal_echo", "operation_id": l.OperationID, "synthetic_cost": true}
	artifact := domain.Artifact{ID: domain.NewID("art"), OrganizationID: l.OrganizationID, TaskID: l.TaskID}
	if _, e = tx.Exec(ctx, "INSERT INTO artifacts(organization_id,id,task_id,name,media_type,content,provenance) VALUES($1,$2,$3,'fixture-result.txt','text/plain',$4,$5)", l.OrganizationID, artifact.ID, l.TaskID, content, provenance); e != nil {
		return e
	}
	if _, e = RegisterCandidateTx(ctx, tx, t, l.RunID, l.OperationID, artifact, "fixture"); e != nil {
		return e
	}
	if _, e = tx.Exec(ctx, "UPDATE tasks SET status='succeeded',result=$3,version=version+1,updated_at=now() WHERE organization_id=$1 AND id=$2", l.OrganizationID, l.TaskID, content); e != nil {
		return e
	}
	if _, e = tx.Exec(ctx, "UPDATE operations SET status='succeeded',lease_owner=NULL,lease_until=NULL WHERE organization_id=$1 AND id=$2", l.OrganizationID, l.OperationID); e != nil {
		return e
	}
	if _, e = tx.Exec(ctx, "UPDATE runs SET status='succeeded' WHERE organization_id=$1 AND id=$2", l.OrganizationID, l.RunID); e != nil {
		return e
	}
	if e = emit(ctx, tx, l.OrganizationID, l.TaskID, "task.succeeded", map[string]any{"verification": "exact_goal_echo", "adapter": "fixture:echo"}); e != nil {
		return e
	}
	return tx.Commit(ctx)
}

// Reconciliation is strictly a known synthetic price, never an unknown bill.
func (s *Store) ReconcileFixtureReservation(ctx context.Context) (bool, error) {
	tx, e := s.beginRuntime(ctx)
	if e != nil {
		return false, e
	}
	defer tx.Rollback(ctx)
	var org, op, taskID, reservationID string
	e = tx.QueryRow(ctx, `SELECT op.organization_id,op.id,op.task_id,r.id FROM operations op
 JOIN tasks t ON t.organization_id=op.organization_id AND t.id=op.task_id
 JOIN agents a ON a.organization_id=t.organization_id AND a.id=t.agent_id AND a.version=t.agent_version
 JOIN reservations r ON r.organization_id=op.organization_id AND r.operation_id=op.id
 WHERE a.model=$1 AND r.category='fixture_legacy' AND r.status='reserved' AND t.status IN('cancelled','blocked')
 AND (op.status IN('blocked','cancelled') OR (op.status='leased' AND op.lease_until<clock_timestamp()))
 ORDER BY op.created_at,op.id FOR UPDATE OF op SKIP LOCKED LIMIT 1`, domain.FixtureModel).Scan(&org, &op, &taskID, &reservationID)
	if errors.Is(e, pgx.ErrNoRows) {
		return false, nil
	}
	if e != nil {
		return false, e
	}
	scopes, e := lockCostScopes(ctx, tx, org, taskID)
	if e != nil {
		return false, e
	}
	r, e := scanReservation(tx.QueryRow(ctx, "SELECT "+reservationColumns+" FROM reservations WHERE organization_id=$1 AND id=$2 AND category='fixture_legacy' AND status='reserved' FOR UPDATE", org, reservationID))
	if e != nil {
		return false, e
	}
	if _, e = tx.Exec(ctx, "UPDATE reservations SET status='settled',actual_microusd=$3,revision=revision+1 WHERE organization_id=$1 AND id=$2", org, r.ID, r.AmountMicroUSD); e != nil {
		return false, e
	}
	if e = applyCostDelta(ctx, tx, &scopes, -r.AmountMicroUSD, r.AmountMicroUSD, false); e != nil {
		return false, e
	}
	if e = recordLedger(ctx, tx, r, "fixture.reconcile", digestValue(op), "settle", -r.AmountMicroUSD, r.AmountMicroUSD); e != nil {
		return false, e
	}
	if _, e = tx.Exec(ctx, "UPDATE operations SET status=CASE WHEN status='leased' THEN 'cancelled' ELSE status END,lease_owner=NULL,lease_until=NULL,fencing_token=fencing_token+1 WHERE organization_id=$1 AND id=$2", org, op); e != nil {
		return false, e
	}
	if e = emit(ctx, tx, org, taskID, "cost.fixture_reconciled", map[string]any{"reservation_id": r.ID, "actual_microusd": r.AmountMicroUSD, "synthetic_cost": true, "basis": "fixture_per_logical_operation"}); e != nil {
		return false, e
	}
	return true, tx.Commit(ctx)
}
