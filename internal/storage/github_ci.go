// SPDX-License-Identifier: Elastic-2.0
package storage

import (
	"context"
	"fmt"
	"strings"

	"agentplatform.local/platform/internal/domain"
	"agentplatform.local/platform/internal/toolruntime"
	"github.com/jackc/pgx/v5"
)

const githubCITaskPrefix = "tsk_ci_"

type GitHubCIRun struct {
	RepositoryID int64
	RunID        int64
	Attempt      int64
}

// The CI task identity authenticates the immutable receiver-authored metadata.
// Only the opening metadata lines are interpreted. Log content can contain
// arbitrary text and never supplies publication arguments.
func bindGitHubCIComment(t domain.Task, c domain.ToolConnection, in domain.PrepareToolExecution, arguments, artifactText string) error {
	a, err := toolruntime.ParseGitHubCIComment(arguments)
	if err != nil || in.OccurrenceID != toolruntime.GitHubCICommentOccurrence || in.ModelProposalID != "" ||
		a.Body != artifactText || t.ParentID != "" || t.ID != githubCITaskID(t.OrganizationID, GitHubCIRun{
		RepositoryID: a.RepositoryID, RunID: a.RunID, Attempt: a.Attempt,
	}) {
		return domain.Err("conflict", "Комментарий CI должен совпадать с точным черновиком и попыткой", 409)
	}
	prefix := strings.SplitN(t.Goal, "\n", 7)
	url := fmt.Sprintf("Run URL: https://github.com/%s/actions/runs/%d/attempts/%d", toolruntime.GitHubCIRepository(c), a.RunID, a.Attempt)
	if len(prefix) < 6 || !strings.EqualFold(prefix[1], url) || prefix[2] != fmt.Sprintf("Workflow ID: %d", a.WorkflowID) ||
		prefix[3] != "Branch: "+a.Branch || prefix[4] != "Commit: "+a.CommitSHA || prefix[5] != "Conclusion: "+a.Conclusion {
		return domain.Err("conflict", "Назначение комментария не совпадает с исходной задачей CI", 409)
	}
	return nil
}

type githubCIAdmission struct {
	taskID       string
	agentVersion int
	maxPerHour   int
	requireLocal bool
}

func githubCITaskID(org string, run GitHubCIRun) string {
	return githubCITaskPrefix + Hash([]byte(fmt.Sprintf("github-ci/v1/%s/%d/%d/%d", org, run.RepositoryID, run.RunID, run.Attempt)))
}

// PreflightGitHubCIEvidence avoids remote reads for completed replays and
// ineligible events. Creation checks the same facts again under write locks.
func (s *Store) PreflightGitHubCIEvidence(ctx context.Context, p domain.Principal, run GitHubCIRun, agentID string, agentVersion int, budget int64, maxPerHour int) (bool, error) {
	if err := githubCIPermission(ctx, nil, p); err != nil {
		return false, err
	}
	tx, err := s.beginPrincipalRead(ctx, p, "tasks.create")
	if err != nil {
		return false, err
	}
	defer tx.Rollback(ctx)
	ci := githubCIAdmission{taskID: githubCITaskID(p.OrganizationID, run), maxPerHour: maxPerHour}
	exists, err := ci.checkLimit(ctx, tx, p.OrganizationID)
	if err != nil {
		return false, err
	}
	if !exists {
		var eligible bool
		err = tx.QueryRow(ctx, `SELECT EXISTS (
			SELECT 1 FROM agents a JOIN model_connections m ON m.organization_id=a.organization_id AND m.model=a.model AND m.active
			WHERE a.organization_id=$1 AND a.id=$2 AND a.version=$3 AND a.published
			AND a.team IS NULL AND a.tools='[]'::jsonb AND a.model_tools='[]'::jsonb
			AND a.budget_microusd >= $4 AND m.profile->>'accounting_mode'=$5
		)`, p.OrganizationID, agentID, agentVersion, budget, domain.ModelAccountingLocalUnmetered).Scan(&eligible)
		if err != nil {
			return false, err
		}
		if !eligible {
			return false, domain.Err("conflict", "Журналы CI требуют опубликованного агента с локальной моделью и без инструментов", 409)
		}
	}
	return exists, tx.Commit(ctx)
}

// CreateGitHubCITask shares the ordinary task transaction and execution limits.
// The task key is stable across delivery headers and service key/account changes.
func (s *Store) CreateGitHubCITask(ctx context.Context, p domain.Principal, run GitHubCIRun, agentID string, agentVersion int, goal string, budget int64, maxPerHour int, requireLocal bool) error {
	if run.RepositoryID <= 0 || run.RunID <= 0 || run.Attempt <= 0 || agentVersion <= 0 || maxPerHour < 1 || maxPerHour > 60 {
		return domain.Err("invalid_request", "Некорректная настройка разбора CI", 400)
	}
	id := githubCITaskID(p.OrganizationID, run)
	// Leave time for a separate human approver after generation. Provider
	// timeouts still bound the single inference request independently.
	one, seconds := 1, 3600
	_, err := s.createTask(ctx, p, id, domain.CreateTask{
		AgentID: agentID, Goal: goal, BudgetMicroUSD: budget, MaxRuns: &one, ExecutionTimeoutSeconds: &seconds,
	}, &githubCIAdmission{taskID: id, agentVersion: agentVersion, maxPerHour: maxPerHour, requireLocal: requireLocal})
	return err
}

func githubCIPermission(_ context.Context, _ pgx.Tx, p domain.Principal) error {
	if p.Kind != domain.PrincipalService || p.CredentialKind != ServiceKeyCredential || !domain.PrincipalHasPermission(p, "tasks.create") {
		return domain.Forbidden
	}
	return nil
}

// createTask takes the organization row lock before this check. Preflight uses
// the same policy read-only; the write transaction is authoritative.
func (ci *githubCIAdmission) checkLimit(ctx context.Context, tx pgx.Tx, org string) (bool, error) {
	var exists bool
	if err := tx.QueryRow(ctx, "SELECT EXISTS(SELECT 1 FROM tasks WHERE organization_id=$1 AND id=$2)", org, ci.taskID).Scan(&exists); err != nil || exists {
		return exists, err
	}
	var admitted int
	if err := tx.QueryRow(ctx, `SELECT count(*) FROM (
		SELECT 1 FROM tasks WHERE organization_id=$1 AND starts_with(id,$2)
		AND created_at>=statement_timestamp()-interval '1 hour' LIMIT $3
	) recent`, org, githubCITaskPrefix, ci.maxPerHour).Scan(&admitted); err != nil {
		return false, err
	}
	if admitted >= ci.maxPerHour {
		return false, &domain.Error{Code: "unavailable", Message: "Достигнут часовой лимит задач CI", Status: 503, Retryable: true}
	}
	return false, nil
}
