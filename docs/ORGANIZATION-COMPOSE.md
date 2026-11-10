# RunVerge on Linux with organization sign-in

Run this profile on a Linux server with Docker Engine and Compose 2.24.4 or later.
It serves the Workspace over HTTPS and uses one configured OpenID Connect issuer.
The HTTPS port starts on host loopback. For team access, bind it to a private
interface reachable by the team and use a certificate the team's browsers trust.
This is the organization deployment preview for test data.
[Docker Compose version and port overrides](https://docs.docker.com/reference/compose-file/merge/#replace-value)

## Set up the provider and private files

Register a web client with your identity provider. Use Authorization Code with
S256 PKCE, client authentication with `client_secret_basic`, and the exact
callback `https://workspace.example.test:8443/auth/oidc/callback`. Replace the
example origin with the actual HTTPS address employees will open. Obtain the
full TLS certificate chain with its leaf first and its private key.

From the source checkout, create database settings for a fresh installation.

```sh
python3 scripts/server_setup.py
sudo install -d -o 10001 -g 10001 -m 0700 deploy/private/identity
```

Use this example to prepare `oidc.json` in a private staging directory.

```json
{
  "issuer": "https://login.example.test",
  "client_id": "runverge-workspace",
  "public_origin": "https://workspace.example.test:8443",
  "redirect_uri": "https://workspace.example.test:8443/auth/oidc/callback",
  "token_auth_method": "client_secret_basic",
  "client_secret_ref": {"file": "client-secret"},
  "deployment_seed_ref": {"file": "deployment-seed.b64"}
}
```

Store the provider's client secret in `client-secret`. Use
`openssl rand -base64 32 > deployment-seed.b64` in the private staging directory
with `umask 077`. Keep this seed with the deployment's protected recovery
materials. Replacing it invalidates existing login transactions and CSRF tokens.
If the IdP uses a private CA, add its certificate as `idp-ca.pem` and set
`"root_ca_file": "idp-ca.pem"` in `oidc.json`.

Name the Workspace certificate chain `tls.crt` and its key `tls.key`. Install
each file into `deploy/private/identity` as the container user with mode 0600.

```sh
sudo install -o 10001 -g 10001 -m 0600 \
  oidc.json client-secret deployment-seed.b64 tls.crt tls.key \
  deploy/private/identity/
```

Install the optional CA file with the same ownership and mode. The API mounts
these files read-only. Store this directory outside Git and the build context.

## Start the deployment and enroll the first owner

Run these commands from the source checkout. Keep `COMPOSE_FILE` set for later
commands against this installation.

```sh
export COMPOSE_FILE=deploy/compose.yaml:deploy/organization.compose.yaml
export RUNVERGE_HTTPS_PORT=8443
docker compose up --build --wait
docker compose --profile admin run --rm admin bootstrap-dedicated \
  --state-dir /state --name "My organization"
```

For team access, set `RUNVERGE_HTTPS_BIND` to the server's private address before
startup. Update the origin and callback when changing the browser-facing host or
port. Compose publishes only the HTTPS API and Workspace port. The readiness
check authenticates the configured certificate and checks the database.

The bootstrap prints the owner principal ID. Ask the identity administrator for
the owner's stable `sub` value for this exact issuer and client. Link it on the
trusted server.

```sh
docker compose --profile admin run --rm admin link-identity \
  --issuer https://login.example.test \
  --subject OWNER_SUBJECT --principal OWNER_PRINCIPAL_ID
```

Open the HTTPS Workspace and choose **Sign in through your organization**.
In **Members**, add the task author and reviewer as regular members. Link each
member's exact issuer and subject with the same host command. Email addresses
and group claims are display information. RunVerge assigns access from the
organization membership.

After confirming the owner's SSO sign-in, use **Security** to require this issuer
for human logins. The dedicated bootstrap keeps its recovery material in the
private admin volume. Employees use the corporate sign-in screen.

## Run a task and remove access

Configure one model and publish an agent using your organization's model settings.
The author starts a task, opens the saved draft, and chooses **Send to a colleague
for review**. The colleague signs in through the same issuer, opens **Assigned to
me**, and records a decision. The author sees it in the task history.

In **Members**, edit a participant and turn off **Allow access to this
organization** to remove their organization access. Revoking old credentials
ends existing sessions and still lets an active member sign in again. Disabling
the membership also keeps the organization unavailable on a new sign-in.

Keep the original database volume, private files and project settings together
across restarts. The [Linux server guide](LINUX-SERVER.md) describes database and
model setup. This profile covers a single deployment and an explicit OIDC
mapping per person. Provider-specific compatibility and production recovery
have separate acceptance requirements.
