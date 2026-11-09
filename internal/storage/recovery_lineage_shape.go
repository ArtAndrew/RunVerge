// SPDX-License-Identifier: Elastic-2.0
package storage

import (
	"context"
	"reflect"

	"github.com/jackc/pgx/v5"
)

// Check exact historical COPY column order/types and primary keys after a
// reviewed projection of current LIKE tables. The caller first validates the
// current catalogue and inherited constraints/indexes with recoveryStageCheckShape.
// This does not permit archive-controlled DDL or arbitrary column removal.
func recoveryLineageStageShape(ctx context.Context, tx pgx.Tx, schema string, inventory RecoverySnapshotInventory) error {
	var count int
	if err := tx.QueryRow(ctx, `SELECT count(*) FROM pg_catalog.pg_class c JOIN pg_catalog.pg_namespace n ON n.oid=c.relnamespace WHERE n.nspname=$1 AND n.oid=pg_catalog.pg_my_temp_schema() AND c.relkind='r'`, schema).Scan(&count); err != nil || count != len(inventory.Tables) {
		return ErrRecoveryStageSchema
	}
	for _, table := range inventory.Tables {
		rows, err := tx.Query(ctx, `SELECT a.attname,a.attnum,a.atttypid,t.typname,a.atttypmod,a.attnotnull,a.attgenerated::text,a.attidentity::text,
 tn.nspname='pg_catalog' AND NOT a.atthasdef
 FROM pg_catalog.pg_attribute a JOIN pg_catalog.pg_class c ON c.oid=a.attrelid JOIN pg_catalog.pg_namespace n ON n.oid=c.relnamespace
 JOIN pg_catalog.pg_type t ON t.oid=a.atttypid JOIN pg_catalog.pg_namespace tn ON tn.oid=t.typnamespace
 WHERE n.nspname=$1 AND c.relname=$2 AND a.attnum>0 AND NOT a.attisdropped ORDER BY a.attnum LIMIT 257`, schema, table.Name)
		if err != nil {
			return ErrRecoveryStageSchema
		}
		columns := make([]RecoverySnapshotColumn, 0, len(table.Columns))
		for rows.Next() {
			var column RecoverySnapshotColumn
			var builtin bool
			if rows.Scan(&column.Name, &column.Number, &column.TypeOID, &column.TypeName, &column.TypeModifier, &column.NotNull, &column.Generated, &column.Identity, &builtin) != nil || !builtin || len(columns) >= len(table.Columns) {
				rows.Close()
				return ErrRecoveryStageSchema
			}
			columns = append(columns, column)
		}
		err = rows.Err()
		rows.Close()
		if err != nil {
			return ErrRecoveryStageSchema
		}
		expected := append([]RecoverySnapshotColumn{}, table.Columns...)
		// LIKE does not preserve declared array dimensions; binary value validation
		// and canonical roundtrip remain mandatory, as in ordinary typed staging.
		for i := range expected {
			expected[i].Dimensions = 0
			expected[i].Generated = ""
			expected[i].Identity = ""
		}
		if !reflect.DeepEqual(columns, expected) {
			return ErrRecoveryStageSchema
		}
		var keys []string
		err = tx.QueryRow(ctx, `SELECT COALESCE(array_agg(a.attname::text ORDER BY k.ord),ARRAY[]::text[])
 FROM pg_catalog.pg_constraint co JOIN pg_catalog.pg_class c ON c.oid=co.conrelid JOIN pg_catalog.pg_namespace n ON n.oid=c.relnamespace
 CROSS JOIN LATERAL pg_catalog.unnest(co.conkey) WITH ORDINALITY k(attnum,ord) JOIN pg_catalog.pg_attribute a ON a.attrelid=c.oid AND a.attnum=k.attnum
 WHERE n.nspname=$1 AND c.relname=$2 AND co.contype='p'`, schema, table.Name).Scan(&keys)
		if err != nil || !reflect.DeepEqual(keys, table.PrimaryKey) {
			return ErrRecoveryStageSchema
		}
	}
	return nil
}
