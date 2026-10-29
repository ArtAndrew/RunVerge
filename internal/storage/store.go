// SPDX-License-Identifier: Elastic-2.0
package storage

import (
	"context"
	"crypto/cipher"
	"crypto/sha256"
	"embed"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"sort"
	"strings"
	"sync"

	"agentplatform.local/platform/internal/domain"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

//go:embed migrations/*.sql
var migrations embed.FS

type Store struct {
	Pool          *pgxpool.Pool
	loginCipher   cipher.AEAD
	runtimeSchema string
	eventMu       sync.Mutex
	eventHub      *taskEventHub
	closed        bool
}

func Open(ctx context.Context, dsn string) (*Store, error) {
	cfg, err := pgxpool.ParseConfig(dsn)
	if err != nil {
		return nil, fmt.Errorf("invalid database configuration")
	}
	cfg.MaxConns = 20
	cfg.MinConns = 1
	p, err := pgxpool.NewWithConfig(ctx, cfg)
	if err != nil {
		return nil, err
	}
	if err = p.Ping(ctx); err != nil {
		p.Close()
		return nil, err
	}
	var runtimeSchema string
	if err = p.QueryRow(ctx, "SELECT current_schema()").Scan(&runtimeSchema); err != nil || runtimeSchema == "" {
		p.Close()
		return nil, fmt.Errorf("database application schema is unavailable")
	}
	return &Store{Pool: p, runtimeSchema: runtimeSchema}, nil
}
func (s *Store) Close() {
	s.eventMu.Lock()
	s.closed = true
	hub := s.eventHub
	if hub != nil {
		hub.cancel()
	}
	s.eventMu.Unlock()
	if hub != nil {
		<-hub.done
	}
	s.Pool.Close()
}
func (s *Store) Migrate(ctx context.Context) error {
	tx, err := s.Pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx)
	if _, err = tx.Exec(ctx, "SELECT pg_advisory_xact_lock(841932079)"); err != nil {
		return err
	}
	if _, err = tx.Exec(ctx, "CREATE TABLE IF NOT EXISTS schema_migrations(name text PRIMARY KEY,sha256 text NOT NULL,applied_at timestamptz NOT NULL DEFAULT now())"); err != nil {
		return err
	}
	files, err := fs.Glob(migrations, "migrations/*.sql")
	if err != nil {
		return err
	}
	sort.Strings(files)
	for _, name := range files {
		body, _ := migrations.ReadFile(name)
		h := Hash(body)
		var existing string
		err = tx.QueryRow(ctx, "SELECT sha256 FROM schema_migrations WHERE name=$1", name).Scan(&existing)
		if err == nil {
			if h != existing {
				return fmt.Errorf("applied migration changed: %s", name)
			}
			continue
		}
		if !errors.Is(err, pgx.ErrNoRows) {
			return err
		}
		if _, err = tx.Exec(ctx, string(body)); err != nil {
			return fmt.Errorf("migration %s: %w", name, err)
		}
		// A migration may backfill rows protected by deferred foreign keys.
		// Validate and drain its pending trigger events before the next file can
		// ALTER those tables. Keep the whole upgrade atomic and restore deferred
		// operation for subsequent migrations with cyclic, exact row bindings.
		if _, err = tx.Exec(ctx, "SET CONSTRAINTS ALL IMMEDIATE"); err != nil {
			return fmt.Errorf("migration %s constraints: %w", name, err)
		}
		if _, err = tx.Exec(ctx, "SET CONSTRAINTS ALL DEFERRED"); err != nil {
			return err
		}
		if _, err = tx.Exec(ctx, "INSERT INTO schema_migrations(name,sha256) VALUES($1,$2)", name, h); err != nil {
			return err
		}
	}
	return tx.Commit(ctx)
}
func Hash(b []byte) string { h := sha256.Sum256(b); return hex.EncodeToString(h[:]) }

type Bootstrap struct {
	OrganizationID string `json:"organization_id"`
	PrincipalID    string `json:"principal_id"`
	Token          string `json:"token"`
}

// Bootstrap is an explicit trusted-host operation, never an unauthenticated HTTP route.
func (s *Store) Bootstrap(ctx context.Context, name string, budget int64) (Bootstrap, error) {
	b := Bootstrap{domain.NewID("org"), domain.NewID("usr"), domain.NewID("ap") + domain.NewID("key")}
	if !validText(name, 120) || budget <= 0 || budget > domain.MaxMoney {
		return b, domain.Err("invalid_request", "Некорректная организация или бюджет", 400)
	}
	tx, e := s.beginRuntime(ctx)
	if e != nil {
		return b, e
	}
	defer tx.Rollback(ctx)
	if _, e = tx.Exec(ctx, "INSERT INTO organizations(id,name,budget_microusd) VALUES($1,$2,$3)", b.OrganizationID, name, budget); e != nil {
		return b, e
	}
	if _, e = tx.Exec(ctx, "INSERT INTO principals(id,name) VALUES($1,$2)", b.PrincipalID, name+" owner"); e != nil {
		return b, e
	}
	if _, e = tx.Exec(ctx, "INSERT INTO memberships(organization_id,principal_id,role) VALUES($1,$2,'owner')", b.OrganizationID, b.PrincipalID); e != nil {
		return b, e
	}
	if _, e = tx.Exec(ctx, "INSERT INTO access_tokens(token_hash,principal_id) VALUES($1,$2)", Hash([]byte(b.Token)), b.PrincipalID); e != nil {
		return b, e
	}
	return b, tx.Commit(ctx)
}

const authSQL = `SELECT p.id,m.organization_id,m.role,p.kind FROM access_tokens t
 JOIN principals p ON p.id=t.principal_id JOIN memberships m ON m.principal_id=p.id JOIN organizations o ON o.id=m.organization_id
 JOIN organization_login_policies q ON q.organization_id=m.organization_id
 WHERE t.token_hash=$1 AND m.organization_id=$2 AND NOT t.revoked AND (t.expires_at IS NULL OR t.expires_at>clock_timestamp())
 AND p.kind='human' AND NOT recovery_historical_principal(p.id) AND p.active AND m.active AND o.active AND q.mode='mixed'
 AND t.credential_generation>m.credential_cutoff AND t.credential_generation>q.human_credential_cutoff`

const ServiceKeyCredential = "service_key"
const serviceAuthSQL = `SELECT p.id,m.organization_id,CASE WHEN t.role_ceiling='viewer' OR m.role='viewer' THEN 'viewer' ELSE 'member' END,p.kind
 FROM service_credentials t JOIN principals p ON p.id=t.principal_id JOIN memberships m ON m.principal_id=p.id AND m.organization_id=t.organization_id
 JOIN organizations o ON o.id=m.organization_id JOIN organization_login_policies q ON q.organization_id=m.organization_id
 WHERE t.token_hash=$1 AND t.organization_id=$2 AND p.service_organization_id=t.organization_id AND p.kind='service' AND NOT recovery_historical_principal(p.id)
 AND m.role IN('member','viewer') AND t.role_ceiling IN('member','viewer') AND NOT t.revoked AND t.expires_at>clock_timestamp()
 AND p.active AND m.active AND o.active AND q.service_access_enabled
 AND t.credential_generation>m.credential_cutoff AND t.credential_generation>q.service_credential_cutoff`

func (s *Store) Authenticate(ctx context.Context, token, org string) (domain.Principal, error) {
	p := domain.Principal{TokenHash: Hash([]byte(token)), CredentialKind: "bearer"}
	if token == "" || !validText(org, 128) || len(token) > 1024 {
		return p, domain.Unauthenticated
	}
	tx, e := s.beginRuntimeTx(ctx, pgx.TxOptions{AccessMode: pgx.ReadOnly})
	if e != nil {
		return p, e
	}
	defer tx.Rollback(ctx)
	e = tx.QueryRow(ctx, authSQL, p.TokenHash, org).Scan(&p.ID, &p.OrganizationID, &p.Role, &p.Kind)
	if errors.Is(e, pgx.ErrNoRows) {
		p.CredentialKind = ServiceKeyCredential
		e = tx.QueryRow(ctx, serviceAuthSQL, p.TokenHash, org).Scan(&p.ID, &p.OrganizationID, &p.Role, &p.Kind)
	}
	if errors.Is(e, pgx.ErrNoRows) {
		return p, domain.Unauthenticated
	}
	return p, e
}

func authorizeTx(ctx context.Context, tx pgx.Tx, p domain.Principal, write bool) (domain.Principal, error) {
	if err := lockOrganizationPolicyTx(ctx, tx, p.OrganizationID); err != nil {
		return p, err
	}
	locks := " FOR SHARE OF t,p,m"
	if p.CredentialKind == BrowserSessionCredential {
		locks += ",i"
	} else if p.CredentialKind != "" && p.CredentialKind != "bearer" && p.CredentialKind != ServiceKeyCredential {
		return p, domain.Unauthenticated
	}
	e := tx.QueryRow(ctx, authQuery(p)+locks, p.TokenHash, p.OrganizationID).Scan(&p.ID, &p.OrganizationID, &p.Role, &p.Kind)
	if errors.Is(e, pgx.ErrNoRows) {
		return p, domain.Unauthenticated
	}
	if e != nil {
		return p, e
	}
	// The lock-acquiring statement may have evaluated expiry before waiting for
	// another transaction. Recheck with a fresh statement and database wall clock
	// only after every selected authority row is locked, before admitting a write.
	e = tx.QueryRow(ctx, authQuery(p), p.TokenHash, p.OrganizationID).Scan(&p.ID, &p.OrganizationID, &p.Role, &p.Kind)
	if errors.Is(e, pgx.ErrNoRows) {
		return p, domain.Unauthenticated
	}
	if e != nil {
		return p, e
	}
	if (p.Kind != domain.PrincipalHuman && p.Kind != domain.PrincipalService) || (write && p.Role == "viewer") {
		return p, domain.Forbidden
	}
	return p, nil
}

type Mutation func(context.Context, pgx.Tx, domain.Principal) (any, error)

func (s *Store) Mutate(ctx context.Context, p domain.Principal, scope, key string, input any, fn Mutation) (json.RawMessage, error) {
	return s.MutateGuarded(ctx, p, scope, key, input, nil, fn)
}

// Guard executes with current locked authority before a cached reply can be
// returned. It reads immutable target identity, without taking business locks
// ahead of the operation->organization->task lock order.
func (s *Store) MutateGuarded(ctx context.Context, p domain.Principal, scope, key string, input any, guard func(context.Context, pgx.Tx, domain.Principal) error, fn Mutation) (json.RawMessage, error) {
	return s.mutateWithPreAuthorization(ctx, p, scope, key, input, nil, guard, fn)
}

// A pre-authorization hook may serialize authority administration before the
// ordinary locked admission. It receives a canonical but unlocked identity;
// every permission must still be checked after authorizeTx has acquired locks.
// The hook must not perform effects or acquire tree, organization-money, or
// execution locks. Ordinary mutations retain their original admission order.
func (s *Store) mutateWithPreAuthorization(ctx context.Context, p domain.Principal, scope, key string, input any, beforeAuthorization, guard func(context.Context, pgx.Tx, domain.Principal) error, fn Mutation) (json.RawMessage, error) {
	if len(key) < 1 || len(key) > 128 {
		return nil, domain.Err("invalid_request", "Нужен Idempotency-Key длиной до 128 символов", 400)
	}
	for _, r := range key {
		if r < 33 || r > 126 {
			return nil, domain.Err("invalid_request", "Некорректный Idempotency-Key", 400)
		}
	}
	body, e := json.Marshal(input)
	if e != nil {
		return nil, e
	}
	digest := Hash(body)
	tx, e := s.beginRuntime(ctx)
	if e != nil {
		return nil, e
	}
	defer tx.Rollback(ctx)
	if beforeAuthorization != nil {
		// Resolve from the original credential domain without taking authority
		// locks before the caller's serialization gate. Never trust supplied IDs.
		e = tx.QueryRow(ctx, authQuery(p), p.TokenHash, p.OrganizationID).Scan(&p.ID, &p.OrganizationID, &p.Role, &p.Kind)
		if errors.Is(e, pgx.ErrNoRows) {
			return nil, domain.Unauthenticated
		}
		if e != nil {
			return nil, e
		}
		if e = beforeAuthorization(ctx, tx, p); e != nil {
			return nil, e
		}
	}
	p, e = authorizeTx(ctx, tx, p, true)
	if e != nil {
		return nil, e
	}
	lock := strings.Join([]string{p.OrganizationID, p.ID, scope, key}, "\x1f")
	if _, e = tx.Exec(ctx, "SELECT pg_advisory_xact_lock(hashtextextended($1,0))", lock); e != nil {
		return nil, e
	}
	p, e = authorizeTx(ctx, tx, p, true)
	if e != nil {
		return nil, e
	}
	if guard != nil {
		if e = guard(ctx, tx, p); e != nil {
			return nil, e
		}
	}
	var oldHash string
	var response []byte
	e = tx.QueryRow(ctx, `SELECT request_hash,response FROM idempotency_keys WHERE organization_id=$1 AND principal_id=$2 AND scope=$3 AND key=$4`, p.OrganizationID, p.ID, scope, key).Scan(&oldHash, &response)
	if e == nil {
		if oldHash != digest {
			return nil, domain.Err("conflict", "Этот ключ использован с другим содержимым", 409)
		}
		return response, tx.Commit(ctx)
	}
	if !errors.Is(e, pgx.ErrNoRows) {
		return nil, e
	}
	value, e := fn(ctx, tx, p)
	if e != nil {
		return nil, e
	}
	// A callback can wait on business locks after initial admission. All of its
	// writes are transactional; reject expired credentials before committing or
	// persisting a reply. Callbacks must never perform external side effects.
	if e = currentActorTx(ctx, tx, p); e != nil {
		return nil, e
	}
	response, e = json.Marshal(value)
	if e != nil {
		return nil, e
	}
	if _, e = tx.Exec(ctx, `INSERT INTO idempotency_keys(organization_id,principal_id,scope,key,request_hash,response) VALUES($1,$2,$3,$4,$5,$6)`, p.OrganizationID, p.ID, scope, key, digest, response); e != nil {
		return nil, e
	}
	return response, tx.Commit(ctx)
}
func emit(ctx context.Context, tx pgx.Tx, org, task, kind string, data any) error {
	body, e := domain.NormalizeEventPayload(kind, data)
	if e != nil {
		return e
	}
	// Serialize allocations until commit per organization. PostgreSQL identity
	// values alone do not order commits: a later visible cursor could otherwise
	// skip an earlier transaction that commits after it.
	if _, e := tx.Exec(ctx, "SELECT id FROM organizations WHERE id=$1 FOR UPDATE", org); e != nil {
		return e
	}
	var sequence int64
	e = tx.QueryRow(ctx, `INSERT INTO events(organization_id,id,task_id,type,data,payload_version) VALUES($1,$2,$3,$4,$5,$6) RETURNING sequence`, org, domain.NewID("evt"), task, kind, body, domain.EventPayloadVersion).Scan(&sequence)
	if e != nil {
		return e
	}
	if _, e = tx.Exec(ctx, "INSERT INTO outbox(sequence,organization_id) VALUES($1,$2)", sequence, org); e != nil {
		return e
	}
	return notifyTaskEventsTx(ctx, tx, org, task)
}

type scanner interface{ Scan(...any) error }

func scanTask(r scanner) (t domain.Task, e error) {
	e = r.Scan(&t.ID, &t.OrganizationID, &t.OwnerID, &t.AgentID, &t.AgentVersion, &t.Goal, &t.Status, &t.BudgetMicroUSD, &t.ReservedMicroUSD, &t.SpentMicroUSD, &t.Version, &t.CreatedAt, &t.UpdatedAt, &t.WaitingReason, &t.Result, &t.RootID, &t.ParentID, &t.Depth, &t.FinancialHold, &t.ModelConnectionID, &t.ModelConnectionVersion, &t.CurrentRunID, &t.AttemptCount, &t.MaxRuns, &t.ExecutionDeadline, &t.CandidateID, &t.AcceptedDecisionID)
	if errors.Is(e, pgx.ErrNoRows) {
		e = domain.NotFound
	}
	return
}

const taskColumns = "id,organization_id,owner_id,agent_id,agent_version,goal,status,budget_microusd,reserved_microusd,spent_microusd,version,created_at,updated_at,waiting_reason,result,root_id,COALESCE(parent_id,''),depth,financial_hold,COALESCE(model_connection_id,''),COALESCE(model_connection_version,0),COALESCE(current_run_id,''),attempt_count,max_runs,execution_deadline,COALESCE(candidate_id,''),COALESCE(accepted_decision_id,'')"

func scanAgent(r scanner) (a domain.Agent, e error) {
	var tools, team, modelTools []byte
	e = r.Scan(&a.ID, &a.OrganizationID, &a.Name, &a.Instructions, &a.Model, &tools, &a.BudgetMicroUSD, &a.Version, &a.Published, &a.CreatedAt, &team, &modelTools)
	if errors.Is(e, pgx.ErrNoRows) {
		e = domain.NotFound
	}
	if e == nil {
		e = json.Unmarshal(tools, &a.Tools)
		if e == nil && len(team) > 0 {
			e = json.Unmarshal(team, &a.Team)
		}
		if e == nil {
			e = json.Unmarshal(modelTools, &a.ModelTools)
		}
	}
	return
}

const agentColumns = "id,organization_id,name,instructions,model,tools,budget_microusd,version,published,created_at,team,model_tools"
