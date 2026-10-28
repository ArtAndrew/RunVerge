// SPDX-License-Identifier: Elastic-2.0
package storage

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"agentplatform.local/platform/internal/domain"
	"agentplatform.local/platform/internal/modelruntime"
)

func teamAgent(t *testing.T, s *Store, p domain.Principal, pattern, join string, parallel int, members ...domain.TeamMember) domain.Agent {
	t.Helper()
	definition := &domain.TeamDefinition{Pattern: pattern, Members: members, JoinPolicy: join, MaxDepth: 16, MaxChildren: len(members), MaxParallel: parallel}
	budget := int64(1_000_000)
	raw, err := s.CreateAgent(context.Background(), p, domain.NewID("key"), domain.CreateAgent{Name: "Team fixture", Instructions: "Perform the bounded member plan", Model: domain.TeamModel, Team: definition, BudgetMicroUSD: &budget})
	if err != nil {
		t.Fatal(err)
	}
	var result domain.Agent
	if err = json.Unmarshal(raw, &result); err != nil {
		t.Fatal(err)
	}
	raw, err = s.PublishAgent(context.Background(), p, result.ID, domain.NewID("key"), result.Version)
	if err != nil {
		t.Fatal(err)
	}
	if err = json.Unmarshal(raw, &result); err != nil {
		t.Fatal(err)
	}
	return result
}
func teamMember(id string, a domain.Agent) domain.TeamMember {
	return domain.TeamMember{ID: id, AgentID: a.ID, AgentVersion: a.Version}
}

// Advancing the poll eligibility clock is a test scheduler convenience. It
// neither changes task deadlines nor bypasses an execution/admission check.
func teamTick(t *testing.T, s *Store) {
	t.Helper()
	if _, err := s.Pool.Exec(context.Background(), "UPDATE team_executions SET next_check_at=clock_timestamp() WHERE status='running'"); err != nil {
		t.Fatal(err)
	}
	if _, err := s.ReconcileTeam(context.Background()); err != nil {
		t.Fatal(err)
	}
}
func finishOneTeamFixture(t *testing.T, s *Store) bool {
	t.Helper()
	lease, err := s.ClaimFixture(context.Background(), "team-test-worker", 30*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	if lease == nil {
		return false
	}
	if err = s.CompleteFixture(context.Background(), lease, "Тестовый результат\n"+domain.ExecutionInput(*lease)); err != nil {
		t.Fatal(err)
	}
	return true
}
func driveFixtureTeam(t *testing.T, s *Store, p domain.Principal, rootID string) domain.Task {
	t.Helper()
	for i := 0; i < 512; i++ {
		teamTick(t, s)
		finishOneTeamFixture(t, s)
		root, err := s.GetTask(context.Background(), p, rootID)
		if err != nil {
			t.Fatal(err)
		}
		if root.CandidateID != "" || root.Status == "blocked" || domain.Terminal(root.Status) {
			return root
		}
	}
	t.Fatal("bounded fixture team did not reach a stable result")
	return domain.Task{}
}

func TestTeamNestedSequencePinsInputsAndSharedCosts(t *testing.T) {
	s, _ := testStore(t)
	p, _ := identity(t, s, 1_000_000)
	leaf := agent(t, s, p)
	inner := teamAgent(t, s, p, "parallel", "all", 2, teamMember("left", leaf), teamMember("right", leaf))
	rootAgent := teamAgent(t, s, p, "sequence", "all", 1, teamMember("draft", inner), teamMember("verify", leaf))
	root := task(t, s, p, rootAgent)
	children, _, err := s.Children(context.Background(), p, root.ID, "", 100)
	if err != nil || len(children) != 2 {
		t.Fatalf("eager direct children: %d %v", len(children), err)
	}
	var initialTasks, initialRuns int
	if err = s.Pool.QueryRow(context.Background(), "SELECT used_tasks,used_attempts FROM task_tree_controls WHERE organization_id=$1 AND root_id=$2", p.OrganizationID, root.ID).Scan(&initialTasks, &initialRuns); err != nil {
		t.Fatal(err)
	}
	if initialTasks != 5 || initialRuns != 1 {
		t.Fatalf("eager tree=%d runs=%d", initialTasks, initialRuns)
	}
	root = driveFixtureTeam(t, s, p, root.ID)
	if root.Status != "waiting_input" || root.CandidateID == "" || root.Result != "" || root.SpentMicroUSD != 3*domain.FixtureCost || root.ReservedMicroUSD != 0 {
		t.Fatalf("unexpected root: status=%s spent=%d reserved=%d reason=%s", root.Status, root.SpentMicroUSD, root.ReservedMicroUSD, root.WaitingReason)
	}
	var contextBody, sourceCandidate string
	if err = s.Pool.QueryRow(context.Background(), `SELECT r.input_context,r.source_candidate_id FROM team_members m JOIN runs r ON r.organization_id=m.organization_id AND r.id=m.child_run_id WHERE m.organization_id=$1 AND m.parent_run_id=$2 AND m.member_id='verify'`, p.OrganizationID, root.CurrentRunID).Scan(&contextBody, &sourceCandidate); err != nil {
		t.Fatal(err)
	}
	var input domain.RunInput
	if err = json.Unmarshal([]byte(contextBody), &input); err != nil {
		t.Fatal(err)
	}
	if len(input.Sources) != 1 || input.Sources[0].CandidateID != sourceCandidate || input.Sources[0].ContentDigest != Hash([]byte(input.Sources[0].Content)) {
		t.Fatal("nested predecessor is not exact immutable input")
	}
	var manifest teamManifest
	if err = json.Unmarshal([]byte(input.Sources[0].Content), &manifest); err != nil || manifest.Pattern != "parallel" || !manifest.AllFixture {
		t.Fatal("verifier did not receive nested team result")
	}
	artifacts, err := s.Artifacts(context.Background(), p, root.ID)
	if err != nil || len(artifacts) != 1 {
		t.Fatal("team artifact missing")
	}
	if artifacts[0].Provenance["verified"] != false || artifacts[0].Provenance["synthetic_cost"] != true {
		t.Fatal("team artifact overstates verification or billing")
	}
	var charges int
	if err = s.Pool.QueryRow(context.Background(), "SELECT count(*) FROM reservations WHERE organization_id=$1 AND root_id=$2", p.OrganizationID, root.ID).Scan(&charges); err != nil || charges != 3 {
		t.Fatalf("physical charge count %d: %v", charges, err)
	}
}

func TestTeamConcurrentTicksDeduplicateAndBoundParallel(t *testing.T) {
	s, dsn := testStore(t)
	p, _ := identity(t, s, 1_000_000)
	leaf := agent(t, s, p)
	a := teamAgent(t, s, p, "parallel", "all", 1, teamMember("one", leaf), teamMember("two", leaf), teamMember("three", leaf))
	root := task(t, s, p, a)
	var group sync.WaitGroup
	failures := make(chan error, 12)
	for i := 0; i < 12; i++ {
		group.Add(1)
		go func() { defer group.Done(); _, err := s.ReconcileTeam(context.Background()); failures <- err }()
	}
	group.Wait()
	close(failures)
	for err := range failures {
		if err != nil {
			t.Fatal(err)
		}
	}
	for i := 0; i < 4; i++ {
		teamTick(t, s)
	}
	var active int
	if err := s.Pool.QueryRow(context.Background(), "SELECT count(*) FROM team_members WHERE organization_id=$1 AND parent_run_id=$2 AND child_run_id IS NOT NULL", p.OrganizationID, root.CurrentRunID).Scan(&active); err != nil || active != 1 {
		t.Fatalf("parallel limit: %d %v", active, err)
	}
	reopened, err := Open(context.Background(), dsn)
	if err != nil {
		t.Fatal(err)
	}
	defer reopened.Close()
	root = driveFixtureTeam(t, reopened, p, root.ID)
	if root.CandidateID == "" || root.SpentMicroUSD != 3000 {
		t.Fatalf("reopened coordinator: %s %s", root.Status, root.WaitingReason)
	}
	var total, distinct int
	if err = s.Pool.QueryRow(context.Background(), "SELECT count(*),count(DISTINCT child_task_id) FROM team_members WHERE organization_id=$1 AND parent_run_id=$2", p.OrganizationID, root.CurrentRunID).Scan(&total, &distinct); err != nil || total != 3 || distinct != 3 {
		t.Fatalf("duplicate child: %d/%d %v", total, distinct, err)
	}
}

func TestTeamQueueCapacityAndTreeLimitAreAtomic(t *testing.T) {
	s, _ := testStore(t)
	p, _ := identity(t, s, 1_000_000)
	leaf := agent(t, s, p)
	inner := teamAgent(t, s, p, "parallel", "all", 2, teamMember("one", leaf), teamMember("two", leaf))
	a := teamAgent(t, s, p, "parallel", "all", 2, teamMember("nested", inner), teamMember("other", leaf))
	if _, err := s.Pool.Exec(context.Background(), "UPDATE organizations SET pending_task_limit=4 WHERE id=$1", p.OrganizationID); err != nil {
		t.Fatal(err)
	}
	_, err := s.CreateTask(context.Background(), p, domain.NewID("key"), domain.CreateTask{Goal: "Queue capacity", AgentID: a.ID, BudgetMicroUSD: 10000})
	code(t, err, "unavailable")
	var count int
	if err = s.Pool.QueryRow(context.Background(), "SELECT count(*) FROM tasks WHERE organization_id=$1", p.OrganizationID).Scan(&count); err != nil || count != 0 {
		t.Fatal("rejected tree partially persisted")
	}
	if _, err = s.Pool.Exec(context.Background(), "UPDATE organizations SET pending_task_limit=1000 WHERE id=$1", p.OrganizationID); err != nil {
		t.Fatal(err)
	}
	members := []domain.TeamMember{}
	for i := 0; i < 32; i++ {
		members = append(members, teamMember(fmt.Sprintf("leaf-%d", i), leaf))
	}
	wide := teamAgent(t, s, p, "parallel", "all", 32, members...)
	raw, err := s.CreateAgent(context.Background(), p, domain.NewID("key"), domain.CreateAgent{Name: "Oversized draft", Instructions: "Bounded plan", Team: &domain.TeamDefinition{Pattern: "parallel", JoinPolicy: "all", MaxDepth: 16, MaxChildren: 4, MaxParallel: 4, Members: []domain.TeamMember{teamMember("a", wide), teamMember("b", wide), teamMember("c", wide), teamMember("d", wide)}}})
	if err != nil {
		t.Fatal(err)
	}
	var outer domain.Agent
	if err = json.Unmarshal(raw, &outer); err != nil {
		t.Fatal(err)
	}
	_, err = s.PublishAgent(context.Background(), p, outer.ID, domain.NewID("key"), outer.Version)
	code(t, err, "invalid_request")
	if err = s.Pool.QueryRow(context.Background(), "SELECT count(*) FROM tasks WHERE organization_id=$1", p.OrganizationID).Scan(&count); err != nil || count != 0 {
		t.Fatal("oversized tree partially persisted")
	}
}

func TestTeamCancellationStopsChildrenPreservesLateCost(t *testing.T) {
	s, _ := testStore(t)
	p, _ := identity(t, s, 1_000_000)
	leaf := agent(t, s, p)
	a := teamAgent(t, s, p, "parallel", "all", 2, teamMember("one", leaf), teamMember("two", leaf))
	root := task(t, s, p, a)
	teamTick(t, s)
	lease, err := s.ClaimFixture(context.Background(), "late-fixture", 30*time.Second)
	if err != nil || lease == nil {
		t.Fatal("child lease missing")
	}
	if _, err = s.CancelTask(context.Background(), p, root.ID, domain.NewID("key")); err != nil {
		t.Fatal(err)
	}
	err = s.CompleteFixture(context.Background(), lease, "Тестовый результат\n"+domain.ExecutionInput(*lease))
	code(t, err, "stale_lease")
	if reconciled, err := s.ReconcileFixtureReservation(context.Background()); err != nil || !reconciled {
		t.Fatalf("trusted fixture reconciliation: %v", err)
	}
	teamTick(t, s)
	if next, err := s.ClaimFixture(context.Background(), "after-cancel", time.Second); err != nil || next != nil {
		t.Fatal("cancelled descendant claimed")
	}
	root, err = s.GetTask(context.Background(), p, root.ID)
	if err != nil {
		t.Fatal(err)
	}
	if root.Status != "cancelled" || root.Result != "" || root.SpentMicroUSD != 1000 || root.ReservedMicroUSD != 0 {
		t.Fatal("cancellation lost late accounting or resurrected result")
	}
	var artifacts int
	if err = s.Pool.QueryRow(context.Background(), "SELECT count(*) FROM artifacts WHERE organization_id=$1", p.OrganizationID).Scan(&artifacts); err != nil || artifacts != 0 {
		t.Fatal("late cancelled child published a result")
	}
}

func TestTeamRealHTTPSequenceRetainsUnknownExposure(t *testing.T) {
	s, _ := testStore(t)
	p, _ := identity(t, s, 1_000_000)
	var mu sync.Mutex
	inputs := []domain.RunInput{}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var request struct {
			Messages []struct {
				Role    string `json:"role"`
				Content string `json:"content"`
			} `json:"messages"`
		}
		if err := json.NewDecoder(r.Body).Decode(&request); err != nil || len(request.Messages) != 2 {
			t.Error("invalid HTTP fixture request")
			w.WriteHeader(400)
			return
		}
		var input domain.RunInput
		if err := json.Unmarshal([]byte(request.Messages[1].Content), &input); err != nil {
			t.Error("missing immutable run input")
			w.WriteHeader(400)
			return
		}
		mu.Lock()
		inputs = append(inputs, input)
		n := len(inputs)
		mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{"id": fmt.Sprintf("local-%d", n), "choices": []any{map[string]any{"message": map[string]any{"role": "assistant", "content": fmt.Sprintf("Unverified local output %d", n)}, "finish_reason": "stop"}}, "usage": map[string]int{"prompt_tokens": 3, "completion_tokens": 2, "total_tokens": 5}})
	}))
	defer server.Close()
	c := domain.ModelConnection{ID: "team-http", OrganizationID: p.OrganizationID, Version: 1, Model: "team-http-fixture", ProviderAccount: "team-test-account", PriceVersion: "test-v1", Endpoint: server.URL, CredentialEnvironment: "PLATFORM_MODEL_KEY_TEAM", MaxChargeMicroUSD: 150, MaxInputTokens: 100, MaxOutputTokens: 16, InputMicroUSDPerMillion: 1_000_000, OutputMicroUSDPerMillion: 2_000_000, FixedMicroUSD: 7, MaxInputBytes: 32768, MaxOutputBytes: 1024, TimeoutMilliseconds: 2000, BoundAssumptions: "Local test fixture, not final provider billing", TokenLimitField: "max_tokens", AllowInsecureLoopback: true, Active: true}
	var err error
	c, err = s.InstallModelConnection(context.Background(), c)
	if err != nil {
		t.Fatal(err)
	}
	budget := int64(10000)
	raw, err := s.CreateAgent(context.Background(), p, domain.NewID("key"), domain.CreateAgent{Name: "HTTP fixture", Instructions: "Return one bounded test candidate", Model: c.Model, BudgetMicroUSD: &budget})
	if err != nil {
		t.Fatal(err)
	}
	var leaf domain.Agent
	if err = json.Unmarshal(raw, &leaf); err != nil {
		t.Fatal(err)
	}
	if _, err = s.PublishAgent(context.Background(), p, leaf.ID, domain.NewID("key"), leaf.Version); err != nil {
		t.Fatal(err)
	}
	first, second := teamMember("writer", leaf), teamMember("reviewer", leaf)
	first.Task = "Draft"
	second.Task = "Review the exact draft"
	a := teamAgent(t, s, p, "delegate_verify", "all", 1, first, second)
	root := task(t, s, p, a)
	for i := 0; i < 32; i++ {
		teamTick(t, s)
		lease, connection, err := s.ClaimModel(context.Background(), "http-team-worker", 30*time.Second)
		if err != nil {
			t.Fatal(err)
		}
		if lease != nil {
			runtime, err := modelruntime.New(s, RuntimeModelConfig(connection, "local-test-key"))
			if err != nil {
				t.Fatal(err)
			}
			out, err := runtime.Execute(context.Background(), lease)
			if err != nil || out.Status != modelruntime.CandidateWaitingInput {
				t.Fatalf("HTTP candidate %s: %v", out.Status, err)
			}
		}
		root, err = s.GetTask(context.Background(), p, root.ID)
		if err != nil {
			t.Fatal(err)
		}
		if root.CandidateID != "" {
			break
		}
	}
	mu.Lock()
	captured := append([]domain.RunInput(nil), inputs...)
	mu.Unlock()
	if len(captured) != 2 || len(captured[1].Sources) != 1 || captured[1].Sources[0].Content != "Unverified local output 1" || captured[1].LocalGoal != second.Task {
		t.Fatal("verifier HTTP input did not bind exact writer candidate")
	}
	if root.Status != "waiting_input" || root.ReservedMicroUSD != 300 || root.SpentMicroUSD != 0 || root.CandidateID == "" {
		t.Fatalf("unknown exposure changed: %s %s held=%d spent=%d", root.Status, root.WaitingReason, root.ReservedMicroUSD, root.SpentMicroUSD)
	}
	artifacts, err := s.Artifacts(context.Background(), p, root.ID)
	if err != nil || len(artifacts) != 1 {
		t.Fatal("join artifact missing")
	}
	if artifacts[0].Provenance["verified"] != false || artifacts[0].Provenance["all_fixture"] != false || !strings.Contains(artifacts[0].Content, "Unverified local output 2") {
		t.Fatal("model verification overstates result or drops verifier text")
	}
	var consumedChild string
	if err = s.Pool.QueryRow(context.Background(), "SELECT child_task_id FROM team_members WHERE organization_id=$1 AND parent_run_id=$2 AND member_id='writer' AND selected_candidate_id IS NOT NULL", p.OrganizationID, root.CurrentRunID).Scan(&consumedChild); err != nil {
		t.Fatal(err)
	}
	_, err = s.CancelTask(context.Background(), p, consumedChild, domain.NewID("key"))
	code(t, err, "conflict")
	var candidate domain.TaskCandidate
	for _, candidate = range mustTeamCandidates(t, s, p, root.ID) {
		if candidate.ID == root.CandidateID {
			break
		}
	}
	_, err = s.ReviewTask(context.Background(), p, root.ID, domain.NewID("key"), domain.ReviewTaskRequest{ExpectedVersion: root.Version, CandidateID: candidate.ID, ArtifactID: candidate.ArtifactID, ArtifactVersion: candidate.ArtifactVersion, ContentDigest: candidate.ContentDigest, Decision: "accept"})
	if err != nil {
		t.Fatal(err)
	}
	root, err = s.GetTask(context.Background(), p, root.ID)
	if err != nil {
		t.Fatal(err)
	}
	if root.Status != "succeeded" || root.ReservedMicroUSD != 300 || root.SpentMicroUSD != 0 {
		t.Fatal("authorized decision released unknown model cost")
	}
	var pending, inherited int
	if err = s.Pool.QueryRow(context.Background(), "SELECT count(*) FROM tasks WHERE organization_id=$1 AND root_id=$2 AND status NOT IN('succeeded','failed','cancelled')", p.OrganizationID, root.ID).Scan(&pending); err != nil || pending != 0 {
		t.Fatal("accepted team leaves pending children")
	}
	if err = s.Pool.QueryRow(context.Background(), "SELECT count(*) FROM task_decisions WHERE organization_id=$1 AND parent_decision_id IS NOT NULL", p.OrganizationID).Scan(&inherited); err != nil || inherited != 2 {
		t.Fatal("inherited decisions not durably attributed")
	}
}

func mustTeamCandidates(t *testing.T, s *Store, p domain.Principal, id string) []domain.TaskCandidate {
	t.Helper()
	c, _, err := s.Candidates(context.Background(), p, id, "", 100)
	if err != nil {
		t.Fatal(err)
	}
	return c
}

func TestTeamRootRetryPreservesLifetimeCountersAndPriorCosts(t *testing.T) {
	s, _ := testStore(t)
	p, _ := identity(t, s, 1_000_000)
	leaf := agent(t, s, p)
	a := teamAgent(t, s, p, "parallel", "all", 2, teamMember("left", leaf), teamMember("right", leaf))
	root := task(t, s, p, a)
	for attempt := 1; attempt <= 3; attempt++ {
		root = driveFixtureTeam(t, s, p, root.ID)
		if root.CandidateID == "" || root.SpentMicroUSD != int64(attempt)*2000 {
			t.Fatalf("attempt%d status=%s reason=%s spent=%d", attempt, root.Status, root.WaitingReason, root.SpentMicroUSD)
		}
		var usedTasks, usedAttempts int
		if err := s.Pool.QueryRow(context.Background(), "SELECT used_tasks,used_attempts FROM task_tree_controls WHERE organization_id=$1 AND root_id=$2", p.OrganizationID, root.ID).Scan(&usedTasks, &usedAttempts); err != nil {
			t.Fatal(err)
		}
		if usedTasks != 1+2*attempt || usedAttempts != 3*attempt {
			t.Fatalf("counters reset: tasks=%d runs=%d", usedTasks, usedAttempts)
		}
		var selected domain.TaskCandidate
		for _, c := range mustTeamCandidates(t, s, p, root.ID) {
			if c.ID == root.CandidateID {
				selected = c
			}
		}
		_, err := s.ReviewTask(context.Background(), p, root.ID, domain.NewID("key"), domain.ReviewTaskRequest{ExpectedVersion: root.Version, CandidateID: selected.ID, ArtifactID: selected.ArtifactID, ArtifactVersion: selected.ArtifactVersion, ContentDigest: selected.ContentDigest, Decision: "request_changes", Reason: "Bounded fixture revision"})
		if err != nil {
			t.Fatal(err)
		}
		beforeRun := root.CurrentRunID
		root, err = s.GetTask(context.Background(), p, root.ID)
		if err != nil {
			t.Fatal(err)
		}
		raw, err := s.RetryTask(context.Background(), p, root.ID, domain.NewID("key"), domain.RetryTaskRequest{ExpectedVersion: root.Version, Reason: "Apply explicit feedback", Feedback: "Keep the fixture response short", AcknowledgeNewCost: true})
		if attempt == 3 {
			code(t, err, "invalid_transition")
			break
		}
		if err != nil {
			t.Fatal(err)
		}
		if err = json.Unmarshal(raw, &root); err != nil {
			t.Fatal(err)
		}
		if root.CurrentRunID == beforeRun || root.AttemptCount != attempt+1 || root.SpentMicroUSD != int64(attempt)*2000 {
			t.Fatal("retry reused run or reset money")
		}
	}
}

func TestTeamPartialJoinClosesCancelledUnstartedBranch(t *testing.T) {
	s, _ := testStore(t)
	p, _ := identity(t, s, 1_000_000)
	leaf := agent(t, s, p)
	a := teamAgent(t, s, p, "parallel", "allow_partial", 2, teamMember("keep", leaf), teamMember("omit", leaf))
	root := task(t, s, p, a)
	var cancelled string
	if err := s.Pool.QueryRow(context.Background(), "SELECT child_task_id FROM team_members WHERE organization_id=$1 AND parent_run_id=$2 AND member_id='omit'", p.OrganizationID, root.CurrentRunID).Scan(&cancelled); err != nil {
		t.Fatal(err)
	}
	if _, err := s.CancelTask(context.Background(), p, cancelled, domain.NewID("key")); err != nil {
		t.Fatal(err)
	}
	root = driveFixtureTeam(t, s, p, root.ID)
	if root.CandidateID == "" || root.SpentMicroUSD != 1000 {
		t.Fatalf("partial join failed: %s %s", root.Status, root.WaitingReason)
	}
	artifacts, err := s.Artifacts(context.Background(), p, root.ID)
	if err != nil || len(artifacts) != 1 {
		t.Fatal("partial result missing")
	}
	var manifest teamManifest
	if err = json.Unmarshal([]byte(artifacts[0].Content), &manifest); err != nil {
		t.Fatal(err)
	}
	if manifest.Complete || len(manifest.Members) != 2 || manifest.Members[1].Status != "cancelled" || manifest.Members[1].Result != nil {
		t.Fatal("partial result silently omitted failed branch")
	}
	var selected domain.TaskCandidate
	for _, c := range mustTeamCandidates(t, s, p, root.ID) {
		if c.ID == root.CandidateID {
			selected = c
		}
	}
	if _, err = s.ReviewTask(context.Background(), p, root.ID, domain.NewID("key"), domain.ReviewTaskRequest{ExpectedVersion: root.Version, CandidateID: selected.ID, ArtifactID: selected.ArtifactID, ArtifactVersion: selected.ArtifactVersion, ContentDigest: selected.ContentDigest, Decision: "accept"}); err != nil {
		t.Fatal(err)
	}
}

func TestTeamDeadlineAndCyclicPlanRejectBeforeDispatch(t *testing.T) {
	s, _ := testStore(t)
	p, _ := identity(t, s, 1_000_000)
	leaf := agent(t, s, p)
	a := teamAgent(t, s, p, "sequence", "all", 1, teamMember("one", leaf))
	seconds := 1
	raw, err := s.CreateTask(context.Background(), p, domain.NewID("key"), domain.CreateTask{Goal: "Expire before dispatch", AgentID: a.ID, BudgetMicroUSD: 10000, ExecutionTimeoutSeconds: &seconds})
	if err != nil {
		t.Fatal(err)
	}
	var root domain.Task
	if err = json.Unmarshal(raw, &root); err != nil {
		t.Fatal(err)
	}
	time.Sleep(time.Until(root.ExecutionDeadline) + 50*time.Millisecond)
	teamTick(t, s)
	root, err = s.GetTask(context.Background(), p, root.ID)
	if err != nil {
		t.Fatal(err)
	}
	if root.Status != "blocked" || root.WaitingReason != "execution_deadline" || root.SpentMicroUSD != 0 || root.ReservedMicroUSD != 0 {
		t.Fatal("expired tree admitted work")
	}
	var attempts int
	if err = s.Pool.QueryRow(context.Background(), "SELECT used_attempts FROM task_tree_controls WHERE organization_id=$1 AND root_id=$2", p.OrganizationID, root.ID).Scan(&attempts); err != nil || attempts != 1 {
		t.Fatal("deadline started child run")
	}
	cyclicID := domain.NewID("agt")
	definition := domain.TeamDefinition{Pattern: "sequence", JoinPolicy: "all", MaxDepth: 16, MaxChildren: 1, MaxParallel: 1, Members: []domain.TeamMember{{ID: "self", AgentID: cyclicID, AgentVersion: 1}}}
	body, _ := json.Marshal(definition)
	if _, err = s.Pool.Exec(context.Background(), "INSERT INTO agents(organization_id,id,name,instructions,model,budget_microusd,team) VALUES($1,$2,'Cycle fixture','Never dispatch','team:coordinator',10000,$3)", p.OrganizationID, cyclicID, body); err != nil {
		t.Fatal(err)
	}
	_, err = s.PublishAgent(context.Background(), p, cyclicID, domain.NewID("key"), 1)
	code(t, err, "invalid_request")
}

func TestTeamRetryKeepsPublishedModelPinAndRejectsRetiredProfile(t *testing.T) {
	s, _ := testStore(t)
	p, _ := identity(t, s, 1_000_000)
	c := domain.ModelConnection{ID: "team-retry-pin", OrganizationID: p.OrganizationID, Version: 1, Model: "team-pin-fixture", ProviderAccount: "team-test-account", PriceVersion: "test-v1", Endpoint: "http://127.0.0.1:1", CredentialEnvironment: "PLATFORM_MODEL_KEY_TEAM", MaxChargeMicroUSD: 150, MaxInputTokens: 100, MaxOutputTokens: 16, InputMicroUSDPerMillion: 1_000_000, OutputMicroUSDPerMillion: 2_000_000, FixedMicroUSD: 7, MaxInputBytes: 32768, MaxOutputBytes: 1024, TimeoutMilliseconds: 2000, BoundAssumptions: "Never-dispatched local pin fixture", TokenLimitField: "max_tokens", AllowInsecureLoopback: true, Active: true}
	var err error
	if c, err = s.InstallModelConnection(context.Background(), c); err != nil {
		t.Fatal(err)
	}
	budget := int64(10000)
	raw, err := s.CreateAgent(context.Background(), p, domain.NewID("key"), domain.CreateAgent{Name: "Pinned fixture", Instructions: "No dispatch expected", Model: c.Model, BudgetMicroUSD: &budget})
	if err != nil {
		t.Fatal(err)
	}
	var leaf domain.Agent
	if err = json.Unmarshal(raw, &leaf); err != nil {
		t.Fatal(err)
	}
	if _, err = s.PublishAgent(context.Background(), p, leaf.ID, domain.NewID("key"), leaf.Version); err != nil {
		t.Fatal(err)
	}
	a := teamAgent(t, s, p, "sequence", "all", 1, teamMember("pinned", leaf))
	root := task(t, s, p, a)
	block := func() {
		t.Helper()
		var child string
		if err := s.Pool.QueryRow(context.Background(), "SELECT child_task_id FROM team_members WHERE organization_id=$1 AND parent_run_id=$2", p.OrganizationID, root.CurrentRunID).Scan(&child); err != nil {
			t.Fatal(err)
		}
		if _, err := s.CancelTask(context.Background(), p, child, domain.NewID("key")); err != nil {
			t.Fatal(err)
		}
		teamTick(t, s)
		var err error
		if root, err = s.GetTask(context.Background(), p, root.ID); err != nil {
			t.Fatal(err)
		}
		if root.Status != "blocked" {
			t.Fatalf("cancelled branch did not block root: %s %s", root.Status, root.WaitingReason)
		}
	}
	block()
	newProfile := c
	newProfile.Version = 2
	newProfile.Active = false
	newProfile.PriceVersion = "test-v2"
	if _, err = s.InstallModelConnection(context.Background(), newProfile); err != nil {
		t.Fatal(err)
	}
	raw, err = s.RetryTask(context.Background(), p, root.ID, domain.NewID("key"), domain.RetryTaskRequest{ExpectedVersion: root.Version, Reason: "Explicit retry", AcknowledgeNewCost: true})
	if err != nil {
		t.Fatal(err)
	}
	if err = json.Unmarshal(raw, &root); err != nil {
		t.Fatal(err)
	}
	var pinnedVersion int
	if err = s.Pool.QueryRow(context.Background(), "SELECT t.model_connection_version FROM team_members m JOIN tasks t ON t.organization_id=m.organization_id AND t.id=m.child_task_id WHERE m.organization_id=$1 AND m.parent_run_id=$2", p.OrganizationID, root.CurrentRunID).Scan(&pinnedVersion); err != nil || pinnedVersion != 1 {
		t.Fatal("retry changed frozen profile pin")
	}
	block()
	newProfile.Version = 3
	newProfile.Active = true
	newProfile.PriceVersion = "test-v3"
	if _, err = s.InstallModelConnection(context.Background(), newProfile); err != nil {
		t.Fatal(err)
	}
	beforeRun, beforeVersion := root.CurrentRunID, root.Version
	_, err = s.RetryTask(context.Background(), p, root.ID, domain.NewID("key"), domain.RetryTaskRequest{ExpectedVersion: root.Version, Reason: "Retired pin retry", AcknowledgeNewCost: true})
	code(t, err, "invalid_request")
	if root, err = s.GetTask(context.Background(), p, root.ID); err != nil {
		t.Fatal(err)
	}
	var usedTasks, usedAttempts int
	if err = s.Pool.QueryRow(context.Background(), "SELECT used_tasks,used_attempts FROM task_tree_controls WHERE organization_id=$1 AND root_id=$2", p.OrganizationID, root.ID).Scan(&usedTasks, &usedAttempts); err != nil {
		t.Fatal(err)
	}
	if root.CurrentRunID != beforeRun || root.Version != beforeVersion || root.AttemptCount != 2 || usedTasks != 3 || usedAttempts != 2 || root.SpentMicroUSD != 0 || root.ReservedMicroUSD != 0 {
		t.Fatal("rejected retired-profile retry changed durable counters or money")
	}
}

func TestTeamProfileLockWaitRechecksDeadlineBeforeChildRun(t *testing.T) {
	s, _ := testStore(t)
	p, _ := identity(t, s, 1_000_000)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	c := domain.ModelConnection{ID: "team-deadline-pin", OrganizationID: p.OrganizationID, Version: 1, Model: "team-deadline-fixture", ProviderAccount: "team-test-account", PriceVersion: "test-v1", Endpoint: "http://127.0.0.1:1", CredentialEnvironment: "PLATFORM_MODEL_KEY_TEAM", MaxChargeMicroUSD: 150, MaxInputTokens: 100, MaxOutputTokens: 16, InputMicroUSDPerMillion: 1_000_000, OutputMicroUSDPerMillion: 2_000_000, FixedMicroUSD: 7, MaxInputBytes: 32768, MaxOutputBytes: 1024, TimeoutMilliseconds: 2000, BoundAssumptions: "Never-dispatched local deadline fixture", TokenLimitField: "max_tokens", AllowInsecureLoopback: true, Active: true}
	var err error
	if c, err = s.InstallModelConnection(ctx, c); err != nil {
		t.Fatal(err)
	}
	budget := int64(10000)
	raw, err := s.CreateAgent(ctx, p, domain.NewID("key"), domain.CreateAgent{Name: "Deadline fixture", Instructions: "No dispatch expected", Model: c.Model, BudgetMicroUSD: &budget})
	if err != nil {
		t.Fatal(err)
	}
	var leaf domain.Agent
	if err = json.Unmarshal(raw, &leaf); err != nil {
		t.Fatal(err)
	}
	if _, err = s.PublishAgent(ctx, p, leaf.ID, domain.NewID("key"), leaf.Version); err != nil {
		t.Fatal(err)
	}
	a := teamAgent(t, s, p, "sequence", "all", 1, teamMember("wait", leaf))
	seconds := 2
	raw, err = s.CreateTask(ctx, p, domain.NewID("key"), domain.CreateTask{Goal: "Expire while the model pin is locked", AgentID: a.ID, BudgetMicroUSD: 10000, ExecutionTimeoutSeconds: &seconds})
	if err != nil {
		t.Fatal(err)
	}
	var root domain.Task
	if err = json.Unmarshal(raw, &root); err != nil {
		t.Fatal(err)
	}
	blocker, err := s.Pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer blocker.Rollback(ctx)
	if _, err = blocker.Exec(ctx, "SELECT id FROM model_connections WHERE organization_id=$1 AND id=$2 AND version=$3 FOR UPDATE", p.OrganizationID, c.ID, c.Version); err != nil {
		t.Fatal(err)
	}
	result := make(chan error, 1)
	go func() { _, err := s.ReconcileTeam(ctx); result <- err }()
	waitForBlockedQuery(t, s, "SELECT active FROM model_connections WHERE organization_id=")
	for {
		var expired bool
		if err = s.Pool.QueryRow(ctx, "SELECT execution_deadline<=clock_timestamp() FROM tasks WHERE organization_id=$1 AND id=$2", p.OrganizationID, root.ID).Scan(&expired); err != nil {
			t.Fatal(err)
		}
		if expired {
			break
		}
		select {
		case <-time.After(10 * time.Millisecond):
		case <-ctx.Done():
			t.Fatal(ctx.Err())
		}
	}
	if err = blocker.Commit(ctx); err != nil {
		t.Fatal(err)
	}
	if err = <-result; err != nil {
		t.Fatalf("expired coordinator must durably block, not return a retrying error: %v", err)
	}
	if root, err = s.GetTask(ctx, p, root.ID); err != nil {
		t.Fatal(err)
	}
	var attempts, runnable int
	if err = s.Pool.QueryRow(ctx, "SELECT used_attempts FROM task_tree_controls WHERE organization_id=$1 AND root_id=$2", p.OrganizationID, root.ID).Scan(&attempts); err != nil {
		t.Fatal(err)
	}
	if err = s.Pool.QueryRow(ctx, "SELECT count(*) FROM operations o JOIN tasks t ON t.organization_id=o.organization_id AND t.id=o.task_id WHERE t.organization_id=$1 AND t.root_id=$2 AND o.status IN('queued','leased')", p.OrganizationID, root.ID).Scan(&runnable); err != nil {
		t.Fatal(err)
	}
	if root.Status != "blocked" || root.WaitingReason != "execution_deadline" || attempts != 1 || runnable != 0 || root.SpentMicroUSD != 0 || root.ReservedMicroUSD != 0 {
		t.Fatalf("expired coordinator admitted child: status=%s reason=%s attempts=%d runnable=%d", root.Status, root.WaitingReason, attempts, runnable)
	}
}
