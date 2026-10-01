# 0014 — Run transcripts: what they hold, how big, how long

- **Status:** Accepted
- **Date:** 2026-09-30

Numbered 0014, not 0002: the design reserves the low numbers for the ADRs it
already plans (0002 is the queue).

## Context

Every attempt at a ticket or task writes a `run_items` row, and that row keeps
the attempt's transcript (`agent.Result.Transcript`) so a bad decision can be
replayed and explained. The column was documented as "contains customer text",
which undersold it. A transcript holds:

- the ticket text: customer names, emails, whatever they wrote;
- every tool output: commerce order details (shipping addresses, items,
  totals) and policy text;
- one copy per attempt, so a retried ticket is stored several times.

Nothing bounded either its size or its age. Spans are a different matter: they
leave for a trace backend with other access rules, so they never carry
message text (only `error.type`); rows stay in our database, next to the
tickets they describe.

## Decision

1. **Say what it holds.** Migration 00009 comments `run_items.transcript` as
   ticket text and commerce tool output, per attempt.
2. **Cap it.** Above `obs.MaxTranscriptBytes` (64 KiB of JSON) the transcript
   is not stored: the column is NULL and `transcript_truncated` is true. A
   normal item is a few KiB; one that big is a runaway loop, and its row
   still has steps, tokens, cost and error. Stored error text is capped at
   `obs.MaxErrorBytes` (1 KiB).
3. **Expire it.** `make purge-runs` deletes `run_items` older than
   `RETENTION_DAYS` (default 30); `run_steps` go with them (ON DELETE
   CASCADE). In production it would run as a scheduled job next to the
   workers.

## Consequences

- Positive: the column's contents are stated where people query it; one row
  can no longer grow without bound; transcripts, the widest copy of customer
  data, live 30 days instead of forever.
- Negative: a transcript over the cap is lost for debugging (the row says so);
  after 30 days an old decision can be explained only from its ticket or task
  record and the prompt SHA of its run, not replayed. Cost history in
  `run_items` goes with it; `runs` rows stay.
- The purge is manual locally (`make purge-runs`); on GCP it is enforced
  (amendment below).

## Alternatives considered

- **No transcript at all.** Smallest risk, but a wrong decision could not be
  explained; the eval cassettes cover only the golden set.
- **Truncate the JSON at the cap.** Keeps a prefix, but a cut transcript is
  invalid JSON and misleads more than it helps.
- **Redact PII before storing.** Worth doing eventually; reliable redaction of
  free text is its own project, and retention bounds the exposure meanwhile.
- **Transcripts in object storage with a lifecycle rule.** The GCP-native
  shape at scale; overkill for the row counts here.

## Amendment (2026-09-30, Slice 7 security audit)

- **Enforced on GCP.** The `purge` Cloud Run Job runs `migrate purge
  -days=<retention_days>` (the same DELETE as `make purge-runs`) daily from
  Cloud Scheduler (`purge_schedule`, default 03:17 UTC). It connects as
  `triage_purger`, in the `purger` role (migration 00013): DELETE on
  `run_items`/`run_steps` and SELECT on `run_items.created_at` only, so the
  job that deletes transcripts cannot read them.
- **Backups extend the window.** Cloud SQL backups and WAL hold every row,
  purged ones included. Terraform sets both explicitly: 7 retained daily
  backups and 7 days of transaction logs for point-in-time recovery. A
  purged transcript is therefore gone from the live database after
  `retention_days` (30) and from every backup at most about 7 days later:
  the effective retention is ~37 days. Shortening it means fewer backups
  or less PITR, a recovery trade-off, not a free change.
