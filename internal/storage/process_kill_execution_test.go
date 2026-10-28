//go:build unix

// SPDX-License-Identifier: Elastic-2.0
package storage

// Author fault tests kill an actual child OS process. The child executes the
// real PostgreSQL/runtime/network paths; wrappers only announce an exact durable
// boundary and stop returning so SIGKILL prevents Go cleanup from running.
// Peers are local synthetic fixtures. These tests do not prove provider billing,
// universal exactly-once effects, or full work-order acceptance.
import (
	"bytes"
	"context"
	"encoding/json"
	"encoding/pem"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
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
	"agentplatform.local/platform/internal/modelruntime"
	"agentplatform.local/platform/internal/toolruntime"
)

const processKillConfigVariable = "AP_PROCESS_KILL_EXECUTION_CONFIG"
const processKillLease = 2 * time.Second

type processKillConfig struct {
	DSN, OrganizationID, TaskID, ExecutionID, Mode, Phase, Checkpoint string
	Block                                                             bool
}
type processKillCheckpoint struct {
	ReservationID string                      `json:"reservation_id,omitempty"`
	Lease         *domain.Lease               `json:"lease,omitempty"`
	ToolLease     *domain.ToolLease           `json:"tool_lease,omitempty"`
	Reservation   *domain.Reservation         `json:"reservation,omitempty"`
	Candidate     *domain.ModelCandidate      `json:"candidate,omitempty"`
	Artifact      *domain.Artifact            `json:"artifact,omitempty"`
	Evidence      *domain.ToolOutcomeEvidence `json:"evidence,omitempty"`
	Receipt       *domain.ToolOutcomeReceipt  `json:"receipt,omitempty"`
	Claimed       bool                        `json:"claimed"`
	Outcome       string                      `json:"outcome,omitempty"`
}
type processKillBoundaryStore struct {
	*Store
	config    processKillConfig
	toolLease *domain.ToolLease
}

func (s *processKillBoundaryStore) checkpoint(ctx context.Context, v processKillCheckpoint) {
	body, err := json.Marshal(v)
	if err != nil {
		panic("checkpoint encoding failed")
	}
	if _, err = s.Pool.Exec(ctx, "INSERT INTO process_kill_test_checkpoints(id,body) VALUES($1,$2)", s.config.Checkpoint, body); err != nil {
		panic("checkpoint persistence failed")
	}
	if s.config.Block {
		select {}
	}
}
func (s *processKillBoundaryStore) ReserveCost(ctx context.Context, l *domain.Lease, in domain.ReserveCostRequest) (domain.Reservation, error) {
	r, err := s.Store.ReserveCost(ctx, l, in)
	if err == nil && s.config.Phase == "reserved" {
		s.checkpoint(context.Background(), processKillCheckpoint{Lease: l, Reservation: &r, ReservationID: r.ID, Claimed: true})
	}
	return r, err
}
func (s *processKillBoundaryStore) PrepareModelDispatch(ctx context.Context, l *domain.Lease, id string) (domain.DispatchPermit, error) {
	p, err := s.Store.PrepareModelDispatch(ctx, l, id)
	if err == nil && s.config.Phase == "marker" {
		s.checkpoint(context.Background(), processKillCheckpoint{Lease: l, ReservationID: id, Claimed: true})
	}
	return p, err
}
func (s *processKillBoundaryStore) CompleteModelCandidate(ctx context.Context, l *domain.Lease, id string, c domain.ModelCandidate) (domain.Artifact, error) {
	if s.config.Phase == "response" {
		s.checkpoint(context.Background(), processKillCheckpoint{Lease: l, ReservationID: id, Candidate: &c, Claimed: true})
	}
	a, err := s.Store.CompleteModelCandidate(ctx, l, id, c)
	if err == nil && s.config.Phase == "persisted" {
		s.checkpoint(context.Background(), processKillCheckpoint{Lease: l, ReservationID: id, Candidate: &c, Artifact: &a, Claimed: true})
	}
	return a, err
}
func (s *processKillBoundaryStore) PrepareToolDispatch(ctx context.Context, l *domain.ToolLease) (domain.ToolDispatchPermit, error) {
	if s.config.Phase == "claimed" {
		s.checkpoint(context.Background(), processKillCheckpoint{ToolLease: l, Claimed: true})
	}
	p, err := s.Store.PrepareToolDispatch(ctx, l)
	if err == nil && s.config.Phase == "marker" {
		s.checkpoint(context.Background(), processKillCheckpoint{ToolLease: l, Claimed: true})
	}
	return p, err
}
func (s *processKillBoundaryStore) RecordToolOutcome(ctx context.Context, in domain.ToolOutcomeEvidence) (domain.ToolOutcomeReceipt, error) {
	if in.Outcome != "unknown" && s.config.Phase == "response" {
		s.checkpoint(context.Background(), processKillCheckpoint{ToolLease: s.toolLease, Evidence: &in, Claimed: true})
	}
	r, err := s.Store.RecordToolOutcome(ctx, in)
	if err == nil && in.Outcome != "unknown" && s.config.Phase == "persisted" {
		s.checkpoint(context.Background(), processKillCheckpoint{ToolLease: s.toolLease, Evidence: &in, Receipt: &r, Claimed: true})
	}
	return r, err
}

func TestProcessKillExecutionChild(t *testing.T) {
	path := os.Getenv(processKillConfigVariable)
	if path == "" {
		t.Skip("subprocess helper")
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal("child config unavailable")
	}
	var cfg processKillConfig
	d := json.NewDecoder(bytes.NewReader(raw))
	d.DisallowUnknownFields()
	if d.Decode(&cfg) != nil {
		t.Fatal("invalid child config")
	}
	u, err := url.Parse(cfg.DSN)
	if err != nil || !strings.HasPrefix(u.Query().Get("search_path"), "test_") {
		t.Fatal("process kill helper requires an isolated test schema")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 45*time.Second)
	defer cancel()
	store, err := Open(ctx, cfg.DSN)
	if err != nil {
		t.Fatal("child store open failed")
	}
	defer store.Close()
	boundary := &processKillBoundaryStore{Store: store, config: cfg}
	switch cfg.Mode {
	case "model":
		l, c, err := store.ClaimModel(ctx, "process-kill-model-worker", processKillLease)
		if err != nil {
			t.Fatal("child model claim failed")
		}
		if l == nil {
			boundary.checkpoint(ctx, processKillCheckpoint{})
			return
		}
		if l.TaskID != cfg.TaskID || l.OrganizationID != cfg.OrganizationID {
			t.Fatal("child selected another task")
		}
		runtime, err := modelruntime.New(boundary, RuntimeModelConfig(c, "process-kill-synthetic-model-key"))
		if err != nil {
			t.Fatal("child model runtime unavailable")
		}
		out, err := runtime.Execute(ctx, l)
		if err != nil {
			t.Fatal("child model execution failed")
		}
		if cfg.Block {
			t.Fatal("model crash boundary not reached")
		}
		boundary.checkpoint(ctx, processKillCheckpoint{Lease: l, Claimed: true, Outcome: out.Status})
	case "tool":
		if cfg.Phase == "" {
			if _, err = store.ReconcileToolUnsent(ctx); err != nil {
				t.Fatal("child tool recovery failed")
			}
		}
		l, c, err := store.ClaimToolExecution(ctx, "process-kill-tool-worker", processKillLease)
		if err != nil {
			t.Fatal("child tool claim failed")
		}
		if l == nil {
			boundary.checkpoint(ctx, processKillCheckpoint{})
			return
		}
		if l.Execution.ID != cfg.ExecutionID || l.Execution.TaskID != cfg.TaskID || l.Execution.OrganizationID != cfg.OrganizationID {
			t.Fatal("child selected another tool action")
		}
		boundary.toolLease = l
		runtime, err := toolruntime.New(boundary, toolruntime.Config{Connection: c, PersistenceTimeout: 5 * time.Second})
		if err != nil {
			t.Fatal("child tool runtime unavailable")
		}
		out, err := runtime.Execute(ctx, l)
		if err != nil {
			t.Fatal("child tool execution failed")
		}
		if cfg.Block {
			t.Fatal("tool crash boundary not reached")
		}
		boundary.checkpoint(ctx, processKillCheckpoint{ToolLease: l, Claimed: true, Outcome: out.Status})
	default:
		t.Fatal("unsupported child mode")
	}
}

type processKillFixture struct {
	s     *Store
	dsn   string
	p     domain.Principal
	task  domain.Task
	model domain.ModelConnection
	tool  domain.ToolConnection
}

func processKillSetup(t *testing.T) *processKillFixture {
	t.Helper()
	f := &processKillFixture{}
	f.s, f.dsn = testStore(t)
	ctx := context.Background()
	f.p, _ = identity(t, f.s, 100000)
	if _, err := f.s.Pool.Exec(ctx, `CREATE TABLE process_kill_test_checkpoints(id text PRIMARY KEY,body jsonb NOT NULL,created_at timestamptz NOT NULL DEFAULT clock_timestamp());
 CREATE TABLE process_kill_test_requests(sequence bigint GENERATED ALWAYS AS IDENTITY PRIMARY KEY,kind text NOT NULL,request_id text NOT NULL,body_digest text NOT NULL,created_at timestamptz NOT NULL DEFAULT clock_timestamp());
 CREATE TABLE process_kill_test_effects(action_id text PRIMARY KEY,execution_id text NOT NULL,intent_hash text NOT NULL,arguments_digest text NOT NULL);`); err != nil {
		t.Fatal(err)
	}
	modelPeer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, err := io.ReadAll(io.LimitReader(r.Body, 65537))
		if err != nil || len(body) > 65536 || r.Header.Get("Authorization") != "Bearer process-kill-synthetic-model-key" || r.Header.Get("X-Request-ID") == "" {
			t.Error("invalid process-kill model request")
			w.WriteHeader(400)
			return
		}
		if _, err = f.s.Pool.Exec(context.Background(), "INSERT INTO process_kill_test_requests(kind,request_id,body_digest) VALUES('model',$1,$2)", r.Header.Get("X-Request-ID"), Hash(body)); err != nil {
			t.Error("model peer request observation failed")
			w.WriteHeader(500)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"id":"process-kill-model-response","choices":[{"index":0,"finish_reason":"stop","message":{"role":"assistant","content":"Unverified candidate from the process-kill fixture"}}],"usage":{"prompt_tokens":3,"completion_tokens":2,"total_tokens":5}}`)
	}))
	t.Cleanup(modelPeer.Close)
	var err error
	f.model, err = f.s.InstallModelConnection(ctx, domain.ModelConnection{ID: "process-kill-model", OrganizationID: f.p.OrganizationID, Version: 1, Model: "process-kill-model-wire", ProviderAccount: "process-kill-synthetic-account", PriceVersion: "v1", Endpoint: modelPeer.URL, CredentialEnvironment: "PLATFORM_MODEL_KEY_PROCESS_KILL", MaxChargeMicroUSD: 300, MaxInputTokens: 1000, InputMicroUSDPerMillion: 1, OutputMicroUSDPerMillion: 1, FixedMicroUSD: 7, MaxOutputTokens: 64, MaxInputBytes: 65536, MaxOutputBytes: 65536, TimeoutMilliseconds: 3000, BoundAssumptions: "Synthetic local process-kill fixture; not a provider invoice", TokenLimitField: "max_tokens", AllowInsecureLoopback: true, Active: true})
	if err != nil {
		t.Fatal(err)
	}
	toolPeer := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, err := io.ReadAll(io.LimitReader(r.Body, 4097))
		var in authorToolWire
		if err != nil || len(body) > 4096 || json.Unmarshal(body, &in) != nil || in.ExecutionID == "" || in.ActionID == "" || in.IntentHash == "" || r.Header.Get("Idempotency-Key") != in.ActionID {
			t.Error("invalid process-kill tool request")
			w.WriteHeader(400)
			return
		}
		tx, err := f.s.Pool.Begin(context.Background())
		if err != nil {
			t.Error("tool peer observation unavailable")
			w.WriteHeader(500)
			return
		}
		defer tx.Rollback(context.Background())
		if _, err = tx.Exec(context.Background(), "INSERT INTO process_kill_test_requests(kind,request_id,body_digest) VALUES('tool',$1,$2)", in.ActionID, Hash(body)); err != nil {
			t.Error(err)
			w.WriteHeader(500)
			return
		}
		if _, err = tx.Exec(context.Background(), "INSERT INTO process_kill_test_effects(action_id,execution_id,intent_hash,arguments_digest) VALUES($1,$2,$3,$4) ON CONFLICT(action_id) DO NOTHING", in.ActionID, in.ExecutionID, in.IntentHash, Hash(in.Arguments)); err != nil {
			t.Error(err)
			w.WriteHeader(500)
			return
		}
		if err = tx.Commit(context.Background()); err != nil {
			t.Error(err)
			w.WriteHeader(500)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{"execution_id": in.ExecutionID, "action_id": in.ActionID, "intent_hash": in.IntentHash, "outcome": "succeeded", "effect_reference": "synthetic-process-kill-effect:" + in.ActionID, "result": "Synthetic durable effect observed; no final billing or verification claim"})
	}))
	t.Cleanup(toolPeer.Close)
	f.tool, err = f.s.InstallToolConnection(ctx, domain.ToolConnection{Tool: domain.ToolInfo{ID: "process-kill-tool", OrganizationID: f.p.OrganizationID, Version: 1, Name: "Process-kill synthetic action", Description: "Local synthetic durable effect fixture", Effect: "write", Destination: "Explicit local TLS fixture", Provider: "process-kill-tool-account", PriceVersion: "v1", MaxChargeMicroUSD: 17, InputSchemaJSON: `{"type":"object","properties":{"value":{"type":"integer"}},"required":["value"],"additionalProperties":false}`, TimeoutSeconds: 3, MaxInputBytes: 4096, MaxOutputBytes: 4096, Active: true}, Transport: "http_json", ResultContract: "agent_platform_action_v1", Endpoint: toolPeer.URL, AllowPrivateNetwork: true, TrustedCAPEM: string(pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: toolPeer.Certificate().Raw}))})
	if err != nil {
		t.Fatal(err)
	}
	budget := int64(10000)
	raw, err := f.s.CreateAgent(ctx, f.p, domain.NewID("key"), domain.CreateAgent{Name: "Process-kill fixture agent", Instructions: "Produce an unverified bounded candidate", Model: f.model.Model, BudgetMicroUSD: &budget})
	if err != nil {
		t.Fatal(err)
	}
	var agent domain.Agent
	if json.Unmarshal(raw, &agent) != nil {
		t.Fatal("decode fixture agent")
	}
	if _, err = f.s.PublishAgent(ctx, f.p, agent.ID, domain.NewID("key"), agent.Version); err != nil {
		t.Fatal(err)
	}
	raw, err = f.s.CreateTask(ctx, f.p, domain.NewID("key"), domain.CreateTask{AgentID: agent.ID, Goal: "Exercise durable process-kill recovery", BudgetMicroUSD: budget})
	if err != nil || json.Unmarshal(raw, &f.task) != nil {
		t.Fatal("create process-kill task", err)
	}
	return f
}
func (f *processKillFixture) prepareTool(t *testing.T) domain.ToolExecutionView {
	t.Helper()
	ctx := context.Background()
	l, c, err := f.s.ClaimModel(ctx, "process-kill-parent-source", 10*time.Second)
	if err != nil || l == nil {
		t.Fatal(err)
	}
	r, err := modelruntime.New(f.s, RuntimeModelConfig(c, "process-kill-synthetic-model-key"))
	if err != nil {
		t.Fatal(err)
	}
	if _, err = r.Execute(ctx, l); err != nil {
		t.Fatal(err)
	}
	current, err := f.s.GetTask(ctx, f.p, f.task.ID)
	if err != nil {
		t.Fatal(err)
	}
	tx, err := f.s.Pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	candidate, artifact, err := loadCandidateTx(ctx, tx, f.p.OrganizationID, current.CandidateID)
	_ = tx.Rollback(ctx)
	if err != nil {
		t.Fatal(err)
	}
	raw, err := f.s.PrepareToolExecution(ctx, f.p, f.task.ID, domain.NewID("key"), domain.PrepareToolExecution{OccurrenceID: "process-kill-one-effect", ExpectedTaskVersion: current.Version, CandidateID: candidate.ID, ArtifactID: artifact.ID, ArtifactVersion: artifact.Version, ContentDigest: candidate.ContentDigest, ToolID: f.tool.Tool.ID, ToolVersion: 1, ArgumentsJSON: `{ "value" : 9007199254740993 }`, ExpiresInSeconds: 60})
	v := independentToolView(t, raw, err)
	raw, err = f.s.DecideToolExecution(ctx, f.p, f.task.ID, v.Execution.ID, domain.NewID("key"), independentToolDecision(v, "approve"))
	v = independentToolView(t, raw, err)
	raw, err = f.s.DispatchToolExecution(ctx, f.p, f.task.ID, v.Execution.ID, domain.NewID("key"), independentToolDispatch(v))
	return independentToolView(t, raw, err)
}

type processKillChild struct {
	command            *exec.Cmd
	done               chan error
	output             bytes.Buffer
	once               sync.Once
	waitErr, errorKill error
	dsn                string
}

func processKillStart(t *testing.T, f *processKillFixture, cfg processKillConfig) *processKillChild {
	t.Helper()
	cfg.DSN = f.dsn
	cfg.OrganizationID = f.p.OrganizationID
	cfg.TaskID = f.task.ID
	if cfg.Checkpoint == "" {
		cfg.Checkpoint = domain.NewID("checkpoint")
	}
	body, err := json.Marshal(cfg)
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(t.TempDir(), "child.json")
	if err = os.WriteFile(path, body, 0600); err != nil {
		t.Fatal(err)
	}
	child := &processKillChild{done: make(chan error, 1), dsn: f.dsn}
	child.command = exec.Command(os.Args[0], "-test.run=^TestProcessKillExecutionChild$", "-test.timeout=50s")
	child.command.Env = append(os.Environ(), processKillConfigVariable+"="+path)
	child.command.Stdout = &child.output
	child.command.Stderr = &child.output
	if err = child.command.Start(); err != nil {
		t.Fatal(err)
	}
	go func() { child.done <- child.command.Wait() }()
	t.Cleanup(func() { child.stop(true) })
	return child
}
func (c *processKillChild) stop(kill bool) {
	c.once.Do(func() {
		if kill {
			c.errorKill = c.command.Process.Kill()
		}
		select {
		case c.waitErr = <-c.done:
		case <-time.After(10 * time.Second):
			// Always reap before exposing output or consuming the once guard.
			// Cleanup cannot be relied on after this callback has completed.
			_ = c.command.Process.Kill()
			<-c.done
			c.waitErr = errors.New("child wait timed out; process killed and reaped")
		}
	})
}
func (c *processKillChild) kill(t *testing.T) {
	t.Helper()
	c.stop(true)
	// SIGKILL can stop an instrumented child before its race detector exits.
	// Wait has reaped the process, so the captured buffer is now stable.
	if strings.Contains(c.output.String(), "WARNING: DATA RACE") || strings.Contains(c.output.String(), "--- FAIL:") {
		t.Fatal("killed execution child reported a race or test failure")
	}
	var exited *exec.ExitError
	if c.errorKill != nil || !errors.As(c.waitErr, &exited) {
		t.Fatal("child was not killed at the announced boundary")
	}
	status, ok := exited.Sys().(syscall.WaitStatus)
	if !ok || !status.Signaled() || status.Signal() != syscall.SIGKILL {
		t.Fatal("child termination was not OS SIGKILL")
	}
}
func (c *processKillChild) wait(t *testing.T) {
	t.Helper()
	c.stop(false)
	if c.waitErr != nil {
		t.Fatal("restarted child failed: " + strings.ReplaceAll(c.output.String(), c.dsn, "[REDACTED_DSN]"))
	}
}
func processKillAwait(t *testing.T, fn func() bool) {
	t.Helper()
	deadline := time.NewTimer(12 * time.Second)
	defer deadline.Stop()
	tick := time.NewTicker(10 * time.Millisecond)
	defer tick.Stop()
	for {
		if fn() {
			return
		}
		select {
		case <-deadline.C:
			t.Fatal("durable process-kill handshake did not become true")
		case <-tick.C:
		}
	}
}
func processKillReadCheckpoint(t *testing.T, s *Store, id string) processKillCheckpoint {
	t.Helper()
	var body []byte
	processKillAwait(t, func() bool {
		attempt, cancel := context.WithTimeout(context.Background(), time.Second)
		defer cancel()
		return s.Pool.QueryRow(attempt, "SELECT body FROM process_kill_test_checkpoints WHERE id=$1", id).Scan(&body) == nil
	})
	var value processKillCheckpoint
	if json.Unmarshal(body, &value) != nil {
		t.Fatal("invalid durable child checkpoint")
	}
	return value
}
func processKillResume(t *testing.T, f *processKillFixture, mode, executionID string) processKillCheckpoint {
	t.Helper()
	id := domain.NewID("resumed")
	child := processKillStart(t, f, processKillConfig{Mode: mode, ExecutionID: executionID, Checkpoint: id})
	value := processKillReadCheckpoint(t, f.s, id)
	child.wait(t)
	return value
}
func processKillWaitLeaseExpiry(t *testing.T, f *processKillFixture, mode, id string) {
	t.Helper()
	query := "SELECT lease_until<=clock_timestamp() FROM operations WHERE organization_id=$1 AND id=$2"
	if mode == "tool" {
		query = "SELECT lease_until<=clock_timestamp() FROM tool_executions WHERE organization_id=$1 AND id=$2"
	}
	processKillAwait(t, func() bool {
		attempt, cancel := context.WithTimeout(context.Background(), time.Second)
		defer cancel()
		var expired bool
		return f.s.Pool.QueryRow(attempt, query, f.p.OrganizationID, id).Scan(&expired) == nil && expired
	})
}
func (f *processKillFixture) counts(t *testing.T, kind string) (int, int) {
	t.Helper()
	var requests, effects int
	if err := f.s.Pool.QueryRow(context.Background(), "SELECT (SELECT count(*) FROM process_kill_test_requests WHERE kind=$1),(SELECT count(*) FROM process_kill_test_effects)", kind).Scan(&requests, &effects); err != nil {
		t.Fatal(err)
	}
	return requests, effects
}

func TestProcessKillModelRuntimeRecoveryMatrix(t *testing.T) {
	for _, phase := range []string{"reserved", "marker", "response", "persisted"} {
		t.Run(phase, func(t *testing.T) {
			f := processKillSetup(t)
			checkpoint := domain.NewID("model_boundary")
			child := processKillStart(t, f, processKillConfig{Mode: "model", Phase: phase, Checkpoint: checkpoint, Block: true})
			at := processKillReadCheckpoint(t, f.s, checkpoint)
			if at.Lease == nil || at.ReservationID == "" {
				t.Fatal("model checkpoint omitted its exact lease/reservation")
			}
			var artifacts, reservations int
			if err := f.s.Pool.QueryRow(context.Background(), "SELECT (SELECT count(*) FROM artifacts WHERE organization_id=$1 AND task_id=$2),(SELECT count(*) FROM reservations WHERE organization_id=$1 AND task_id=$2)", f.p.OrganizationID, f.task.ID).Scan(&artifacts, &reservations); err != nil || reservations != 1 {
				t.Fatal("model boundary lost the single durable reservation", err)
			}
			expectedRequests := 0
			if phase == "response" || phase == "persisted" {
				expectedRequests = 1
			}
			requests, _ := f.counts(t, "model")
			if requests != expectedRequests {
				t.Fatal("peer request count did not prove the announced model boundary", requests)
			}
			if phase == "persisted" {
				if artifacts != 1 || at.Artifact == nil {
					t.Fatal("committed candidate boundary missing")
				}
			} else if artifacts != 0 {
				t.Fatal("candidate persisted before its announced boundary")
			}
			child.kill(t)
			if phase != "persisted" {
				processKillWaitLeaseExpiry(t, f, "model", at.Lease.OperationID)
			}
			if phase == "response" {
				if at.Candidate == nil {
					t.Fatal("response boundary omitted the parsed candidate")
				}
				// Use the exact valid original reservation/candidate before recovery
				// changes task state, so expiry itself must reject this old lease.
				_, err := f.s.CompleteModelCandidate(context.Background(), at.Lease, at.ReservationID, *at.Candidate)
				code(t, err, "stale_lease")
			}
			resumed := processKillResume(t, f, "model", "")
			current, err := f.s.GetTask(context.Background(), f.p, f.task.ID)
			if err != nil {
				t.Fatal(err)
			}
			switch phase {
			case "reserved":
				if !resumed.Claimed || resumed.Lease == nil || resumed.Lease.Token <= at.Lease.Token || current.Status != "waiting_input" {
					t.Fatal("proven unsent model work was lost instead of fenced and recovered")
				}
				_, err = f.s.PrepareModelDispatch(context.Background(), at.Lease, at.ReservationID)
				code(t, err, "stale_lease")
				requests, _ = f.counts(t, "model")
				if requests != 1 {
					t.Fatal("recovered model work sent more than once")
				}
			case "marker", "response":
				if resumed.Claimed || current.Status != "blocked" || current.WaitingReason != "model_outcome_unknown" || current.ReservedMicroUSD != 300 || current.SpentMicroUSD != 0 {
					t.Fatal("ambiguous model attempt was resent, lost or falsely settled")
				}
			case "persisted":
				if resumed.Claimed || current.Status != "waiting_input" || current.WaitingReason != "model_candidate_pending" {
					t.Fatal("committed model candidate did not survive process restart")
				}
				var candidateArtifact string
				if err = f.s.Pool.QueryRow(context.Background(), "SELECT artifact_id FROM task_candidates WHERE organization_id=$1 AND id=$2", f.p.OrganizationID, current.CandidateID).Scan(&candidateArtifact); err != nil || candidateArtifact != at.Artifact.ID {
					t.Fatal("restart replaced the committed artifact", err)
				}
			}
			if again := processKillResume(t, f, "model", ""); again.Claimed {
				t.Fatal("second process restart blindly repeated model work")
			}
			requests, _ = f.counts(t, "model")
			if phase == "reserved" {
				expectedRequests = 1
			}
			if requests != expectedRequests {
				t.Fatal("physical model request duplicated across restart", requests)
			}
			if err = f.s.Pool.QueryRow(context.Background(), "SELECT count(*) FROM reservations WHERE organization_id=$1 AND task_id=$2", f.p.OrganizationID, f.task.ID).Scan(&reservations); err != nil || reservations != 1 {
				t.Fatal("restart duplicated model reservation", err)
			}
			independentBalances(t, f.s, f.p.OrganizationID, []string{f.task.ID}, 300, 0)
			costs, err := f.s.Costs(context.Background(), f.p, f.task.ID, CostCursors{Limit: 100})
			if err != nil || costs.ReservedMicroUSD != 300 || costs.SpentMicroUSD != 0 || costs.UnknownMicroUSD != 300 {
				t.Fatal("model restart lost or falsely settled the full unknown monetary hold", err)
			}
		})
	}
}
func TestProcessKillToolRuntimeRecoveryMatrix(t *testing.T) {
	for _, phase := range []string{"claimed", "marker", "response", "persisted"} {
		t.Run(phase, func(t *testing.T) {
			f := processKillSetup(t)
			before := f.prepareTool(t)
			checkpoint := domain.NewID("tool_boundary")
			child := processKillStart(t, f, processKillConfig{Mode: "tool", Phase: phase, ExecutionID: before.Execution.ID, Checkpoint: checkpoint, Block: true})
			at := processKillReadCheckpoint(t, f.s, checkpoint)
			if at.ToolLease == nil {
				t.Fatal("tool checkpoint omitted its exact lease")
			}
			expectedRequests := 0
			if phase == "response" || phase == "persisted" {
				expectedRequests = 1
			}
			requests, effects := f.counts(t, "tool")
			if requests != expectedRequests || effects != expectedRequests {
				t.Fatal("durable peer observations do not match tool crash boundary", requests, effects)
			}
			child.kill(t)
			if phase != "persisted" {
				processKillWaitLeaseExpiry(t, f, "tool", before.Execution.ID)
			}
			resumed := processKillResume(t, f, "tool", before.Execution.ID)
			after, err := f.s.GetToolExecution(context.Background(), f.p, f.task.ID, before.Execution.ID)
			if err != nil {
				t.Fatal(err)
			}
			if after.Execution.IntentHash != before.Execution.IntentHash || after.Task.CurrentRunID != before.Task.CurrentRunID || after.Task.CandidateID != before.Task.CandidateID {
				t.Fatal("tool restart changed its frozen source/intent/Run")
			}
			switch phase {
			case "claimed":
				if !resumed.Claimed || resumed.ToolLease == nil || resumed.ToolLease.FencingToken <= at.ToolLease.FencingToken || after.Execution.Status != "succeeded" || after.Receipt == nil {
					t.Fatal("proven unsent tool lease failed to recover")
				}
				_, err = f.s.RecordToolOutcome(context.Background(), domain.ToolOutcomeEvidence{OrganizationID: f.p.OrganizationID, ExecutionID: before.Execution.ID, IntentHash: before.Execution.IntentHash, FencingToken: at.ToolLease.FencingToken, Outcome: "unknown"})
				code(t, err, "forbidden")
				expectedRequests = 1
			case "marker", "response":
				if resumed.Claimed || after.Execution.Status != "unknown" || after.Receipt != nil || after.Task.Status != "blocked" {
					t.Fatal("ambiguous external action was retried or lost")
				}
				if phase == "response" {
					bad := *at.Evidence
					bad.FencingToken++
					_, err = f.s.RecordToolOutcome(context.Background(), bad)
					code(t, err, "forbidden")
					receipt, err := f.s.RecordToolOutcome(context.Background(), *at.Evidence)
					if err != nil || receipt.ID == "" {
						t.Fatal("exact late factual observation could not be recovered after worker death", err)
					}
					replay, err := f.s.RecordToolOutcome(context.Background(), *at.Evidence)
					if err != nil || replay.ID != receipt.ID {
						t.Fatal("late observation replay duplicated receipt", err)
					}
				}
			case "persisted":
				if resumed.Claimed || after.Execution.Status != "succeeded" || after.Receipt == nil || at.Receipt == nil || after.Receipt.ID != at.Receipt.ID {
					t.Fatal("committed tool receipt did not survive the killed caller")
				}
			}
			if again := processKillResume(t, f, "tool", before.Execution.ID); again.Claimed {
				t.Fatal("second process restart blindly repeated an external action")
			}
			requests, effects = f.counts(t, "tool")
			if requests != expectedRequests || effects != expectedRequests {
				t.Fatal("physical request/effect duplicated after process restart", requests, effects)
			}
			independentBalances(t, f.s, f.p.OrganizationID, []string{f.task.ID}, 317, 0)
			var reservations, receipts int
			if err = f.s.Pool.QueryRow(context.Background(), "SELECT (SELECT count(*) FROM reservations WHERE organization_id=$1 AND task_id=$2),(SELECT count(*) FROM tool_outcome_receipts WHERE organization_id=$1 AND execution_id=$3)", f.p.OrganizationID, f.task.ID, before.Execution.ID).Scan(&reservations, &receipts); err != nil || reservations != 2 {
				t.Fatal("restart duplicated a reservation", err)
			}
			wantReceipts := 1
			if phase == "marker" {
				wantReceipts = 0
			}
			if receipts != wantReceipts {
				t.Fatal("receipt was lost or duplicated", receipts)
			}
		})
	}
}
