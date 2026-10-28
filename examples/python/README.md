<!-- SPDX-License-Identifier: Apache-2.0 -->
# Python API example

`fixture_task.py` creates a draft, publishes the inspected version, submits a
task, reads stored events, and prints its text artifacts. It explicitly uses
`fixture:echo`, a development fixture. A passing run does not demonstrate real
model inference or production readiness.

Install the SDK from the repository root and configure an authorized token:

```sh
python3 -m pip install ./packages/python-sdk
export AGENT_PLATFORM_URL='http://127.0.0.1:8080'
export AGENT_PLATFORM_ORGANIZATION_ID='your-organization-id'
# Supply AGENT_PLATFORM_TOKEN securely from your local environment.
python3 examples/python/fixture_task.py --request-key sdk-example-1
```

The token needs permission to create and publish agents and to create tasks.
There is no unauthenticated bootstrap in this example; follow server setup
instructions to obtain the organization and credentials.

Reuse the same request key after a lost response. For a different example run,
choose a new key. Waiting expires after 30 seconds by default; this stops the
example's polling, not the task. Use `--wait-seconds` to change the limit and the
SDK's explicit `cancel_task` method if cancellation is intended.

License: Apache-2.0.

`configuration.py` (0.12) only inspects by default. Its explicit plan mode
creates and statically validates a plan, without approving, applying, probing or
issuing credentials:

```sh
python3 examples/python/configuration.py
python3 examples/python/configuration.py --create-plan APPROVED_DESTINATION \
  --destination-version 1 --known-resource-version 0 \
  --request-key unique-config-1 --reason 'Set up approved connection'
```

Zero means the resource is absent; supply a known existing version otherwise.
Delegated service/member callers also pass `--grant-id EXISTING_GRANT`. The grant
must include inspect, plan and validate on that exact destination. Discovery is
paged; the example checks at most20 pages. Review the returned diff and policy
decision through the SDK/UI. Owner approval, apply and a cost-acknowledged
verification remain separate explicit methods, shown in the SDK README. The
example never expands its own permission or retries a POST automatically.

`model_tools.py` contains explicit helpers for contract 0.7: prepare an exact
model proposal, then separately continue the inspected completed batch. Reuse
an authenticated SDK client. Neither helper approves or dispatches tools; the
continuation helper admits a new potentially charged model turn and must only
be called after inspecting all results. Unknown effects stop it.
