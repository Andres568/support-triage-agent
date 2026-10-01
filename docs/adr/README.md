# Architecture Decision Records

One short document per significant decision: context, decision, consequences
and alternatives considered. Accepted ADRs are not edited; a new ADR
supersedes an old one.

| #    | Decision                                                  | Status   |
|------|-----------------------------------------------------------|----------|
| 0001 | [Local environment with high parity to GCP](0001-local-parity.md) | Accepted |
| 0002 | [Postgres work queue with leases and a (run, attempt) fencing token](0002-postgres-work-queue.md) | Accepted |
| 0003 | [Agent failures escalate; infrastructure failures retry](0003-agent-failures-escalate.md) | Accepted |
| 0004 | [Two agents joined by a transactional outbox of structured tasks](0004-transactional-outbox-handoff.md) | Accepted |
| 0005 | [Orders agent sees exactly one order](0005-orders-agent-one-order.md) | Accepted |
| 0006 | [Two provider adapters: Anthropic Messages and stateless OpenAI Responses](0006-provider-adapters.md) | Accepted |
| 0007 | [Cost in integer micro-USD from a dated price table, frozen at write time](0007-cost-micro-usd.md) | Accepted |
| 0008 | [Postgres runs/run_items/run_steps are the source of truth; OTel spans are a view](0008-runs-are-source-of-truth.md) | Accepted |
| 0009 | [Evals: what we score, and what the CI gate can and cannot catch](0009-evals-and-ci-gate.md) | Accepted |
| 0010 | [Scheduled batch jobs, locally and on GCP (with AWS equivalents)](0010-scheduler-and-gcp-shape.md) | Accepted |
| 0011 | [Terraform: tested, linted and scanned, never applied here](0011-terraform-tested-not-applied.md) | Accepted |
| 0012 | [Untrusted ticket text is JSON-encoded in the prompt](0012-ticket-text-json-encoded.md) | Accepted |
| 0013 | [One container image with several entrypoints](0013-one-image-several-entrypoints.md) | Accepted |
| 0014 | [Run transcripts: what they hold, how big, how long](0014-run-transcript-retention.md) | Accepted |
| 0015 | [Security model: threats, what enforces what, and what we assume](0015-security-model.md) | Accepted |
