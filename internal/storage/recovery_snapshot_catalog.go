// SPDX-License-Identifier: Elastic-2.0
package storage

import (
	"context"
	"encoding/json"
	"fmt"
	"math"
	"reflect"
	"strconv"
	"strings"
	"time"

	"agentplatform.local/platform/internal/domain"
	"agentplatform.local/platform/internal/installation"
	"github.com/jackc/pgx/v5"
)

func recoverySnapshotInspect(ctx context.Context, tx pgx.Tx, schema string) (recoverySnapshotHeader, string, error) {
	var h recoverySnapshotHeader
	// Cooperates with the existing migration runner. ACCESS SHARE below keeps
	// every validated relation stable for COPY. No source DDL is executed.
	if _, err := tx.Exec(ctx, "SELECT pg_catalog.pg_advisory_xact_lock_shared(841932079)"); err != nil {
		return h, "", recoverySnapshotError(ctx, ErrRecoverySnapshotSource)
	}
	var cut time.Time
	// The first SELECT can establish the RR snapshot before waiting on the
	// migration gate. Use the transaction start as a conservative lower bound
	// for reconciliation, never a later wall clock that understates the gap.
	if err := tx.QueryRow(ctx, "SELECT current_setting('server_version_num')::integer,current_setting('server_encoding'),pg_catalog.pg_current_snapshot()::text,transaction_timestamp()").Scan(&h.PostgreSQLVersion, &h.Encoding, &h.MVCCSnapshot, &cut); err != nil {
		return h, "", recoverySnapshotError(ctx, ErrRecoverySnapshotSource)
	}
	if schema == "" || h.PostgreSQLVersion < 180000 || h.PostgreSQLVersion >= 190000 || h.Encoding != "UTF8" {
		return h, "", ErrRecoverySnapshotSchema
	}
	// A source policy must never silently filter historical rows from an export.
	if _, err := tx.Exec(ctx, "SET LOCAL row_security=off"); err != nil {
		return h, "", recoverySnapshotError(ctx, ErrRecoverySnapshotSource)
	}
	inventory, err := recoverySnapshotInventory(ctx, tx, schema, true)
	if err != nil {
		return h, "", err
	}
	h.Inventory = inventory
	raw, _ := json.Marshal(inventory)
	h.InventorySHA256 = Hash(raw)
	if h.InventorySHA256 != recoverySnapshotInventorySHA256 {
		return h, "", ErrRecoverySnapshotSchema
	}
	rows, err := tx.Query(ctx, "SELECT name,sha256 FROM "+pgx.Identifier{schema, "schema_migrations"}.Sanitize()+" ORDER BY name LIMIT 1025")
	if err != nil {
		return h, "", recoverySnapshotError(ctx, ErrRecoverySnapshotSchema)
	}
	for rows.Next() {
		var name, digest string
		if rows.Scan(&name, &digest) != nil {
			rows.Close()
			return h, "", ErrRecoverySnapshotSchema
		}
		if len(h.Migrations) >= 1024 {
			rows.Close()
			return h, "", ErrRecoverySnapshotSchema
		}
		h.Migrations = append(h.Migrations, installation.Migration{Name: name, SHA256: digest})
	}
	err = rows.Err()
	rows.Close()
	if err != nil || !reflect.DeepEqual(h.Migrations, EmbeddedSchemaIdentity()) {
		return h, "", recoverySnapshotError(ctx, ErrRecoverySnapshotSchema)
	}
	var count int
	var source, state string
	var normalFields bool
	control := pgx.Identifier{schema, "runtime_recovery_control"}.Sanitize()
	if err = tx.QueryRow(ctx, "SELECT count(*),COALESCE(min(installation_id),''),COALESCE(min(state),''),COALESCE(bool_and(singleton AND restore_id IS NULL AND backup_id IS NULL AND source_id IS NULL AND cut_at IS NULL AND schema_sha256 IS NULL AND held_at IS NULL),false) FROM "+control).Scan(&count, &source, &state, &normalFields); err != nil || count != 1 || state != "normal" || !normalFields || !recoverySnapshotSourceID.MatchString(source) {
		return h, "", recoverySnapshotError(ctx, ErrRecoverySnapshotSource)
	}
	h.Kind, h.Format, h.APIVersion = "header", RecoverySnapshotFormat, domain.ContractVersion
	h.Metadata = RecoverySnapshotMetadata{SourceID: source, BackupID: domain.NewID("bak"), CutUTC: cut.UTC().Format(time.RFC3339Nano), SchemaSHA256: recoverySchemaSHA256()}
	return h, schema, nil
}

// All returned identifiers must first match the reviewed compiled fingerprint.
// The pin covers relation/table/column identity and binary types, not trust in
// arbitrary database functions or application admission behavior.
func recoverySnapshotInventory(ctx context.Context, tx pgx.Tx, schema string, lock bool) (RecoverySnapshotInventory, error) {
	result := RecoverySnapshotInventory{Tables: []RecoverySnapshotTable{}, Sequences: []RecoverySnapshotSequenceDefinition{}}
	rows, err := tx.Query(ctx, `SELECT c.relname,c.relkind::text,c.relpersistence::text,c.relrowsecurity,c.relforcerowsecurity,c.relispartition,
 EXISTS(SELECT 1 FROM pg_catalog.pg_inherits i WHERE i.inhrelid=c.oid OR i.inhparent=c.oid)
 FROM pg_catalog.pg_class c JOIN pg_catalog.pg_namespace n ON n.oid=c.relnamespace
 WHERE n.nspname=$1 AND c.relkind IN('r','p','f','v','m') ORDER BY c.relname COLLATE "C" LIMIT 257`, schema)
	if err != nil {
		return result, recoverySnapshotError(ctx, ErrRecoverySnapshotSchema)
	}
	for rows.Next() {
		var name, kind, persistence string
		var rls, forced, partition, inherits bool
		if rows.Scan(&name, &kind, &persistence, &rls, &forced, &partition, &inherits) != nil || !recoverySnapshotName.MatchString(name) || kind != "r" || persistence != "p" || rls || forced || partition || inherits || len(result.Tables) >= 256 {
			rows.Close()
			return result, ErrRecoverySnapshotSchema
		}
		result.Tables = append(result.Tables, RecoverySnapshotTable{Name: name, Exported: recoveryBusinessTable(name), Columns: []RecoverySnapshotColumn{}, PrimaryKey: []string{}})
	}
	err = rows.Err()
	rows.Close()
	if err != nil || len(result.Tables) == 0 {
		return result, recoverySnapshotError(ctx, ErrRecoverySnapshotSchema)
	}
	byName := map[string]int{}
	for i, t := range result.Tables {
		byName[t.Name] = i
		if lock {
			if _, err = tx.Exec(ctx, "LOCK TABLE "+pgx.Identifier{schema, t.Name}.Sanitize()+" IN ACCESS SHARE MODE"); err != nil {
				return result, recoverySnapshotError(ctx, ErrRecoverySnapshotSchema)
			}
		}
	}
	rows, err = tx.Query(ctx, `SELECT c.relname,a.attname,a.attnum,a.atttypid,tn.nspname,t.typname,a.atttypmod,a.attndims,a.attnotnull,a.attgenerated::text,a.attidentity::text
 FROM pg_catalog.pg_attribute a JOIN pg_catalog.pg_class c ON c.oid=a.attrelid JOIN pg_catalog.pg_namespace n ON n.oid=c.relnamespace
 JOIN pg_catalog.pg_type t ON t.oid=a.atttypid JOIN pg_catalog.pg_namespace tn ON tn.oid=t.typnamespace
 WHERE n.nspname=$1 AND c.relkind='r' AND a.attnum>0 AND NOT a.attisdropped ORDER BY c.relname COLLATE "C",a.attnum LIMIT 4097`, schema)
	if err != nil {
		return result, recoverySnapshotError(ctx, ErrRecoverySnapshotSchema)
	}
	count := 0
	for rows.Next() {
		var table, namespace string
		var col RecoverySnapshotColumn
		if rows.Scan(&table, &col.Name, &col.Number, &col.TypeOID, &namespace, &col.TypeName, &col.TypeModifier, &col.Dimensions, &col.NotNull, &col.Generated, &col.Identity) != nil {
			rows.Close()
			return result, ErrRecoverySnapshotSchema
		}
		i, ok := byName[table]
		count++
		if !ok || count > 4096 || namespace != "pg_catalog" || !recoverySnapshotName.MatchString(col.Name) || col.Number <= 0 || len(result.Tables[i].Columns) >= 256 {
			rows.Close()
			return result, ErrRecoverySnapshotSchema
		}
		result.Tables[i].Columns = append(result.Tables[i].Columns, col)
	}
	err = rows.Err()
	rows.Close()
	if err != nil {
		return result, recoverySnapshotError(ctx, ErrRecoverySnapshotSchema)
	}
	rows, err = tx.Query(ctx, `SELECT c.relname,a.attname
 FROM pg_catalog.pg_constraint co JOIN pg_catalog.pg_class c ON c.oid=co.conrelid JOIN pg_catalog.pg_namespace n ON n.oid=c.relnamespace
 CROSS JOIN LATERAL pg_catalog.unnest(co.conkey) WITH ORDINALITY k(attnum,ord)
 JOIN pg_catalog.pg_attribute a ON a.attrelid=c.oid AND a.attnum=k.attnum
 WHERE n.nspname=$1 AND co.contype='p' ORDER BY c.relname COLLATE "C",k.ord LIMIT 4097`, schema)
	if err != nil {
		return result, recoverySnapshotError(ctx, ErrRecoverySnapshotSchema)
	}
	for rows.Next() {
		var table, col string
		if rows.Scan(&table, &col) != nil {
			rows.Close()
			return result, ErrRecoverySnapshotSchema
		}
		i, ok := byName[table]
		if !ok || len(result.Tables[i].PrimaryKey) >= 256 {
			rows.Close()
			return result, ErrRecoverySnapshotSchema
		}
		result.Tables[i].PrimaryKey = append(result.Tables[i].PrimaryKey, col)
	}
	err = rows.Err()
	rows.Close()
	if err != nil {
		return result, recoverySnapshotError(ctx, ErrRecoverySnapshotSchema)
	}
	for _, t := range result.Tables {
		if len(t.Columns) == 0 || len(t.PrimaryKey) == 0 {
			return result, ErrRecoverySnapshotSchema
		}
	}
	rows, err = tx.Query(ctx, `SELECT c.relname,s.seqtypid,s.seqstart,s.seqmin,s.seqmax,s.seqincrement,s.seqcache,s.seqcycle
 FROM pg_catalog.pg_sequence s JOIN pg_catalog.pg_class c ON c.oid=s.seqrelid JOIN pg_catalog.pg_namespace n ON n.oid=c.relnamespace
 WHERE n.nspname=$1 ORDER BY c.relname COLLATE "C" LIMIT 65`, schema)
	if err != nil {
		return result, recoverySnapshotError(ctx, ErrRecoverySnapshotSchema)
	}
	for rows.Next() {
		var seq RecoverySnapshotSequenceDefinition
		if rows.Scan(&seq.Name, &seq.TypeOID, &seq.Start, &seq.Minimum, &seq.Maximum, &seq.Increment, &seq.Cache, &seq.Cycle) != nil || len(result.Sequences) >= 64 {
			rows.Close()
			return result, ErrRecoverySnapshotSchema
		}
		result.Sequences = append(result.Sequences, seq)
	}
	err = rows.Err()
	rows.Close()
	if err != nil {
		return result, recoverySnapshotError(ctx, ErrRecoverySnapshotSchema)
	}
	return result, nil
}

func recoverySnapshotColumns(t RecoverySnapshotTable) string {
	names := make([]string, len(t.Columns))
	for i, c := range t.Columns {
		names[i] = pgx.Identifier{c.Name}.Sanitize()
	}
	return strings.Join(names, ",")
}
func recoverySnapshotCopySQL(schema string, t RecoverySnapshotTable) string {
	keys := make([]string, len(t.PrimaryKey))
	for i, k := range t.PrimaryKey {
		keys[i] = pgx.Identifier{k}.Sanitize()
		for _, c := range t.Columns {
			if c.Name == k && c.TypeOID == 25 {
				keys[i] += ` COLLATE "C"`
			}
		}
	}
	return "COPY (SELECT " + recoverySnapshotColumns(t) + " FROM " + pgx.Identifier{schema, t.Name}.Sanitize() + " ORDER BY " + strings.Join(keys, ",") + ") TO STDOUT (FORMAT binary)"
}

func recoverySnapshotPreflight(ctx context.Context, tx pgx.Tx, schema string, inventory RecoverySnapshotInventory, l RecoverySnapshotLimits) (map[string]int64, error) {
	counts := map[string]int64{}
	var total int64
	for _, t := range inventory.Tables {
		if !t.Exported {
			continue
		}
		var count, maxSize int64
		// Bound row admission before the PG driver receives a potentially huge
		// COPY frame. record_send's extra OIDs make the size exact/conservative.
		query := "SELECT count(*),COALESCE(max(octet_length(pg_catalog.record_send(ROW(" + recoverySnapshotColumns(t) + ")))::bigint),0) FROM (SELECT " + recoverySnapshotColumns(t) + " FROM " + pgx.Identifier{schema, t.Name}.Sanitize() + " LIMIT " + strconv.FormatInt(l.MaxRowsPerTable+1, 10) + ") AS bounded"
		if err := tx.QueryRow(ctx, query).Scan(&count, &maxSize); err != nil {
			return nil, recoverySnapshotError(ctx, ErrRecoverySnapshotSource)
		}
		if count > l.MaxRowsPerTable || count > l.MaxRows-total || maxSize > l.MaxRowBytes+2+4*int64(len(t.Columns)) {
			return nil, ErrRecoverySnapshotLimit
		}
		counts[t.Name] = count
		total += count
	}
	return counts, nil
}

func recoverySnapshotReadState(ctx context.Context, tx pgx.Tx, schema string, l RecoverySnapshotLimits) (recoverySnapshotState, error) {
	result := recoverySnapshotState{Kind: "state", Sequences: []RecoverySnapshotSequence{}, KeyReferences: []RecoverySnapshotKeyReference{}, Organizations: []RecoverySnapshotOrganization{}}
	q := func(name string) string { return pgx.Identifier{schema, name}.Sanitize() }
	// Non-MVCC sequence observations are deliberately taken after COPY. They
	// can include aborted/post-cut increments. They never prove source fencing.
	maxQueries := []struct{ name, sql string }{
		{"credential_origin_sequence", "SELECT GREATEST(COALESCE((SELECT max(credential_generation) FROM " + q("access_tokens") + "),0),COALESCE((SELECT max(credential_generation) FROM " + q("browser_sessions") + "),0),COALESCE((SELECT max(credential_generation) FROM " + q("service_credentials") + "),0),COALESCE((SELECT max(credential_cutoff) FROM " + q("memberships") + "),0),COALESCE((SELECT max(human_credential_cutoff) FROM " + q("organization_login_policies") + "),0),COALESCE((SELECT max(service_credential_cutoff) FROM " + q("organization_login_policies") + "),0))"},
		{"events_sequence_seq", "SELECT GREATEST(COALESCE((SELECT max(sequence) FROM " + q("events") + "),0),COALESCE((SELECT max(sequence) FROM " + q("outbox") + "),0),COALESCE((SELECT max(old_after_sequence) FROM " + q("event_replay_window_audit") + "),0),COALESCE((SELECT max(after_sequence) FROM " + q("task_event_windows") + "),0),COALESCE((SELECT max(new_after_sequence) FROM " + q("event_replay_window_audit") + "),0),COALESCE((SELECT max(start_after_sequence) FROM " + q("webhook_subscriptions") + "),0),COALESCE((SELECT max(last_admitted_sequence) FROM " + q("webhook_subscriptions") + "),0),COALESCE((SELECT max(event_sequence) FROM " + q("webhook_deliveries") + "),0))"},
	}
	for _, entry := range maxQueries {
		seq := RecoverySnapshotSequence{Name: entry.name}
		if err := tx.QueryRow(ctx, entry.sql).Scan(&seq.IncludedMaximum); err != nil {
			return result, recoverySnapshotError(ctx, ErrRecoverySnapshotSource)
		}
		if err := tx.QueryRow(ctx, "SELECT last_value,is_called FROM "+q(entry.name)).Scan(&seq.ObservedLast, &seq.IsCalled); err != nil {
			return result, recoverySnapshotError(ctx, ErrRecoverySnapshotSource)
		}
		high := max(seq.ObservedLast, seq.IncludedMaximum)
		if high < 1 || high == math.MaxInt64 {
			return result, ErrRecoverySnapshotLimit
		}
		seq.NextMinimum = high + 1
		result.Sequences = append(result.Sequences, seq)
	}
	query := "SELECT kind,organization_id,id,version,environment FROM (" +
		"SELECT 'model' AS kind,organization_id,id,version,credential_environment AS environment FROM " + q("model_connections") +
		" UNION ALL SELECT 'tool',organization_id,id,version,profile->>'credential_environment' FROM " + q("tool_connections") +
		" UNION ALL SELECT 'purpose',organization_id,id,version,profile->>'credential_environment' FROM " + q("credential_purpose_profiles") +
		" UNION ALL SELECT 'webhook',organization_id,id,version,profile->>'signing_key_environment' FROM " + q("webhook_destinations") +
		") refs WHERE environment IS NOT NULL AND environment<>'' ORDER BY kind COLLATE \"C\",organization_id COLLATE \"C\",id COLLATE \"C\",version LIMIT " + strconv.Itoa(l.MaxMetadataEntries+1)
	rows, err := tx.Query(ctx, query)
	if err != nil {
		return result, recoverySnapshotError(ctx, ErrRecoverySnapshotSource)
	}
	for rows.Next() {
		var ref RecoverySnapshotKeyReference
		if rows.Scan(&ref.Kind, &ref.OrganizationID, &ref.ID, &ref.Version, &ref.Environment) != nil {
			rows.Close()
			return result, ErrRecoverySnapshotInvalid
		}
		if len(result.KeyReferences) >= l.MaxMetadataEntries {
			rows.Close()
			return result, ErrRecoverySnapshotLimit
		}
		result.KeyReferences = append(result.KeyReferences, ref)
	}
	err = rows.Err()
	rows.Close()
	if err != nil {
		return result, recoverySnapshotError(ctx, ErrRecoverySnapshotSource)
	}
	query = "SELECT o.id,COALESCE((SELECT max(e.sequence) FROM " + q("events") + " e WHERE e.organization_id=o.id),0),COALESCE((SELECT max(w.after_sequence) FROM " + q("task_event_windows") + " w WHERE w.organization_id=o.id),0) FROM " + q("organizations") + " o ORDER BY o.id COLLATE \"C\" LIMIT " + strconv.Itoa(l.MaxMetadataEntries+1)
	rows, err = tx.Query(ctx, query)
	if err != nil {
		return result, recoverySnapshotError(ctx, ErrRecoverySnapshotSource)
	}
	for rows.Next() {
		var org RecoverySnapshotOrganization
		if rows.Scan(&org.OrganizationID, &org.EventHead, &org.RetainedAfterMaximum) != nil {
			rows.Close()
			return result, ErrRecoverySnapshotInvalid
		}
		if len(result.Organizations) >= l.MaxMetadataEntries {
			rows.Close()
			return result, ErrRecoverySnapshotLimit
		}
		result.Organizations = append(result.Organizations, org)
	}
	err = rows.Err()
	rows.Close()
	if err != nil {
		return result, recoverySnapshotError(ctx, ErrRecoverySnapshotSource)
	}
	return result, nil
}

func recoverySnapshotValidateHeader(h recoverySnapshotHeader) error {
	raw, _ := json.Marshal(h.Inventory)
	if h.Kind != "header" || h.Format != RecoverySnapshotFormat || h.APIVersion != domain.ContractVersion ||
		!recoverySnapshotSourceID.MatchString(h.Metadata.SourceID) || !recoverySnapshotBackupID.MatchString(h.Metadata.BackupID) ||
		h.Metadata.SchemaSHA256 != recoverySchemaSHA256() || !reflect.DeepEqual(h.Migrations, EmbeddedSchemaIdentity()) ||
		h.PostgreSQLVersion < 180000 || h.PostgreSQLVersion >= 190000 || h.Encoding != "UTF8" ||
		h.InventorySHA256 != recoverySnapshotInventorySHA256 || Hash(raw) != recoverySnapshotInventorySHA256 ||
		len(h.MVCCSnapshot) == 0 || len(h.MVCCSnapshot) > 65536 {
		return ErrRecoverySnapshotSchema
	}
	cut, err := time.Parse(time.RFC3339Nano, h.Metadata.CutUTC)
	if err != nil || cut.Location() != time.UTC || cut.UTC().Format(time.RFC3339Nano) != h.Metadata.CutUTC {
		return ErrRecoverySnapshotInvalid
	}
	// Snapshot text is explanatory provenance, never SQL or a replay token.
	parts := strings.Split(h.MVCCSnapshot, ":")
	if len(parts) != 3 {
		return ErrRecoverySnapshotInvalid
	}
	for _, v := range parts[:2] {
		if _, err = strconv.ParseUint(v, 10, 64); err != nil {
			return ErrRecoverySnapshotInvalid
		}
	}
	if parts[2] != "" {
		for _, v := range strings.Split(parts[2], ",") {
			if _, err = strconv.ParseUint(v, 10, 64); err != nil {
				return ErrRecoverySnapshotInvalid
			}
		}
	}
	return nil
}

func recoverySnapshotValidateState(s recoverySnapshotState, inventory RecoverySnapshotInventory, l RecoverySnapshotLimits) error {
	if s.Kind != "state" || s.Sequences == nil || s.KeyReferences == nil || s.Organizations == nil || len(s.Sequences) != len(inventory.Sequences) || len(s.KeyReferences) > l.MaxMetadataEntries || len(s.Organizations) > l.MaxMetadataEntries {
		return ErrRecoverySnapshotInvalid
	}
	for i, seq := range s.Sequences {
		if seq.Name != inventory.Sequences[i].Name || seq.ObservedLast < 1 || seq.IncludedMaximum < 0 || seq.NextMinimum <= seq.ObservedLast || seq.NextMinimum <= seq.IncludedMaximum || seq.NextMinimum != max(seq.ObservedLast, seq.IncludedMaximum)+1 || seq.NextMinimum < 1 {
			return ErrRecoverySnapshotInvalid
		}
	}
	previous := ""
	for _, ref := range s.KeyReferences {
		if (ref.Kind != "model" && ref.Kind != "tool" && ref.Kind != "purpose" && ref.Kind != "webhook") || !validText(ref.OrganizationID, 128) || !validText(ref.ID, 128) || ref.Version < 1 || !recoverySnapshotCredentialRef.MatchString(ref.Environment) {
			return ErrRecoverySnapshotInvalid
		}
		key := fmt.Sprintf("%s\x00%s\x00%s\x00%020d", ref.Kind, ref.OrganizationID, ref.ID, ref.Version)
		if previous != "" && key <= previous {
			return ErrRecoverySnapshotInvalid
		}
		previous = key
	}
	previous = ""
	for _, org := range s.Organizations {
		if !validText(org.OrganizationID, 128) || org.OrganizationID <= previous || org.EventHead < 0 || org.RetainedAfterMaximum < 0 || org.RetainedAfterMaximum > org.EventHead {
			return ErrRecoverySnapshotInvalid
		}
		previous = org.OrganizationID
	}
	return nil
}
