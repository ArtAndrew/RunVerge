// SPDX-License-Identifier: Elastic-2.0
package storage

import (
	"context"
	"errors"
	"time"

	"agentplatform.local/platform/internal/recoveryaccess"
	"github.com/jackc/pgx/v5"
)

var ErrRecoveryRead = errors.New("recovery data unavailable")

// RecoveryReader is configured only by the trusted recovery host, never a tenant
// request. Current authority lives outside the restored DB. No ordinary viewer,
// browser session, bootstrap identity or imported grant is accepted here.
type RecoveryReader struct {
	store     *Store
	authority *recoveryaccess.Authority
}

func NewRecoveryReader(store *Store, authority *recoveryaccess.Authority) (*RecoveryReader, error) {
	if store == nil || authority == nil {
		return nil, recoveryaccess.ErrDenied
	}
	return &RecoveryReader{store: store, authority: authority}, nil
}

// RecoveryReadNotice accompanies EVERY response, even an empty page. The cutoff
// is historical; no endpoint certifies present status, full factual provenance,
// source shutdown, window reconciliation or permission to execute old work.
type RecoveryReadNotice struct {
	TargetID                   string `json:"target_id"`
	RestoreID                  string `json:"restore_id"`
	SnapshotSHA256             string `json:"snapshot_sha256"`
	OrganizationID             string `json:"organization_id"`
	CutUTC                     string `json:"cut_utc"`
	PostCutInterval            string `json:"post_cut_interval"`
	UntrustedHistoricalContent bool   `json:"untrusted_historical_content"`
	ExecutionHeld              bool   `json:"execution_held"`
}

type RecoveryTaskSummary struct {
	ID               string `json:"id"`
	GoalPreview      string `json:"goal_preview"`
	StatusAtCut      string `json:"status_at_cut"`
	VersionAtCut     int    `json:"version_at_cut"`
	CandidateID      string `json:"candidate_id,omitempty"`
	ReservedMicroUSD int64  `json:"reserved_microusd,string"`
	SpentMicroUSD    int64  `json:"spent_microusd,string"`
}

type RecoveryTaskPage struct {
	Notice      RecoveryReadNotice    `json:"notice"`
	Tasks       []RecoveryTaskSummary `json:"tasks"`
	NextAfterID string                `json:"next_after_id,omitempty"`
}

type RecoveryResult struct {
	Notice          RecoveryReadNotice `json:"notice"`
	TaskID          string             `json:"task_id"`
	CandidateID     string             `json:"candidate_id"`
	ArtifactID      string             `json:"artifact_id"`
	ArtifactVersion int                `json:"artifact_version"`
	ContentDigest   string             `json:"content_digest"`
	MediaType       string             `json:"media_type"`
	Content         string             `json:"content"`
}

// ListTasks returns at most50 summaries with a240-character goal preview. Its
// keyset cursor is merely a selector within the separately authorized org.
func (r *RecoveryReader) ListTasks(ctx context.Context, token, org, afterID string, limit int) (result RecoveryTaskPage, err error) {
	if r == nil || limit < 1 || limit > 50 || len(afterID) > 128 {
		return result, ErrRecoveryRead
	}
	err = r.read(ctx, token, org, func(ctx context.Context, tx pgx.Tx, notice RecoveryReadNotice) error {
		q := func(name string) string { return pgx.Identifier{r.store.runtimeSchema, name}.Sanitize() }
		rows, e := tx.Query(ctx, `SELECT CASE WHEN octet_length(id)<=128 THEN id END,left(goal,240),status,version,CASE WHEN octet_length(COALESCE(candidate_id,''))<=128 THEN COALESCE(candidate_id,'') END,reserved_microusd,spent_microusd FROM `+q("tasks")+` WHERE organization_id=$1 AND id>$2 AND `+q("recovery_is_historical")+`('tasks',jsonb_build_array(organization_id,id)) ORDER BY id LIMIT $3`, org, afterID, limit+1)
		if e != nil {
			return ErrRecoveryRead
		}
		defer rows.Close()
		result.Notice = notice
		result.Tasks = make([]RecoveryTaskSummary, 0, limit)
		for rows.Next() {
			var task RecoveryTaskSummary
			if rows.Scan(&task.ID, &task.GoalPreview, &task.StatusAtCut, &task.VersionAtCut, &task.CandidateID, &task.ReservedMicroUSD, &task.SpentMicroUSD) != nil {
				return ErrRecoveryRead
			}
			if len(result.Tasks) == limit {
				result.NextAfterID = result.Tasks[limit-1].ID
				break
			}
			result.Tasks = append(result.Tasks, task)
		}
		if rows.Err() != nil {
			return ErrRecoveryRead
		}
		return nil
	})
	if err != nil {
		return RecoveryTaskPage{}, err
	}
	return result, nil
}

// Result reads one exact saved candidate version. It never returns credential,
// configuration, event, audit, cache or raw provenance records. Digest integrity
// does not establish that the historical text's factual claims are true.
func (r *RecoveryReader) Result(ctx context.Context, token, org, candidateID string) (result RecoveryResult, err error) {
	if r == nil || len(candidateID) == 0 || len(candidateID) > 128 {
		return result, ErrRecoveryRead
	}
	err = r.read(ctx, token, org, func(ctx context.Context, tx pgx.Tx, notice RecoveryReadNotice) error {
		q := func(name string) string { return pgx.Identifier{r.store.runtimeSchema, name}.Sanitize() }
		e := tx.QueryRow(ctx, `SELECT c.task_id,c.id,a.id,a.version,c.content_digest,a.media_type,a.content FROM `+q("task_candidates")+` c JOIN `+q("artifacts")+` a ON a.organization_id=c.organization_id AND a.id=c.artifact_id AND a.version=c.artifact_version AND a.task_id=c.task_id
 WHERE c.organization_id=$1 AND c.id=$2 AND octet_length(a.content)<=262144
 AND octet_length(c.task_id)<=128 AND octet_length(a.id)<=128 AND octet_length(a.media_type)<=128
 AND `+q("recovery_is_historical")+`('task_candidates',jsonb_build_array(c.organization_id,c.id))
 AND `+q("recovery_is_historical")+`('artifacts',jsonb_build_array(a.organization_id,a.id,a.version))`, org, candidateID).Scan(&result.TaskID, &result.CandidateID, &result.ArtifactID, &result.ArtifactVersion, &result.ContentDigest, &result.MediaType, &result.Content)
		if e != nil || Hash([]byte(result.Content)) != result.ContentDigest {
			return ErrRecoveryRead
		}
		result.Notice = notice
		return nil
	})
	if err != nil {
		return RecoveryResult{}, err
	}
	return result, nil
}

func (r *RecoveryReader) read(ctx context.Context, token, org string, read func(context.Context, pgx.Tx, RecoveryReadNotice) error) error {
	return r.authority.Read(ctx, token, org, func(ctx context.Context, c recoveryaccess.Claims) (err error) {
		s := r.store
		connection, e := s.Pool.Acquire(ctx)
		if e != nil {
			return ErrRecoveryRead
		}
		defer connection.Release()
		tx, e := connection.BeginTx(ctx, pgx.TxOptions{AccessMode: pgx.ReadOnly, IsoLevel: pgx.RepeatableRead})
		if e != nil {
			return ErrRecoveryRead
		}
		defer func() {
			cleanup, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			if tx.Rollback(cleanup) != nil {
				_ = connection.Conn().Close(cleanup)
				err = ErrRecoveryRead
			}
		}()
		// Incoming pool search_path is not part of the trusted application schema.
		// Prevent outside-schema overloads from influencing built-in read helpers.
		if _, e = tx.Exec(ctx, "SET LOCAL search_path=pg_catalog,pg_temp"); e != nil {
			return ErrRecoveryRead
		}
		if _, e = tx.Exec(ctx, "SET LOCAL statement_timeout='10000'"); e != nil {
			return ErrRecoveryRead
		}
		if _, e = tx.Exec(ctx, "SET LOCAL lock_timeout='5000'"); e != nil {
			return ErrRecoveryRead
		}
		if _, e = tx.Exec(ctx, "SELECT pg_catalog.pg_advisory_xact_lock_shared(841932079)"); e != nil {
			return ErrRecoveryRead
		}
		if _, e = tx.Exec(ctx, "SELECT pg_catalog.pg_advisory_xact_lock_shared(841932080,pg_catalog.hashtext($1))", s.runtimeSchema); e != nil {
			return ErrRecoveryRead
		}
		if _, e = validateRecoverySemanticCatalog(ctx, tx, s.runtimeSchema); e != nil {
			return ErrRecoveryRead
		}
		held, e := readRecoveryState(ctx, s.runtimeSchema, tx)
		if e != nil || held.State != "held" || held.InstallationID != c.Binding.TargetID || held.RestoreID != c.Binding.RestoreID {
			return ErrRecoveryRead
		}
		// Do not load the potentially large, cross-organization operator reports
		// merely to serve a small authorized result.
		var receipt RecoveryImportReceipt
		var cut time.Time
		e = tx.QueryRow(ctx, `SELECT profile,target_installation_id,restore_id,backup_id,source_id,cut_at,snapshot_sha256,schema_sha256,inventory_sha256,catalog_sha256 FROM `+pgx.Identifier{s.runtimeSchema, "recovery_import_receipts"}.Sanitize()+` WHERE singleton`).Scan(&receipt.Profile, &receipt.TargetInstallationID, &receipt.RestoreID, &receipt.BackupID, &receipt.SourceID, &cut, &receipt.SnapshotSHA256, &receipt.SchemaSHA256, &receipt.InventorySHA256, &receipt.CatalogSHA256)
		receipt.CutUTC = cut.UTC().Format(time.RFC3339Nano)
		if e != nil || receipt.Profile != "pg18-held-import-v1" || receipt.TargetInstallationID != held.InstallationID || receipt.RestoreID != held.RestoreID || receipt.BackupID != held.BackupID || receipt.SourceID != held.SourceID || receipt.CutUTC != held.CutUTC || receipt.SchemaSHA256 != held.SchemaSHA256 || receipt.SchemaSHA256 != EmbeddedSchemaSHA256() || receipt.CatalogSHA256 != recoverySemanticCatalogSHA256 || receipt.InventorySHA256 != recoverySnapshotInventorySHA256 || receipt.SnapshotSHA256 != c.Binding.SnapshotSHA256 {
			return ErrRecoveryRead
		}
		// Only this small header reaches a scoped reader; receipt reports contain
		// cross-organization private operator metadata and must never be returned.
		notice := RecoveryReadNotice{TargetID: held.InstallationID, RestoreID: held.RestoreID, SnapshotSHA256: receipt.SnapshotSHA256, OrganizationID: org, CutUTC: held.CutUTC, PostCutInterval: "unresolved_open_ended", UntrustedHistoricalContent: true, ExecutionHeld: true}
		return read(ctx, tx, notice)
	})
}
