# ADR-0015: Security model: threats, what enforces what, and what we assume

- Status: Accepted
- Date: 2026-09-30

## Context

The Slice 7 security audit ran STRIDE over the system: two LLM agents that
read customer email (untrusted text) and act through tools, a read-only
commerce API, a Postgres queue holding customer PII and transcripts, and a
GCP deployment driven by GitHub Actions. This ADR records the threats, the
control that answers each and where it lives, and the assumptions the
controls rest on. The code-review and security-audit findings were fixed in the same
slice; this is the map, not the audit report.

## Threats and controls

| STRIDE | Threat | Control | Where |
|---|---|---|---|
| Spoofing | Any GitHub repo, workflow or branch mints deploy credentials | WIF condition: `repository_id`, `repository_owner_id`, `ref == refs/heads/main`, `job_workflow_ref == <repo>/.github/workflows/deploy.yml@refs/heads/main`, `environment == prod`; binding on `attribute.repository_id` | `infra/terraform/wif.tf`, tftest |
| Spoofing | Anyone calls commerce-api | No `allUsers`; `run.invoker` for the two worker SAs only; the worker sends a metadata-server ID token | `iam.tf`, `internal/commerce` |
| Spoofing | A sender claims someone else's email to read their orders | `tickets.sender_verified`; the support gate forces a human draft with no follow-ups when false | migration 00012, `internal/support` |
| Tampering | Prompt injection in a ticket makes an agent act | Shadow autonomy pinned in Terraform and compose; deterministic gates after the model (refund limits, draft-content gate, high-risk match); tools are read-only | `run.tf`, `internal/triage`, `internal/support`, `internal/orders` |
| Tampering | A compromised workload rewrites data outside its lane | Per-role DB users: `commerce_ro` (SELECT orders, policies), `worker_rw` (queues, runs, transcripts; no orders/policies), `purger` (DELETE old run_items, reads only `created_at`); owner only for migrate | migration 00013, `scripts/check-db-roles.sql` in `make db-check` |
| Tampering | A moved tag changes what runs | Images by digest (validated), base images and Actions by SHA/digest, secret versions by number (no `latest`) | `variables.tf`, `Dockerfile`, `compose.yaml`, `ci.yml` |
| Tampering | Two migrate runs race | goose Postgres session (advisory) lock | `cmd/migrate` |
| Repudiation | An agent decision cannot be explained later | `runs`/`run_items`/`run_steps` with prompt SHA, model, cost, transcript (capped) | `internal/obs`, ADR-0014 |
| Info disclosure | Transcripts (customer text + order details) kept forever | Daily `purge` job (30 days) + explicit backup/WAL retention (7 + 7 days) | `run.tf`, `scheduler.tf`, `sql.tf`, ADR-0014 |
| Info disclosure | Secrets in state, images, logs or git | Secret values added out-of-band; write-only DB passwords; `.dockerignore`; errors and logs carry no email/PII (tests); gitleaks in `make lint` | `secrets.tf`, `sql.tf`, `internal/commerce`, `internal/obs`, `Makefile` |
| Info disclosure | A workload reads keys it does not use | One SA per job; each reads only its own database URL; only the configured model provider's key exists and is mounted | `iam.tf`, `secrets.tf`, tftest |
| Info disclosure | Direct DB access over the network | Public IP with no authorized networks, `ENCRYPTED_ONLY`; Cloud Run connects via the Cloud SQL volume (`cloudsql.client`) | `sql.tf` |
| DoS / cost | A huge ticket or a runaway loop burns tokens | 32 KiB ticket cap, pre-flight input estimate, per-run cost cap, token budget, item timeout, task timeout validated against the batch, commerce-api `max_instance_count = 3` | migration 00012, `cmd/worker`, `internal/agent`, `variables.tf`, `run.tf` |
| Elevation | The deployer does more than roll images | `run.developer` per job and service (not project), `artifactregistry.writer` on the repo, `actAs` on the runtime SAs only; resources are created by a human apply (ADR-0011) | `iam.tf` |
| Elevation | Container escape as root | Distroless, `USER 65532:65532` | `Dockerfile` |
| Supply chain | Vulnerable or stale dependencies | govulncheck and trivy (config) in `make check`; Dependabot weekly; `GITHUB_TOKEN` only on the `tflint --init` step | `Makefile`, `.github/` |

## Assumptions (not enforced by code here)

1. **`customer_email` comes from an authenticated channel.** Intake must set
   `sender_verified = true` only after a DMARC pass on the inbound mail or a
   logged-in form; anything else inserts false. The column and the gate
   exist (migration 00012). The column defaults to false (migration 00015):
   an intake that forgets it gets a human draft with no follow-ups, and the
   seed sets true explicitly. worker_rw cannot UPDATE the column (00014).
2. **Model text is rendered as plain text.** Drafts and reasons are
   model-written from untrusted input. Any future UI must escape them (no
   HTML/Markdown rendering, no auto-linking) so an injected ticket cannot
   script the reviewer's browser.
3. **Local model weights are pinned by tag only.** Ollama pulls
   `qwen3:8b`/`qwen3:4b-instruct` by tag; a re-pushed tag changes the model
   under the evals. Acceptable for local and CI replay (cassettes pin
   outputs); production uses hosted APIs with dated model ids.
4. **Password DB auth is an interim.** The login users (`triage_commerce`,
   `triage_worker`, `triage_purger`) have write-only passwords held in
   Secret Manager. Next step: Cloud SQL IAM database authentication (a
   `CLOUD_IAM_SERVICE_ACCOUNT` user per SA, granted into the same roles),
   which removes the passwords. It needs the Cloud SQL Go connector (or the
   Auth Proxy with `--auto-iam-authn`) because Cloud Run's built-in Cloud SQL
   volume does not log in with IAM, so it was not done here.
5. **The deploy workflow exists before WIF is applied.** Terraform pins
   `deploy.yml` on `main` in the `prod` environment; the repository must
   create that workflow and environment (with required reviewers).
   Creating new Run jobs/services stays a human `terraform apply`.
6. **New tables get explicit grants.** 00013 sets no default privileges:
   each migration that adds a table grants it to the roles that need it,
   and `scripts/check-db-roles.sql` should gain a line for it.
7. **Cloud SQL built-in users are more than their roles (residual risk).**
   Every Cloud SQL built-in (password) user, including `triage_commerce`,
   `triage_worker` and `triage_purger`, is a member of `cloudsqlsuperuser`,
   which has CREATEROLE and CREATEDB. The grants of 00013/00014 bound what
   they read and write in this database, but a leaked password could still
   create roles or databases, and could reach objects owned by or granted
   to `cloudsqlsuperuser`. Migration 00014 removes what it can without
   verifying Cloud SQL itself: the `public` schema is owned by the migrating
   user when it may take it (else a NOTICE, and the owner is kept), PUBLIC
   has no CREATE there, and PUBLIC keeps only CONNECT on the database. None
   of this was verified against Cloud SQL (local Postgres has no
   `cloudsqlsuperuser`); `scripts/check-db-roles.sql` proves only the local
   grants. The real fix is the IAM database users of assumption 4
   (`CLOUD_IAM_SERVICE_ACCOUNT`), which are not members of
   `cloudsqlsuperuser`; it is the next step.
8. **Login users get their roles on every migrate run.** 00013 granted only
   users that existed when it ran; `migrate up` (the prod migrate job) now
   also runs `migrate grants`, which grants each existing login user its
   group role (idempotent, missing users skipped).
9. **Spend is bounded per run, not per day, in code.** Terraform sets
   `MAX_RUN_COST_MICROS` (`var.max_run_cost_micros`, default 1 USD, 0
   rejected), which stops one execution from claiming more work once spent;
   in-flight items can overshoot it by up to CONCURRENCY items. With two
   jobs every five minutes that still allows several hundred USD a day in
   the worst case, so the daily bound is outside the code: a spend limit on
   the provider account (Anthropic/OpenAI/xAI console) and a GCP billing
   budget with alerts on the project. Neither is in Terraform here (the
   billing account is not part of this module); set both before enabling
   a paid model.

## Consequences

- Positive: every control has a named place and most have a test (tftest,
  `check-db-roles.sql`, Go tests), so a regression fails `make check`.
- Negative: more moving parts at deploy time (four database URLs, three
  login users, per-job SAs). The assumptions above are the residual risk.
- Not covered: network egress control from the jobs (no VPC), and PII
  redaction inside transcripts (ADR-0014 bounds their lifetime instead).

## Alternatives considered

- **One database user for everything.** Simplest; a prompt-injected worker
  bug or leaked worker URL would expose orders and allow deletes. Rejected.
- **Project-level `run.developer` for the deployer.** Lets CI create any Run
  resource in the project. Rejected for per-resource grants.
- **Repository name in the WIF condition.** Names can be re-registered after
  a rename or deletion; numeric ids cannot.
