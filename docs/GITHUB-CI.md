# GitHub Actions failure intake

The local development candidate can open one RunVerge task when a configured GitHub Actions workflow fails. The task contains the run link, branch and commit. It asks for a summary of the event and the log evidence needed to diagnose it. The resulting draft appears in Workspace under **Needs attention**.

## Configure the receiver

1. Publish a dedicated, single-model RunVerge agent with an empty tool list. Record its ID and version.
2. Create a dedicated service account with role `member` in the target organization. From the trusted installation host, issue a key with `platform issue-service-token` and role ceiling `member`. The key expires according to organization policy. Replace the key in the private configuration and restart the receiver when rotating it.
3. Generate a random secret with `openssl rand -hex 32`. Use that text both in the GitHub webhook and the private RunVerge configuration. Save the configuration as an owner-only file and replace the placeholders below.

```json
{
  "organization_id": "ORG_ID",
  "service_token": "SERVICE_TOKEN_FROM_PRIVATE_FILE",
  "webhook_secret": "RANDOM_64_CHARACTER_HEX_SECRET",
  "repository": "OWNER/REPOSITORY",
  "repository_id": 123,
  "workflow_id": 456,
  "branch": "main",
  "agent_id": "AGENT_ID",
  "agent_version": 1,
  "budget_microusd": 1000,
  "max_per_hour": 5
}
```

Start `platform serve --github-ci-config /private/path/ci.json` with the installation's TLS settings. With `--trusted-tls-proxy`, bind the application to loopback and configure the proxy to expose only `POST /hooks/github/ci` for this integration. Keep the proxy's request deadline within GitHub's webhook deadline. Configure a repository webhook for **Workflow runs**, with content type `application/json` and the same secret. [GitHub webhook setup](https://docs.github.com/en/webhooks/using-webhooks/creating-webhooks) · [GitHub signature verification](https://docs.github.com/en/webhooks/using-webhooks/validating-webhook-deliveries)

The receiver accepts completed runs with conclusion `failure` or `timed_out` from a push on the configured branch and repository, with the configured workflow ID. It considers the signed run timestamp recent for 24 hours, with five minutes of future clock tolerance. An exact run attempt can create one task per RunVerge organization. An explicit GitHub rerun with a new attempt number can create a new task. The service actor, pinned agent version, one RunVerge attempt, per-task budget, organization limits and configured rolling hourly cap apply before model execution. New tasks have a one-hour lifetime to leave time for a separate reviewer. The model connection still bounds request duration.

Successful admission and duplicate deliveries return an empty `202`. Ignored events return `204`. An expired or revoked service key returns `403`. An unavailable store or exhausted admission limit returns `503`. Review delivery failures in GitHub and redeliver after resolving the cause. [Handling failed deliveries](https://docs.github.com/en/webhooks/using-webhooks/handling-failed-webhook-deliveries)

## Include log evidence

Add `"actions_read_token"` to the private configuration to enable log reads. Use a GitHub App installation token or a fine-grained personal access token restricted to the selected repository with **Actions: Read**. The agent must use a RunVerge local unmetered model connection with a loopback endpoint. The receiver checks the agent, model and hourly limit before fetching evidence, and repeats admission checks before creating the task. Replay of an existing task skips the fetch. Log reads are paced per receiver process.

RunVerge checks the repository, workflow, run attempt and commit against the GitHub API. It then requests the log archive for that exact attempt. Temporary archive downloads use an allowed public HTTPS host with no API authorization header. The current host policy accepts Azure Blob and GitHub Actions archive subdomains. A download outside the supported host policy, an unavailable archive, a response over the configured limits or a log without an explicit error marker returns `503`. The receiver keeps the fetch within the webhook request deadline. [GitHub Actions workflow run and log endpoints](https://docs.github.com/en/rest/actions/workflow-runs#download-workflow-run-attempt-logs)

Only a bounded excerpt and a file digest enter the task. Workspace checks citations against this saved excerpt. The `#L…` references identify its source-file lines inside RunVerge. The excerpt remains visible to users with access to the task. Credential-like lines are filtered heuristically. Enable this mode for repositories whose logs can be shown to those users.

Without `actions_read_token`, the receiver uses metadata. Current log verification uses a synthetic GitHub peer and local model endpoint.

## Publish the reviewed draft on its commit

Install the optional comment tool for the same repository on the trusted RunVerge host.

```sh
platform install-github-ci-comment --organization ORG_ID --repository OWNER/REPOSITORY \
  --credential-environment PLATFORM_TOOL_KEY_GITHUB_CI_COMMENT
```

Set that variable privately on the worker to a repository-scoped GitHub credential that can read the workflow run and create a commit comment. The credential reference is installed separately from the intake token. The tool posts on the commit identified by the workflow run. [Read a workflow run](https://docs.github.com/en/rest/actions/workflow-runs#get-a-workflow-run) · [Create a commit comment](https://docs.github.com/en/rest/commits/comments#create-a-commit-comment)

1. Open the draft in Workspace and finish any edits before preparing the action. In **Actions**, select the GitHub CI comment tool. For new CI tasks, Workspace fills the attempt, commit and exact draft text from the current snapshot.
2. Review the arguments. A different authorized person approves the exact text and destination. A separate explicit dispatch is required.
3. The worker checks the latest attempt for the same run before starting the POST. A changed attempt, mismatched commit or unavailable check withholds the comment. The GitHub read and write are separate operations, so a rerun can still begin between them.
4. If GitHub returns a matching receipt, RunVerge records the commit comment reference. If the response is lost or differs, use the existing reconciliation flow. One task has one publication occurrence, including after an uncertain outcome.

The action and review of the draft are separate records. Review the full proposed comment before sending it, including any information copied from the log.
