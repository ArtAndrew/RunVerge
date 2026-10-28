//go:build unix

// SPDX-License-Identifier: Elastic-2.0
package worker_test

// Unlike the storage runtime matrix, both the crashing and replacement child
// processes here execute the actual Worker.Run scheduler. A test-only trigger
// waits on a parent-owned PostgreSQL advisory lock at an exact SQL boundary;
// no production deadline, lease, counter, status or migration is rewritten.
import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"agentplatform.local/platform/internal/domain"
	"agentplatform.local/platform/internal/storage"
	"agentplatform.local/platform/internal/worker"
)

const processKillWorkerConfigVariable = "AP_PROCESS_KILL_ACTUAL_WORKER_CONFIG"
const processKillAdvisoryNamespace = 2300817

type processKillWorkerConfig struct{ DSN, OrganizationID, TaskID, GrantID, Checkpoint string }

func TestProcessKillActualWorkerChild(t *testing.T) {
	path := os.Getenv(processKillWorkerConfigVariable)
	if path == "" {
		t.Skip("subprocess helper")
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal("actual worker private config unavailable")
	}
	var cfg processKillWorkerConfig
	d := json.NewDecoder(bytes.NewReader(raw))
	d.DisallowUnknownFields()
	if d.Decode(&cfg) != nil {
		t.Fatal("invalid actual worker child config")
	}
	u, err := url.Parse(cfg.DSN)
	if err != nil || !strings.HasPrefix(u.Query().Get("search_path"), "worker_test_") {
		t.Fatal("actual worker kill helper requires isolated PostgreSQL schema")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 45*time.Second)
	defer cancel()
	s, err := storage.Open(ctx, cfg.DSN)
	if err != nil {
		t.Fatal("actual worker child database unavailable")
	}
	defer s.Close()
	done := make(chan error, 1)
	w := &worker.Worker{Store: s, ID: domain.NewID("process_kill_actual_worker"), Concurrency: 1, Log: slog.New(slog.NewTextHandler(io.Discard, nil)), ResolveModelCredential: func(string) (string, error) { return "author-synthetic-automatic-model", nil }}
	go func() { done <- w.Run(ctx) }()
	tick := time.NewTicker(10 * time.Millisecond)
	defer tick.Stop()
	for {
		select {
		case <-ctx.Done():
			t.Fatal("actual worker never reached its bounded final checkpoint")
		case <-done:
			t.Fatal("actual worker exited before parent SIGKILL")
		case <-tick.C:
		}
		attempt, stop := context.WithTimeout(ctx, time.Second)
		var final bool
		err = s.Pool.QueryRow(attempt, `SELECT EXISTS(SELECT 1 FROM tasks t JOIN task_automation_grants g ON g.organization_id=t.organization_id AND g.task_id=t.id WHERE t.organization_id=$1 AND t.id=$2 AND g.id=$3 AND g.status='completed' AND t.status='waiting_input' AND t.waiting_reason='model_candidate_pending')`, cfg.OrganizationID, cfg.TaskID, cfg.GrantID).Scan(&final)
		stop()
		if err != nil || !final {
			continue
		}
		body, _ := json.Marshal(map[string]any{"task_id": cfg.TaskID, "grant_id": cfg.GrantID, "stage": "actual_worker_final_persisted"})
		if _, err = s.Pool.Exec(ctx, "INSERT INTO process_kill_worker_checkpoints(id,body) VALUES($1,$2)", cfg.Checkpoint, body); err != nil {
			t.Fatal("actual worker final checkpoint not durable")
		}
		// Keep the real scheduler alive until the parent kills and reaps this exact
		// process; a normal context return would not exercise process loss.
		select {
		case <-done:
			t.Fatal("actual worker stopped without parent SIGKILL")
		case <-ctx.Done():
			t.Fatal("actual worker parent did not kill the announced checkpoint")
		}
	}
}

type processKillWorkerChild struct {
	cmd          *exec.Cmd
	done         chan error
	output       bytes.Buffer
	once         sync.Once
	err, killErr error
	dsn          string
}

func processKillStartActualWorker(t *testing.T, f *automationWorkerFixture, application, checkpoint string) *processKillWorkerChild {
	t.Helper()
	u, err := url.Parse(f.store.Pool.Config().ConnString())
	if err != nil {
		t.Fatal("isolated worker DSN unavailable")
	}
	q := u.Query()
	q.Set("application_name", application)
	u.RawQuery = q.Encode()
	cfg := processKillWorkerConfig{DSN: u.String(), OrganizationID: f.principal.OrganizationID, TaskID: f.task.ID, GrantID: f.grant.ID, Checkpoint: checkpoint}
	body, _ := json.Marshal(cfg)
	path := filepath.Join(t.TempDir(), "actual-worker.json")
	if err = os.WriteFile(path, body, 0600); err != nil {
		t.Fatal(err)
	}
	child := &processKillWorkerChild{done: make(chan error, 1), dsn: cfg.DSN}
	child.cmd = exec.Command(os.Args[0], "-test.run=^TestProcessKillActualWorkerChild$", "-test.timeout=50s")
	child.cmd.Env = append(os.Environ(), processKillWorkerConfigVariable+"="+path)
	child.cmd.Stdout = &child.output
	child.cmd.Stderr = &child.output
	if err = child.cmd.Start(); err != nil {
		t.Fatal(err)
	}
	go func() { child.done <- child.cmd.Wait() }()
	t.Cleanup(child.stop)
	return child
}
func (c *processKillWorkerChild) stop() {
	c.once.Do(func() {
		c.killErr = c.cmd.Process.Kill()
		select {
		case c.err = <-c.done:
		case <-time.After(10 * time.Second):
			_ = c.cmd.Process.Kill()
			<-c.done
			c.err = errors.New("actual worker wait timed out; killed and reaped")
		}
	})
}
func (c *processKillWorkerChild) kill(t *testing.T) {
	t.Helper()
	c.stop()
	// A forced signal must not hide diagnostics emitted before process death.
	// stop has waited for all output-copy goroutines before this inspection.
	if strings.Contains(c.output.String(), "WARNING: DATA RACE") || strings.Contains(c.output.String(), "--- FAIL:") {
		t.Fatal("killed actual worker reported a race or test failure")
	}
	var exited *exec.ExitError
	if c.killErr != nil || !errors.As(c.err, &exited) {
		t.Fatal("actual worker did not die from parent SIGKILL: " + strings.ReplaceAll(c.output.String(), c.dsn, "[REDACTED_DSN]"))
	}
	status, ok := exited.Sys().(syscall.WaitStatus)
	if !ok || !status.Signaled() || status.Signal() != syscall.SIGKILL {
		t.Fatal("actual worker child termination was not SIGKILL")
	}
}
func processKillWorkerAwait(t *testing.T, fn func(context.Context) bool) {
	t.Helper()
	deadline := time.NewTimer(12 * time.Second)
	defer deadline.Stop()
	tick := time.NewTicker(10 * time.Millisecond)
	defer tick.Stop()
	for {
		attempt, cancel := context.WithTimeout(context.Background(), time.Second)
		ok := fn(attempt)
		cancel()
		if ok {
			return
		}
		select {
		case <-deadline.C:
			t.Fatal("actual worker PostgreSQL handshake deadline exceeded")
		case <-tick.C:
		}
	}
}

type processKillWorkerSnapshot struct {
	ModelTurns, QueuedContinuation, ToolExecutions, Receipts, AutoModelTurns, AutoToolExecutions int
	Allocated                                                                                    int64
	TaskStatus, TaskReason, RunID                                                                string
}

func processKillReadWorkerSnapshot(t *testing.T, f *automationWorkerFixture) processKillWorkerSnapshot {
	t.Helper()
	var v processKillWorkerSnapshot
	err := f.store.Pool.QueryRow(context.Background(), `SELECT
 (SELECT count(*) FROM model_turns WHERE organization_id=$1 AND task_id=$2),
 (SELECT count(*) FROM operations o JOIN model_turns m ON m.organization_id=o.organization_id AND m.operation_id=o.id WHERE o.organization_id=$1 AND o.task_id=$2 AND m.ordinal=2 AND o.status='queued'),
 (SELECT count(*) FROM tool_executions WHERE organization_id=$1 AND task_id=$2),
 (SELECT count(*) FROM tool_outcome_receipts r JOIN tool_executions e ON e.organization_id=r.organization_id AND e.id=r.execution_id WHERE e.organization_id=$1 AND e.task_id=$2),
 (SELECT model_turns FROM task_automation_usage WHERE organization_id=$1 AND task_id=$2),
 (SELECT tool_executions FROM task_automation_usage WHERE organization_id=$1 AND task_id=$2),
 (SELECT allocated_microusd FROM task_automation_usage WHERE organization_id=$1 AND task_id=$2),status,waiting_reason,current_run_id FROM tasks WHERE organization_id=$1 AND id=$2`, f.principal.OrganizationID, f.task.ID).Scan(&v.ModelTurns, &v.QueuedContinuation, &v.ToolExecutions, &v.Receipts, &v.AutoModelTurns, &v.AutoToolExecutions, &v.Allocated, &v.TaskStatus, &v.TaskReason, &v.RunID)
	if err != nil {
		t.Fatal("read actual worker boundary snapshot", err)
	}
	return v
}
func TestProcessKillActualWorkerNativeContinuationCommitBoundaries(t *testing.T) {
	for _, phase := range []string{"before_continuation_commit", "after_continuation_commit_before_claim"} {
		t.Run(phase, func(t *testing.T) {
			f := newAutomationWorkerFixture(t, "read", "read_only", false, false)
			ctx := context.Background()
			if _, err := f.store.Pool.Exec(ctx, "CREATE TABLE process_kill_worker_checkpoints(id text PRIMARY KEY,body jsonb NOT NULL,created_at timestamptz NOT NULL DEFAULT clock_timestamp())"); err != nil {
				t.Fatal(err)
			}
			// The lock is transaction-scoped and belongs to this isolated test parent.
			// Advisory locks are database-wide, so choose an independent random key;
			// no elapsed-time fault injection is involved.
			var randomKey [4]byte
			if _, err := rand.Read(randomKey[:]); err != nil {
				t.Fatal("create isolated advisory lock key", err)
			}
			key := int(binary.BigEndian.Uint32(randomKey[:]) & 0x3fffffff)
			barrier, err := f.store.Pool.Begin(ctx)
			if err != nil {
				t.Fatal(err)
			}
			defer barrier.Rollback(context.Background())
			if _, err = barrier.Exec(ctx, "SELECT pg_advisory_xact_lock($1,$2)", processKillAdvisoryNamespace, key); err != nil {
				t.Fatal(err)
			}
			functionBody := ""
			triggerDDL := ""
			if phase == "before_continuation_commit" {
				functionBody = fmt.Sprintf(`CREATE FUNCTION process_kill_pause_execution() RETURNS trigger LANGUAGE plpgsql AS $$ BEGIN IF NEW.ordinal=2 THEN PERFORM pg_advisory_xact_lock(%d,%d); END IF; RETURN NEW; END $$`, processKillAdvisoryNamespace, key)
				triggerDDL = "CREATE TRIGGER process_kill_pause BEFORE INSERT ON model_turns FOR EACH ROW EXECUTE FUNCTION process_kill_pause_execution()"
			} else {
				functionBody = fmt.Sprintf(`CREATE FUNCTION process_kill_pause_execution() RETURNS trigger LANGUAGE plpgsql AS $$ BEGIN IF NEW.status='leased' AND OLD.status='queued' AND EXISTS(SELECT 1 FROM model_turns m WHERE m.organization_id=NEW.organization_id AND m.operation_id=NEW.id AND m.ordinal=2) THEN PERFORM pg_advisory_xact_lock(%d,%d); END IF; RETURN NEW; END $$`, processKillAdvisoryNamespace, key)
				triggerDDL = "CREATE TRIGGER process_kill_pause BEFORE UPDATE ON operations FOR EACH ROW EXECUTE FUNCTION process_kill_pause_execution()"
			}
			if _, err = f.store.Pool.Exec(ctx, functionBody); err != nil {
				t.Fatal(err)
			}
			if _, err = f.store.Pool.Exec(ctx, triggerDDL); err != nil {
				t.Fatal(err)
			}
			application := domain.NewID("kill_worker")
			first := processKillStartActualWorker(t, f, application, domain.NewID("never_final_before_kill"))
			var blockedQuery string
			var backendPID int
			processKillWorkerAwait(t, func(attempt context.Context) bool {
				return f.store.Pool.QueryRow(attempt, `SELECT a.pid,a.query FROM pg_stat_activity a JOIN pg_locks l ON l.pid=a.pid WHERE a.application_name=$1 AND l.locktype='advisory' AND NOT l.granted AND l.classid=$2::oid AND l.objid=$3::oid AND l.objsubid=2`, application, processKillAdvisoryNamespace, key).Scan(&backendPID, &blockedQuery) == nil
			})
			if phase == "before_continuation_commit" && !strings.Contains(blockedQuery, "INSERT INTO model_turns") {
				t.Fatal("worker did not block at the exact continuation insert")
			}
			if phase != "before_continuation_commit" && !strings.Contains(blockedQuery, "UPDATE operations SET status='leased'") {
				t.Fatal("worker did not block at the exact next-operation claim")
			}
			before := processKillReadWorkerSnapshot(t, f)
			if before.ToolExecutions != 1 || before.Receipts != 1 || before.AutoToolExecutions != 1 || before.RunID != f.task.CurrentRunID || f.modelCalls.Load() != 1 || f.toolCalls.Load() != 1 {
				t.Fatal("actual native worker did not reach a durable registered-tool boundary", before)
			}
			if phase == "before_continuation_commit" {
				if before.ModelTurns != 1 || before.QueuedContinuation != 0 || before.AutoModelTurns != 0 || before.Allocated != 17 || before.TaskStatus != "waiting_input" {
					t.Fatal("uncommitted continuation leaked into durable state", before)
				}
			} else {
				if before.ModelTurns != 2 || before.QueuedContinuation != 1 || before.AutoModelTurns != 1 || before.Allocated != 1017 || before.TaskStatus != "queued" {
					t.Fatal("committed continuation missing before its killed claimant", before)
				}
			}
			body, _ := json.Marshal(map[string]any{"stage": phase, "observed_backend_pid": backendPID, "snapshot": before, "model_requests": f.modelCalls.Load(), "tool_requests": f.toolCalls.Load(), "fault": "parent_SIGKILL_after_postgresql_lock_handshake"})
			if _, err = f.store.Pool.Exec(ctx, "INSERT INTO process_kill_worker_checkpoints(id,body) VALUES($1,$2)", "parent-boundary", body); err != nil {
				t.Fatal(err)
			}
			first.kill(t)
			// Release only the test barrier, then PostgreSQL can observe the dead
			// connection and roll back its unfinished transaction before restart.
			if err = barrier.Rollback(ctx); err != nil {
				t.Fatal(err)
			}
			relation := "model_turns"
			if phase != "before_continuation_commit" {
				relation = "operations"
			}
			ddlCtx, stop := context.WithTimeout(ctx, 5*time.Second)
			_, err = f.store.Pool.Exec(ddlCtx, "DROP TRIGGER process_kill_pause ON "+relation+"; DROP FUNCTION process_kill_pause_execution()")
			stop()
			if err != nil {
				t.Fatal("test barrier did not release after process death", err)
			}
			afterCrash := processKillReadWorkerSnapshot(t, f)
			if afterCrash.ModelTurns != before.ModelTurns || afterCrash.QueuedContinuation != before.QueuedContinuation || afterCrash.AutoModelTurns != before.AutoModelTurns || afterCrash.Allocated != before.Allocated {
				t.Fatal("dead child committed or lost a continuation during rollback", before, afterCrash)
			}
			finalCheckpoint := domain.NewID("worker_final")
			replacement := processKillStartActualWorker(t, f, domain.NewID("kill_worker_restart"), finalCheckpoint)
			processKillWorkerAwait(t, func(attempt context.Context) bool {
				var found bool
				return f.store.Pool.QueryRow(attempt, "SELECT EXISTS(SELECT 1 FROM process_kill_worker_checkpoints WHERE id=$1)", finalCheckpoint).Scan(&found) == nil && found
			})
			replacement.kill(t)
			current, err := f.store.GetTask(ctx, f.principal, f.task.ID)
			if err != nil || current.Status != "waiting_input" || current.WaitingReason != "model_candidate_pending" || current.CurrentRunID != f.task.CurrentRunID || current.AcceptedDecisionID != "" {
				t.Fatal("actual worker restart lost the Run or accepted its own final answer", err)
			}
			state := f.state(t)
			if state.Grant.ID != f.grant.ID || state.Grant.Status != "completed" || state.Usage.ModelTurns != 1 || state.Usage.ToolExecutions != 1 || state.Usage.AllocatedMicroUSD != 1017 {
				t.Fatal("native continuation reused another consent or reset allocation", state.Usage)
			}
			if f.modelCalls.Load() != 2 || f.toolCalls.Load() != 1 || current.ReservedMicroUSD != 2017 || current.SpentMicroUSD != 0 {
				t.Fatal("actual worker restart duplicated physical work or reset prior liabilities", f.modelCalls.Load(), f.toolCalls.Load(), current.ReservedMicroUSD)
			}
			var rawMessages, digest, grantID string
			var turns, reservations int
			if err = f.store.Pool.QueryRow(ctx, `SELECT m.messages_json,m.messages_digest,m.automation_grant_id,(SELECT count(*) FROM model_turns WHERE organization_id=$1 AND task_id=$2),(SELECT count(*) FROM reservations WHERE organization_id=$1 AND task_id=$2) FROM model_turns m WHERE m.organization_id=$1 AND m.task_id=$2 AND m.ordinal=2`, f.principal.OrganizationID, f.task.ID).Scan(&rawMessages, &digest, &grantID, &turns, &reservations); err != nil || grantID != f.grant.ID || digest != storage.Hash([]byte(rawMessages)) || turns != 2 || reservations != 3 {
				t.Fatal("native transcript/operation/reservation binding changed across killed coordinator", err)
			}
			var messages []domain.ModelMessage
			if json.Unmarshal([]byte(rawMessages), &messages) != nil || len(messages) != 4 || messages[2].Role != "assistant" || messages[3].Role != "tool" {
				t.Fatal("restart did not reconstruct the exact native context")
			}
			var quoted struct {
				ExecutionID, ProposalID string
				Untrusted               bool
				Receipt                 *domain.ToolOutcomeReceipt
			}
			// The durable wire names are snake_case, so decode the two identifiers
			// explicitly rather than treating generated model text as authority.
			var exact map[string]json.RawMessage
			if json.Unmarshal([]byte(messages[3].Content), &exact) != nil {
				t.Fatal("invalid quoted tool evidence")
			}
			_ = json.Unmarshal(exact["execution_id"], &quoted.ExecutionID)
			_ = json.Unmarshal(exact["proposal_id"], &quoted.ProposalID)
			_ = json.Unmarshal(exact["untrusted"], &quoted.Untrusted)
			_ = json.Unmarshal(exact["receipt"], &quoted.Receipt)
			execution := f.execution(t)
			if quoted.ExecutionID != execution.Execution.ID || quoted.ProposalID != execution.Execution.ModelProposalID || !quoted.Untrusted || quoted.Receipt == nil || quoted.Receipt.ID != execution.Receipt.ID || quoted.Receipt.Method != "registered_tool_response" {
				t.Fatal("restart lost exact observation identity or promoted tool text")
			}
		})
	}
}
