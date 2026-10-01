# 0001 — Local environment with high parity to GCP

- **Status:** Accepted
- **Date:** 2026-09-29

## Context

The agent is designed to run on GCP (Cloud Run Jobs, Cloud Scheduler,
Cloud SQL, Secret Manager), but this project is built and evaluated locally:
the cloud infrastructure is described in Terraform and tested, never applied.

That only works if "it passes locally" is strong evidence that "it would work
in production". The usual ways it fails:

- Toolchain drift: a different Go, psql or Terraform version on each machine
  and in CI.
- Runtime drift: code runs one way on a laptop and another way in a container.
- Configuration drift: the app reads secrets or settings through paths that
  only exist in one environment.
- Hidden global state: global gcloud credentials, shared Docker volumes, ports
  or tools installed system-wide by another project.

## Decision

Provision the project in three layers, each pinned by a content hash.

### 1. Toolchain: Nix flake + direnv

`flake.nix` declares a devShell with every tool (Go, golangci-lint, goose,
psql, Terraform, tflint, trivy, gcloud, Ollama, actionlint, git, make, jq).
`flake.lock` pins the exact nixpkgs commit (`rev`) and a hash of its contents
(`narHash`), so a laptop, CI and any contributor's machine resolve the same
versions. CI runs the same shell: `nix develop --command make check`.

direnv loads the devShell on `cd` and unloads it on leave. `.envrc` also:

- sets `CLOUDSDK_CONFIG=$PWD/.gcloud`, so this repo never sees global gcloud
  credentials;
- sets `COMPOSE_PROJECT_NAME`, so containers, networks and volumes are
  prefixed and cannot collide with other projects;
- loads `.env` (gitignored) for local secrets.

`GOTOOLCHAIN=local` stops `go` from silently downloading a different
toolchain.

### 2. Runtime: containers via Docker Compose

What runs in production is a container image, not the Nix shell. Locally,
`compose.yaml` runs the same images, standing in for each GCP service:

| GCP (production)   | Local stand-in                        | Parity                          |
|--------------------|---------------------------------------|---------------------------------|
| Cloud Run Job      | Same container image                  | Identical                       |
| Cloud SQL (PG 18)  | `postgres:18.6` (Debian) container    | High (TCP instead of connector) |
| Secret Manager     | `.env` → environment variables        | Same contract                   |
| Cloud Scheduler    | Local scheduler container             | Medium (no IAM)                 |
| Pub/Sub (optional) | Official Pub/Sub emulator             | High                            |
| Cloud Logging      | JSON `slog` on stdout                 | Same format                     |
| Cloud Trace        | OpenTelemetry collector + Jaeger      | Same OTLP protocol              |
| Hosted LLM APIs    | Ollama, native on the host            | Same API shape                  |

The application follows 12-factor rules: it only knows environment
variables, stdout and a database DSN. It never calls the Secret Manager API;
Cloud Run resolves pinned secret versions into environment variables.
Migrations run as a separate job, locally (`make migrate`) and in production.

The Postgres image is the Debian variant, not Alpine: Alpine's musl libc
handles locales and collation differently, which affects the full-text
search used by `search_policy`.

All services bind to `127.0.0.1` on a dedicated port block (1xxxx) to avoid
ports reserved by other projects on the same machine.

### 3. Infrastructure: Terraform, tested but not applied

`infra/terraform` (added in phase 6) describes the production resources. CI runs `fmt`,
`validate`, `tflint`, `trivy config` and `terraform test` with mocked
providers. Green tests are the proof; nothing is applied.

### Cross-cutting rule: trust hashes, not names

Every external dependency is pinned by a content hash rather than a movable
name:

| Dependency         | Movable name              | Pinned by                   |
|--------------------|---------------------------|-----------------------------|
| Toolchain          | `nixpkgs-unstable` branch | `rev` + `narHash` in `flake.lock` |
| Container images   | `postgres:18.6` tag       | `@sha256:` digest           |
| GitHub Actions     | `@v7` tag                 | Commit SHA                  |

A tag can be moved to different content by its owner or by an attacker (as
happened to `tj-actions/changed-files` in March 2025); a hash cannot.

## Consequences

### Positive

- One command set for everyone: `make up`, `make migrate`, `make seed`,
  `make check` behave the same on a laptop and in CI.
- Nothing is installed globally except Nix and direnv. Leaving the directory
  restores the shell exactly.
- Upgrades are explicit and reviewable: `nix flake update` or a new digest
  shows up as a diff, and `git revert` undoes it.
- `make check` runs its database checks in a throwaway `triage_check`
  database, so it never touches development data.

### Negative

- **Nix learning curve.** Contributors must install Nix and direnv once.
- **Flakes are still marked experimental upstream** and must be enabled
  (`experimental-features = nix-command flakes`). They are the de facto
  standard, so the risk of a breaking change is low but not zero.
- **Pinning means no automatic updates**, including security fixes. Needs a
  bot (Dependabot or Renovate) once the repo is on GitHub.
- **Two sources for the Go version**: the flake and the Dockerfile's builder
  image. CI must check they match.

### Known parity gaps

- **Scheduler invocation.** Locally a container calls the job directly; in
  GCP, Cloud Scheduler invokes it with a service-account token. IAM and
  authentication are covered only by Terraform tests.
- **Cloud SQL connector.** Locally the app uses plain TCP; in production the
  connection goes through the Cloud SQL connector or Auth Proxy.
- **Quotas and limits** (Cloud Run timeouts, API rate limits) do not exist
  locally.
- **Local LLM on the host, not in Docker.** Docker on macOS runs in a Linux
  VM without access to the Mac's GPU (Metal), so Ollama runs natively and
  containers reach it over the network.
- **macOS `/usr/bin` shims break inside the devShell.** The devShell points
  `DEVELOPER_DIR` at Nix's Apple SDK, so Apple's `xcrun`-based shims (such
  as `/usr/bin/git`) fail with `error: tool 'git' not found`. Any such tool
  the project needs goes into the flake; `git` already is.
- **GNU vs BSD userland.** Inside the devShell, tools like `sed` are the GNU
  versions, not macOS's BSD ones (`sed -i ''` fails). Scripts must target
  the devShell, which also matches Linux CI.

## Alternatives considered

| Alternative                          | Why not                                                                 |
|--------------------------------------|-------------------------------------------------------------------------|
| Homebrew / asdf / mise               | Pins versions, but not the whole dependency graph; shared global state. |
| Dev containers                       | Full isolation, but heavy on macOS and no GPU access for local models. |
| Toolchain setup actions in CI        | A second source of versions that drifts from local.                    |
| Real GCP dev project                 | Highest fidelity, but costs money and needs credentials to contribute. |
| Nix-built container image (`dockerTools`, `nix2container`) | Single source of truth for Go, but niche and harder to review; a standard multi-stage Dockerfile is the common default. |
