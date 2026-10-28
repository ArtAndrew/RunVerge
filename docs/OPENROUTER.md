# OpenRouter text profile preview

This profile connects a single RunVerge agent to one explicitly selected OpenRouter
text model. This source-build preview is not included in
the alpha.2 download. A successful HTTP compatibility probe is not an installed
application, user workflow or final-billing acceptance test.

The `openrouter_text_v1` profile uses the fixed HTTPS Chat Completions endpoint,
requests supported parameters, disables provider fallbacks and sets provider price
filters from the same rates used by RunVerge's reservation. It sends text only;
tool conversations, automatic model routers and web-search variants are excluded.
The existing generic connection profile is unchanged.

## Select the model and data policy

Choose an exact model ID from [OpenRouter's catalog](https://openrouter.ai/models).
For a free test choose a specific ID ending in `:free`. Check its current pricing:
free profiles require zero input, output and fixed request prices. Do not configure
a paid fallback. Availability and rate limits may prevent a free request from running.

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
  --model liquid/lfm-2.5-2.6b:free --provider-account my-openrouter-test \
  --price-version operator-free-v1 \
  --input-usd-per-million 0 --output-usd-per-million 0 \
  --max-call-usd 0.000001 --data-collection allow \
  --out "$HOME/.config/runverge/openrouter-v1.json"
```

This example allows provider data collection for public test text. For confidential
data, choose a model/provider policy that meets your requirements and use `deny`.
The model name is an example, not a promise of continued availability.

Preparation reads no API key, connects to no database, performs no inference and
refuses to replace an existing file. Rates are USD per million tokens; the optional
`--fixed-usd-per-request` is USD per request. Amounts must be plain decimals with
at most six fractional digits. The call reserve must cover the configured maximum
token charge. `--max-input-tokens` is an operator assumption, not a tokenizer result.

Install the profile with `platform install-model --file` using the selected
installation's private database settings. This stores an immutable connection;
it does not call the model. For a changed profile use the next `--version` and a
new filename. Existing tasks retain their original connection and price version.

Save the key using hidden input:

```sh
python3 scripts/model_key.py --out "$HOME/.config/runverge/openrouter.env"
```

Agents and deployment tools can instead supply an existing private raw-key file
with `--key-file /absolute/private/key-file`; the key never appears in arguments.

Run the worker with this private env_file loaded by your process manager. Keep the
key out of command arguments, source files and support reports. Do not load it into
the API process. Select the installed model when creating an agent, publish the
agent, then create a task. The draft still requires review.

## Linux Compose preview

Follow [the Linux installation guide](LINUX-SERVER.md) first. Save the key to a new
file with the helper above, then select that file for the worker:

```sh
export RUNVERGE_MODEL_ENV_FILE="$HOME/.config/runverge/openrouter.env"
```

Create the profile inside the admin volume so its ownership matches the installer:

```sh
docker compose -f deploy/compose.yaml --profile admin run --rm admin prepare-model \
  --provider openrouter --organization-id YOUR_ORGANIZATION_ID \
  --model liquid/lfm-2.5-2.6b:free --provider-account my-openrouter-test \
  --price-version operator-free-v1 \
  --input-usd-per-million 0 --output-usd-per-million 0 \
  --max-call-usd 0.000001 --data-collection allow \
  --out /state/openrouter-v1.json
docker compose -f deploy/compose.yaml --profile admin run --rm admin install-model --file /state/openrouter-v1.json
docker compose -f deploy/compose.yaml up -d --force-recreate worker
```

Keep `RUNVERGE_MODEL_ENV_FILE` set in your deployment environment for later worker
restarts. Only the worker receives this env_file. The API and image build do not.
Do not publish a resolved Compose configuration containing credentials.

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
