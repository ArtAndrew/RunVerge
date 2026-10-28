// SPDX-License-Identifier: Elastic-2.0
package storage

import (
	"context"
	"encoding/hex"
	"encoding/json"
	"errors"
	"math"
	"strings"
	"time"

	"agentplatform.local/platform/internal/domain"
	"github.com/jackc/pgx/v5"
)

const reservationColumns = "id,organization_id,task_id,operation_id,COALESCE(action_id,''),root_id,scope_task_ids,charge_key,category,provider,price_version,amount_microusd,actual_microusd,status,revision,created_at,COALESCE(pending_quarantine_id,'')"

func scanReservation(row scanner) (r domain.Reservation, e error) {
	e = row.Scan(&r.ID, &r.OrganizationID, &r.TaskID, &r.OperationID, &r.ActionID, &r.RootID, &r.ScopeTaskIDs, &r.ChargeKey, &r.Category, &r.Provider, &r.PriceVersion, &r.AmountMicroUSD, &r.ActualMicroUSD, &r.Status, &r.Revision, &r.CreatedAt, &r.PendingQuarantineID)
	return r, rowExists(e)
}

type costScopes struct {
	org                     string
	budget, reserved, spent int64
	active, hold            bool
	tasks                   []domain.Task
	now                     time.Time
}

// Admission first locks immutable owner authority rows. Then: operation ->
// organization -> root-to-leaf tasks -> reservation -> action. Trusted cost
// ingestion needs no live execution lease or current user membership.
func lockCostScopes(ctx context.Context, tx pgx.Tx, org, taskID string) (costScopes, error) {
	s := costScopes{org: org}
	e := tx.QueryRow(ctx, "SELECT budget_microusd,reserved_microusd,spent_microusd,active,financial_hold FROM organizations WHERE id=$1 FOR UPDATE", org).Scan(&s.budget, &s.reserved, &s.spent, &s.active, &s.hold)
	if e != nil {
		return s, rowExists(e)
	}
	var ids []string
	e = tx.QueryRow(ctx, `WITH RECURSIVE chain AS (
 SELECT id,parent_id,depth FROM tasks WHERE organization_id=$1 AND id=$2
 UNION ALL SELECT p.id,p.parent_id,p.depth FROM tasks p JOIN chain c ON p.id=c.parent_id WHERE p.organization_id=$1
 ) SELECT array_agg(id ORDER BY depth) FROM chain`, org, taskID).Scan(&ids)
	if e != nil {
		return s, e
	}
	if len(ids) == 0 {
		return s, domain.NotFound
	}
	rows, e := tx.Query(ctx, "SELECT "+taskColumns+" FROM tasks WHERE organization_id=$1 AND id=ANY($2::text[]) ORDER BY depth,id FOR UPDATE", org, ids)
	if e != nil {
		return s, e
	}
	defer rows.Close()
	for rows.Next() {
		t, e := scanTask(rows)
		if e != nil {
			return s, e
		}
		s.tasks = append(s.tasks, t)
	}
	if e = rows.Err(); e != nil {
		return s, e
	}
	if len(s.tasks) != len(ids) || s.tasks[0].RootID != s.tasks[0].ID || len(ids) > domain.MaxTaskDepth+1 {
		return s, errors.New("invalid accounting lineage")
	}
	rows.Close()
	if e = tx.QueryRow(ctx, "SELECT clock_timestamp()").Scan(&s.now); e != nil {
		return s, e
	}
	return s, nil
}
func (s costScopes) ids() []string {
	out := make([]string, len(s.tasks))
	for i, t := range s.tasks {
		out[i] = t.ID
	}
	return out
}
func (s costScopes) admit(amount int64) error {
	if !s.active {
		return domain.Forbidden
	}
	if s.hold || s.spent > s.budget || s.reserved > s.budget-s.spent || amount > s.budget-s.spent-s.reserved {
		return domain.Err("budget_exceeded", "Лимит организации исчерпан или требуется сверка расходов", 409)
	}
	for _, t := range s.tasks {
		if !t.ExecutionDeadline.IsZero() && !s.now.Before(t.ExecutionDeadline) {
			return domain.Err("invalid_transition", "Истёк срок выполнения дерева задач", 409)
		}
		if domain.Terminal(t.Status) || t.Status == "blocked" || t.Status == "waiting_approval" {
			return domain.Err("invalid_transition", "Задача или её родитель завершены", 409)
		}
		if t.FinancialHold || t.SpentMicroUSD > t.BudgetMicroUSD || t.ReservedMicroUSD > t.BudgetMicroUSD-t.SpentMicroUSD || amount > t.BudgetMicroUSD-t.SpentMicroUSD-t.ReservedMicroUSD {
			return domain.Err("budget_exceeded", "Лимит дерева задач исчерпан или требуется сверка расходов", 409)
		}
	}
	return nil
}
func checkedCounter(current, delta int64) (int64, error) {
	if current < 0 || (delta > 0 && current > math.MaxInt64-delta) || (delta < 0 && delta < -current) {
		return 0, domain.Err("invalid_request", "Расход за пределами поддерживаемой точности", 400)
	}
	return current + delta, nil
}
func applyCostDelta(ctx context.Context, tx pgx.Tx, s *costScopes, reserveDelta, spentDelta int64, hold bool) error {
	if e := preflightCostDelta(*s, reserveDelta, spentDelta); e != nil {
		return e
	}
	reserved, e := checkedCounter(s.reserved, reserveDelta)
	if e != nil {
		return e
	}
	spent, e := checkedCounter(s.spent, spentDelta)
	if e != nil {
		return e
	}
	over := spent > s.budget || reserved > s.budget-spent
	if _, e = tx.Exec(ctx, "UPDATE organizations SET reserved_microusd=$2,spent_microusd=$3,financial_hold=financial_hold OR $4 WHERE id=$1", s.org, reserved, spent, hold || over); e != nil {
		return e
	}
	for i := range s.tasks {
		t := &s.tasks[i]
		r, e := checkedCounter(t.ReservedMicroUSD, reserveDelta)
		if e != nil {
			return e
		}
		v, e := checkedCounter(t.SpentMicroUSD, spentDelta)
		if e != nil {
			return e
		}
		over := v > t.BudgetMicroUSD || r > t.BudgetMicroUSD-v
		if _, e = tx.Exec(ctx, "UPDATE tasks SET reserved_microusd=$3,spent_microusd=$4,financial_hold=financial_hold OR $5,version=version+1,updated_at=now() WHERE organization_id=$1 AND id=$2", s.org, t.ID, r, v, hold || over); e != nil {
			return e
		}
		t.ReservedMicroUSD = r
		t.SpentMicroUSD = v
		t.FinancialHold = t.FinancialHold || hold || over
	}
	s.reserved = reserved
	s.spent = spent
	s.hold = s.hold || hold || over
	return nil
}
func digestValue(v any) string { body, _ := json.Marshal(v); return Hash(body) }
func validDigest(value string) bool {
	b, e := hex.DecodeString(value)
	return e == nil && len(b) == 32 && strings.ToLower(value) == value
}
func checkLeaseTx(ctx context.Context, tx pgx.Tx, l *domain.Lease) error {
	if err := checkLeaseFenceTx(ctx, tx, l); err != nil {
		return err
	}
	return staleOperationAuthorityTx(ctx, tx, l)
}

func checkLeaseFenceTx(ctx context.Context, tx pgx.Tx, l *domain.Lease) error {
	var task, run string
	var valid bool
	e := tx.QueryRow(ctx, "SELECT task_id,run_id,status='leased' AND lease_owner=$3 AND fencing_token=$4 AND lease_until>clock_timestamp() FROM operations WHERE organization_id=$1 AND id=$2 FOR UPDATE", l.OrganizationID, l.OperationID, l.OwnerID, l.Token).Scan(&task, &run, &valid)
	if e != nil {
		return rowExists(e)
	}
	if !valid || task != l.TaskID || run != l.RunID {
		return domain.Err("stale_lease", "Исполнитель потерял право на действие", 409)
	}
	if e = tx.QueryRow(ctx, "SELECT current_run_id=$3 AND execution_deadline>clock_timestamp() FROM tasks WHERE organization_id=$1 AND id=$2", l.OrganizationID, l.TaskID, l.RunID).Scan(&valid); e != nil {
		return rowExists(e)
	}
	if !valid {
		return domain.Err("stale_lease", "Попытка или срок задачи изменились", 409)
	}
	return nil
}
func authorizeTaskOwner(ctx context.Context, tx pgx.Tx, org, owner string) error {
	var permitted bool
	e := tx.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM memberships m JOIN principals p ON p.id=m.principal_id JOIN organization_login_policies q ON q.organization_id=m.organization_id WHERE m.organization_id=$1 AND m.principal_id=$2 AND m.active AND p.active AND m.role<>'viewer' AND (p.kind='human' OR (p.kind='service' AND q.service_access_enabled AND p.service_organization_id=m.organization_id)))`, org, owner).Scan(&permitted)
	if e != nil {
		return e
	}
	if !permitted {
		return domain.Forbidden
	}
	return nil
}
func lockAdmissionOwnerTx(ctx context.Context, tx pgx.Tx, org, taskID string) error {
	var owner string
	if e := tx.QueryRow(ctx, "SELECT owner_id FROM tasks WHERE organization_id=$1 AND id=$2", org, taskID).Scan(&owner); e != nil {
		return rowExists(e)
	}
	ancestors, err := ancestorAuthorityIDsTx(ctx, tx, org, taskID)
	if err != nil {
		return err
	}
	if err = lockAuthorityPrincipalsTx(ctx, tx, org, append([]string{owner}, ancestors...)); err != nil {
		return err
	}
	if err = authorizeTaskOwner(ctx, tx, org, owner); err != nil {
		return err
	}
	return checkAncestorRunAuthorityTx(ctx, tx, org, taskID)
}
func recordLedger(ctx context.Context, tx pgx.Tx, r domain.Reservation, key, hash, kind string, reserveDelta, spentDelta int64) error {
	_, e := tx.Exec(ctx, "INSERT INTO ledger_entries(organization_id,id,reservation_id,task_id,root_id,event_key,event_hash,kind,delta_reserved_microusd,delta_spent_microusd,synthetic) VALUES($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11)", r.OrganizationID, domain.NewID("led"), r.ID, r.TaskID, r.RootID, key, hash, kind, reserveDelta, spentDelta, r.Category == "fixture_legacy")
	return e
}
func ledgerReplay(ctx context.Context, tx pgx.Tx, r domain.Reservation, key, hash string) (bool, error) {
	var old string
	e := tx.QueryRow(ctx, "SELECT event_hash FROM ledger_entries WHERE organization_id=$1 AND reservation_id=$2 AND event_key=$3", r.OrganizationID, r.ID, key).Scan(&old)
	if errors.Is(e, pgx.ErrNoRows) {
		return false, nil
	}
	if e != nil {
		return false, e
	}
	if old != hash {
		return true, domain.Err("conflict", "Повтор денежного события содержит другие данные", 409)
	}
	return true, nil
}

// ReserveCost atomically admits one physical attempt and persists its intent.
// Only trusted kernel code calls this method; HTTP/MCP users cannot submit bills.
func (s *Store) ReserveCost(ctx context.Context, l *domain.Lease, in domain.ReserveCostRequest) (domain.Reservation, error) {
	var empty domain.Reservation
	if !validText(in.ChargeKey, 128) || !validText(in.Provider, 128) || !validText(in.PriceVersion, 128) || !validDigest(in.IntentHash) || in.AmountMicroUSD < 0 || in.AmountMicroUSD > domain.MaxMoney {
		return empty, domain.Err("invalid_request", "Некорректный резерв или версия тарифа", 400)
	}
	switch in.Category {
	case "model", "tool", "compute", "verification", "recovery":
	default:
		return empty, domain.Err("invalid_request", "Неподдерживаемая категория расходов", 400)
	}
	tx, e := s.beginRuntime(ctx)
	if e != nil {
		return empty, e
	}
	defer tx.Rollback(ctx)
	if e = lockModelAutomationAuthorityTx(ctx, tx, l.OrganizationID, l.TaskID, l.OperationID); e != nil {
		return empty, e
	}
	if e = checkLeaseTx(ctx, tx, l); e != nil {
		return empty, e
	}
	scopes, e := lockCostScopes(ctx, tx, l.OrganizationID, l.TaskID)
	if e != nil {
		return empty, e
	}
	if e = authorizeTaskOwner(ctx, tx, l.OrganizationID, scopes.tasks[len(scopes.tasks)-1].OwnerID); e != nil {
		return empty, e
	}
	if e = checkTaskModelAdmissionTx(ctx, tx, scopes.tasks[len(scopes.tasks)-1], l, &in); e != nil {
		return empty, e
	}
	if e = checkBoundAutomationModelTx(ctx, tx, scopes.tasks[len(scopes.tasks)-1], l.OperationID); e != nil {
		return empty, e
	}
	hash := digestValue(in)
	existing, e := scanReservation(tx.QueryRow(ctx, "SELECT "+reservationColumns+" FROM reservations WHERE organization_id=$1 AND charge_key=$2 FOR UPDATE", l.OrganizationID, in.ChargeKey))
	if e == nil {
		if existing.OperationID != l.OperationID || existing.TaskID != l.TaskID {
			return empty, domain.Err("conflict", "Ключ платной попытки принадлежит другой операции", 409)
		}
		replay, e := ledgerReplay(ctx, tx, existing, "reserve", hash)
		if e != nil {
			return empty, e
		}
		if !replay {
			return empty, domain.Err("conflict", "Этот резерв создан другим протоколом", 409)
		}
		if e = checkBoundAutomationModelTx(ctx, tx, scopes.tasks[len(scopes.tasks)-1], l.OperationID); e != nil {
			return empty, e
		}
		return existing, tx.Commit(ctx)
	}
	if !errors.Is(e, domain.NotFound) {
		return empty, e
	}
	if e = scopes.admit(in.AmountMicroUSD); e != nil {
		return empty, e
	}
	if e = checkLeaseTx(ctx, tx, l); e != nil {
		return empty, e
	}
	if e = checkBoundAutomationModelTx(ctx, tx, scopes.tasks[len(scopes.tasks)-1], l.OperationID); e != nil {
		return empty, e
	}
	actionID := domain.NewID("act")
	id := domain.NewID("res")
	if _, e = tx.Exec(ctx, "INSERT INTO actions(organization_id,id,task_id,operation_id,intent_hash,idempotency_key,status) VALUES($1,$2,$3,$4,$5,$6,'pending')", l.OrganizationID, actionID, l.TaskID, l.OperationID, in.IntentHash, in.ChargeKey); e != nil {
		return empty, e
	}
	r, e := scanReservation(tx.QueryRow(ctx, "INSERT INTO reservations(organization_id,id,task_id,operation_id,amount_microusd,status,charge_key,category,provider,price_version,action_id,root_id,scope_task_ids) VALUES($1,$2,$3,$4,$5,'reserved',$6,$7,$8,$9,$10,$11,$12) RETURNING "+reservationColumns, l.OrganizationID, id, l.TaskID, l.OperationID, in.AmountMicroUSD, in.ChargeKey, in.Category, in.Provider, in.PriceVersion, actionID, scopes.tasks[0].ID, scopes.ids()))
	if e != nil {
		return empty, e
	}
	if e = applyCostDelta(ctx, tx, &scopes, in.AmountMicroUSD, 0, false); e != nil {
		return empty, e
	}
	if e = recordLedger(ctx, tx, r, "reserve", hash, "reserve", in.AmountMicroUSD, 0); e != nil {
		return empty, e
	}
	if e = emit(ctx, tx, r.OrganizationID, r.TaskID, "cost.reserved", map[string]any{"reservation_id": r.ID, "amount_microusd": r.AmountMicroUSD, "category": r.Category, "price_version": r.PriceVersion}); e != nil {
		return empty, e
	}
	return r, tx.Commit(ctx)
}

// PrepareCostDispatch preserves the generic internal API. Model workers must
// use PrepareModelDispatch and enforce its bounded, non-renewable send permit.
func (s *Store) PrepareCostDispatch(ctx context.Context, l *domain.Lease, id string) error {
	_, e := s.prepareDispatch(ctx, l, id, false)
	return e
}
func (s *Store) PrepareModelDispatch(ctx context.Context, l *domain.Lease, id string) (domain.DispatchPermit, error) {
	return s.prepareDispatch(ctx, l, id, true)
}
func (s *Store) prepareDispatch(ctx context.Context, l *domain.Lease, id string, model bool) (domain.DispatchPermit, error) {
	var empty domain.DispatchPermit
	tx, e := s.beginRuntime(ctx)
	if e != nil {
		return empty, e
	}
	defer tx.Rollback(ctx)
	if e = lockModelAutomationAuthorityTx(ctx, tx, l.OrganizationID, l.TaskID, l.OperationID); e != nil {
		return empty, e
	}
	if e = checkLeaseTx(ctx, tx, l); e != nil {
		return empty, e
	}
	scopes, e := lockCostScopes(ctx, tx, l.OrganizationID, l.TaskID)
	if e != nil {
		return empty, e
	}
	task := scopes.tasks[len(scopes.tasks)-1]
	if e = authorizeTaskOwner(ctx, tx, l.OrganizationID, task.OwnerID); e != nil {
		return empty, e
	}
	if e = scopes.admit(0); e != nil {
		return empty, e
	}
	if e = checkTaskModelAdmissionTx(ctx, tx, task, l, nil); e != nil {
		return empty, e
	}
	r, e := scanReservation(tx.QueryRow(ctx, "SELECT "+reservationColumns+" FROM reservations WHERE organization_id=$1 AND id=$2 FOR UPDATE", l.OrganizationID, id))
	if e != nil {
		return empty, e
	}
	if r.OperationID != l.OperationID || r.TaskID != l.TaskID || r.ActionID == "" || r.Status != "reserved" || r.PendingQuarantineID != "" || (model && r.Category != "model") {
		return empty, domain.Err("conflict", "Reservation cannot be dispatched", 409)
	}
	var actionStatus, intent string
	if e = tx.QueryRow(ctx, "SELECT status,intent_hash FROM actions WHERE organization_id=$1 AND id=$2 FOR UPDATE", r.OrganizationID, r.ActionID).Scan(&actionStatus, &intent); e != nil {
		return empty, rowExists(e)
	}
	if actionStatus != "pending" {
		return empty, domain.Err("conflict", "Action was already dispatched or stopped", 409)
	}
	if e = checkBoundAutomationModelTx(ctx, tx, task, l.OperationID); e != nil {
		return empty, e
	}
	automationDeadline := task.ExecutionDeadline
	if grantID, grantErr := boundAutomationModelGrantTx(ctx, tx, l.OrganizationID, l.OperationID); grantErr != nil {
		return empty, grantErr
	} else if grantID != "" {
		g, grantErr := loadAutomationGrantTx(ctx, tx, l.OrganizationID, grantID, false)
		if grantErr != nil {
			return empty, grantErr
		}
		if g.ExpiresAt.Before(automationDeadline) {
			automationDeadline = g.ExpiresAt
		}
	}
	permit := domain.DispatchPermit{OrganizationID: l.OrganizationID, TaskID: l.TaskID, OperationID: l.OperationID, ActionID: r.ActionID, ReservationID: r.ID, IntentHash: intent}
	if probeDeadline, probeErr := configurationModelProbeDeadlineTx(ctx, tx, l.OrganizationID, l.TaskID); probeErr != nil {
		return empty, probeErr
	} else if !probeDeadline.IsZero() && probeDeadline.Before(automationDeadline) {
		automationDeadline = probeDeadline
	}
	e = tx.QueryRow(ctx, `WITH authority AS MATERIALIZED (
	 SELECT clock_timestamp() AS issued_at,o.lease_until,o.fencing_token,t.execution_deadline FROM operations o JOIN tasks t ON t.organization_id=o.organization_id AND t.id=o.task_id WHERE o.organization_id=$1 AND o.id=$3 AND o.status='leased' AND o.lease_owner=$4 AND o.fencing_token=$5 AND o.lease_until>clock_timestamp() AND o.run_id=t.current_run_id AND t.execution_deadline>clock_timestamp()
 ) UPDATE actions a SET status='sent',dispatch_issued_at=x.issued_at,dispatch_not_after=LEAST(x.lease_until,x.execution_deadline,$6::timestamptz,x.issued_at+interval '2 seconds'),dispatch_fencing_token=x.fencing_token
 FROM authority x WHERE a.organization_id=$1 AND a.id=$2 AND a.status='pending' AND x.issued_at<x.lease_until AND x.issued_at<x.execution_deadline
 RETURNING a.dispatch_issued_at,a.dispatch_not_after,a.dispatch_fencing_token`, r.OrganizationID, r.ActionID, l.OperationID, l.OwnerID, l.Token, automationDeadline).Scan(&permit.IssuedAt, &permit.NotAfter, &permit.FencingToken)
	if errors.Is(e, pgx.ErrNoRows) {
		return empty, domain.Err("stale_lease", "Dispatch lease expired", 409)
	}
	if e != nil {
		return empty, e
	}
	permit.ValidFor = permit.NotAfter.Sub(permit.IssuedAt)
	if permit.ValidFor <= 0 || permit.ValidFor > 2*time.Second {
		return empty, domain.Err("stale_lease", "Invalid dispatch window", 409)
	}
	if model {
		if e = markConfigurationModelProbeDispatchTx(ctx, tx, l, r, permit); e != nil {
			return empty, e
		}
	}
	if e = recordLedger(ctx, tx, r, "dispatch", digestValue(r.ActionID), "dispatch", 0, 0); e != nil {
		return empty, e
	}
	if e = emit(ctx, tx, r.OrganizationID, r.TaskID, "action.dispatched", map[string]any{"action_id": r.ActionID, "reservation_id": r.ID, "dispatch_not_after": permit.NotAfter, "fencing_token": permit.FencingToken}); e != nil {
		return empty, e
	}
	if e = tx.Commit(ctx); e != nil {
		return empty, e
	}
	return permit, nil
}

func lockReservation(ctx context.Context, tx pgx.Tx, org, id string) (domain.Reservation, costScopes, error) {
	var empty domain.Reservation
	var scopes costScopes
	var op, task string
	e := tx.QueryRow(ctx, "SELECT operation_id,task_id FROM reservations WHERE organization_id=$1 AND id=$2", org, id).Scan(&op, &task)
	if e != nil {
		return empty, scopes, rowExists(e)
	}
	if _, e = tx.Exec(ctx, "SELECT id FROM operations WHERE organization_id=$1 AND id=$2 FOR UPDATE", org, op); e != nil {
		return empty, scopes, e
	}
	scopes, e = lockCostScopes(ctx, tx, org, task)
	if e != nil {
		return empty, scopes, e
	}
	r, e := scanReservation(tx.QueryRow(ctx, "SELECT "+reservationColumns+" FROM reservations WHERE organization_id=$1 AND id=$2 FOR UPDATE", org, id))
	if e != nil {
		return empty, scopes, e
	}
	if digestValue(scopes.ids()) != digestValue(r.ScopeTaskIDs) || r.RootID != scopes.tasks[0].ID {
		return empty, scopes, errors.New("accounting snapshot mismatch")
	}
	return r, scopes, nil
}
func (s *Store) MarkCostUnknown(ctx context.Context, org, id, eventKey, reason string) (domain.Reservation, error) {
	var empty domain.Reservation
	if !validText(eventKey, 128) || !validText(reason, 256) {
		return empty, domain.Err("invalid_request", "Нужна причина неизвестного расхода", 400)
	}
	tx, e := s.beginRuntime(ctx)
	if e != nil {
		return empty, e
	}
	defer tx.Rollback(ctx)
	r, _, e := lockReservation(ctx, tx, org, id)
	if e != nil {
		return empty, e
	}
	key := "unknown:" + eventKey
	hash := digestValue(reason)
	replay, e := ledgerReplay(ctx, tx, r, key, hash)
	if e != nil {
		return empty, e
	}
	if replay {
		return r, tx.Commit(ctx)
	}
	if r.PendingQuarantineID != "" {
		return empty, domain.Err("conflict", "Требуется разрешение карантина стоимости", 409)
	}
	if r.Status != "reserved" && r.Status != "unknown" {
		return empty, domain.Err("conflict", "Исход уже определён", 409)
	}
	r, e = scanReservation(tx.QueryRow(ctx, "UPDATE reservations SET status='unknown',revision=revision+1 WHERE organization_id=$1 AND id=$2 RETURNING "+reservationColumns, org, id))
	if e != nil {
		return empty, e
	}
	if r.ActionID != "" {
		if _, e = tx.Exec(ctx, "UPDATE actions SET status='unknown' WHERE organization_id=$1 AND id=$2 AND status='sent'", org, r.ActionID); e != nil {
			return empty, e
		}
	}
	if e = recordLedger(ctx, tx, r, key, hash, "unknown", 0, 0); e != nil {
		return empty, e
	}
	if e = emit(ctx, tx, org, r.TaskID, "cost.unknown", map[string]any{"reservation_id": id, "reason": reason}); e != nil {
		return empty, e
	}
	return r, tx.Commit(ctx)
}

// ReleaseUnsentCost proves non-dispatch from the locked action row. An expired
// lease or a network timeout alone is never proof of a free external call.
func (s *Store) ReleaseUnsentCost(ctx context.Context, org, id, eventKey string) (domain.Reservation, error) {
	var empty domain.Reservation
	if !validText(eventKey, 128) {
		return empty, domain.Err("invalid_request", "Нужен ключ освобождения", 400)
	}
	tx, e := s.beginRuntime(ctx)
	if e != nil {
		return empty, e
	}
	defer tx.Rollback(ctx)
	r, scopes, e := lockReservation(ctx, tx, org, id)
	if e != nil {
		return empty, e
	}
	key := "release:" + eventKey
	hash := digestValue("not_dispatched")
	replay, e := ledgerReplay(ctx, tx, r, key, hash)
	if e != nil {
		return empty, e
	}
	if replay {
		return r, tx.Commit(ctx)
	}
	if (r.Status != "reserved" && r.Status != "unknown") || r.ActionID == "" || r.PendingQuarantineID != "" {
		return empty, domain.Err("conflict", "Этот резерв нельзя освободить как неотправленный", 409)
	}
	result, e := tx.Exec(ctx, "UPDATE actions SET status='rejected' WHERE organization_id=$1 AND id=$2 AND status='pending'", org, r.ActionID)
	if e != nil {
		return empty, e
	}
	if result.RowsAffected() != 1 {
		return empty, domain.Err("conflict", "Вызов уже отправлен; требуется подтверждённая стоимость", 409)
	}
	if e = applyCostDelta(ctx, tx, &scopes, -r.AmountMicroUSD, 0, false); e != nil {
		return empty, e
	}
	r, e = scanReservation(tx.QueryRow(ctx, "UPDATE reservations SET status='released',revision=revision+1 WHERE organization_id=$1 AND id=$2 RETURNING "+reservationColumns, org, id))
	if e != nil {
		return empty, e
	}
	if e = recordLedger(ctx, tx, r, key, hash, "release", -r.AmountMicroUSD, 0); e != nil {
		return empty, e
	}
	if e = emit(ctx, tx, org, r.TaskID, "cost.released", map[string]any{"reservation_id": id, "basis": "not_dispatched"}); e != nil {
		return empty, e
	}
	return r, tx.Commit(ctx)
}
