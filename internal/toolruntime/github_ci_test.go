// SPDX-License-Identifier: Elastic-2.0
package toolruntime

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"strings"
	"testing"
	"time"

	"agentplatform.local/platform/internal/domain"
)

func TestGitHubCICommentChecksLatestAndNeverRepeatsPost(t *testing.T) {
	const endpoint = "https://api.github.com/repos/octo-org/sample"
	c := testProfile(endpoint, "http_json")
	c.ResultContract, c.AllowPrivateNetwork = domain.ToolResultContractGitHubCICommentV1, false
	c.Tool.RequireDistinctApprover = true
	c.Tool.Provider, c.Tool.PriceVersion = "github", GitHubCICommentPriceVersion
	c.Tool.Destination = "https://github.com/octo-org/sample"
	c.Tool.InputSchemaJSON, c.Tool.InputSchemaDigest = GitHubCICommentSchema, digest([]byte(GitHubCICommentSchema))
	a := GitHubCIComment{RepositoryID: 10, WorkflowID: 20, RunID: 100, Attempt: 1, Branch: "main",
		CommitSHA: strings.Repeat("a", 40), Conclusion: "failure", Body: "Disk full before tests.\nExact text: <fixture>."}
	for _, item := range []struct {
		name        string
		attempt     int64
		invalidRead bool
		lost        bool
		wrongReply  bool
		want        string
	}{
		{name: "current", attempt: 1, want: "succeeded"},
		{name: "rerun", attempt: 2, want: "rejected"},
		{name: "facts_unavailable", attempt: 1, invalidRead: true, want: "rejected"},
		{name: "lost_receipt", attempt: 1, lost: true, want: "unknown"},
		{name: "mismatched_receipt", attempt: 1, wrongReply: true, want: "unknown"},
	} {
		t.Run(item.name, func(t *testing.T) {
			store := &memoryStore{}
			r, err := New(store, Config{Connection: c, Credential: "private-fixture", PersistenceTimeout: time.Second})
			if err != nil {
				t.Fatal(err)
			}
			read, posted := 0, 0
			r.githubCIHTTP = func(_ context.Context, method, target string, payload []byte, key string, deadline time.Time) (int, []byte, error) {
				if !time.Now().Before(deadline) || key != "" {
					t.Error("invalid freshness or retry header")
				}
				if method == http.MethodGet {
					read++
					if target != endpoint+"/actions/runs/100" || len(payload) != 0 {
						t.Error("did not read the latest exact run")
					}
					if item.invalidRead {
						return 0, nil, errors.New("fixture network error")
					}
					facts, _ := json.Marshal(map[string]any{"id": a.RunID, "run_attempt": item.attempt,
						"workflow_id": a.WorkflowID, "head_sha": a.CommitSHA, "head_branch": a.Branch,
						"event": "push", "status": "completed", "conclusion": a.Conclusion,
						"repository":      map[string]any{"id": a.RepositoryID, "full_name": "octo-org/sample"},
						"head_repository": map[string]any{"id": a.RepositoryID}})
					return 200, facts, nil
				}
				posted++
				var body map[string]string
				if method != http.MethodPost || target != endpoint+"/commits/"+a.CommitSHA+"/comments" ||
					!store.sent.Load() || read != 1 || json.Unmarshal(payload, &body) != nil ||
					len(body) != 1 || body["body"] != a.Body {
					t.Error("wrong or unpermitted publication")
				}
				if item.lost {
					return 0, nil, errors.New("fixture reply lost after write")
				}
				if item.wrongReply {
					body["body"] = "different"
				}
				reply, _ := json.Marshal(map[string]any{"id": 77, "commit_id": a.CommitSHA, "body": body["body"]})
				return 201, reply, nil
			}
			lease := testLease(c)
			raw, _ := json.Marshal(a)
			lease.Execution.OccurrenceID = GitHubCICommentOccurrence
			lease.Execution.ArgumentsJSON = string(raw)
			lease.Execution.ArgumentsDigest = digest(raw)
			lease.Execution.ContentDigest = digest([]byte(a.Body))
			lease.Execution.Provider, lease.Execution.PriceVersion = c.Tool.Provider, c.Tool.PriceVersion
			lease.Execution.IntentHash = domain.ToolIntentDigest(lease.Execution)
			out, err := r.Execute(context.Background(), lease)
			if out.Status != item.want || (err != nil) != (item.want == "unknown") || store.last.Outcome != item.want ||
				read != 1 || (posted == 0) != (item.want == "rejected") || store.unknown.Load() != 1 {
				t.Fatalf("unexpected publication outcome: %+v %v, reads=%d posts=%d", out, err, read, posted)
			}
			if item.want == "succeeded" && store.last.EffectReference != c.Tool.Destination+"/commit/"+a.CommitSHA+"#commitcomment-77" {
				t.Fatal("wrong commit receipt reference")
			}
			if posted != 0 {
				if _, err := r.Execute(context.Background(), lease); err == nil || posted != 1 {
					t.Fatal("an approved occurrence sent a second POST")
				}
			}
		})
	}
}
