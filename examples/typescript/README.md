<!-- SPDX-License-Identifier: Apache-2.0 -->
# TypeScript API example

Build `packages/typescript-sdk` first. Node.js 24 can run the example's TypeScript
directly using built-in type stripping:

```sh
export AGENT_PLATFORM_URL='http://127.0.0.1:8080'
export AGENT_PLATFORM_ORGANIZATION_ID='your-organization-id'
# Supply AGENT_PLATFORM_TOKEN securely from your local environment.
node examples/typescript/fixture_task.ts --request-key typescript-example-1
```

The example creates a draft, publishes the inspected version, creates a task,
polls stored JSON events, and prints text artifacts. It explicitly selects
`fixture:echo`, a development fixture. It does not demonstrate real inference
or production readiness.

The token needs permission to create and publish agents and to create tasks.
There is no unauthenticated bootstrap in this example. Obtain credentials and
organization membership through the server's authorized setup flow.

Reuse the request key after a lost response; choose a new key for different
work. Waiting stops after 30 seconds by default without cancelling the task.
Use `--wait-seconds` to change that limit or call the SDK's `cancelTask` method
when cancellation is intended.

License: Apache-2.0.

`configuration.ts` (0.12) only inspects by default. Explicit plan mode creates
and statically validates one plan, with no approval, apply, verification or
credential issuance:

```sh
node examples/typescript/configuration.ts
node examples/typescript/configuration.ts --create-plan APPROVED_DESTINATION \
  --destination-version 1 --known-resource-version 0 \
  --request-key unique-config-1 --reason 'Set up approved connection'
```

Zero means the resource is absent; otherwise provide its known version.
Delegated callers add `--grant-id EXISTING_GRANT` with inspect/plan/validate
actions on that exact destination. The example checks at most20 discovery pages.
Review the server diff/decision, then separately use the SDK's owner approval,
apply and explicitly cost-acknowledged verification methods documented in its
README. No administrative permission expansion or POST retry is automatic.

`model_tools.ts` exports explicit helpers for contract 0.7. Pass an authenticated
SDK client to prepare an exact model proposal, and separately continue a fully
inspected completed batch. No approval or dispatch is automatic. Calling the
continuation helper admits a new potentially charged model turn. Unknown effects
stop it; the final model prose still needs its ordinary result review.
