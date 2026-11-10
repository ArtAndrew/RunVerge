# RunVerge team pilot on Linux

Use this profile on a trusted Linux host with Docker Engine and Docker Compose
with `up --wait` support. Use synthetic or sanitized test data. Check the server
with `uname -m`: `x86_64` is Linux amd64 (Intel or AMD), and `aarch64` is Linux
arm64. Choose the image for the server's architecture.

With a source checkout, run one command from the repository root:

```sh
docker compose -f deploy/pilot.compose.yaml up --build --wait --wait-timeout 900
```

The first source startup needs internet access to build the application and
download 1.12 GB of local model weights. Allow at least 4 GB of available RAM
and enough disk space for build layers, the model and PostgreSQL.

If you received an image archive and deployment archive from a verified build,
use the files for your server's architecture together:

```sh
sha256sum -c SHA256SUMS
mkdir runverge-pilot
tar -xzf runverge-pilot-deploy.tar.gz -C runverge-pilot
arch=amd64 # Use arm64 for an aarch64 server.
docker image load -i "runverge-pilot-linux-$arch-image.tar.gz"
cd runverge-pilot
docker compose -f deploy/pilot.compose.yaml up --no-build --wait --wait-timeout 900
```

The loaded image removes the application build from the first startup. Compose
still pulls the dependency images and downloads the verified model weights.

Compose prepares a fresh PostgreSQL database with separate database administrator
and application roles. The pilot organization has an owner, two regular members
and a published agent named **Pilot · Draft an announcement**. The agent uses Qwen2.5 1.5B on the
server's CPU. No model account or API key is needed. RunVerge downloads the weights
from [Qwen's pinned model revision](https://huggingface.co/Qwen/Qwen2.5-1.5B-Instruct-GGUF/tree/91cad51170dc346986eccefdc2dd33a9da36ead9)
and checks their SHA-256 before startup. Tasks start only when a user submits them.

## Sign in and review a result

Retrieve the three private handoff files on the trusted host. Compose keeps them in
a volume mounted only in the setup service that creates the accounts. Choose a
new private destination each time you copy them.

```sh
access_dir=$(mktemp -d)
docker compose -f deploy/pilot.compose.yaml cp seed:/state/bootstrap.json "$access_dir/owner.json"
docker compose -f deploy/pilot.compose.yaml cp seed:/state/contributor.json "$access_dir/contributor.json"
docker compose -f deploy/pilot.compose.yaml cp seed:/state/reviewer.json "$access_dir/reviewer.json"
```

1. Open [http://127.0.0.1:8080](http://127.0.0.1:8080). From another computer, first
   use your existing SSH access, for example `ssh -N -L 8080:127.0.0.1:8080 your-server`.
   Only the Workspace port is published, on loopback. Keep this pilot behind that tunnel.
2. Sign in as the owner with `organization_id` and `token` from `owner.json`.
   Keep the default API address `/api/v1`. Use the owner account to manage members.
3. Open a second browser profile and sign in with `contributor.json`. Start a new
   task, choose an example under **Start with an example**, keep the preselected
   published agent, and create the task.
4. When the draft is ready, open **Send to a colleague for review** in the result.
   Select **Pilot reviewer** and choose **Assign reviewer**.
5. In a separate browser profile, sign in with `reviewer.json`. Open **Assigned to me**,
   compare the draft with the task facts, then accept it or request changes.
6. Return to the contributor's task to see the colleague's saved decision.

Each member key lasts 24 hours. Use the original organization and member principal
IDs to issue a fresh key when needed:

```sh
docker compose -f deploy/pilot.compose.yaml run --rm --no-deps seed issue-token \
  --organization ORGANIZATION_ID --principal MEMBER_PRINCIPAL_ID \
  --ttl 24h --out /state/member-next.json
docker compose -f deploy/pilot.compose.yaml cp seed:/state/member-next.json "$access_dir/member-next.json"
```

Treat the three files as separate credentials. Delete local copies
you no longer need and use **Members** to revoke access at the end of a trial.
The local model has no provider tariff. Its answers still need a person's review,
and the host's hardware and electricity costs are separate.

## Operate the pilot

- Keep the original volumes together. A repeat of the same startup command uses the
  original credentials and configuration. An operator can change the Workspace port
  with `RUNVERGE_PILOT_PORT`.
- Use `docker compose -f deploy/pilot.compose.yaml stop` to pause the services. Use
  the startup command above to resume the same installation.
- This profile uses local token sign-in and organization-wide task visibility.
  Once assigned, the selected colleague has the decision on that exact draft.
  Unassigned drafts follow the existing owner and member permissions. If you
  started an earlier pilot, add the reviewer through **Members**.
