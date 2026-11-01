// SPDX-License-Identifier: Elastic-2.0
package storage

import (
	"context"
	"regexp"
	"time"

	"github.com/jackc/pgx/v5"
)

var recoveryWitnessNonce = regexp.MustCompile(`^[a-f0-9]{32}$`)

// HoldRecoverySourceWitness uses THIS runtime's existing pool. It creates no
// task, authority or accounting fact. The read-only transaction exposes a fresh
// challenge briefly to the trusted database-container observer, then rolls back.
func (s *Store) HoldRecoverySourceWitness(ctx context.Context, source, nonce string) error {
	if !validRecoveryID(source, "inst") || !recoveryWitnessNonce.MatchString(nonce) {
		return ErrRecoveryProvision
	}
	bounded, cancel := context.WithTimeout(ctx, 21*time.Second)
	defer cancel()
	tx, err := s.Pool.BeginTx(bounded, pgx.TxOptions{AccessMode: pgx.ReadOnly})
	if err != nil {
		return ErrRecoveryProvision
	}
	defer func() {
		cleanup, stop := context.WithTimeout(context.Background(), 3*time.Second)
		defer stop()
		// pgx closes a connection when rollback fails; an uncertain transaction
		// must never be returned to the runtime pool as usable.
		_ = tx.Rollback(cleanup)
	}()
	if _, err = validateRecoverySemanticCatalog(bounded, tx, s.runtimeSchema); err != nil {
		return ErrRecoveryProvision
	}
	state, err := readRecoveryState(bounded, s.runtimeSchema, tx)
	if err != nil || state.State != "normal" || state.InstallationID != source {
		return ErrRecoveryProvision
	}
	var admitted bool
	if tx.QueryRow(bounded, `SELECT current_database()='runverge' AND current_user='runverge' AND session_user=current_user AND NOT pg_catalog.pg_is_in_recovery() AND NOT rolsuper AND NOT rolcreatedb AND NOT rolcreaterole AND NOT rolreplication AND NOT rolbypassrls FROM pg_catalog.pg_roles WHERE rolname=current_user`).Scan(&admitted) != nil || !admitted {
		return ErrRecoveryProvision
	}
	var ignored string
	if tx.QueryRow(bounded, "SELECT pg_catalog.set_config('application_name',$1,true)", "rvg_witness_"+nonce).Scan(&ignored) != nil {
		return ErrRecoveryProvision
	}
	if _, err = tx.Exec(bounded, "SELECT pg_catalog.pg_sleep(18)"); err != nil {
		return ErrRecoveryProvision
	}
	return tx.Rollback(bounded)
}
