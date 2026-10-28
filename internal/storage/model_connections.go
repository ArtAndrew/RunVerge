// SPDX-License-Identifier: Elastic-2.0
package storage

import (
	"context"
	"encoding/json"
	"errors"
	"reflect"
	"regexp"
	"time"

	"agentplatform.local/platform/internal/domain"
	"agentplatform.local/platform/internal/modelgateway"
	"agentplatform.local/platform/internal/modelruntime"
	"github.com/jackc/pgx/v5"
)

var modelCredentialReference = regexp.MustCompile(`^PLATFORM_MODEL_KEY_[A-Z0-9_]{1,64}$`)

// RuntimeModelConfig never persists key. Its formatter is deliberately redacted.
func RuntimeModelConfig(c domain.ModelConnection, key string) modelruntime.Config {
	return modelruntime.Config{
		Gateway:        modelgateway.Config{Endpoint: c.Endpoint, APIKey: key, Model: c.Model, MaxOutputTokens: c.MaxOutputTokens, MaxInputBytes: c.MaxInputBytes, Timeout: time.Duration(c.TimeoutMilliseconds) * time.Millisecond, AllowInsecureLoopback: c.AllowInsecureLoopback, TokenLimitField: c.TokenLimitField},
		Price:          modelruntime.PriceProfile{AccountingMode: c.AccountingMode, ProviderAccount: c.ProviderAccount, Version: c.PriceVersion, Model: c.Model, BoundAssumptions: c.BoundAssumptions, MaxChargeMicroUSD: c.MaxChargeMicroUSD, MaxInputTokens: c.MaxInputTokens, InputMicroUSDPerMillion: c.InputMicroUSDPerMillion, OutputMicroUSDPerMillion: c.OutputMicroUSDPerMillion, FixedMicroUSD: c.FixedMicroUSD},
		MaxOutputBytes: c.MaxOutputBytes, PersistenceTimeout: 5 * time.Second,
	}
}

func scanModelConnection(row scanner) (domain.ModelConnection, error) {
	var c domain.ModelConnection
	var profile []byte
	var active bool
	var created time.Time
	if e := row.Scan(&profile, &active, &created); e != nil {
		return c, rowExists(e)
	}
	if e := json.Unmarshal(profile, &c); e != nil {
		return c, e
	}
	c.Active = active
	c.CreatedAt = created
	return c, nil
}

// InstallModelConnection is trusted-host configuration, not an unauthenticated
// route. Repeating the same immutable ID/version is side-effect free. A new
// version explicitly retires conflicting active versions; accepted tasks never
// silently move to a different endpoint, price revision or secret reference.
func (s *Store) InstallModelConnection(ctx context.Context, c domain.ModelConnection) (domain.ModelConnection, error) {
	var empty domain.ModelConnection
	if e := validateModelConnection(c); e != nil {
		return empty, e
	}
	tx, e := s.beginRuntime(ctx)
	if e != nil {
		return empty, e
	}
	defer tx.Rollback(ctx)
	if e = configurationGateTx(ctx, tx, c.OrganizationID); e != nil {
		return empty, e
	}
	// Trusted legacy host installation needs no caller or login-policy row.
	if e = lockConfigurationOrganizationOnlyTx(ctx, tx, c.OrganizationID); e != nil {
		return empty, e
	}
	created, e := installModelConnectionTx(ctx, tx, c)
	if e != nil {
		return empty, e
	}
	return created, tx.Commit(ctx)
}

// Caller holds the configuration gate and organization row. No I/O or commit.
func validateModelConnection(c domain.ModelConnection) error {
	if !validText(c.ID, 128) || !validText(c.OrganizationID, 128) || c.Version < 1 || c.MaxOutputBytes > 262144 || !modelCredentialReference.MatchString(c.CredentialEnvironment) || c.TimeoutMilliseconds < 1 || c.TimeoutMilliseconds > 600000 {
		return domain.Err("invalid_request", "Некорректный профиль модели или ссылка на секрет", 400)
	}
	if _, e := modelruntime.New(&Store{}, RuntimeModelConfig(c, "configuration-validation-only")); e != nil {
		return domain.Err("invalid_request", "Профиль модели не прошёл проверку границ и тарифа", 400)
	}

	return nil
}

func installModelConnectionTx(ctx context.Context, tx pgx.Tx, c domain.ModelConnection) (domain.ModelConnection, error) {
	var empty domain.ModelConnection
	if e := validateModelConnection(c); e != nil {
		return empty, e
	}
	active := c.Active
	c.Active = false
	c.CreatedAt = time.Time{}
	profile, e := json.Marshal(c)
	if e != nil {
		return empty, e
	}
	existing, e := scanModelConnection(tx.QueryRow(ctx, "SELECT profile,active,created_at FROM model_connections WHERE organization_id=$1 AND id=$2 AND version=$3 FOR UPDATE", c.OrganizationID, c.ID, c.Version))
	if e == nil {
		old := existing
		old.Active = false
		old.CreatedAt = time.Time{}
		if digestValue(old) != Hash(profile) {
			return empty, domain.Err("conflict", "Версия модели уже содержит другую конфигурацию", 409)
		}
		return existing, nil
	}
	if !errors.Is(e, domain.NotFound) {
		return empty, e
	}
	var latest int
	if e = tx.QueryRow(ctx, "SELECT COALESCE(max(version),0) FROM model_connections WHERE organization_id=$1 AND id=$2", c.OrganizationID, c.ID).Scan(&latest); e != nil {
		return empty, e
	}
	if c.Version != latest+1 {
		return empty, domain.Err("conflict", "Версия модели должна следовать за предыдущей", 409)
	}
	if active {
		if _, e = tx.Exec(ctx, "UPDATE model_connections SET active=false WHERE organization_id=$1 AND active AND (id=$2 OR model=$3)", c.OrganizationID, c.ID, c.Model); e != nil {
			return empty, e
		}
	}
	created, e := scanModelConnection(tx.QueryRow(ctx, "INSERT INTO model_connections(organization_id,id,version,model,provider_account,price_version,credential_environment,profile,active) VALUES($1,$2,$3,$4,$5,$6,$7,$8,$9) RETURNING profile,active,created_at", c.OrganizationID, c.ID, c.Version, c.Model, c.ProviderAccount, c.PriceVersion, c.CredentialEnvironment, profile, active))
	if e != nil {
		return empty, e
	}
	return created, nil
}

func (s *Store) Models(ctx context.Context, p domain.Principal, raw string, limit int) ([]domain.AvailableModel, string, error) {
	result := []domain.AvailableModel{}
	if limit < 1 || limit > 100 {
		return nil, "", domain.Err("invalid_request", "limit должен быть от 1 до 100", 400)
	}
	position, e := decodeCursor(raw)
	if e != nil {
		return nil, "", e
	}
	tx, e := s.beginPrincipalRead(ctx, p, "tasks.read")
	if e != nil {
		return nil, "", e
	}
	defer tx.Rollback(ctx)
	rows, e := tx.Query(ctx, `SELECT m.profile,m.active,m.created_at FROM model_connections m WHERE m.organization_id=$1 AND m.active AND (m.created_at,m.id)<($2,$3)
 AND NOT EXISTS(SELECT 1 FROM configuration_resource_bindings b JOIN configuration_destinations d ON d.organization_id=b.organization_id AND d.id=b.destination_id AND d.version=b.destination_version
 JOIN credential_purpose_profiles p ON p.organization_id=b.organization_id AND p.id=b.credential_purpose_id AND p.version=b.credential_purpose_version
 WHERE b.organization_id=m.organization_id AND b.kind='model' AND b.target_id=m.id AND b.resource_version=m.version AND (NOT d.active OR NOT p.active))
 ORDER BY m.created_at DESC,m.id DESC LIMIT $4`, p.OrganizationID, position.CreatedAt, position.ID, limit+1)
	if e != nil {
		return nil, "", e
	}
	defer rows.Close()
	for rows.Next() {
		c, e := scanModelConnection(rows)
		if e != nil {
			return nil, "", e
		}
		result = append(result, domain.AvailableModel{AccountingMode: domain.EffectiveModelAccountingMode(c.AccountingMode), MaxInputTokens: c.MaxInputTokens, MaxOutputBytes: c.MaxOutputBytes, TimeoutMilliseconds: c.TimeoutMilliseconds, ID: c.ID, Model: c.Model, Version: c.Version, PriceVersion: c.PriceVersion, MaxChargeMicroUSD: c.MaxChargeMicroUSD, MaxInputBytes: c.MaxInputBytes, MaxOutputTokens: c.MaxOutputTokens, BoundAssumptions: c.BoundAssumptions, CreatedAt: c.CreatedAt})
	}
	if e = rows.Err(); e != nil {
		return nil, "", e
	}
	next := ""
	if len(result) > limit {
		result = result[:limit]
		last := result[limit-1]
		next = encodeCursor(last.CreatedAt, last.ID)
	}
	return result, next, tx.Commit(ctx)
}

func checkTaskModelAdmissionTx(ctx context.Context, tx pgx.Tx, t domain.Task, l *domain.Lease, in *domain.ReserveCostRequest) error {
	if t.ModelConnectionID == "" {
		return nil
	}
	c, e := scanModelConnection(tx.QueryRow(ctx, "SELECT profile,active,created_at FROM model_connections WHERE organization_id=$1 AND id=$2 AND version=$3 FOR SHARE", t.OrganizationID, t.ModelConnectionID, t.ModelConnectionVersion))
	if e != nil {
		return e
	}
	if !c.Active {
		return domain.Err("forbidden", "Подключение модели отключено", 403)
	}
	if e = checkConfigurationResourceAdmissionTx(ctx, tx, t.OrganizationID, "model", c.ID, c.Version); e != nil {
		return e
	}
	operationID := ""
	if l != nil {
		operationID = l.OperationID
	}
	if e = checkConfigurationModelProbeAuthorityTx(ctx, tx, t, operationID); e != nil {
		return e
	}
	if in != nil && (in.Category != "model" || in.Provider != c.ProviderAccount || in.PriceVersion != c.PriceVersion || in.AmountMicroUSD != c.MaxChargeMicroUSD) {
		return domain.Err("conflict", "Вызов не совпадает с закреплённым профилем модели", 409)
	}

	agent, e := scanAgent(tx.QueryRow(ctx, "SELECT "+agentColumns+" FROM agents WHERE organization_id=$1 AND id=$2 AND version=$3", t.OrganizationID, t.AgentID, t.AgentVersion))
	if e != nil {
		return e
	}
	if !agent.Published || agent.Model != c.Model {
		return modelSnapshotConflict()
	}
	if l == nil {
		// Explicit new-Run admission has no operation/lease yet. It still
		// rechecks immutable model/tool activation, while cost reservation must
		// always supply a lease and the exact durable execution transcript.
		if in != nil {
			return domain.Forbidden
		}
		_, e = modelToolConnectionsTx(ctx, tx, t, agent)
		if e != nil {
			return e
		}
		return checkModelTurnBudgetTx(ctx, tx, t)
	}
	snapshot := *l
	snapshot.Task, snapshot.Agent = t, agent
	snapshot.RunInput, e = loadRunInputTx(ctx, tx, t.OrganizationID, t.ID, l.RunID)
	if e != nil {
		return e
	}
	if _, e = loadModelContextTx(ctx, tx, t, &snapshot); e != nil {
		return e
	}
	if !reflect.DeepEqual(snapshot.ModelTools, l.ModelTools) || !reflect.DeepEqual(snapshot.ModelMessages, l.ModelMessages) {
		return modelSnapshotConflict()
	}
	if in != nil {
		runtime, e := modelruntime.New(&Store{}, RuntimeModelConfig(c, "canonical-intent-only"))
		if e != nil {
			return domain.Err("conflict", "Invalid persisted model profile", 409)
		}
		key, intent, e := runtime.DescribeIntent(snapshot)
		if e != nil || key != in.ChargeKey || intent != in.IntentHash {
			return domain.Err("conflict", "Worker intent differs from persisted task", 409)
		}
	}
	return nil
}
