# Local provider control API

Run the controller on the trusted Linux operator host. It serves HTTP over a
private Unix socket. A client runs under the same OS account and supplies its
own provider credential. Tenant Workspaces and the execution engine keep their
existing listeners and authentication. This profile is for local test data.

## Start the controller

Use the private registry and pinned image ID from the
[dedicated installation guide](LINUX-SERVER.md#dedicated-installations-for-a-provider-trial).
Choose a short private socket directory. Keep the credential header and policy
outside the checkout. Generate the first credential once.

~~~sh
socket_dir=$(mktemp -d /tmp/runverge-provider.XXXXXXXX)
policy_dir="$HOME/runverge-provider-access"
mkdir -m 700 "$policy_dir"

python3 scripts/provider_api.py keygen \
  --principal panel-operator --tenant '*' \
  --allow read --allow review --allow provision --allow suspend \
  --allow resume --allow export --allow delete \
  --out "$policy_dir/operator.header" --policy-out "$policy_dir/policy.json"

python3 scripts/provider_api.py serve --state-dir "$provider_state" \
  --socket-dir "$socket_dir" --policy "$policy_dir/policy.json" --image-id "$image_id"
~~~

The header file contains the secret. Supply it directly to curl so it stays out
of the command arguments. The policy stores only a key fingerprint and its
scopes. Issue separate credentials for different operators or integrations.
Use `--tenant trial-a --allow read` for a reader limited to one customer.
Additional `keygen` calls return a policy entry to place in the private
policy's `credentials` array. Replace the policy atomically. Removing an entry
revokes that credential at the next request and before queued work is dispatched.

## Submit and inspect an operation

Choose an unused port and an idempotency key specific to the operation.
Run these commands from another terminal with the same socket and policy paths.

~~~sh
curl --silent --show-error --unix-socket "$socket_dir/control.sock" \
  --header "@$policy_dir/operator.header" --header 'Content-Type: application/json' \
  --header 'Idempotency-Key: create-trial-a-001' \
  --data '{"operation":"provision","name":"Trial A","port":18081}' \
  http://localhost/v1/tenants/trial-a/operations

curl --silent --show-error --unix-socket "$socket_dir/control.sock" \
  --header "@$policy_dir/operator.header" \
  http://localhost/v1/tenants/trial-a/operations/create-trial-a-001
~~~

Use `/v1/tenants/{tenant}` as the prefix for the following endpoints.
Successful responses contain a `data` object. Error responses contain an
`error.code`.

| Method and suffix | Behavior |
| --- | --- |
| `GET` with no suffix | Saved tenant lifecycle and generation. The response identifies the registry as its source. |
| `POST /operations` | Submit an operation with `Idempotency-Key`. Queued and running operations return `202`. Repeating the exact request returns its current state. A changed request with that key returns `409`. |
| `GET /operations/{key}` | Read the state, requester, available result and recent audit transitions. |
| `GET /operations` | List up to 50 operations. Continue with `?before=` and the returned `next_before`. |
| `POST /operations/{key}/retry` | Explicitly retry a safely repeatable operation in `needs_review` after inspecting the installation. |
| `POST /operations/{key}/acknowledge` | Record that the operator reviewed the uncertain outcome. The operation becomes `reviewed` and carries no success receipt. |

Provisioning takes `name`, `port` and optional `budget_microusd`. The server
supplies the pinned image ID. Suspend, resume, export and delete take
`expected_generation` from status. Delete also requires
`confirm_organization_id`. Retry and acknowledgment are available to the
requester and to operators with the `review` permission for that tenant.
Either route still requires permission for the original operation.

To enable export, start the server with `--keys-dir` and `--archives-dir`.
Use two separate private directories outside the checkout and registry.
Name each key file `{tenant}.json`. The server writes the archive under
`{tenant}/{idempotency-key}` in the archive directory and returns the receipt
digest. The archive and its keys stay in the host's private directories.

The controller records operations in the registry before running them.
An interrupted running operation enters `needs_review` on restart. An
interrupted export must be inspected outside the API. Review it before
acknowledging it or submitting a new export. Tenant services can keep running
during a controller outage. Remote access and the cloud provider's identity
integration are separate deployment work.
