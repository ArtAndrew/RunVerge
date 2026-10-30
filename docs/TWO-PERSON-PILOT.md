# A contributor and a reviewer

Development walkthrough, not a supported alpha.5 deployment. Use a fresh local installation and synthetic data. Remote access, SSO onboarding, backup/restore and mandatory independent approval are outside this walkthrough.

The smallest shared workflow uses two separate human accounts:

| Account | Job in this example | Existing permissions |
|---|---|---|
| Owner | Configure agents, review the contributor's result, manage access | Can review other members' tasks and administer the organization |
| Member | Start a team task and inspect its results | Can create tasks and review their own; cannot review another person's task or manage members |

Both accounts can read tasks in the organization. There are no private task folders in this profile. This is a voluntary review workflow, not an enforced two-person approval rule: the creator can still accept their own result. Do not give someone the administrator role just to obtain a narrow reviewer permission.

## Set up a separate login

1. Sign into Workspace as the owner. Open **Members → Add local member**, choose **Member**, enter a display name and a reason, then create the account. This does not email an invitation or create a login credential.
2. The trusted installation operator copies that new member's principal ID and issues a short-lived credential using the installed binary. Use the same private database environment as the server; never paste its password into a command or screenshot.

```sh
/absolute/installation/bin/platform issue-token \
  --organization ORGANIZATION_ID \
  --principal NEW_MEMBER_PRINCIPAL_ID \
  --ttl 1h \
  --out /absolute/private-state/new-member.json
```

The output path must be new. The command writes the credential to a private file and does not print the token. This is an operator command, not an API endpoint available to ordinary users. It validates an existing active human membership and does not create or promote one.

3. For a local walkthrough, use a separate browser profile on the same machine. In **Connection**, enter the API address, organization ID and the member's token from that private file. Do not copy the owner's bootstrap token. The token remains in page memory and must be entered again after a reload.

This credential identifies the person globally; the organization in the file is a selected context, not a cryptographic restriction to that organization. A newly created member has only the membership created above. Keep the file private; do not send it to an issue tracker or include it in a screen recording. This local walkthrough does not establish a secure remote deployment or a complete invitation flow.

## Do the work

1. As owner, create and publish a [draft-and-review team](TEAM-REVIEW.md). Choose two published agents with appropriate instructions and a shared budget.
2. As member, create a task using that team and synthetic source material. Both stages run under the task's limits. An aggregate result waits for a decision; the reviewer's response does not accept it automatically.
3. As owner, find the task in **Tasks → Needs attention**. Open its saved results, compare them with the original material and record **Accept**, **Reject** or **Request changes**. Your decision is attached to the exact saved result and your identity.
4. As member, reopen the same task and inspect that decision. Feedback alone does not launch another attempt. A new attempt requires a separate action and consent to possible cost.

The fixture executors used by the package check test this sequence but do not draft or review intelligently. Real-model quality and independent user usability need their own evaluation.

## Stop work and end access

Stop the root task to stop its pending team work. Work already sent to a provider may still have an effect or cost; cancellation does not erase it.

To end the current login, the owner opens **Members → Revoke previous logins** for the member, supplies a reason and confirms. Existing credentials then lose access to this organization, including after a server restart. The account's role and active membership remain. Already authorized durable work is not cancelled by this action. To remove the person's membership access and invalidate their execution authority, disable the member instead; already sent operations and uncertain costs still need separate handling.

A fresh credential issued by the trusted operator is a new authorization, not a way to restore the revoked credential. See the [membership contract](../contracts/MEMBERSHIPS.md) for the distinction between ending a login, disabling a member and cancelling a task.
