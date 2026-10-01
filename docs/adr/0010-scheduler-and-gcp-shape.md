# 0010 — Scheduled batch jobs, locally and on GCP

- **Status:** Accepted
- **Date:** 2026-09-30

## Context

Both agents are batch workers: one execution claims up to `BATCH_SIZE`
items, finalizes them under a fenced lease (internal/queue) and exits. They
need a trigger every few minutes, locally and in the cloud, with the same
image and the same configuration (environment variables). The local stack
must not need a docker socket, a third-party scheduler image, or Ollama for
`make up` and CI.

## Decision

1. **One image, every command.** The Dockerfile builds `./cmd/...` into
   `/app/` (`commerce-api`, `worker`, `migrate`; not `eval`, see the amendment). Compose services
   and Cloud Run pick a binary with the entrypoint, so one digest is built,
   scanned and deployed.
2. **Local scheduler = `worker schedule`.** Every `-every` it runs the jobs
   (`-jobs=support,orders`) in order, each as a fresh subprocess of the same
   binary, like a Cloud Run Job execution. It forwards the environment, logs
   each exit code, keeps ticking after a failed job, and on SIGTERM passes
   SIGTERM to the running job (which stops claiming, finishes its item and
   closes the run) and kills it after `-grace`. `-overlap=skip` drops a tick
   while the previous one is still running.
3. **Cloud Scheduler does not skip overlapping fires**, and a fire can be
   retried. The queue makes that harmless: two executions claim with
   `FOR UPDATE SKIP LOCKED`, and finalize is fenced by `claimed_by` and
   `attempts`, so no item is finalized twice (the queue concurrency test).
   `-overlap=skip` is a local nicety, not a correctness requirement.
4. **Migrations are a job.** `cmd/migrate up` applies the embedded
   migrations (goose v3.28.0 library, `db.Migrations`), locally as the
   compose `migrate` one-shot and on GCP as the `migrate` Cloud Run Job run
   on deploy. Dev keeps the goose CLI; both share `goose_db_version`.
5. **Compose profiles.** `migrate` and `worker-scheduler` are in the
   `agents` profile (`make agents-up`, which also starts Jaeger): `make up`
   and CI never need Ollama, and `up --wait` does not trip on an exited
   one-shot. The scheduler reaches host Ollama at `host.docker.internal`.
6. **Shadow by default everywhere.** `AUTONOMY` defaults to `shadow` in the
   worker, compose pins it, and Terraform pins it on both worker jobs (a
   test checks it). There is no outbound email in this demo: `auto_replied`
   means "would be sent".
7. **Service-to-service auth.** commerce-api on Cloud Run has no `allUsers`
   invoker. The worker SA has `run.invoker` on that service only, and the
   worker attaches a metadata-server ID token when `COMMERCE_API_AUDIENCE`
   is set (a small RoundTripper, no new dependency).

## GCP shape and AWS equivalents

| GCP (infra/terraform) | AWS equivalent |
|---|---|
| Cloud Run Job (support-worker, orders-worker, migrate) | ECS Fargate scheduled task / AWS Batch |
| Cloud Run service (commerce-api) | ECS service or App Runner |
| Cloud Scheduler (POST `jobs/<job>:run`, OAuth token) | EventBridge Scheduler |
| Cloud SQL for PostgreSQL 18 | RDS for PostgreSQL |
| Secret Manager (pinned versions) | Secrets Manager |
| Artifact Registry (immutable tags, deploy by digest) | ECR |
| Workload Identity Federation (GitHub OIDC) | IAM OIDC provider + `AssumeRoleWithWebIdentity` |
| Cloud Logging (the slog JSON fields) | CloudWatch Logs |
| Cloud Trace (OTLP; not wired yet, see the amendment) | X-Ray via ADOT (OTLP) |
| Pub/Sub (not used: the Postgres queue replaces it) | SQS/SNS |

## Consequences

- Latency to pick up a ticket is up to one interval (2 min locally, 5 min on
  GCP by default); fine for email support.
- The scheduler is about 150 lines, tested with a fake exec and a fake
  ticker. The real subprocess and SIGTERM path was checked by hand
  (`make agents-up`, then `make agents-down` during a job).
- Orders runs right after support in the same tick; on GCP the two jobs
  have separate schedules, so a task waits up to one interval longer.

## Alternatives considered

- **Ofelia or cron in a container:** needs the docker socket or a
  third-party image, and differs more from Cloud Run Jobs than a subprocess.
- **A long-running worker that polls:** simpler locally, but it is not the
  Cloud Run Job shape, and scale-to-zero is lost.
- **Pub/Sub push to a service:** at-least-once delivery still needs the DB
  fencing, and it adds a moving part the queue already covers.

## Amendment (2026-09-30, Slice 7 review and security audit)

- **Tracing on GCP is not on.** The jobs set no `OTEL_EXPORTER_OTLP_ENDPOINT`
  and no collector is deployed, so they export no traces; Cloud Logging has
  the slog JSON. Turning it on means an OpenTelemetry Collector (sidecar or
  service) exporting to Cloud Trace, then setting the endpoint on the jobs.
  Locally, Jaeger remains the trace backend.
- **The image ships `commerce-api`, `worker` and `migrate` only.** `eval`
  needs `evals/`, which `.dockerignore` excludes; it runs from the devShell.
- **One service account per job.** `support-worker` and `orders-worker`
  each run as their own SA with `run.invoker` on commerce-api, the database
  URL of the `worker_rw` role, and only the configured model provider's key
  (ADR-0015). `migrate` also runs `migrate purge`, the daily retention job
  (ADR-0014), under its own `purge` SA.
- **Worker batch knobs are set by Terraform** (`BATCH_SIZE`, `CONCURRENCY`,
  `ITEM_TIMEOUT`, `FINALIZE_TIMEOUT`, `LEASE`), and the task timeout is
  validated to cover a whole batch: `ceil(batch / concurrency) * (item +
  finalize)`, and at least one lease.
- **Migrations take a Postgres advisory lock** (goose session locker), so a
  retried deploy and a manual run cannot apply migrations concurrently.
- **Local scheduler shutdown**: compose passes `-grace=30s` and waits 45s
  (`stop_grace_period`) before SIGKILL.
