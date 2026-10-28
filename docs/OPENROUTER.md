# OpenRouter text profile preview

This profile connects a single RunVerge agent to one explicitly selected OpenRouter
text model. The native package contains the setup guide and key helper described
below. The older alpha.2 download does not include this profile.
A successful HTTP compatibility probe is not an installed
application, user workflow or final-billing acceptance test.

The `openrouter_text_v1` profile uses the fixed HTTPS Chat Completions endpoint,
requests supported parameters, disables provider fallbacks and sets provider price
filters from the same rates used by RunVerge's reservation. It sends text only;
tool conversations, automatic model routers and web-search variants are excluded.
The existing generic connection profile is unchanged.

The worker reserves the call budget, prepares a TLS connection without sending
HTTP or the key, then checks permission and the current execution lease again.
The same connection can send one request within the existing short dispatch
window. Connection preparation and response share one overall timeout.

## Select the model and data policy

Choose an exact model ID from [OpenRouter's catalog](https://openrouter.ai/models).
For a free test choose a specific ID ending in `:free`. Check its current pricing:
free profiles require zero input, output and fixed request prices. Do not configure
a paid fallback. Availability and rate limits may prevent a free request from running.
An HTTP `429` blocks the attempt; RunVerge does not retry it automatically or
switch models. A new attempt requires explicit consent. An HTTP rejection alone
does not settle the internal reservation or prove a final provider charge.

`--data-collection deny` is the default. It asks OpenRouter to exclude providers
that collect data. This is not a Zero Data Retention guarantee. Some free routes
require `--data-collection allow`; use that only when you accept the provider's
data practices. Account-wide restrictions still apply. Changing this setting in
a profile does not change the OpenRouter account settings. A privacy mismatch
causes a refusal, not an automatic switch to another policy or paid model.

See OpenRouter's [routing controls](https://openrouter.ai/docs/guides/routing/provider-selection)
and [provider data practices](https://openrouter.ai/docs/guides/privacy/provider-logging).

## Native installation

Create a private directory and prepare the profile with the newly built binary:

```sh
mkdir -p -m 700 "$HOME/.config/runverge"
/absolute/installation/bin/platform prepare-model \
  --provider openrouter --organization-id YOUR_ORGANIZATION_ID \
  --model nvidia/nemotron-3.5-lightning:free --provider-account my-openrouter-test \
  --price-version operator-free-v1 \
  --input-usd-per-million 0 --output-usd-per-million 0 \
  --max-call-usd 0.000001 --max-output-tokens 2048 --data-collection allow \
  --out "$HOME/.config/runverge/openrouter-v1.json"
```

This example allows provider data collection for public test text. For confidential
data, choose a model/provider policy that meets your requirements and use `deny`.
The model name is an example, not a promise of continued availability.
Output tokens include the model's reasoning as well as its answer. A very small
limit can be consumed before any answer appears. The setup default is 2,048;
choose a different bound explicitly when needed. Increasing the bound for a paid
model also requires a reservation large enough to cover the configured token rates.

Preparation reads no API key, connects to no database, performs no inference and
refuses to replace an existing file. Rates are USD per million tokens; the optional
`--fixed-usd-per-request` is USD per request. Amounts must be plain decimals with
at most six fractional digits. The call reserve must cover the configured maximum
token charge. `--max-input-tokens` is an operator assumption, not a tokenizer result.

Install the profile with `platform install-model --file` using the selected
installation's private database settings. This stores an immutable connection;
it does not call the model. For a changed profile use the next `--version` and a
new filename. Existing tasks retain their original connection and price version.

Save the key using hidden input. The native package includes the helper below;
this optional setup helper requires Python 3, while the application itself does not:

```sh
python3 /absolute/installation/helpers/model_key.py --out "$HOME/.config/runverge/openrouter.env"
```

Agents and deployment tools can instead supply an existing private raw-key file
with `--key-file /absolute/private/key-file`; the key never appears in arguments.

From a source checkout, the helper is `scripts/model_key.py`. You can also create
the private worker settings in a local editor without the helper.

In the separate worker terminal, load the runtime settings created in the
quickstart and the new key file, then start the worker:

```sh
set -a
. /absolute/runverge-private/runtime.env
. "$HOME/.config/runverge/openrouter.env"
set +a
/absolute/installation/bin/platform worker --concurrency 1
```

Load only files you created and trust: `.` executes a shell file. Keep the key out
of command arguments, source files and support reports. The API terminal loads only
its runtime settings. Select the installed model when creating an agent, publish
the agent, then create a task. The draft still requires review.

## Linux Compose preview

Follow [the Linux installation guide](LINUX-SERVER.md) first. Save the key to a new
file using `python3 scripts/model_key.py --out "$HOME/.config/runverge/openrouter.env"`
from the source checkout, then select that file for the worker:

```sh
export RUNVERGE_MODEL_ENV_FILE="$HOME/.config/runverge/openrouter.env"
```

Create the profile inside the admin volume so its ownership matches the installer:

```sh
docker compose -f deploy/compose.yaml --profile admin run --rm admin prepare-model \
  --provider openrouter --organization-id YOUR_ORGANIZATION_ID \
  --model nvidia/nemotron-3.5-lightning:free --provider-account my-openrouter-test \
  --price-version operator-free-v1 \
  --input-usd-per-million 0 --output-usd-per-million 0 \
  --max-call-usd 0.000001 --max-output-tokens 2048 --data-collection allow \
  --out /state/openrouter-v1.json
docker compose -f deploy/compose.yaml --profile admin run --rm admin install-model --file /state/openrouter-v1.json
docker compose -f deploy/compose.yaml up -d --force-recreate worker
```

Keep `RUNVERGE_MODEL_ENV_FILE` set in your deployment environment for later worker
restarts. Only the worker receives this env_file. The API and image build do not.
Do not publish a resolved Compose configuration containing credentials.

## If a model does not answer

- `provider_shared_pool_limited` with HTTP `429` means the upstream provider's
  shared pool refused the call. This does not establish that the API key's daily
  quota is exhausted. Check the quota privately with OpenRouter's
  [current-key endpoint](https://openrouter.ai/docs/api/api-reference/api-keys/get-current-api-key).
- `output_budget_exhausted` means generation reached the output limit without an
  answer. Configure an adequate limit before explicitly starting a new attempt.
- A `404` can mean the selected model has no route matching the requested privacy
  or price filters. Review the model and policy; RunVerge does not relax them.

These fixed codes appear in worker diagnostics. Do not publish raw provider errors
or the response from the current-key endpoint. A new model or limit requires a new
connection version; tasks already created keep their original configuration.


## Results and costs

The gateway makes at most one HTTP request per admitted attempt. Errors, timeouts,
model identity mismatches and incomplete usage do not authorize automatic retries.
The response must report the selected model and a generation ID. A new attempt
requires separate consent and retains the prior result.

A free request uses zero provider price filters. The positive one-micro-USD value
in the example is an internal admission hold, not an OpenRouter charge. The platform
retains unknown accounting rather than settling that hold from model response data.
OpenRouter's reported usage/cost is not final billing evidence in RunVerge. Credit
purchase fees, taxes and final account reconciliation remain outside this preview.
