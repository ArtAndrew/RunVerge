// SPDX-License-Identifier: Elastic-2.0
package storage

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"math"
	"strconv"
	"strings"
	"testing"
	"time"

	"agentplatform.local/platform/internal/domain"
)

func TestEventCursorCanonicalScopedCoordinates(t *testing.T) {
	for _, after := range []int64{0, 1, 9007199254740993, math.MaxInt64} {
		raw := encodeTaskEventCursor("org_test", "task_test", after)
		got, err := decodeTaskEventCursor(raw, "org_test", "task_test")
		if err != nil || got != after {
			t.Fatalf("exact coordinate %d: %d %v", after, got, err)
		}
	}
	for _, body := range []string{
		`{"v":1,"org":"org_test","task":"task_test","after":"01"}`,
		`{"v":1,"org":"org_test","task":"task_test","after":"-0"}`,
		`{"v":1,"org":"org_test","task":"task_test","after":"+1"}`,
		`{"v":1,"org":"org_test","task":"task_test","after":"9223372036854775808"}`,
		`{"v":1,"org":"org_test","task":"task_test","after":1}`,
		`{"v":1,"org":"org_test","task":"task_test","after":"1","after":"1"}`,
		`{"v":1,"org":"org_test","task":"task_test","after":"1","extension":0}`,
		`{"v":2,"org":"org_test","task":"task_test","after":"1"}`,
		`{"v":1,"org":"org_other","task":"task_test","after":"1"}`,
		`{"v":1,"org":"org_test","task":"task_other","after":"1"}`,
		`{"org":"org_test","v":1,"task":"task_test","after":"1"}`,
		` {"v":1,"org":"org_test","task":"task_test","after":"1"}`,
		`{"v":1,"org":"org_test","task":"task_test","after":"1"} {}`,
	} {
		_, err := decodeTaskEventCursor(base64.RawURLEncoding.EncodeToString([]byte(body)), "org_test", "task_test")
		code(t, err, "invalid_request")
	}
	_, err := decodeTaskEventCursor(strings.Repeat("A", 1025), "org_test", "task_test")
	code(t, err, "invalid_request")
	_, err = decodeTaskEventCursor(encodeTaskEventCursor("org_test", "task_test", 1)+"=", "org_test", "task_test")
	code(t, err, "invalid_request")
}

func authorEmitEvents(t *testing.T, s *Store, org, task string, count int) []int64 {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	tx, err := s.Pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback(ctx)
	for i := 0; i < count; i++ {
		if err = emit(ctx, tx, org, task, "task.blocked", map[string]any{"reason": fmt.Sprintf("author-event-%d", i)}); err != nil {
			t.Fatal(err)
		}
	}
	if err = tx.Commit(ctx); err != nil {
		t.Fatal(err)
	}
	rows, err := s.Pool.Query(ctx, "SELECT sequence FROM events WHERE organization_id=$1 AND task_id=$2 ORDER BY sequence", org, task)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	var out []int64
	for rows.Next() {
		var seq int64
		if err = rows.Scan(&seq); err != nil {
			t.Fatal(err)
		}
		out = append(out, seq)
	}
	if err = rows.Err(); err != nil {
		t.Fatal(err)
	}
	return out
}

func TestEventSnapshotMatchesCommittedTaskAndCursor(t *testing.T) {
	s, _ := testStore(t)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	p, _ := identity(t, s, 100000)
	v := task(t, s, p, agent(t, s, p))
	before, err := s.TaskEventSnapshot(ctx, p, v.ID)
	if err != nil {
		t.Fatal(err)
	}
	tx, err := s.Pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback(ctx)
	if err = cancelTasksTx(ctx, tx, p.OrganizationID, []string{v.ID}, "author_snapshot_boundary"); err != nil {
		t.Fatal(err)
	}
	uncommitted, err := s.TaskEventSnapshot(ctx, p, v.ID)
	if err != nil || uncommitted.Task.Status != v.Status || uncommitted.Cursor != before.Cursor || len(uncommitted.Events) != len(before.Events) {
		t.Fatalf("uncommitted state leaked: %+v %v", uncommitted, err)
	}
	if err = tx.Commit(ctx); err != nil {
		t.Fatal(err)
	}
	after, err := s.TaskEventSnapshot(ctx, p, v.ID)
	if err != nil || after.Task.Status != "cancelled" || after.Cursor == before.Cursor || len(after.Events) != len(before.Events)+1 || after.Events[len(after.Events)-1].Type != "task.cancelled" {
		t.Fatalf("committed snapshot mismatch: %+v %v", after, err)
	}
	batch, err := s.ReplayTaskEvents(ctx, p, v.ID, before.Cursor, 10)
	if err != nil || len(batch.Events) != 1 || batch.Events[0].Type != "task.cancelled" || batch.Cursor != after.Cursor {
		t.Fatalf("snapshot-to-replay gap: %+v %v", batch, err)
	}
	var unpaired int
	if err = s.Pool.QueryRow(ctx, "SELECT count(*) FROM events e LEFT JOIN outbox o USING(sequence) WHERE e.organization_id=$1 AND o.sequence IS NULL", p.OrganizationID).Scan(&unpaired); err != nil || unpaired != 0 {
		t.Fatalf("event/outbox atomicity: %d %v", unpaired, err)
	}
}

func TestEventReplayWindowIsScopedMonotonicAndKeepsArchive(t *testing.T) {
	s, _ := testStore(t)
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	p, _ := identity(t, s, 100000)
	other, _ := identity(t, s, 100000)
	a := agent(t, s, p)
	v := task(t, s, p, a)
	sequences := authorEmitEvents(t, s, p.OrganizationID, v.ID, 70)
	otherTask := task(t, s, p, a)
	// Global sequence gaps are legal; a cursor is bounded by this task's head.
	otherSeq := authorEmitEvents(t, s, p.OrganizationID, otherTask.ID, 1)
	snapshot, err := s.TaskEventSnapshot(ctx, p, v.ID)
	if err != nil || len(snapshot.Events) != domain.EventSnapshotLimit || !snapshot.HasEarlierEvents {
		t.Fatalf("bounded preview: %+v %v", snapshot, err)
	}
	raw := snapshot.ReplayCursor
	var replayed []int64
	for i := 0; i < 20; i++ {
		batch, e := s.ReplayTaskEvents(ctx, p, v.ID, raw, 7)
		if e != nil || batch.ResetRequired {
			t.Fatalf("valid replay: %+v %v", batch, e)
		}
		for _, event := range batch.Events {
			replayed = append(replayed, event.Sequence)
		}
		raw = batch.Cursor
		if !batch.HasMore {
			break
		}
	}
	if fmt.Sprint(replayed) != fmt.Sprint(sequences) || raw != snapshot.Cursor {
		t.Fatalf("replay omitted/repeated coordinates: %v != %v", replayed, sequences)
	}
	_, err = s.ReplayTaskEvents(ctx, other, v.ID, snapshot.Cursor, 10)
	code(t, err, "not_found")
	_, err = s.ReplayTaskEvents(ctx, p, otherTask.ID, snapshot.Cursor, 10)
	code(t, err, "invalid_request")
	_, err = s.ReplayTaskEvents(ctx, p, v.ID, encodeTaskEventCursor(p.OrganizationID, v.ID, otherSeq[len(otherSeq)-1]), 10)
	code(t, err, "invalid_request")

	floor := sequences[30]
	window, err := s.AdvanceEventReplayWindow(ctx, p.OrganizationID, v.ID, floor, "author bounded live replay")
	if err != nil || window.AfterSequence != floor || window.Version != 2 {
		t.Fatalf("advance: %+v %v", window, err)
	}
	reset, err := s.ReplayTaskEvents(ctx, p, v.ID, snapshot.ReplayCursor, 10)
	if err != nil || !reset.ResetRequired || reset.ResetReason != "history_expired" || len(reset.Events) != 0 || reset.Cursor != "" || reset.HasMore {
		t.Fatalf("expired cursor silently advanced: %+v %v", reset, err)
	}
	boundary, err := s.ReplayTaskEvents(ctx, p, v.ID, encodeTaskEventCursor(p.OrganizationID, v.ID, floor), 10)
	if err != nil || boundary.ResetRequired || len(boundary.Events) != 10 || boundary.Events[0].Sequence != sequences[31] {
		t.Fatalf("floor equality: %+v %v", boundary, err)
	}
	_, err = s.AdvanceEventReplayWindow(ctx, p.OrganizationID, v.ID, floor-1, "cannot move backwards")
	code(t, err, "invalid_transition")
	_, err = s.AdvanceEventReplayWindow(ctx, p.OrganizationID, v.ID, otherSeq[len(otherSeq)-1], "cannot skip future")
	code(t, err, "invalid_request")
	if _, err = s.AdvanceEventReplayWindow(ctx, p.OrganizationID, v.ID, floor, "idempotent same floor"); err != nil {
		t.Fatal(err)
	}
	archive, _, err := s.Events(ctx, p, v.ID, 0, 100)
	if err != nil || len(archive) != len(sequences) {
		t.Fatalf("logical floor deleted archive: %d %v", len(archive), err)
	}
	var audits, events, outbox int
	if err = s.Pool.QueryRow(ctx, `SELECT (SELECT count(*) FROM event_replay_window_audit WHERE organization_id=$1 AND task_id=$2),(SELECT count(*) FROM events WHERE organization_id=$1 AND task_id=$2),(SELECT count(*) FROM outbox o JOIN events e USING(sequence) WHERE e.organization_id=$1 AND e.task_id=$2)`, p.OrganizationID, v.ID).Scan(&audits, &events, &outbox); err != nil || audits != 1 || events != len(sequences) || outbox != events {
		t.Fatalf("logical window audit/archive: %d %d %d %v", audits, events, outbox, err)
	}
}

func TestEventLegacyRawPrecisionAndOversizedReplayNeverSkips(t *testing.T) {
	s, _ := testStore(t)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	p, _ := identity(t, s, 100000)
	v := task(t, s, p, agent(t, s, p))
	before, err := s.TaskEventSnapshot(ctx, p, v.ID)
	if err != nil {
		t.Fatal(err)
	}
	legacy := `{"wide":9007199254740993,"opaque":"` + strings.Repeat("x", domain.MaxEventBatchBytes+100) + `"}`
	if _, err = s.Pool.Exec(ctx, `INSERT INTO events(organization_id,id,task_id,type,data,payload_version) VALUES($1,$2,$3,'historical.opaque',$4,0)`, p.OrganizationID, domain.NewID("evt"), v.ID, legacy); err != nil {
		t.Fatal(err)
	}
	batch, err := s.ReplayTaskEvents(ctx, p, v.ID, before.Cursor, 10)
	code(t, err, "invalid_transition")
	if batch.Cursor != "" || len(batch.Events) != 0 {
		t.Fatal("unavailable legacy value advanced cursor")
	}
	archive, _, err := s.Events(ctx, p, v.ID, 0, 100)
	if err != nil {
		t.Fatal(err)
	}
	last := archive[len(archive)-1]
	var exact string
	if err = s.Pool.QueryRow(ctx, "SELECT data::text FROM events WHERE organization_id=$1 AND id=$2", p.OrganizationID, last.ID).Scan(&exact); err != nil {
		t.Fatal(err)
	}
	if last.PayloadVersion != 0 || last.Data != nil || last.LegacyDataJSON != exact || !strings.Contains(last.LegacyDataJSON, "9007199254740993") {
		t.Fatal("legacy payload precision/type was changed")
	}
	wire, err := json.Marshal(last)
	if err != nil || !strings.Contains(string(wire), `"sequence":"`+strconv.FormatInt(last.Sequence, 10)+`"`) || strings.Contains(string(wire), `"data":`) {
		t.Fatalf("legacy envelope precision: %s %v", wire, err)
	}
}

func TestEventSnapshotPreviewByteBudgetAndFreshAuthorization(t *testing.T) {
	s, _ := testStore(t)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	p, _ := identity(t, s, 100000)
	v := task(t, s, p, agent(t, s, p))
	for i := 0; i < 40; i++ {
		if _, err := s.Pool.Exec(ctx, `INSERT INTO events(organization_id,id,task_id,type,data,payload_version) VALUES($1,$2,$3,'historical.preview',$4,0)`, p.OrganizationID, domain.NewID("evt"), v.ID, `{"opaque":"`+strings.Repeat("x", 10000)+`"}`); err != nil {
			t.Fatal(err)
		}
	}
	snapshot, err := s.TaskEventSnapshot(ctx, p, v.ID)
	if err != nil || !snapshot.HasEarlierEvents || len(snapshot.Events) == 0 || len(snapshot.Events) >= 40 || snapshot.Task.ID != v.ID || snapshot.Cursor == "" {
		t.Fatalf("preview byte bound: %d %v", len(snapshot.Events), err)
	}
	body, _ := json.Marshal(snapshot.Events)
	if len(body) > domain.MaxSnapshotEventBytes {
		t.Fatalf("preview bytes %d", len(body))
	}
	if err = s.CheckEventReadAccess(ctx, p, v.ID); err != nil {
		t.Fatal(err)
	}
	if _, err = s.Pool.Exec(ctx, "UPDATE access_tokens SET revoked=true WHERE token_hash=$1", p.TokenHash); err != nil {
		t.Fatal(err)
	}
	code(t, s.CheckEventReadAccess(ctx, p, v.ID), "unauthenticated")
	_, err = s.TaskEventSnapshot(ctx, p, v.ID)
	code(t, err, "unauthenticated")
	_, err = s.ReplayTaskEvents(ctx, p, v.ID, snapshot.Cursor, 10)
	code(t, err, "unauthenticated")
}

func TestEventTypedWriterPreservesWideMoneyAndFence(t *testing.T) {
	s, _ := testStore(t)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	p, _ := identity(t, s, 100000)
	v := task(t, s, p, agent(t, s, p))
	before, err := s.TaskEventSnapshot(ctx, p, v.ID)
	if err != nil {
		t.Fatal(err)
	}
	tx, err := s.Pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback(ctx)
	// Exercise the event serializer at its full bigint boundary. These are
	// serializer fixtures, not fabricated accounting/provider evidence.
	if err = emit(ctx, tx, p.OrganizationID, v.ID, "cost.reserved", map[string]any{"reservation_id": "res_wide_event", "amount_microusd": int64(math.MaxInt64), "category": "model", "price_version": "test-price-v1"}); err != nil {
		t.Fatal(err)
	}
	if err = emit(ctx, tx, p.OrganizationID, v.ID, "operation.claimed", map[string]any{"operation_id": "op_wide_event", "fencing_token": int64(math.MaxInt64), "attempt": 1, "adapter": "fixture", "synthetic_cost": true}); err != nil {
		t.Fatal(err)
	}
	if err = tx.Commit(ctx); err != nil {
		t.Fatal(err)
	}
	batch, err := s.ReplayTaskEvents(ctx, p, v.ID, before.Cursor, 10)
	if err != nil || len(batch.Events) != 2 {
		t.Fatalf("wide event replay: %+v %v", batch, err)
	}
	for i, key := range []string{"amount_microusd", "fencing_token"} {
		if batch.Events[i].PayloadVersion != 1 || batch.Events[i].Data[key] != "9223372036854775807" || batch.Events[i].LegacyDataJSON != "" {
			t.Fatalf("wide typed event %s lost precision/type", key)
		}
	}
}

func TestEventWriterRejectsUnregisteredPayloadWithoutHistory(t *testing.T) {
	s, _ := testStore(t)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	p, _ := identity(t, s, 100000)
	v := task(t, s, p, agent(t, s, p))
	before, err := s.TaskEventSnapshot(ctx, p, v.ID)
	if err != nil {
		t.Fatal(err)
	}
	for _, invalid := range []struct {
		kind string
		data map[string]any
	}{
		{"unregistered.author_event", map[string]any{"reason": "invalid"}},
		{"task.blocked", map[string]any{"reason": 4}},
		{"task.blocked", map[string]any{"reason": "valid", "unknown_field": true}},
	} {
		tx, e := s.Pool.Begin(ctx)
		if e != nil {
			t.Fatal(e)
		}
		err = emit(ctx, tx, p.OrganizationID, v.ID, invalid.kind, invalid.data)
		if err == nil {
			t.Fatal("unregistered/malformed event was admitted")
		}
		if e = tx.Rollback(ctx); e != nil {
			t.Fatal(e)
		}
	}
	after, err := s.TaskEventSnapshot(ctx, p, v.ID)
	if err != nil || after.Cursor != before.Cursor || len(after.Events) != len(before.Events) {
		t.Fatal("invalid event polluted durable history")
	}
}

func TestEventReaderRejectsMalformedStoredV1DecimalTypes(t *testing.T) {
	for _, fixture := range []struct {
		name, kind, payload string
	}{
		{"money", "cost.reserved", `{"reservation_id":"res_bad_event","amount_microusd":9007199254740993,"category":"model","price_version":"test-price-v1"}`},
		{"fence", "operation.claimed", `{"operation_id":"op_bad_event","fencing_token":9007199254740993,"attempt":1,"adapter":"fixture","synthetic_cost":true}`},
	} {
		t.Run(fixture.name, func(t *testing.T) {
			s, _ := testStore(t)
			ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
			defer cancel()
			p, _ := identity(t, s, 100000)
			v := task(t, s, p, agent(t, s, p))
			before, err := s.TaskEventSnapshot(ctx, p, v.ID)
			if err != nil {
				t.Fatal(err)
			}
			// Trusted SQL injects a broken stored-v1 fixture. Reader validation
			// must reject it, rather than normalize it and return numeric money.
			if _, err = s.Pool.Exec(ctx, `INSERT INTO events(organization_id,id,task_id,type,data,payload_version) VALUES($1,$2,$3,$4,$5,1)`, p.OrganizationID, domain.NewID("evt"), v.ID, fixture.kind, fixture.payload); err != nil {
				t.Fatal(err)
			}
			batch, err := s.ReplayTaskEvents(ctx, p, v.ID, before.Cursor, 10)
			code(t, err, "invalid_transition")
			if batch.Cursor != "" || len(batch.Events) != 0 {
				t.Fatal("malformed typed record advanced the consumer")
			}
		})
	}
}

func authorDrainWake(ch <-chan struct{}) {
	for {
		select {
		case _, ok := <-ch:
			if !ok {
				return
			}
		default:
			return
		}
	}
}

func authorAwaitWake(t *testing.T, ctx context.Context, ch <-chan struct{}) {
	t.Helper()
	select {
	case _, ok := <-ch:
		if !ok {
			t.Fatal("subscription closed unexpectedly")
		}
	case <-ctx.Done():
		t.Fatal("event wakeup deadline")
	}
}

func TestEventReplayWindowCommitWakesQuietSubscribersWithoutNewFact(t *testing.T) {
	s, _ := testStore(t)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	p, _ := identity(t, s, 100000)
	v := task(t, s, p, agent(t, s, p))
	before, err := s.TaskEventSnapshot(ctx, p, v.ID)
	if err != nil {
		t.Fatal(err)
	}
	ch, err := s.SubscribeTaskEvents(ctx, p.OrganizationID, v.ID)
	if err != nil {
		t.Fatal(err)
	}
	authorDrainWake(ch)
	if _, err = s.AdvanceEventReplayWindow(ctx, p.OrganizationID, v.ID, before.Window.ThroughSequence, "author prompt reset on otherwise quiet task"); err != nil {
		t.Fatal(err)
	}
	authorAwaitWake(t, ctx, ch)
	batch, err := s.ReplayTaskEvents(ctx, p, v.ID, before.ReplayCursor, 10)
	if err != nil || !batch.ResetRequired || batch.Cursor != "" || len(batch.Events) != 0 {
		t.Fatalf("quiet window wake did not expose committed reset: %+v %v", batch, err)
	}
	var count, audits int
	if err = s.Pool.QueryRow(ctx, "SELECT (SELECT count(*) FROM events WHERE organization_id=$1 AND task_id=$2),(SELECT count(*) FROM event_replay_window_audit WHERE organization_id=$1 AND task_id=$2)", p.OrganizationID, v.ID).Scan(&count, &audits); err != nil || count != len(before.Events) || audits != 1 {
		t.Fatal("window notification created an event or omitted its audit", count, audits, err)
	}
}

func TestEventNotificationsSharedCommitScopeAndReconnect(t *testing.T) {
	s, dsn := testStore(t)
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	p, _ := identity(t, s, 100000)
	a := agent(t, s, p)
	v := task(t, s, p, a)
	other := task(t, s, p, a)
	first, err := s.SubscribeTaskEvents(ctx, p.OrganizationID, v.ID)
	if err != nil {
		t.Fatal(err)
	}
	slow, err := s.SubscribeTaskEvents(ctx, p.OrganizationID, v.ID)
	if err != nil {
		t.Fatal(err)
	}
	wrong, err := s.SubscribeTaskEvents(ctx, p.OrganizationID, other.ID)
	if err != nil {
		t.Fatal(err)
	}
	secondStore, err := Open(ctx, dsn)
	if err != nil {
		t.Fatal(err)
	}
	defer secondStore.Close()
	remote, err := secondStore.SubscribeTaskEvents(ctx, p.OrganizationID, v.ID)
	if err != nil {
		t.Fatal(err)
	}
	for _, ch := range []<-chan struct{}{first, slow, wrong, remote} {
		authorDrainWake(ch)
	}
	rolledBack, err := s.Pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if err = emit(ctx, rolledBack, p.OrganizationID, v.ID, "task.blocked", map[string]any{"reason": "notification_rollback_boundary"}); err != nil {
		rolledBack.Rollback(ctx)
		t.Fatal(err)
	}
	if err = rolledBack.Rollback(ctx); err != nil {
		t.Fatal(err)
	}
	select {
	case <-first:
		t.Fatal("notification escaped a rolled-back transaction")
	case <-time.After(40 * time.Millisecond):
	}
	tx, err := s.Pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback(ctx)
	if err = emit(ctx, tx, p.OrganizationID, v.ID, "task.blocked", map[string]any{"reason": "notification_commit_boundary"}); err != nil {
		t.Fatal(err)
	}
	select {
	case <-first:
		t.Fatal("notification arrived before transaction commit")
	case <-time.After(40 * time.Millisecond):
	}
	if err = tx.Commit(ctx); err != nil {
		t.Fatal(err)
	}
	authorAwaitWake(t, ctx, first)
	authorAwaitWake(t, ctx, remote)
	if len(slow) != 1 {
		t.Fatal("same-store slow subscription did not coalesce")
	}
	select {
	case <-wrong:
		t.Fatal("notification crossed task scope")
	default:
	}
	authorEmitEvents(t, s, p.OrganizationID, v.ID, 30)
	authorAwaitWake(t, ctx, first)
	if len(slow) != 1 {
		t.Fatal("slow reader buffered more than one hint")
	}
	s.eventMu.Lock()
	hub := s.eventHub
	s.eventMu.Unlock()
	hub.mu.Lock()
	oldPID, subscribers := hub.backendPID, hub.count
	hub.mu.Unlock()
	if oldPID == 0 || subscribers != 3 {
		t.Fatal("one backend listener did not serve all local subscriptions")
	}
	var terminated bool
	if err = s.Pool.QueryRow(ctx, "SELECT pg_terminate_backend($1)", oldPID).Scan(&terminated); err != nil || !terminated {
		t.Fatalf("listener disconnect: %v", err)
	}
	deadline := time.NewTimer(5 * time.Second)
	defer deadline.Stop()
	tick := time.NewTicker(5 * time.Millisecond)
	defer tick.Stop()
	for {
		hub.mu.Lock()
		pid := hub.backendPID
		hub.mu.Unlock()
		if pid != 0 && pid != oldPID {
			break
		}
		select {
		case <-tick.C:
		case <-deadline.C:
			t.Fatal("listener failed to reconnect within bound")
		}
	}
	authorAwaitWake(t, ctx, first)
	batch, err := s.ReplayTaskEvents(ctx, p, v.ID, "", 100)
	if err != nil || len(batch.Events) != 32 {
		t.Fatalf("coalesced/reconnected wake lost durable history: %d %v", len(batch.Events), err)
	}
}

func TestEventSubscriptionCancellationAndStoreClose(t *testing.T) {
	s, _ := testStore(t)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	p, _ := identity(t, s, 100000)
	v := task(t, s, p, agent(t, s, p))
	child, stop := context.WithCancel(ctx)
	ch, err := s.SubscribeTaskEvents(child, p.OrganizationID, v.ID)
	if err != nil {
		t.Fatal(err)
	}
	authorDrainWake(ch)
	stop()
	select {
	case _, ok := <-ch:
		if ok {
			t.Fatal("cancelled channel stayed open")
		}
	case <-ctx.Done():
		t.Fatal("cancelled subscription leaked")
	}
	ch, err = s.SubscribeTaskEvents(ctx, p.OrganizationID, v.ID)
	if err != nil {
		t.Fatal(err)
	}
	authorDrainWake(ch)
	done := make(chan struct{})
	go func() { s.Close(); close(done) }()
	select {
	case <-done:
	case <-ctx.Done():
		t.Fatal("Store.Close left the listener connection running")
	}
	if _, ok := <-ch; ok {
		t.Fatal("closed Store retained a subscription")
	}
	if _, err = s.SubscribeTaskEvents(ctx, p.OrganizationID, v.ID); err == nil {
		t.Fatal("closed Store accepted a new subscription")
	}
}
