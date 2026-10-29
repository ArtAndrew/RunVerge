// SPDX-License-Identifier: Elastic-2.0
package storage

import (
	"context"
	"encoding/json"

	"github.com/jackc/pgx/v5"
)

// The ordinary global gate remains first. This independent historical fence
// rejects old work before authority-denial cleanup can mutate its evidence.
// Keys follow the trusted physical PK order; table names are source constants.
func admitRecoveryRowTx(ctx context.Context, tx pgx.Tx, table string, keys ...any) error {
	encoded, err := json.Marshal(keys)
	if err != nil {
		return RecoveryHeld
	}
	var historical bool
	if err = tx.QueryRow(ctx, "SELECT recovery_is_historical($1,$2::jsonb)", table, encoded).Scan(&historical); err != nil {
		return recoveryGateError(err)
	}
	if historical {
		return RecoveryHeld
	}
	return nil
}

func admitRecoveryTaskTx(ctx context.Context, tx pgx.Tx, org, task string) error {
	return admitRecoveryRowTx(ctx, tx, "tasks", org, task)
}
