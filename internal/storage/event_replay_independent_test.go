// SPDX-License-Identifier: Elastic-2.0
package storage

import (
	"context"
	"encoding/json"
	"strconv"
	"strings"
	"testing"
	"time"

	"agentplatform.local/platform/internal/domain"
	"github.com/jackc/pgx/v5"
)

// Observe the exact blocker, rather than accepting another concurrently
// running test's blocked statement as evidence that this barrier was reached.
func independentEventBlocked(t *testing.T, s *Store, ctx context.Context, blocker uint32, prefix string) {
	t.Helper()
	ticker := time.NewTicker(5 * time.Millisecond)
	defer ticker.Stop()
	for {
		var blocked bool
		err := s.Pool.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM pg_stat_activity
 WHERE datname=current_database() AND wait_event_type='Lock'
 AND $1::integer=ANY(pg_blocking_pids(pid)) AND query LIKE $2)`, blocker, prefix+"%").Scan(&blocked)
		if err != nil {
			t.Fatal(err)
		}
		if blocked {
			return
		}
		select {
		case <-ctx.Done():
			t.Fatal("exact event-read database barrier was not reached", ctx.Err())
		case <-ticker.C:
		}
	}
}

func independentEventArchiveDigest(t *testing.T, s *Store, ctx context.Context, org string) string {
	t.Helper()
	var digest string
	err := s.Pool.QueryRow(ctx, `SELECT md5(COALESCE((SELECT string_agg(row_to_json(e)::text,'' ORDER BY sequence)
 FROM events e WHERE organization_id=$1),'')) || ':' ||
 md5(COALESCE((SELECT string_agg(row_to_json(o)::text,'' ORDER BY sequence)
 FROM outbox o WHERE organization_id=$1),''))`, org).Scan(&digest)
	if err != nil {
		t.Fatal(err)
	}
	return digest
}

func TestIndependentEventSnapshotCommitBetweenReadsStaysAtomic(t *testing.T) {
	s, _ := testStore(t)
	ctx, cancel := context.WithTimeout(context.Background(), 12*time.Second)
	defer cancel()
	p, _ := identity(t, s, 100000)
	v := task(t, s, p, agent(t, s, p))
	before, err := s.TaskEventSnapshot(ctx, p, v.ID)
	if err != nil {
		t.Fatal(err)
	}
	barrier, err := s.Pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer barrier.Rollback(ctx)
	if _, err = barrier.Exec(ctx, "LOCK TABLE task_event_windows IN ACCESS EXCLUSIVE MODE"); err != nil {
		t.Fatal(err)
	}
	type observed struct {
		snapshot domain.TaskEventSnapshot
		err      error
	}
	done := make(chan observed, 1)
	go func() {
		value, err := s.TaskEventSnapshot(ctx, p, v.ID)
		done <- observed{value, err}
	}()
	// The task and authorization reads have already established the RR view;
	// only the subsequent window/high-water read is blocked here.
	independentEventBlocked(t, s, ctx, barrier.Conn().PgConn().PID(), "SELECT after_sequence,version,")
	if _, err = s.CancelTask(ctx, p, v.ID, domain.NewID("key")); err != nil {
		t.Fatal("concurrent cancellation could not commit independently", err)
	}
	current, err := s.GetTask(ctx, p, v.ID)
	if err != nil || current.Status != "cancelled" {
		t.Fatal("writer did not actually commit before releasing reader", err)
	}
	if err = barrier.Commit(ctx); err != nil {
		t.Fatal(err)
	}
	result := <-done
	if result.err != nil {
		t.Fatal(result.err)
	}
	if result.snapshot.Task.Status != before.Task.Status || result.snapshot.Task.Version != before.Task.Version || result.snapshot.Cursor != before.Cursor || result.snapshot.Window != before.Window || len(result.snapshot.Events) != len(before.Events) {
		t.Fatal("snapshot combined task state and event coordinates from different commits")
	}
	batch, err := s.ReplayTaskEvents(ctx, p, v.ID, result.snapshot.Cursor, 10)
	if err != nil || batch.ResetRequired || len(batch.Events) != 1 || batch.Events[0].Type != "task.cancelled" || batch.Cursor == result.snapshot.Cursor {
		t.Fatal("snapshot handoff lost or duplicated the concurrently committed event", err)
	}
	latest, err := s.TaskEventSnapshot(ctx, p, v.ID)
	if err != nil || latest.Task.Status != "cancelled" || latest.Cursor != batch.Cursor {
		t.Fatal("fresh state did not converge to the replay boundary", err)
	}
}

func TestIndependentEventQuietTaskFloorRestartsWithoutResetLoop(t *testing.T) {
	s, _ := testStore(t)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	p, _ := identity(t, s, 100000)
	a := agent(t, s, p)
	quiet := task(t, s, p, a)
	quietBefore, err := s.TaskEventSnapshot(ctx, p, quiet.ID)
	if err != nil {
		t.Fatal(err)
	}
	noisy := task(t, s, p, a)
	if _, err = s.CancelTask(ctx, p, noisy.ID, domain.NewID("key")); err != nil {
		t.Fatal(err)
	}
	noisyBefore, err := s.TaskEventSnapshot(ctx, p, noisy.ID)
	if err != nil || noisyBefore.Window.ThroughSequence <= quietBefore.Window.ThroughSequence {
		t.Fatal("fixture did not create a genuine unrelated higher sequence", err)
	}
	archive := independentEventArchiveDigest(t, s, ctx, p.OrganizationID)
	if _, err = s.AdvanceEventReplayWindow(ctx, p.OrganizationID, noisy.ID, noisyBefore.Window.ThroughSequence, "Independent noisy task replay retention"); err != nil {
		t.Fatal(err)
	}
	quietUnchanged, err := s.TaskEventSnapshot(ctx, p, quiet.ID)
	if err != nil || quietUnchanged.Window != quietBefore.Window || quietUnchanged.Cursor != quietBefore.Cursor {
		t.Fatal("another task's replay retention changed the quiet task", err)
	}
	if _, err = s.AdvanceEventReplayWindow(ctx, p.OrganizationID, quiet.ID, quietBefore.Window.ThroughSequence, "Independent quiet task replay retention"); err != nil {
		t.Fatal(err)
	}
	expired, err := s.ReplayTaskEvents(ctx, p, quiet.ID, quietBefore.ReplayCursor, 10)
	if err != nil || !expired.ResetRequired || expired.ResetReason != "history_expired" || expired.Cursor != "" || len(expired.Events) != 0 || expired.HasMore {
		t.Fatal("expired replay silently skipped events or advanced its cursor", err)
	}
	recovered, err := s.TaskEventSnapshot(ctx, p, quiet.ID)
	if err != nil || recovered.Task.ID != quiet.ID || recovered.Cursor != quietBefore.Cursor || len(recovered.Events) != 0 || !recovered.HasEarlierEvents {
		t.Fatal("quiet task recovery did not preserve its own exact head", err)
	}
	empty, err := s.ReplayTaskEvents(ctx, p, quiet.ID, recovered.Cursor, 10)
	if err != nil || empty.ResetRequired || empty.Cursor != recovered.Cursor || len(empty.Events) != 0 {
		t.Fatal("fresh quiet-task snapshot immediately expired again", err)
	}
	if got := independentEventArchiveDigest(t, s, ctx, p.OrganizationID); got != archive {
		t.Fatal("logical retention rewrote historical events or outbox")
	}
	if _, err = s.CancelTask(ctx, p, quiet.ID, domain.NewID("key")); err != nil {
		t.Fatal(err)
	}
	next, err := s.ReplayTaskEvents(ctx, p, quiet.ID, recovered.Cursor, 10)
	if err != nil || next.ResetRequired || len(next.Events) != 1 || next.Events[0].TaskID != quiet.ID || next.Events[0].Type != "task.cancelled" {
		t.Fatal("new event after an empty recovery boundary was lost", err)
	}
}

func TestIndependentEventWideDatabasePositionSurvivesWireAndReplay(t *testing.T) {
	s, _ := testStore(t)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	p, _ := identity(t, s, 100000)
	v := task(t, s, p, agent(t, s, p))
	before, err := s.TaskEventSnapshot(ctx, p, v.ID)
	if err != nil {
		t.Fatal(err)
	}
	const previous int64 = 9007199254740992
	// Advance only this disposable schema's actual identity sequence. The event
	// itself is then emitted by the ordinary authenticated cancellation command.
	if _, err = s.Pool.Exec(ctx, "SELECT setval(pg_get_serial_sequence('events','sequence'),$1,true)", previous); err != nil {
		t.Fatal(err)
	}
	if _, err = s.CancelTask(ctx, p, v.ID, domain.NewID("key")); err != nil {
		t.Fatal(err)
	}
	batch, err := s.ReplayTaskEvents(ctx, p, v.ID, before.Cursor, 1)
	if err != nil || batch.ResetRequired || len(batch.Events) != 1 || batch.Events[0].Sequence != previous+1 || batch.Window.ThroughSequence != previous+1 {
		t.Fatal("real bigint event was rounded or mistaken for an expired history gap", err)
	}
	wire, err := json.Marshal(batch)
	if err != nil {
		t.Fatal(err)
	}
	var envelope map[string]json.RawMessage
	if json.Unmarshal(wire, &envelope) != nil || !strings.Contains(string(envelope["events"]), `"sequence":"9007199254740993"`) || !strings.Contains(string(envelope["window"]), `"through_sequence":"9007199254740993"`) {
		t.Fatal("public batch encoded a bigint as an unsafe JSON number")
	}
	position, err := decodeTaskEventCursor(batch.Cursor, p.OrganizationID, v.ID)
	if err != nil || strconv.FormatInt(position, 10) != "9007199254740993" {
		t.Fatal("wire cursor did not preserve the stored bigint", err)
	}
	repeated, err := s.ReplayTaskEvents(ctx, p, v.ID, batch.Cursor, 1)
	if err != nil || len(repeated.Events) != 0 || repeated.Cursor != batch.Cursor || repeated.ResetRequired {
		t.Fatal("wide position replay duplicated the accepted event", err)
	}
}

func TestIndependentEventFreshAdmissionAfterBlockedReplayRejectsRevocation(t *testing.T) {
	s, _ := testStore(t)
	ctx, cancel := context.WithTimeout(context.Background(), 12*time.Second)
	defer cancel()
	p, _ := identity(t, s, 100000)
	v := task(t, s, p, agent(t, s, p))
	before, err := s.TaskEventSnapshot(ctx, p, v.ID)
	if err != nil {
		t.Fatal(err)
	}
	barrier, err := s.Pool.BeginTx(ctx, pgx.TxOptions{})
	if err != nil {
		t.Fatal(err)
	}
	defer barrier.Rollback(ctx)
	if _, err = barrier.Exec(ctx, "LOCK TABLE events IN ACCESS EXCLUSIVE MODE"); err != nil {
		t.Fatal(err)
	}
	type replayed struct {
		batch domain.TaskEventBatch
		err   error
	}
	done := make(chan replayed, 1)
	go func() {
		value, err := s.ReplayTaskEvents(ctx, p, v.ID, before.Cursor, 10)
		done <- replayed{value, err}
	}()
	independentEventBlocked(t, s, ctx, barrier.Conn().PgConn().PID(), "SELECT after_sequence,version,")
	if _, err = s.Pool.Exec(ctx, "UPDATE access_tokens SET revoked=true WHERE token_hash=$1", p.TokenHash); err != nil {
		t.Fatal(err)
	}
	if err = barrier.Commit(ctx); err != nil {
		t.Fatal(err)
	}
	result := <-done
	if result.err != nil || result.batch.Cursor != before.Cursor {
		t.Fatal("previously admitted RR replay did not complete at its own boundary", result.err)
	}
	// This extra current admission is essential before a live transport emits
	// the replay it prepared under the older snapshot. No old Principal role or
	// successful previous replay can substitute for a currently valid token.
	code(t, s.CheckEventReadAccess(ctx, p, v.ID), "unauthenticated")
	_, err = s.ReplayTaskEvents(ctx, p, v.ID, before.Cursor, 10)
	code(t, err, "unauthenticated")
}
