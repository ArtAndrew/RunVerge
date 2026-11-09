// SPDX-License-Identifier: Elastic-2.0
package toolruntime

import (
	"context"
	"encoding/json"
	jsonv2 "encoding/json/v2"
	"fmt"
	"net/http"
	"regexp"
	"strings"
	"time"

	"agentplatform.local/platform/internal/domain"
)

const GitHubCICommentOccurrence = "github-ci-comment"
const GitHubCICommentPriceVersion = "github-ci-comment-v1"
const GitHubCICommentSchema = `{"type":"object","properties":{"repository_id":{"type":"integer","minimum":1},"workflow_id":{"type":"integer","minimum":1},"run_id":{"type":"integer","minimum":1},"run_attempt":{"type":"integer","minimum":1},"branch":{"type":"string","minLength":1,"maxLength":128},"commit_sha":{"type":"string","minLength":40,"maxLength":40},"conclusion":{"type":"string","enum":["failure","timed_out"]},"body":{"type":"string","minLength":1,"maxLength":16384}},"required":["repository_id","workflow_id","run_id","run_attempt","branch","commit_sha","conclusion","body"],"additionalProperties":false}`

const githubCIAPI = "https://api.github.com/repos/"

var (
	githubCIRepo   = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9-]{0,98}/[A-Za-z0-9_.-]{1,100}$`)
	githubCIBranch = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9_./-]{0,127}$`)
	githubCICommit = regexp.MustCompile(`^[a-f0-9]{40}$`)
)

type GitHubCIComment struct {
	RepositoryID int64  `json:"repository_id"`
	WorkflowID   int64  `json:"workflow_id"`
	RunID        int64  `json:"run_id"`
	Attempt      int64  `json:"run_attempt"`
	Branch       string `json:"branch"`
	CommitSHA    string `json:"commit_sha"`
	Conclusion   string `json:"conclusion"`
	Body         string `json:"body"`
}

func GitHubCIRepository(c domain.ToolConnection) string {
	return strings.TrimPrefix(c.Endpoint, githubCIAPI)
}

func validGitHubCICommentProfile(c domain.ToolConnection) bool {
	repo := GitHubCIRepository(c)
	t := c.Tool
	return c.Endpoint == githubCIAPI+repo && githubCIRepo.MatchString(repo) &&
		!strings.HasSuffix(repo, "/.") && !strings.HasSuffix(repo, "/..") &&
		t.Destination == "https://github.com/"+repo && t.InputSchemaJSON == GitHubCICommentSchema &&
		t.Provider == "github" && t.PriceVersion == GitHubCICommentPriceVersion &&
		c.Transport == "http_json" && c.CredentialEnvironment != "" && !c.AllowPrivateNetwork && c.TrustedCAPEM == "" &&
		t.Effect == "write" && t.RequireDistinctApprover && !t.AutomationAllowed && t.MaxOutputBytes >= 4096
}

func ParseGitHubCIComment(raw string) (GitHubCIComment, error) {
	var a GitHubCIComment
	if jsonv2.Unmarshal([]byte(raw), &a, jsonv2.RejectUnknownMembers(true)) != nil ||
		a.RepositoryID < 1 || a.WorkflowID < 1 || a.RunID < 1 || a.Attempt < 1 ||
		!githubCIBranch.MatchString(a.Branch) || !githubCICommit.MatchString(a.CommitSHA) ||
		(a.Conclusion != "failure" && a.Conclusion != "timed_out") ||
		!text(a.Body, 16384) || strings.TrimSpace(a.Body) == "" {
		return GitHubCIComment{}, failed("invalid_github_ci_comment")
	}
	return a, nil
}

type githubCIExchange func(context.Context, string, string, []byte, string, time.Time) (int, []byte, error)

func (r *Runtime) githubCIRequest(ctx context.Context, method, endpoint string, payload []byte, key string, deadline time.Time) (int, []byte, error) {
	if r.githubCIHTTP != nil {
		return r.githubCIHTTP(ctx, method, endpoint, payload, key, deadline)
	}
	return r.httpExchange(ctx, method, endpoint, payload, key, deadline)
}

// This is a read of the current attempt, not the immutable historical
// /attempts/{number} resource. The caller must still obtain a fresh DB permit.
func (r *Runtime) githubCICurrent(ctx context.Context, a GitHubCIComment, deadline time.Time) bool {
	ctx, cancel := context.WithDeadline(ctx, deadline)
	defer cancel()
	endpoint := fmt.Sprintf("%s/actions/runs/%d", r.config.Connection.Endpoint, a.RunID)
	status, raw, err := r.githubCIRequest(ctx, http.MethodGet, endpoint, nil, "", deadline)
	var facts struct {
		ID         int64  `json:"id"`
		Attempt    int64  `json:"run_attempt"`
		WorkflowID int64  `json:"workflow_id"`
		HeadSHA    string `json:"head_sha"`
		HeadBranch string `json:"head_branch"`
		Event      string `json:"event"`
		Status     string `json:"status"`
		Conclusion string `json:"conclusion"`
		Repository struct {
			ID       int64  `json:"id"`
			FullName string `json:"full_name"`
		} `json:"repository"`
		HeadRepository struct {
			ID int64 `json:"id"`
		} `json:"head_repository"`
	}
	return err == nil && status == http.StatusOK && jsonv2.Unmarshal(raw, &facts) == nil &&
		facts.ID == a.RunID && facts.Attempt == a.Attempt && facts.WorkflowID == a.WorkflowID &&
		facts.HeadSHA == a.CommitSHA && facts.HeadBranch == a.Branch && facts.Event == "push" &&
		facts.Status == "completed" && facts.Conclusion == a.Conclusion && facts.Repository.ID == a.RepositoryID &&
		facts.HeadRepository.ID == a.RepositoryID && strings.EqualFold(facts.Repository.FullName, GitHubCIRepository(r.config.Connection))
}

func (r *Runtime) githubCIComment(ctx context.Context, e domain.ToolExecution, a GitHubCIComment, current bool, deadline time.Time) ([]byte, error) {
	result := wireResult{ExecutionID: e.ID, ActionID: e.ActionID, IntentHash: e.IntentHash}
	if !current || !time.Now().Before(deadline) {
		result.Outcome = "rejected"
		result.EffectReference = "github-ci:publication-withheld"
		result.Result = "RunVerge withheld the comment because the latest CI attempt could not be confirmed as current."
		return json.Marshal(result)
	}
	payload, err := json.Marshal(map[string]string{"body": a.Body})
	if err != nil {
		return nil, err
	}
	endpoint := r.config.Connection.Endpoint + "/commits/" + a.CommitSHA + "/comments"
	// GitHub does not document an idempotent create-comment contract. A
	// failed or invalid receipt stays unknown; never retry the POST.
	status, raw, err := r.githubCIRequest(ctx, http.MethodPost, endpoint, payload, "", deadline)
	var comment struct {
		ID       int64  `json:"id"`
		CommitID string `json:"commit_id"`
		Body     string `json:"body"`
	}
	if err != nil || status != http.StatusCreated || jsonv2.Unmarshal(raw, &comment) != nil ||
		comment.ID < 1 || comment.CommitID != a.CommitSHA || comment.Body != a.Body {
		return nil, failed("github_comment_outcome_unknown")
	}
	result.Outcome = "succeeded"
	result.EffectReference = fmt.Sprintf("%s/commit/%s#commitcomment-%d", r.config.Connection.Tool.Destination, a.CommitSHA, comment.ID)
	result.Result = "GitHub returned a matching receipt for the approved comment."
	return json.Marshal(result)
}
