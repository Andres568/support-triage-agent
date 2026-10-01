# 0011 — Terraform: tested, linted and scanned, never applied here

- **Status:** Accepted
- **Date:** 2026-09-30

## Context

The project describes its GCP deployment (ADR-0010) but runs locally, and CI
has no cloud credentials. Unapplied Terraform rots unless something checks
it, and "it validates" says nothing about the properties that matter:
least privilege, no public invoker, pinned images and secrets, a protected
database.

## Decision

`make tf-check` (part of `make check`, so CI runs it too):

1. `terraform fmt -check`, `init -backend=false`, `validate`.
2. `terraform test`: every run is `command = plan` against
   `mock_provider "google"` with `override_during = plan`, so computed
   attributes (emails, URIs, connection names) have values and no API is
   called. The tests assert properties, not the resource list:
   - jobs: image pinned by digest, no `latest` secret version,
     `deletion_protection = false`, `AUTONOMY=shadow`, task timeout ≥ lease,
     `max_retries = 1`, migrate gets only `DATABASE_URL`;
   - IAM: the worker's only project role is `cloudsql.client`, the scheduler
     has no project role and invokes exactly the two worker jobs, migrate
     reads only `database-url`, WIF is limited to the repository, no
     `allUsers`/`allAuthenticatedUsers` anywhere;
   - SQL: POSTGRES_18, ENTERPRISE, ENCRYPTED_ONLY, no authorized networks,
     deletion protection, backups;
   - scheduler: the `jobs/<job>:run` URI, POST, the cloud-platform scope;
   - variables: `expect_failures` for a tag-only image, a `latest` secret
     version, and a task timeout shorter than the lease.
3. `tflint` with `tflint-ruleset-google` 0.40.0 and the terraform
   `recommended` preset.
4. `trivy config --exit-code 1`. One finding is ignored inline, on the
   `ip_configuration` block only: GCP-0017 (public IP). There are no
   authorized networks and TLS is required; Cloud Run connects through its
   Cloud SQL volume (the Auth Proxy, IAM-checked). A private IP needs a VPC
   and Private Service Access, cost and parts this demo does not need.

Secret values never enter state: Terraform creates the secret containers
only, versions are added out-of-band and pinned by number; the database
password is a write-only (`password_wo`) ephemeral variable.

## Consequences

- Plan-level tests catch wrong wiring and policy regressions, not API
  behavior (quotas, org policies, a tier unavailable in a region). The first
  real `apply` is still a risk; it is documented, not hidden.
- `init` and `tflint --init` download the provider and the ruleset once;
  CI caches `~/.terraform.d/plugin-cache` and `~/.tflint.d` and passes
  `GITHUB_TOKEN` to avoid rate limits. `.terraform.lock.hcl` is committed
  with linux_amd64 and darwin_arm64 hashes.

## Alternatives considered

- **Apply to a sandbox project in CI:** the strongest check, but it needs
  credentials, billing and cleanup, out of scope for a demo.
- **Terratest (Go):** needs a real apply; `terraform test` with mocks is
  native and runs offline after the first init.
- **Checkov instead of trivy:** similar coverage; trivy is already in the
  devShell and also scans images.

## Amendment (2026-09-30, Slice 7 review and security audit)

The test list above grew (15 runs). Beyond it they now assert: exactly the
two worker SAs invoke commerce-api (distinct mocked emails, so the check is
not vacuous); WIF pins repository id, owner id, `refs/heads/main`, the
`deploy.yml@refs/heads/main` workflow and the `prod` environment, and binds
on `attribute.repository_id`; `run.developer` is per job and service, not on
the project; each runtime reads only its own database URL and the workers
only the configured provider's key; backup and WAL retention; the purge job
and its daily schedule; `expect_failures` for a missing or extra secret key,
a local-only model provider, and a task timeout shorter than a batch. The
no-`allUsers` assertion is best effort (it lists the bindings this module
declares); trivy flags public IAM members on any resource. `make lint` adds
govulncheck and gitleaks; Dependabot proposes weekly updates.
