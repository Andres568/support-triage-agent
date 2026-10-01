# 0013 — One container image with several entrypoints

- **Status:** Accepted
- **Date:** 2026-09-30

## Context

Production runs three programs: the commerce API (a service), the workers
(jobs, one per agent) and migrations (a job; also the purge job, ADR-0014).
Every image must be pinned by digest (ADR-0001), scanned, and deployed by
CI. One image per program multiplies builds, digests to pin in Terraform
and chances that programs from different commits run together.

## Decision

- The Dockerfile builds `./cmd/commerce-api`, `./cmd/worker` and
  `./cmd/migrate` into `/app/` in one multi-stage build
  (`CGO_ENABLED=0`, `-trimpath`). Compose services and Cloud Run pick a
  binary by entrypoint (e.g. `/app/worker`); the commerce-api healthcheck
  is `/app/commerce-api healthcheck`.
- Runtime base: `gcr.io/distroless/static-debian13:nonroot`, pinned by
  digest, running as `USER 65532:65532` (numeric, so `runAsNonRoot` checks
  can verify it). No shell, no package manager.
- `cmd/eval` is not shipped: it needs `evals/` (cases, cassettes), which
  `.dockerignore` excludes; it runs from the devShell.

## Consequences

- Positive: one digest to build, scan, pin and roll back; every component
  of a deploy comes from the same commit; a small attack surface.
- Negative: every program ships with every other's code, and a change to
  any one rebuilds and redeploys the image for all. No shell makes
  debugging inside a running container harder (by design).

## Alternatives considered

- **One image per command:** smaller images, but three digests that can
  drift apart and three times the CI and Terraform wiring.
- **A single binary with subcommands:** one entrypoint, but merges
  unrelated flag sets and dependencies into one program; separate binaries
  keep each `main` small.
- **A distro base (Debian slim, Alpine):** convenient for debugging, at the
  cost of a shell and packages to patch.
