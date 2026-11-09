// SPDX-License-Identifier: Elastic-2.0
package storage

import (
	"bytes"
	"context"
	"errors"
	"github.com/jackc/pgx/v5"
	"io"
	"strings"
	"testing"
	"time"
)

func TestRecoverySourceEvidenceAuthorDrainAndPhysicalBinding(t *testing.T) {
	f := recoveryFenceAuthorSetup(t)
	ctx := context.Background()
	p, _, _ := recoveryAuthorPopulate(t, f.source)
	before, err := f.source.ExportRecoverySnapshot(ctx, io.Discard, DefaultRecoverySnapshotLimits())
	if err != nil {
		t.Fatal(err)
	}
	expected := RecoverySourceIdentity{SourceID: f.intent.SourceInstallationID, SystemID: f.intent.SystemID, DatabaseOID: f.intent.SourceOID, AfterCutUTC: before.Metadata.CutUTC}
	witnessed := 0
	expected.Observe = func(ctx context.Context, nonce string) error {
		var count int
		if err := f.admin.QueryRow(ctx, "SELECT count(*) FROM pg_catalog.pg_stat_activity WHERE datid=$1 AND application_name=$2 AND state='idle in transaction'", expected.DatabaseOID, "rvg_evidence_"+nonce).Scan(&count); err != nil || count != 1 {
			return errors.New("exact export backend absent")
		}
		witnessed++
		return nil
	}
	old, err := pgx.Connect(ctx, f.sourceDSN)
	if err != nil {
		t.Fatal(err)
	}
	defer old.Close(ctx)
	if _, err = old.Exec(ctx, "BEGIN"); err != nil {
		t.Fatal(err)
	}
	if _, err = old.Exec(ctx, "UPDATE organizations SET name='late source transaction' WHERE id=$1", p.OrganizationID); err != nil {
		t.Fatal(err)
	}
	f.source.Pool.Reset()
	bounded, cancel := context.WithTimeout(ctx, 250*time.Millisecond)
	var blocked bytes.Buffer
	_, err = f.source.ExportRecoverySourceEvidence(bounded, &blocked, DefaultRecoverySnapshotLimits(), expected)
	cancel()
	if err == nil || blocked.Len() != 0 || witnessed != 0 {
		t.Fatal("unsettled backend reached export snapshot")
	}
	waitCtx, waitCancel := context.WithTimeout(ctx, 10*time.Second)
	defer waitCancel()
	type result struct {
		report RecoverySnapshotReport
		err    error
	}
	completed := make(chan result, 1)
	go func() {
		r, e := f.source.ExportRecoverySourceEvidence(waitCtx, io.Discard, DefaultRecoverySnapshotLimits(), expected)
		completed <- result{r, e}
	}()
	select {
	case <-completed:
		t.Fatal("capture completed while old transaction remained open")
	case <-time.After(150 * time.Millisecond):
	}
	if _, err = old.Exec(ctx, "COMMIT"); err != nil {
		t.Fatal(err)
	}
	if err = old.Close(ctx); err != nil {
		t.Fatal(err)
	}
	finished := <-completed
	if finished.err != nil {
		t.Fatal("capture did not continue after old backend finished", finished.err)
	}
	for _, row := range finished.report.Tables {
		if row.Name == "organizations" {
			for _, prior := range before.Tables {
				if prior.Name == row.Name && prior.SHA256 == row.SHA256 {
					t.Fatal("snapshot preceded late COMMIT")
				}
			}
		}
	}
	for _, fault := range []string{"source", "system", "oid", "future_cut", "witness", "none"} {
		t.Run(fault, func(t *testing.T) {
			want := expected
			switch fault {
			case "source":
				want.SourceID = "inst_" + strings.Repeat("f", 32)
			case "system":
				want.SystemID = "wrong"
			case "oid":
				want.DatabaseOID++
			case "future_cut":
				want.AfterCutUTC = time.Now().Add(time.Hour).UTC().Format(time.RFC3339Nano)
			case "witness":
				want.Observe = func(context.Context, string) error { return errors.New("wrong physical endpoint") }
			}
			var body bytes.Buffer
			after, e := f.source.ExportRecoverySourceEvidence(ctx, &body, DefaultRecoverySnapshotLimits(), want)
			if fault != "none" {
				if e == nil || body.Len() != 0 {
					t.Fatal("mismatched source exported")
				}
				return
			}
			if e != nil {
				t.Fatal(e)
			}
			validated, e := ValidateRecoverySnapshot(ctx, bytes.NewReader(body.Bytes()), DefaultRecoverySnapshotLimits())
			if e != nil || validated.SHA256 != after.SHA256 {
				t.Fatal("invalid evidence stream", e)
			}
			var previous, current RecoverySnapshotTableSummary
			for _, row := range before.Tables {
				if row.Name == "organizations" {
					previous = row
				}
			}
			for _, row := range after.Tables {
				if row.Name == "organizations" {
					current = row
				}
			}
			if previous == current {
				t.Fatal("late committed row absent from evidence")
			}
		})
	}
	prepared, err := pgx.Connect(ctx, f.sourceDSN)
	if err != nil {
		t.Fatal(err)
	}
	defer prepared.Close(ctx)
	if _, err = prepared.Exec(ctx, "BEGIN; UPDATE organizations SET name='unresolved prepared transaction'; PREPARE TRANSACTION 'source_evidence_owned_fixture'"); err != nil {
		t.Fatal("prepared fixture", err)
	}
	if err = prepared.Close(ctx); err != nil {
		t.Fatal(err)
	}
	defer func() {
		cleanup, e := pgx.Connect(ctx, f.sourceDSN)
		if e != nil {
			t.Error(e)
			return
		}
		defer cleanup.Close(ctx)
		if _, e = cleanup.Exec(ctx, "ROLLBACK PREPARED 'source_evidence_owned_fixture'"); e != nil {
			t.Error(e)
		}
	}()
	bounded, cancel = context.WithTimeout(ctx, 250*time.Millisecond)
	blocked.Reset()
	_, err = f.source.ExportRecoverySourceEvidence(bounded, &blocked, DefaultRecoverySnapshotLimits(), expected)
	cancel()
	if err == nil || blocked.Len() != 0 {
		t.Fatal("prepared transaction admitted into evidence")
	}
}
