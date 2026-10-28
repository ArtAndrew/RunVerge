// SPDX-License-Identifier: Elastic-2.0
package main

import (
	"bytes"
	"context"
	"encoding/json"
	"strconv"
	"strings"
	"testing"
	"time"

	"agentplatform.local/platform/internal/domain"
)

func TestEventReplayActualHostCLIIsAuditedAndPreservesHistory(t *testing.T) {
	s, dsn, b := independentCLIDB(t)
	ctx := context.Background()
	p, err := s.Authenticate(ctx, b.Token, b.OrganizationID)
	if err != nil {
		t.Fatal(err)
	}
	raw, err := s.CreateAgent(ctx, p, "event-cli-agent", domain.CreateAgent{Name: "CLI replay fixture", Instructions: "Synthetic only"})
	if err != nil {
		t.Fatal(err)
	}
	var a domain.Agent
	if json.Unmarshal(raw, &a) != nil {
		t.Fatal("agent")
	}
	if _, err = s.PublishAgent(ctx, p, a.ID, "event-cli-publish", a.Version); err != nil {
		t.Fatal(err)
	}
	raw, err = s.CreateTask(ctx, p, "event-cli-task", domain.CreateTask{AgentID: a.ID, Goal: "Exercise host replay boundary", BudgetMicroUSD: 10000})
	if err != nil {
		t.Fatal(err)
	}
	var task domain.Task
	if json.Unmarshal(raw, &task) != nil {
		t.Fatal("task")
	}
	var head int64
	if err = s.Pool.QueryRow(ctx, "SELECT max(sequence) FROM events WHERE organization_id=$1 AND task_id=$2", b.OrganizationID, task.ID).Scan(&head); err != nil {
		t.Fatal(err)
	}
	run := func(after string, wantSuccess bool) {
		t.Helper()
		childCtx, cancel := context.WithTimeout(ctx, 8*time.Second)
		defer cancel()
		cmd := independentCLICommand(t, childCtx, dsn, "advance-event-replay", "--organization", b.OrganizationID, "--task", task.ID, "--after", after, "--reason", "Audited host fixture")
		var stdout, stderr bytes.Buffer
		cmd.Stdout = &stdout
		cmd.Stderr = &stderr
		err := cmd.Run()
		if (err == nil) != wantSuccess || childCtx.Err() != nil {
			t.Fatal("unexpected host CLI outcome", err)
		}
		if strings.Contains(stdout.String(), b.Token) || strings.Contains(stderr.String(), b.Token) || strings.Contains(stdout.String(), dsn) || strings.Contains(stderr.String(), dsn) {
			t.Fatal("host CLI exposed credentials")
		}
		if wantSuccess {
			var v domain.EventReplayWindow
			if json.Unmarshal(stdout.Bytes(), &v) != nil || v.AfterSequence != head || v.ThroughSequence != head || v.Version != 2 {
				t.Fatal("host CLI returned invalid window")
			}
		} else if stdout.Len() != 0 {
			t.Fatal("rejected host advance produced success output")
		}
	}
	run(strconv.FormatInt(head, 10), true)
	run(strconv.FormatInt(head, 10), true)
	run("0", false)
	run("01", false)
	run(strconv.FormatInt(head+1, 10), false)
	var audits, events, outbox int
	if err = s.Pool.QueryRow(ctx, "SELECT (SELECT count(*) FROM event_replay_window_audit),(SELECT count(*) FROM events),(SELECT count(*) FROM outbox)").Scan(&audits, &events, &outbox); err != nil || audits != 1 || events != 1 || outbox != 1 {
		t.Fatal("host replay changed history or duplicated audit", err)
	}
	history, _, err := s.Events(ctx, p, task.ID, 0, 100)
	if err != nil || len(history) != 1 || history[0].Sequence != head || history[0].Type != "task.created" || history[0].PayloadVersion != 1 {
		t.Fatal("logical replay advance hid or rewrote immutable full history", err)
	}
}
