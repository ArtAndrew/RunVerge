# RunVerge 0.14.0-alpha.8

A Linux preview for teams that want an agent to draft something and a colleague to decide what to use.

- **Two Linux packages.** Choose AMD64 for Intel or AMD, or ARM64 for Arm. Each archive includes a Docker image, Compose files and the installation guides.
- **A ready team pilot.** Compose starts PostgreSQL and a local CPU model, sets up an owner and two members, and publishes a sample agent. The first start downloads about 1.12 GB of model weights.
- **Review by a colleague.** The author sends one saved draft to a named reviewer. The reviewer opens **Assigned to me** and accepts the draft or sends it back with feedback.
- **Organization sign-in.** The separate HTTPS profile uses your OpenID Connect provider and explicit user-to-membership links. An owner can disable a member's access.
- **Seven languages.** The README and workflow diagrams are available in English, Russian, Spanish, Brazilian Portuguese, German, French and Simplified Chinese.

## Get started

Unpack the package for your architecture, and run these commands.

```sh
docker image load -i image.tar.gz
docker compose -f deploy/pilot.compose.yaml up --no-build --wait --wait-timeout 900
```

[Team pilot](COMPOSE-PILOT.md) · [Organization sign-in](ORGANIZATION-COMPOSE.md) · [Support scope](SUPPORT.md)

This is a prerelease for fresh installations and test data. We exercised the organization profile with a disposable OIDC issuer and a deterministic local model fixture on native Linux. For the earlier macOS ARM64 bundle, use [alpha.7](https://github.com/ArtAndrew/RunVerge/releases/tag/0.14.0-alpha.7).
